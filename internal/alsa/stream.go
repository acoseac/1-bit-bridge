package alsa

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Stream is a USB card's /proc/asound/cardN/streamM: for each direction
// the device streams in, the endpoint's live state and the altsets the
// device offers. Only USB cards have one; streamM belongs to PCM device M.
type Stream struct {
	// Playback is the "Playback:" section, nil when the device does not
	// play.
	Playback *StreamDirection `json:"playback"`
	// Capture is the "Capture:" section, nil when the device does not
	// record.
	Capture *StreamDirection `json:"capture"`
}

// StreamDirection is one section of a stream file.
type StreamDirection struct {
	// Status is the endpoint's state as printed: "Running" while the
	// device streams, "Stop" when it does not; empty when the line is
	// missing.
	Status string `json:"status"`
	// Running is what the kernel prints under "Status: Running", nil under
	// any other status.
	Running *RunningEndpoint `json:"running"`
	// Altsets are the device's alternate settings for this direction, in
	// the order the kernel lists them.
	Altsets []Altset `json:"altsets"`
}

// RunningEndpoint is the block a running endpoint prints: the altset the
// interface is set to, which is what the DAC is receiving, and the
// endpoint's measured rate. Each field is nil when its line is absent,
// which the kernel does when the stream has no format or data endpoint
// yet (and Feedback Format, when the endpoint has no feedback).
type RunningEndpoint struct {
	Interface  *int `json:"interface"`
	Altset     *int `json:"altset"`
	PacketSize *int `json:"packetSize"`
	// MomentaryFreqHz is the rate the endpoint is moving frames at, from
	// the device's feedback on an asynchronous endpoint. It is not the
	// nominal rate: a DSD256 stream at 352800 frames/s read 355053 on the
	// captured iFi.
	MomentaryFreqHz *int64 `json:"momentaryFreqHz"`
	// MomentaryFreq is the same rate in the kernel's own unit, frames per
	// USB (micro)frame in 16.16 fixed point, as printed: "0x2c.61b2".
	MomentaryFreq string `json:"momentaryFreq"`
	// FeedbackFormat is the feedback value's fixed-point format, "16.16".
	FeedbackFormat string `json:"feedbackFormat"`
}

// Altset is one alternate setting of a USB audio interface: one way the
// device can be fed. Interface and Altset are nil only in a file whose
// header lines did not parse.
type Altset struct {
	Interface *int `json:"interface"`
	Altset    *int `json:"altset"`
	// Formats are the ALSA sample formats the altset takes, every name on
	// its one Format line, in the kernel's order: ["S24_3LE"], or
	// ["SPECIAL", "DSD_U32_BE"] for a native-DSD one. SPECIAL is the
	// format name of SNDRV_PCM_FORMAT_SPECIAL, not a prefix.
	Formats  []string `json:"formats"`
	Channels *int     `json:"channels"`
	// Rates are the discrete rates, empty when the altset takes a
	// continuous range (RateRange) or lists none.
	Rates     []int      `json:"rates"`
	RateRange *RateRange `json:"rateRange"`
	// Bits is the altset's significant-bit count. Nil when the stream has
	// no Bits line: unknown, not a guess. Kernels before 5.1 never print
	// one; from 5.1 on they print one per altset for UAC1 and UAC2 alike.
	Bits *int `json:"bits"`
	// DSDRaw is present when the altset carries DSD without DoP framing
	// ("DSD raw: DOP=0, bitrev=0"), nil for every other altset.
	DSDRaw     *DSDRaw  `json:"dsdRaw"`
	ChannelMap []string `json:"channelMap"`
}

// DSDFormat returns the altset's DSD sample format, the first name with
// the kernel's DSD_ prefix, wherever that name sits on the Format line.
// SPECIAL is not one: a native-DSD altset lists it beside the DSD format.
// The result is empty when the altset offers no DSD format.
func (a Altset) DSDFormat() string {
	for _, name := range a.Formats {
		if strings.HasPrefix(name, "DSD_") && len(name) > len("DSD_") {
			return name
		}
	}
	return ""
}

