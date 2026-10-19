// Fuzz coverage for the /proc/asound parsers, one target per parser.
//
// The input is kernel text, and through `--proc-root` also a tree captured
// on another box and copied here, so a parser must survive anything: no
// panic, and output BOUNDED by its input. Each target also holds the
// parser to the package's promise that it invents nothing: every string it
// returns is text the input holds, every number one the input spelled, and
// a verdict line ("closed") stands alone. The nightly job finds these by
// their names (.github/workflows/fuzz.yml).
package alsa

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// seedCaptures adds every ALSA file of the test cases' trees that is
// named name (a thread's /proc/<tid>/status is not an ALSA status).
func seedCaptures(f *testing.F, name string) {
	f.Helper()
	seeded := 0
	err := filepath.WalkDir("testdata", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != name ||
			!strings.Contains(filepath.ToSlash(path), "/proc/asound/") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		f.Add(b)
		seeded++
		return nil
	})
	if err != nil {
		f.Fatal(err)
	}
	if seeded == 0 {
		f.Fatalf("no fixture named %s to seed from", name)
	}
}

// checkBounded holds a parse's output to its input: its JSON is at most
// 64 bytes per input byte (an altset header line becomes an object of
// about ten fields), it reports at most maxProblems problems and a count,
// and no problem is longer than a line number, a message and two quotes
// of maxQuoted bytes can make.
func checkBounded(t *testing.T, in []byte, out any, probs []string) {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("output does not marshal: %v", err)
	}
	if len(b) > 64*len(in)+4096 {
		t.Fatalf("%d bytes of input made %d bytes of output", len(in), len(b))
	}
	if probs == nil || len(probs) > maxProblems+1 {
		t.Fatalf("%d problems (nil: %v), want a list of at most %d", len(probs), probs == nil, maxProblems+1)
	}
	for _, p := range probs {
		if len(p) > 1024 {
			t.Fatalf("a %d-byte problem: %.120q", len(p), p)
		}
	}
}

// checkFromInput fails when s is not text the input holds, or spans a line.
func checkFromInput(t *testing.T, in []byte, field, s string) {
	t.Helper()
	if s == "" {
		return
	}
	if !strings.Contains(string(in), s) || strings.ContainsAny(s, "\n") {
		t.Fatalf("%s %q is not one line of the input", field, s)
	}
}

// checkNonNeg fails on a negative optional number: every one the parsers
// return came from a digit string.
func checkNonNeg[T int | int64](t *testing.T, field string, p *T) {
	t.Helper()
	if p != nil && *p < 0 {
		t.Fatalf("%s is negative: %d", field, *p)
	}
}

// checkDeterministic parses again and compares.
func checkDeterministic[T any](t *testing.T, in []byte, first T, firstProbs []string, parse func([]byte) (T, []string)) {
	t.Helper()
	again, probs := parse(in)
	if !reflect.DeepEqual(first, again) || !reflect.DeepEqual(firstProbs, probs) {
		t.Fatal("two parses of one input disagree")
	}
}

func FuzzParseCards(f *testing.F) {
	seedCaptures(f, "cards")
	f.Add([]byte("--- no soundcards ---\n"))
	f.Add([]byte("10 [sndrpihifiberry]: HifiberryDacp - \n                      x\n 0 [A]: d - n\n"))
	f.Add([]byte(" 1 [x]:  - \r\n  \n99999999999999999999 [y]: a - b\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		cards, probs := ParseCards(in)
		checkBounded(t, in, cards, probs)
		checkDeterministic(t, in, cards, probs, ParseCards)
		if len(cards) > strings.Count(string(in), "\n")+1 {
			t.Fatalf("%d cards from %d lines", len(cards), strings.Count(string(in), "\n")+1)
		}
		seen := map[int]bool{}
		for _, c := range cards {
			if c.Index < 0 || seen[c.Index] {
				t.Fatalf("card index %d is negative or repeated", c.Index)
			}
			seen[c.Index] = true
			for field, s := range map[string]string{"id": c.ID, "driver": c.Driver, "name": c.Name, "long name": c.LongName} {
				checkFromInput(t, in, field, s)
			}
		}
	})
}

func FuzzParseUSBID(f *testing.F) {
	seedCaptures(f, "usbid")
	f.Add([]byte("FFFF:0000\r\n"))
	f.Add([]byte("+0b1:3009"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, in []byte) {
		id, err := ParseUSBID(in)
		if err != nil {
			if id != (USBID{}) {
				t.Fatalf("an error with an id: %v", id)
			}
			if len(err.Error()) > 1024 {
				t.Fatalf("a %d-byte error", len(err.Error()))
			}
			return
		}
		// An id is the input, and it reads back as itself.
		if !strings.EqualFold(strings.TrimSpace(string(in)), id.String()) {
			t.Fatalf("id %s from input %q", id, in)
		}
		if again, err := ParseUSBID([]byte(id.String())); err != nil || again != id {
			t.Fatalf("%s reads back as %v, %v", id, again, err)
		}
	})
}

