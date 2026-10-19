package alsa

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// HWParams is a PCM substream's hw_params file: the parameters the
// process that opened it chose, which are what the device is being fed.
type HWParams struct {
	// Closed is true when the file reads "closed": no process has the
	// substream open, and every other field is empty.
	Closed bool `json:"closed"`
	// NoSetup is true when it reads "no setup": a process has it open and
	// has not chosen parameters yet.
	NoSetup bool `json:"noSetup"`
	// Access is how frames reach the buffer ("RW_INTERLEAVED",
	// "MMAP_INTERLEAVED").
	Access string `json:"access"`
	// Format is the sample format the device receives ("S24_3LE",
	// "DSD_U32_BE").
	Format    string `json:"format"`
	Subformat string `json:"subformat"`
	Channels  *int   `json:"channels"`
	// Rate is the frame rate. For a DSD format it counts the format's
	// words: DSD256 (11.2896 MHz) in DSD_U32_BE is 352800.
	Rate *int `json:"rate"`
	// RateNum and RateDen are the exact rate as a fraction, which the
	// kernel prints beside Rate: "352800 (352800/1)".
	RateNum    *int   `json:"rateNum"`
	RateDen    *int   `json:"rateDen"`
	PeriodSize *int64 `json:"periodSize"`
	BufferSize *int64 `json:"bufferSize"`
}

// Status is a PCM substream's status file: its state, the thread that
// holds it, and the buffer's counters.
type Status struct {
	// Closed is true when the file reads "closed": no process has the
	// substream open, and every other field is empty.
	Closed bool `json:"closed"`
	// Error is the errno the kernel printed ("error -19") when it could
	// not take the status; nil otherwise.
	Error *int `json:"error"`
	// State is the substream's state as printed: "RUNNING", "PREPARED",
	// "PAUSED", "XRUN", …
	State string `json:"state"`
	// OwnerPID is the id of the THREAD that opened the substream, as
	// seen from the reader's pid namespace (0 when it is not visible from
	// there). For MPD it is the output thread, not the process: the
	// reader resolves the process through the thread's Tgid.
	OwnerPID *int `json:"ownerPid"`
	// TriggerTime and Timestamp are the kernel's "%lld.%09lld" seconds,
	// as printed.
	TriggerTime string `json:"triggerTime"`
	Timestamp   string `json:"tstamp"`
	Delay       *int64 `json:"delay"`
	Avail       *int64 `json:"avail"`
	AvailMax    *int64 `json:"availMax"`
	HWPtr       *int64 `json:"hwPtr"`
	ApplPtr     *int64 `json:"applPtr"`
}

// hwRate matches hw_params' "352800 (352800/1)", printed "%u (%u/%u)".
var hwRate = regexp.MustCompile(`^(\d+) \((\d+)/(\d+)\)$`)

// kernelSeconds matches status' "%lld.%09lld" times.
var kernelSeconds = regexp.MustCompile(`^-?\d+\.\d{9}$`)

// errorLine matches status' "error %d", printed when taking it failed.
var errorLine = regexp.MustCompile(`^error (-?\d+)$`)

// ParseHWParams parses a substream's hw_params. The kernel prints all
// seven of its lines or one word, so a missing one is reported: the file
// was cut short.
func ParseHWParams(b []byte) (HWParams, []string) {
	var p problems
	var h HWParams
	ls := lines(b)
	if blank(ls) {
		p.addf(0, "empty file")
		return h, p.result()
	}
	if word, rest, ok := soleVerdict(ls); ok {
		switch word {
		case "closed":
			h.Closed = true
			reportTrailing(&p, rest, "closed")
			return h, p.result()
		case "no setup":
			h.NoSetup = true
			reportTrailing(&p, rest, "no setup")
			return h, p.result()
		}
	}
	seen := map[string]bool{}
	for i, line := range ls {
		ln := i + 1
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, v, ok := cutKey(line, ":")
		if !ok {
			p.addf(ln, "not a hw_params line: %s", quote(line))
			continue
		}
		if repeated(&p, ln, seen, k, hwParamsKeys) {
			continue
		}
		switch k {
		case "access", "format", "subformat":
			if v == "" || strings.ContainsAny(v, " \t") {
				p.addf(ln, "%s %s is not one word", k, quote(v))
				continue
			}
			switch k {
			case "access":
				h.Access = v
			case "format":
				h.Format = v
			default:
				h.Subformat = v
			}
		case "channels":
			n, ok := atoiNonNeg(v)
			if !ok {
				p.addf(ln, "channels %s is not a number", quote(v))
				continue
			}
			h.Channels = intPtr(n)
		case "rate":
			m := hwRate.FindStringSubmatch(v)
			if m == nil {
				p.addf(ln, "rate %s is not \"RATE (NUM/DEN)\"", quote(v))
				continue
			}
			rate, ok1 := atoiNonNeg(m[1])
			num, ok2 := atoiNonNeg(m[2])
			den, ok3 := atoiNonNeg(m[3])
			if !ok1 || !ok2 || !ok3 {
				p.addf(ln, "rate %s is out of range", quote(v))
				continue
			}
			h.Rate, h.RateNum, h.RateDen = intPtr(rate), intPtr(num), intPtr(den)
		case "period_size", "buffer_size":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				p.addf(ln, "%s %s is not a number", k, quote(v))
				continue
			}
			if k == "period_size" {
				h.PeriodSize = int64Ptr(n)
			} else {
				h.BufferSize = int64Ptr(n)
			}
		}
	}
	for _, k := range hwParamsKeys {
		if !seen[k] {
			p.addf(0, "no %s line (cut short?)", k)
		}
	}
	return h, p.result()
}