// RateRange is an altset's continuous rate range ("8000 - 96000
// (continuous)").
type RateRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// DSDRaw is a native-DSD altset's framing: whether the kernel packs DSD
// into DoP frames for it, and whether it reverses each byte's bit order.
// A flag is nil when its value did not parse.
type DSDRaw struct {
	DOP    *bool `json:"dop"`
	Bitrev *bool `json:"bitrev"`
}

// numberedHeader matches the altset list's two header lines, which carry
// a number and no separator: "Interface 1" and "Altset 4".
var numberedHeader = regexp.MustCompile(`^(Interface|Altset) (\S+)$`)

// momentaryFreq matches "355053 Hz (0x2c.61b2)": the kernel prints
// "%u Hz (%#x.%04x)", and %#x of zero is "0", without the 0x.
var momentaryFreq = regexp.MustCompile(`^(\d+) Hz(?: \(((?:0x[0-9a-f]+|0)\.[0-9a-f]{4})\))?$`)

// feedbackFormat matches the "%d.%d" of "Feedback Format".
var feedbackFormat = regexp.MustCompile(`^\d+\.\d+$`)

// continuousRates matches "8000 - 96000 (continuous)".
var continuousRates = regexp.MustCompile(`^(\d+) - (\d+) \(continuous\)$`)

// altsetBuild is an Altset being read, with which of the lines the kernel
// always prints have been seen, so a cut-short entry can be reported.
type altsetBuild struct {
	Altset
	line int
	seen map[string]bool // the fields read, by key
}

