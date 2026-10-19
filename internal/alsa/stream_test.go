package alsa

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// TestParseStreamOnEveryCapture reads the iFi's stream0 in each state the
// Pi was captured in: stopped, then playing native DSD256 on altset 4
// (DSD_U32_BE), 44.1/16 on altset 3 (S16_LE) and 96, 192 and 384 kHz at 24
// bit on altset 2 (S24_3LE), each with the endpoint's measured rate. The
// altset list is the same four in every capture.
func TestParseStreamOnEveryCapture(t *testing.T) {
	for _, c := range captures {
		t.Run(c.name, func(t *testing.T) {
			s, probs := ParseStream(readFixture(t, c.name, "asound/card5/stream0"))
			if len(probs) != 0 {
				t.Errorf("problems on a captured file: %q", probs)
			}
			if s.Capture != nil {
				t.Errorf("a playback-only DAC read with a Capture section: %+v", s.Capture)
			}
			p := s.Playback
			if p == nil {
				t.Fatal("no Playback section")
			}
			if !c.running {
				if p.Status != "Stop" || p.Running != nil {
					t.Errorf("idle: status %q, running %+v; want Stop and no running block", p.Status, p.Running)
				}
			} else {
				r := p.Running
				if p.Status != "Running" || r == nil {
					t.Fatalf("status %q, running %+v; want a Running block", p.Status, r)
				}
				if deref(r.Interface) != 1 || deref(r.Altset) != c.altset || deref(r.PacketSize) != c.packetSize ||
					deref(r.MomentaryFreqHz) != c.freqHz || r.MomentaryFreq != c.freqRaw || r.FeedbackFormat != "16.16" {
					t.Errorf("running = interface %d altset %d packet %d freq %d (%s) feedback %q; want 1, %d, %d, %d (%s), 16.16",
						deref(r.Interface), deref(r.Altset), deref(r.PacketSize), deref(r.MomentaryFreqHz), r.MomentaryFreq,
						r.FeedbackFormat, c.altset, c.packetSize, c.freqHz, c.freqRaw)
				}
			}
			if len(p.Altsets) != len(capturedAltsets) {
				t.Fatalf("%d altsets, want %d", len(p.Altsets), len(capturedAltsets))
			}
			for i, want := range capturedAltsets {
				a := p.Altsets[i]
				if deref(a.Interface) != 1 || deref(a.Altset) != want.altset || !reflect.DeepEqual(a.Formats, want.formats) ||
					deref(a.Channels) != 2 || deref(a.Bits) != want.bits || !reflect.DeepEqual(a.Rates, capturedRates) ||
					a.RateRange != nil || !reflect.DeepEqual(a.ChannelMap, []string{"FL", "FR"}) {
					t.Errorf("altset %d = %+v", want.altset, a)
				}
				switch {
				case want.dsdRaw && (a.DSDRaw == nil || a.DSDRaw.DOP == nil || *a.DSDRaw.DOP || a.DSDRaw.Bitrev == nil || *a.DSDRaw.Bitrev):
					t.Errorf("altset %d's DSD raw = %+v, want DOP=0 bitrev=0", want.altset, a.DSDRaw)
				case !want.dsdRaw && a.DSDRaw != nil:
					t.Errorf("altset %d is PCM and read as DSD raw %+v", want.altset, a.DSDRaw)
				}
			}
		})
	}
}

// TestParseStreamOrdersProblemsByLineWhenTheCapIsReached: a cut-short
// report is filed when the altset ends, on the line the altset began,
// which is earlier than the problems already filed. At the cap those
// kept lines are still in line order, and the count of the ones left
// out is last.
func TestParseStreamOrdersProblemsByLineWhenTheCapIsReached(t *testing.T) {
	// Seven problems on the first altset, then its missing Format line
	// (filed when the next altset starts, on the earlier line), which
	// fills the cap. The second altset's problems are the ones left out.
	in := "Playback:\n  Interface 1\n    Altset 1\n    Channels: x\n" +
		"    Rates: a, b, c, d, e, f\n    Bits: 16\n" +
		"  Interface 1\n    Altset 2\n    Channels: y\n"
	_, probs := ParseStream([]byte(in))
	if len(probs) != maxProblems+1 || !strings.HasSuffix(probs[len(probs)-1], " more not shown") {
		t.Fatalf("problems =\n%s\nwant %d entries and a count last", strings.Join(probs, "\n"), maxProblems+1)
	}
	prev := 0
	for _, p := range probs[:len(probs)-1] {
		var line int
		if _, err := fmt.Sscanf(p, "line %d:", &line); err != nil || line < prev {
			t.Fatalf("problems out of line order at %q:\n%s", p, strings.Join(probs, "\n"))
		}
		prev = line
	}
	if !strings.Contains(probs[0], "no Format line") {
		t.Errorf("first problem = %q; the cut-short report is on the altset's first line", probs[0])
	}
}