func FuzzParseStream(f *testing.F) {
	seedCaptures(f, "stream0")
	f.Add([]byte("X : USB Audio\n\nCapture:\n  Status: Running\n    Momentary freq = 0 Hz (0.0000)\n" +
		"  Interface 2\n    Altset 1\n    Rates: 8000 - 96000 (continuous)\n    DSD raw: DOP=1, bitrev=1\n"))
	f.Add([]byte("Playback:\n    Altset 2\n    Altset 3\n  Interface x\n    Format:\n    Rates: ,,\n"))
	// A second Status line after a Running one: the shape of the input this
	// target found (testdata/fuzz/FuzzParseStream), with a state, which is
	// what reaches the rule now that an empty Status line is its own case.
	f.Add([]byte("Playback:\n  Status: Running\n  Status: Stop\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		s, probs := ParseStream(in)
		checkBounded(t, in, s, probs)
		checkDeterministic(t, in, s, probs, ParseStream)
		lines := strings.Count(string(in), "\n") + 1
		altsets := 0
		for _, d := range []*StreamDirection{s.Playback, s.Capture} {
			if d == nil {
				continue
			}
			checkFromInput(t, in, "status", d.Status)
			if (d.Running != nil) != (d.Status == "Running") {
				t.Fatalf("status %q with running block %+v", d.Status, d.Running)
			}
			if r := d.Running; r != nil {
				checkNonNeg(t, "interface", r.Interface)
				checkNonNeg(t, "altset", r.Altset)
				checkNonNeg(t, "packet size", r.PacketSize)
				checkNonNeg(t, "momentary freq", r.MomentaryFreqHz)
				checkFromInput(t, in, "momentary freq", r.MomentaryFreq)
				checkFromInput(t, in, "feedback format", r.FeedbackFormat)
			}
			altsets += len(d.Altsets)
			for _, a := range d.Altsets {
				if a.Formats == nil || a.Rates == nil || a.ChannelMap == nil {
					t.Fatalf("an altset with a nil list: %+v", a)
				}
				checkNonNeg(t, "interface", a.Interface)
				checkNonNeg(t, "altset", a.Altset)
				checkNonNeg(t, "channels", a.Channels)
				checkNonNeg(t, "bits", a.Bits)
				for _, r := range a.Rates {
					checkNonNeg(t, "rate", &r)
				}
				if a.RateRange != nil && (a.RateRange.Min < 0 || a.RateRange.Max < 0) {
					t.Fatalf("a negative rate range %+v", a.RateRange)
				}
				for _, tok := range append(append([]string{}, a.Formats...), a.ChannelMap...) {
					checkFromInput(t, in, "token", tok)
				}
			}
		}
		if altsets > lines {
			t.Fatalf("%d altsets from %d lines", altsets, lines)
		}
	})
}

func FuzzParseHWParams(f *testing.F) {
	seedCaptures(f, "hw_params")
	f.Add([]byte("no setup\n"))
	f.Add([]byte("closed\nrate: 1 (1/1)\n"))
	f.Add([]byte("rate: 4294967295 (4294967295/0)\nchannels: 99999999999999999999\nformat: \n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		h, probs := ParseHWParams(in)
		checkBounded(t, in, h, probs)
		checkDeterministic(t, in, h, probs, ParseHWParams)
		if h.Closed && h.NoSetup {
			t.Fatal("closed and no setup at once")
		}
		if (h.Closed && h != (HWParams{Closed: true})) || (h.NoSetup && h != (HWParams{NoSetup: true})) {
			t.Fatalf("a verdict with parameters: %+v", h)
		}
		for field, s := range map[string]string{"access": h.Access, "format": h.Format, "subformat": h.Subformat} {
			checkFromInput(t, in, field, s)
			if strings.ContainsAny(s, " \t") {
				t.Fatalf("%s %q is more than one word", field, s)
			}
		}
		checkNonNeg(t, "channels", h.Channels)
		checkNonNeg(t, "rate", h.Rate)
		checkNonNeg(t, "rate num", h.RateNum)
		checkNonNeg(t, "rate den", h.RateDen)
		checkNonNeg(t, "period size", h.PeriodSize)
		checkNonNeg(t, "buffer size", h.BufferSize)
	})
}

func FuzzParseStatus(f *testing.F) {
	seedCaptures(f, "status")
	f.Add([]byte("error -19\n"))
	f.Add([]byte("error 99999999999999999999\n"))
	f.Add([]byte("state: XRUN\nowner_pid   : 0\ntrigger_time: -1.000000000\ndelay       : -9223372036854775808\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		st, probs := ParseStatus(in)
		checkBounded(t, in, st, probs)
		checkDeterministic(t, in, st, probs, ParseStatus)
		if st.Closed && !reflect.DeepEqual(st, Status{Closed: true}) {
			t.Fatalf("closed with fields: %+v", st)
		}
		if st.Error != nil && !reflect.DeepEqual(st, Status{Error: st.Error}) {
			t.Fatalf("an error verdict with fields: %+v", st)
		}
		checkFromInput(t, in, "state", st.State)
		if strings.ContainsAny(st.State, " \t") {
			t.Fatalf("state %q is more than one word", st.State)
		}
		checkNonNeg(t, "owner_pid", st.OwnerPID)
		for field, s := range map[string]string{"trigger_time": st.TriggerTime, "tstamp": st.Timestamp} {
			checkFromInput(t, in, field, s)
			if s != "" && !kernelSeconds.MatchString(s) {
				t.Fatalf("%s %q is not the kernel's seconds", field, s)
			}
		}
	})
}