// ParseStream parses a USB card's streamM file. See the package doc for
// how it treats lines it does not know or cannot read.
func ParseStream(b []byte) (Stream, []string) {
	var p problems
	var s Stream
	var dir *StreamDirection    // the section being read
	var cur *altsetBuild        // the altset being read
	var sawStatus bool          // the section's Status line has been read
	var runSeen map[string]bool // the endpoint block's lines read, by key
	// finish closes the altset being read, reporting each line the kernel
	// has printed for every altset since v4.19 that it never saw. Bits is
	// not one of them: proc_dump_substream_formats gained it in 5.1
	// (c7a13264918b) and prints it for UAC1 and UAC2 alike, so a stream
	// from before that has no Bits line and the count is unknown.
	finish := func() {
		if cur == nil {
			return
		}
		for _, need := range []string{"Format", "Channels", "Rates"} {
			if !cur.seen[need] {
				p.addf(cur.line, "%s has no %s line (cut short?)", describeAltset(cur.Altset), need)
			}
		}
		if cur.Altset.Altset == nil {
			p.addf(cur.line, "%s has no Altset line", describeAltset(cur.Altset))
		}
		dir.Altsets = append(dir.Altsets, cur.Altset)
		cur = nil
	}
	start := func(ln int) {
		finish()
		cur = &altsetBuild{
			Altset: Altset{Formats: []string{}, Rates: []int{}, ChannelMap: []string{}},
			line:   ln,
			seen:   map[string]bool{},
		}
	}
	ls := lines(b)
	if blank(ls) {
		p.addf(0, "empty file")
		return s, p.result()
	}
	for i, line := range ls {
		ln := i + 1
		t := strings.TrimSpace(line)
		switch {
		case t == "":
			continue
		case t == "Playback:" || t == "Capture:":
			finish()
			d := &StreamDirection{Altsets: []Altset{}}
			if t == "Playback:" {
				if s.Playback != nil {
					p.addf(ln, "a second Playback section; the first is kept")
					d = &StreamDirection{Altsets: []Altset{}} // read, then dropped
				} else {
					s.Playback = d
				}
			} else {
				if s.Capture != nil {
					p.addf(ln, "a second Capture section; the first is kept")
					d = &StreamDirection{Altsets: []Altset{}}
				} else {
					s.Capture = d
				}
			}
			dir, sawStatus, runSeen = d, false, map[string]bool{}
			continue
		case dir == nil:
			// The file's first line names the card and the PCM ("<long
			// name> : USB Audio"); nothing else comes before a section.
			if ln != 1 {
				p.addf(ln, "outside a Playback or Capture section: %s", quote(line))
			}
			continue
		}
		if status, ok := strings.CutPrefix(t, "Status:"); ok {
			switch {
			case cur != nil || len(dir.Altsets) > 0:
				p.addf(ln, "a Status line after the altsets began: %s", quote(line))
			case sawStatus:
				// The kernel prints one per section; a second would leave
				// a Running block under a status that is not Running.
				p.addf(ln, "a second Status line; the first is kept: %s", quote(line))
			case strings.TrimSpace(status) == "":
				sawStatus = true
				p.addf(ln, "a Status line with no state")
			default:
				sawStatus = true
				dir.Status = strings.TrimSpace(status)
				if dir.Status == "Running" {
					dir.Running = &RunningEndpoint{}
				}
			}
			continue
		}
		// The endpoint block's lines are "key = value" and the altsets'
		// "key: value"; whichever separator comes first decides, so a
		// colon line whose value holds " = " stays an altset line.
		if eq := strings.Index(t, " = "); eq >= 0 {
			if colon := strings.Index(t, ":"); colon < 0 || eq < colon {
				k, v := strings.TrimSpace(t[:eq]), strings.TrimSpace(t[eq+len(" = "):])
				parseRunningLine(&p, ln, dir, runSeen, cur != nil || len(dir.Altsets) > 0, k, v, line)
				continue
			}
		}
		if m := numberedHeader.FindStringSubmatch(t); m != nil {
			n, ok := atoiNonNeg(m[2])
			switch {
			case m[1] == "Interface":
				start(ln)
				if ok {
					cur.Interface = intPtr(n)
				} else {
					p.addf(ln, "interface number %s is not a number", quote(m[2]))
				}
			case cur == nil || cur.Altset.Altset != nil:
				// An Altset line with no Interface line before it: a new
				// entry whose interface is unknown.
				start(ln)
				p.addf(ln, "an Altset line with no Interface line before it")
				fallthrough
			default:
				if ok {
					cur.Altset.Altset = intPtr(n)
				} else {
					p.addf(ln, "altset number %s is not a number", quote(m[2]))
				}
			}
			continue
		}
		k, v, ok := cutKey(t, ":")
		if !ok {
			p.addf(ln, "not a stream line: %s", quote(line))
			continue
		}
		if cur == nil {
			// Before its first Interface line a section holds only its
			// Status line and the endpoint block's " = " lines, so an
			// altset field here belongs to no altset.
			if isAltsetKey(k) {
				p.addf(ln, "%s line before any Interface line", k)
			}
			continue
		}
		parseAltsetLine(&p, ln, cur, k, v)
	}
	finish()
	return s, p.result()
}

// runningKeys are the endpoint block's lines ParseStream reads.
var runningKeys = []string{"Interface", "Altset", "Packet Size", "Momentary freq", "Feedback Format"}

// parseRunningLine reads one line of a running endpoint's block. inList
// says the altset list has begun, after which no block line belongs.
func parseRunningLine(p *problems, ln int, dir *StreamDirection, seen map[string]bool, inList bool, k, v, line string) {
	if dir.Running == nil || inList {
		p.addf(ln, "an endpoint line outside a Running status block: %s", quote(line))
		return
	}
	if repeated(p, ln, seen, k, runningKeys) {
		return
	}
	r := dir.Running
	switch k {
	case "Interface", "Altset", "Packet Size":
		n, ok := atoiNonNeg(v)
		if !ok {
			p.addf(ln, "%s %s is not a number", k, quote(v))
			return
		}
		switch k {
		case "Interface":
			r.Interface = intPtr(n)
		case "Altset":
			r.Altset = intPtr(n)
		default:
			r.PacketSize = intPtr(n)
		}
	case "Momentary freq":
		m := momentaryFreq.FindStringSubmatch(v)
		if m == nil {
			p.addf(ln, "momentary freq %s is not \"N Hz (0xI.FFFF)\"", quote(v))
			return
		}
		hz, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			p.addf(ln, "momentary freq %s is out of range", quote(m[1]))
			return
		}
		r.MomentaryFreqHz = int64Ptr(hz)
		r.MomentaryFreq = m[2]
	case "Feedback Format":
		if !feedbackFormat.MatchString(v) {
			p.addf(ln, "feedback format %s is not \"N.N\"", quote(v))
			return
		}
		r.FeedbackFormat = v
	}
}