// TestParseStreamReportsTheGarbledFixture reads testdata's garbled stream0:
// an endpoint block with two values that do not parse, a rate that is not
// a number, a line no stream file has, a DSD raw flag with no value, and
// a file cut off in the middle of a line. The truncated "Bi" is not a
// stream line; a Bits line that never arrived is unknown, not cut short.
// Each defect is reported where it is, the values around them are kept,
// and none of the broken ones is guessed.
func TestParseStreamReportsTheGarbledFixture(t *testing.T) {
	s, probs := ParseStream(readFixture(t, "garbled-stream0", "asound/card0/stream0"))
	wantProbs := []string{
		`line 6: Altset "two" is not a number`,
		`line 8: momentary freq "fast Hz" is not "N Hz (0xI.FFFF)"`,
		`line 15: rate "x96000" is not a number`,
		`line 19: not a stream line: "@@@@ not a stream0 line @@@@"`,
		`line 25: DSD raw field "bitrev=" is not DOP=0|1 or bitrev=0|1`,
		`line 26: not a stream line: "    Bi"`,
	}
	if !reflect.DeepEqual(probs, wantProbs) {
		t.Errorf("problems =\n%q\nwant\n%q", probs, wantProbs)
	}
	p := s.Playback
	if p == nil || p.Running == nil {
		t.Fatalf("playback = %+v; want a Running section", p)
	}
	r := p.Running
	if deref(r.Interface) != 1 || r.Altset != nil || deref(r.PacketSize) != 400 ||
		r.MomentaryFreqHz != nil || r.MomentaryFreq != "" || r.FeedbackFormat != "16.16" {
		t.Errorf("running = %+v; want interface 1, no altset, packet 400, no freq, feedback 16.16", r)
	}
	if len(p.Altsets) != 2 {
		t.Fatalf("%d altsets, want 2", len(p.Altsets))
	}
	a1, a2 := p.Altsets[0], p.Altsets[1]
	if !reflect.DeepEqual(a1.Rates, []int{44100, 48000}) || deref(a1.Bits) != 32 || !reflect.DeepEqual(a1.Formats, []string{"S32_LE"}) {
		t.Errorf("altset 1 = %+v; want S32_LE, 32 bit, the two rates that parse", a1)
	}
	if deref(a2.Altset) != 2 || a2.Bits != nil || !reflect.DeepEqual(a2.Formats, []string{"SPECIAL", "DSD_U32_BE"}) ||
		len(a2.Rates) != 8 || a2.DSDRaw == nil || a2.DSDRaw.DOP == nil || *a2.DSDRaw.DOP || a2.DSDRaw.Bitrev != nil {
		t.Errorf("altset 2 = %+v (DSD raw %+v); want DSD_U32_BE, no bits, 8 rates, DOP=0 and bitrev unknown", a2, a2.DSDRaw)
	}
}