// hwParamsKeys are the lines an hw_params file prints when parameters are
// set, in the kernel's order.
var hwParamsKeys = []string{"access", "format", "subformat", "channels", "rate", "period_size", "buffer_size"}

// statusKeys are the lines a status file prints when the substream is
// open, in the kernel's order.
var statusKeys = []string{"state", "owner_pid", "trigger_time", "tstamp", "delay", "avail", "avail_max", "hw_ptr", "appl_ptr"}

// ParseStatus parses a substream's status. Like hw_params, it is either
// one line ("closed", "error N") or all of statusKeys; a missing key is
// reported.
func ParseStatus(b []byte) (Status, []string) {
	var p problems
	var st Status
	ls := lines(b)
	if blank(ls) {
		p.addf(0, "empty file")
		return st, p.result()
	}
	if word, rest, ok := soleVerdict(ls); ok {
		if word == "closed" {
			st.Closed = true
			reportTrailing(&p, rest, "closed")
			return st, p.result()
		}
		if m := errorLine.FindStringSubmatch(word); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				st.Error = intPtr(n)
				reportTrailing(&p, rest, word)
				return st, p.result()
			}
		}
	}
	seen := map[string]bool{}
	for i, line := range ls {
		ln := i + 1
		t := strings.TrimSpace(line)
		if t == "" || t == "-----" {
			continue
		}
		k, v, ok := cutKey(t, ":")
		if !ok {
			p.addf(ln, "not a status line: %s", quote(line))
			continue
		}
		if repeated(&p, ln, seen, k, statusKeys) {
			continue
		}
		switch k {
		case "state":
			if v == "" || strings.ContainsAny(v, " \t") {
				p.addf(ln, "state %s is not one word", quote(v))
				continue
			}
			st.State = v
		case "owner_pid":
			n, ok := atoiNonNeg(v)
			if !ok {
				p.addf(ln, "owner_pid %s is not a number", quote(v))
				continue
			}
			st.OwnerPID = intPtr(n)
		case "trigger_time", "tstamp":
			if !kernelSeconds.MatchString(v) {
				p.addf(ln, "%s %s is not \"SECONDS.NANOSECONDS\"", k, quote(v))
				continue
			}
			if k == "trigger_time" {
				st.TriggerTime = v
			} else {
				st.Timestamp = v
			}
		case "delay", "avail", "avail_max", "hw_ptr", "appl_ptr":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				p.addf(ln, "%s %s is not a number", k, quote(v))
				continue
			}
			switch k {
			case "delay":
				st.Delay = int64Ptr(n)
			case "avail":
				st.Avail = int64Ptr(n)
			case "avail_max":
				st.AvailMax = int64Ptr(n)
			case "hw_ptr":
				st.HWPtr = int64Ptr(n)
			default:
				st.ApplPtr = int64Ptr(n)
			}
		}
	}
	for _, k := range statusKeys {
		if !seen[k] {
			p.addf(0, "no %s line (cut short?)", k)
		}
	}
	return st, p.result()
}

// soleVerdict reports whether a file is a one-line verdict ("closed",
// "no setup", "error -19") rather than key lines: its first non-blank
// line has no colon. rest are the non-blank lines after it, which a
// verdict never has.
func soleVerdict(ls []string) (word string, rest []string, ok bool) {
	for i, l := range ls {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if strings.Contains(t, ":") {
			return "", nil, false
		}
		for _, r := range ls[i+1:] {
			if strings.TrimSpace(r) != "" {
				rest = append(rest, r)
			}
		}
		return t, rest, true
	}
	return "", nil, false
}

// repeated marks a known key seen, and reports whether it was seen before:
// the kernel prints each once, and a second one is reported and dropped
// rather than taken, so the first value stands. A key outside known is
// never repeated, since it is not read.
func repeated(p *problems, ln int, seen map[string]bool, k string, known []string) bool {
	if !slices.Contains(known, k) {
		return false
	}
	if seen[k] {
		p.addf(ln, "a second %s line; the first is kept", k)
		return true
	}
	seen[k] = true
	return false
}

// reportTrailing reports lines after a one-line verdict.
func reportTrailing(p *problems, rest []string, verdict string) {
	if len(rest) > 0 {
		p.addf(2, "%d line(s) after %q: %s", len(rest), verdict, quote(rest[0]))
	}
}