// altsetKeys are the altset fields ParseStream reads.
var altsetKeys = []string{"Format", "Channels", "Rates", "Bits", "DSD raw", "Channel map"}

// isAltsetKey reports whether k is one of altsetKeys.
func isAltsetKey(k string) bool { return slices.Contains(altsetKeys, k) }

// parseAltsetLine reads one "key: value" line of an altset entry. Keys it
// does not read (Endpoint, Data packet interval, the Sync lines) are
// ignored.
func parseAltsetLine(p *problems, ln int, cur *altsetBuild, k, v string) {
	if repeated(p, ln, cur.seen, k, altsetKeys) {
		return
	}
	switch k {
	case "Format":
		cur.Formats = strings.Fields(v)
	case "Channels", "Bits":
		n, ok := atoiNonNeg(v)
		if !ok {
			p.addf(ln, "%s %s is not a number", k, quote(v))
			return
		}
		if k == "Channels" {
			cur.Channels = intPtr(n)
		} else {
			cur.Bits = intPtr(n)
		}
	case "Rates":
		parseRates(p, ln, cur, v)
	case "DSD raw":
		cur.DSDRaw = parseDSDRaw(p, ln, v)
	case "Channel map":
		cur.ChannelMap = strings.Fields(v)
	}
}

// parseRates reads a Rates value: "44100, 48000, …" or "8000 - 96000
// (continuous)". A rate that does not parse is reported and left out; the
// ones around it are kept.
func parseRates(p *problems, ln int, cur *altsetBuild, v string) {
	if m := continuousRates.FindStringSubmatch(v); m != nil {
		lo, okLo := atoiNonNeg(m[1])
		hi, okHi := atoiNonNeg(m[2])
		if !okLo || !okHi {
			p.addf(ln, "continuous rate range %s is out of range", quote(v))
			return
		}
		cur.RateRange = &RateRange{Min: lo, Max: hi}
		return
	}
	if v == "" {
		return
	}
	for _, f := range strings.Split(v, ",") {
		f = strings.TrimSpace(f)
		n, ok := atoiNonNeg(f)
		if !ok {
			p.addf(ln, "rate %s is not a number", quote(f))
			continue
		}
		cur.Rates = append(cur.Rates, n)
	}
}

// parseDSDRaw reads "DOP=0, bitrev=0". The kernel prints each flag as %d
// of a bool, so only 0 and 1 are values.
func parseDSDRaw(p *problems, ln int, v string) *DSDRaw {
	d := &DSDRaw{}
	for _, f := range strings.Split(v, ",") {
		k, val, ok := cutKey(f, "=")
		var flag *bool
		switch val {
		case "0":
			flag = boolPtr(false)
		case "1":
			flag = boolPtr(true)
		}
		switch {
		case ok && k == "DOP" && flag != nil:
			d.DOP = flag
		case ok && k == "bitrev" && flag != nil:
			d.Bitrev = flag
		default:
			p.addf(ln, "DSD raw field %s is not DOP=0|1 or bitrev=0|1", quote(strings.TrimSpace(f)))
		}
	}
	return d
}

// describeAltset names an altset for a problem: "interface 1 altset 4",
// with "?" for a number that did not parse.
func describeAltset(a Altset) string {
	num := func(n *int) string {
		if n == nil {
			return "?"
		}
		return strconv.Itoa(*n)
	}
	return "interface " + num(a.Interface) + " altset " + num(a.Altset)
}