// TestParseStreamShapes covers what the captures do not: a continuous rate
// range, a Capture section, a running block with no format or altset yet
// and a zero measured rate (which the kernel prints without its 0x), and
// the misplaced lines a parse reports.
func TestParseStreamShapes(t *testing.T) {
	t.Run("continuous rates, a capture section, a block with no altset", func(t *testing.T) {
		in := "Some Card at usb-1, full speed : USB Audio\n\nPlayback:\n  Status: Running\n" +
			"    Packet Size = 0\n    Momentary freq = 0 Hz (0.0000)\n    Some Future Line = 7\n" +
			"  Interface 2\n    Altset 1\n    Format: S16_LE S24_LE\n    Channels: 2\n" +
			"    Endpoint: 0x02 (2 OUT) (ADAPTIVE)\n    Rates: 8000 - 96000 (continuous)\n    Bits: 16\n" +
			"    Max Packet Size: 512\n\nCapture:\n  Status: Stop\n  Interface 3\n    Altset 1\n" +
			"    Format: S16_LE\n    Channels: 1\n    Rates: 48000\n    Bits: 16\n    Channel map: MONO\n"
		s, probs := ParseStream([]byte(in))
		if len(probs) != 0 {
			t.Errorf("problems: %q", probs)
		}
		one, two, three := 1, 2, 3
		wantPlay := &StreamDirection{
			Status:  "Running",
			Running: &RunningEndpoint{PacketSize: intPtr(0), MomentaryFreqHz: int64Ptr(0), MomentaryFreq: "0.0000"},
			Altsets: []Altset{{Interface: &two, Altset: &one, Formats: []string{"S16_LE", "S24_LE"}, Channels: &two,
				Rates: []int{}, RateRange: &RateRange{Min: 8000, Max: 96000}, Bits: intPtr(16), ChannelMap: []string{}}},
		}
		wantCap := &StreamDirection{
			Status: "Stop",
			Altsets: []Altset{{Interface: &three, Altset: &one, Formats: []string{"S16_LE"}, Channels: &one,
				Rates: []int{48000}, Bits: intPtr(16), ChannelMap: []string{"MONO"}}},
		}
		if !reflect.DeepEqual(s.Playback, wantPlay) {
			t.Errorf("playback = %+v\nwant       %+v", s.Playback, wantPlay)
		}
		if !reflect.DeepEqual(s.Capture, wantCap) {
			t.Errorf("capture = %+v\nwant      %+v", s.Capture, wantCap)
		}
	})
	t.Run("misplaced lines", func(t *testing.T) {
		in := "Card : USB Audio\nFormat: S16_LE\nPlayback:\n  Status: Stop\n    Altset = 3\n" +
			"    Format: S16_LE\n    Endpoint: 0x01\n    Altset 5\n    Format: S16_LE\n    Channels: 2\n" +
			"    Rates: 44100\n    Bits: 16\n  Status: Running\nPlayback:\n  Status: Running\n"
		s, probs := ParseStream([]byte(in))
		wantProbs := []string{
			`line 2: outside a Playback or Capture section: "Format: S16_LE"`,
			`line 5: an endpoint line outside a Running status block: "    Altset = 3"`,
			`line 6: Format line before any Interface line`,
			`line 8: an Altset line with no Interface line before it`,
			`line 13: a Status line after the altsets began: "  Status: Running"`,
			`line 14: a second Playback section; the first is kept`,
		}
		if !reflect.DeepEqual(probs, wantProbs) {
			t.Errorf("problems =\n%q\nwant\n%q", probs, wantProbs)
		}
		if s.Playback == nil || s.Playback.Status != "Stop" || len(s.Playback.Altsets) != 1 ||
			s.Playback.Altsets[0].Interface != nil || deref(s.Playback.Altsets[0].Altset) != 5 {
			t.Errorf("playback = %+v; want the first section, Stop, one altset 5 of no known interface", s.Playback)
		}
	})
	t.Run("a second Status line", func(t *testing.T) {
		// Found by FuzzParseStream: a later Status line replaced the state
		// and left the Running block of the first, so a section read
		// "Status: " (or "Stop") with a running endpoint.
		s, probs := ParseStream([]byte("Playback:\nStatus:Running\nStatus:\n  Status: Stop\n"))
		wantProbs := []string{
			`line 3: a second Status line; the first is kept: "Status:"`,
			`line 4: a second Status line; the first is kept: "  Status: Stop"`,
		}
		if !reflect.DeepEqual(probs, wantProbs) {
			t.Errorf("problems =\n%q\nwant\n%q", probs, wantProbs)
		}
		if s.Playback == nil || s.Playback.Status != "Running" || s.Playback.Running == nil {
			t.Errorf("playback = %+v; want the first status, Running, and its block", s.Playback)
		}
		s, probs = ParseStream([]byte("Playback:\n  Status:\n"))
		if s.Playback == nil || s.Playback.Status != "" || s.Playback.Running != nil ||
			!reflect.DeepEqual(probs, []string{"line 2: a Status line with no state"}) {
			t.Errorf("an empty status = %+v, %q", s.Playback, probs)
		}
	})
	t.Run("a repeated field keeps its first value", func(t *testing.T) {
		in := "Playback:\n  Status: Running\n    Altset = 4\n    Altset = 2\n  Interface 1\n    Altset 4\n" +
			"    Format: SPECIAL DSD_U32_BE\n    Channels: 2\n    Rates: 352800\n    Bits: 32\n    Bits: 16\n"
		s, probs := ParseStream([]byte(in))
		wantProbs := []string{
			"line 4: a second Altset line; the first is kept",
			"line 11: a second Bits line; the first is kept",
		}
		if !reflect.DeepEqual(probs, wantProbs) {
			t.Errorf("problems =\n%q\nwant\n%q", probs, wantProbs)
		}
		if p := s.Playback; p == nil || deref(p.Running.Altset) != 4 || len(p.Altsets) != 1 || deref(p.Altsets[0].Bits) != 32 {
			t.Errorf("playback = %+v; want the running altset 4 and 32 bits, the first values", p)
		}
	})
	t.Run("every format on one line, and a DSD name that is not first", func(t *testing.T) {
		// proc_dump_substream_formats prints the altset's bitmask on one
		// Format line (v4.19 through v6.17). SPECIAL is a format name.
		s, probs := ParseStream(readFixture(t, "multi-format", "asound/card0/stream0"))
		if len(probs) != 0 {
			t.Fatalf("problems: %q", probs)
		}
		p := s.Playback
		if p == nil || len(p.Altsets) != 3 {
			t.Fatalf("playback = %+v; want three altsets", p)
		}
		if !reflect.DeepEqual(p.Altsets[0].Formats, []string{"S16_LE", "S24_3LE"}) || deref(p.Altsets[0].Bits) != 24 {
			t.Errorf("altset 1 = %+v; want both PCM formats and the one Bits line", p.Altsets[0])
		}
		if !reflect.DeepEqual(p.Altsets[1].Formats, []string{"SPECIAL", "DSD_U32_BE"}) ||
			p.Altsets[1].DSDFormat() != "DSD_U32_BE" {
			t.Errorf("altset 2 formats = %v, DSD %q; want SPECIAL kept and DSD_U32_BE found second",
				p.Altsets[1].Formats, p.Altsets[1].DSDFormat())
		}
		if !reflect.DeepEqual(p.Altsets[2].Formats, []string{"S32_LE", "DSD_U32_LE"}) ||
			p.Altsets[2].DSDFormat() != "DSD_U32_LE" {
			t.Errorf("altset 3 formats = %v, DSD %q; want DSD_U32_LE found after S32_LE",
				p.Altsets[2].Formats, p.Altsets[2].DSDFormat())
		}
		if p.Altsets[0].DSDFormat() != "" {
			t.Errorf("a PCM altset read as DSD %q", p.Altsets[0].DSDFormat())
		}
	})
	t.Run("a second Format line is a repeated key, not more formats", func(t *testing.T) {
		// One altset is one Format line. A second line is not the kernel
		// listing another format of the same altset.
		in := "Playback:\n  Interface 1\n    Altset 1\n    Format: S16_LE S24_3LE\n" +
			"    Format: DSD_U32_BE\n    Channels: 2\n    Rates: 48000\n    Bits: 16\n"
		s, probs := ParseStream([]byte(in))
		if !reflect.DeepEqual(probs, []string{"line 5: a second Format line; the first is kept"}) {
			t.Errorf("problems = %q", probs)
		}
		if p := s.Playback; p == nil || len(p.Altsets) != 1 ||
			!reflect.DeepEqual(p.Altsets[0].Formats, []string{"S16_LE", "S24_3LE"}) ||
			p.Altsets[0].DSDFormat() != "" {
			t.Errorf("formats = %+v; want the first line only", s.Playback)
		}
	})
	t.Run("a pre-5.1 altset has no Bits line and is not cut short", func(t *testing.T) {
		// v4.19 and v5.0 print no Bits line, for every USB device. v5.1
		// and later print one per altset, UAC1 and UAC2 alike.
		s, probs := ParseStream(readFixture(t, "uac1-bits-less", "asound/card0/stream0"))
		if len(probs) != 0 {
			t.Fatalf("problems = %q; a missing Bits line is unknown, not cut short", probs)
		}
		p := s.Playback
		if p == nil || len(p.Altsets) != 2 {
			t.Fatalf("playback = %+v", p)
		}
		for _, a := range p.Altsets {
			if a.Bits != nil || len(a.Formats) == 0 {
				t.Errorf("altset %+v; want formats and unknown bits", a)
			}
		}
	})
	t.Run("empty file", func(t *testing.T) {
		s, probs := ParseStream([]byte("\n\n"))
		if s.Playback != nil || s.Capture != nil || !reflect.DeepEqual(probs, []string{"empty file"}) {
			t.Errorf("ParseStream(empty) = %+v, %q", s, probs)
		}
	})
}
