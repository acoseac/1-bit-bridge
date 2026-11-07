package alsapcm_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdconv"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The player's DSD converted to PCM (B728 B11; the design's sections 4.1,
// 4.3, 4.7 and 4.8) through the engine's Run, the backend and the real PCM
// sink over the fake kernel. A DAC the profile database does not know, with
// no choice of its own, converts every rate under the bridge's auto, whatever
// controller queued the track: the engine's commands are every controller's.
// What the DAC is given is held, byte for byte, to an oracle the converter
// computes apart from the backend: each occurrence alone, zero-padded, where
// a join abuts, and the two occurrences' bits concatenated where it is
// continuous, which at 24 bits is one converter over both tracks' bits and at
// 16 bits the converter's own chain, whose dither starts again at each
// occurrence's first frame.

// convPlan is the test DAC's conversion plan: its altsets take S24_3LE to
// 176.4 kHz with 24 significant bits, the table's first packing.
var convPlan = dsdconv.Plan{RateHz: 176400, ValidBits: 24}

// newConvRig is a rig whose DAC the profile database does not know, its DSD
// state the defaults: auto converts every rate, to 176.4 kHz / 24 bits in
// S24_3LE for the test DAC.
func newConvRig(t *testing.T, opts ...func(*rigOptions)) *dsdRig {
	t.Helper()
	return newDSDRigWith(t, dsdstate.Defaults(), func(p *playback.DSDPolicy) { p.Profiles = nil }, opts...)
}

// withSteps hands the rig's backend steps' seam (playback.Seams.Converting).
func withSteps(steps *convSteps) func(*rigOptions) {
	return func(o *rigOptions) {
		if o.seams == nil {
			o.seams = &playback.Seams{}
		}
		o.seams.Converting = steps.seam
	}
}

// noise is DSD content of no pattern a filter could favour: each byte of
// each channel a hash of its place and the seed.
func noise(seed int) func(ch, i int) byte {
	return func(ch, i int) byte {
		x := uint64(i)*0x9E3779B97F4A7C15 + uint64(ch)*0xBF58476D1CE4E5B9 + uint64(seed)*0x94D049BB133111EB
		x ^= x >> 31
		x *= 0xBF58476D1CE4E5B9
		x ^= x >> 29
		return byte(x)
	}
}

// overmodulated is noise(seed) with a stretch of channel 0 all ones, past
// full scale for a converter: from byte at, n bytes.
func overmodulated(seed, at, n int) func(ch, i int) byte {
	f := noise(seed)
	return func(ch, i int) byte {
		if ch == 0 && i >= at && i < at+n {
			return 0xFF
		}
		return f(ch, i)
	}
}

// track is a DSF of seconds of fill's DSD at bitRate.
func track(bitRate int, seconds float64, fill func(ch, i int) byte) []byte {
	return playbacktest.DSF(bitRate, seconds, fill)
}

// joined is one DSF holding a's seconds of DSD, then b's, at bitRate: the
// two tracks' bits concatenated, as one continuous stream.
func joined(bitRate int, sa float64, fa func(ch, i int) byte, sb float64, fb func(ch, i int) byte) []byte {
	na := int(sa * float64(bitRate) / 8)
	return playbacktest.DSF(bitRate, sa+sb, func(ch, i int) byte {
		if i < na {
			return fa(ch, i)
		}
		return fb(ch, i-na)
	})
}

// dsdOf is the DSD decoder of a DSF file.
func dsdOf(t *testing.T, file []byte) decode.DSDDecoder {
	t.Helper()
	src, err := decode.Open(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	d, ok := src.(decode.DSDDecoder)
	if !ok {
		t.Fatalf("the content opened as %T, not DSD", src)
	}
	return d
}

// readAll reads c to its end, interleaved; a held converter's window point
// is the caller's.
func readAll(t *testing.T, c *dsdconv.Converter) ([]int32, error) {
	t.Helper()
	var out []int32
	buf := make([]int32, 2*4096)
	for {
		n, err := c.ReadFrames(bg, buf)
		out = append(out, buf[:n]...)
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			return out, nil
		default:
			return out, err
		}
	}
}

// converted is file's DSD converted alone under plan, as a converted
// occurrence nothing continues is, zero-padded at its end: its samples,
// interleaved, and the samples it saturated.
func converted(t *testing.T, file []byte, plan dsdconv.Plan) ([]int32, int64) {
	t.Helper()
	c, err := dsdconv.New(dsdOf(t, file), plan, dsdconv.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	out, err := readAll(t, c)
	if err != nil {
		t.Fatal(err)
	}
	return out, c.Clipped()
}

// chained is files' DSD converted as one continuous chain under plan, each
// occurrence's converter holding its window and continued into the next
// (dsdconv's Continue), the last one's ended: what the player gives a DAC
// for occurrences dsdconv.Continuous joins, each one's dither starting again
// at its first frame.
func chained(t *testing.T, plan dsdconv.Plan, files ...[]byte) []int32 {
	t.Helper()
	var convs []*dsdconv.Converter
	for _, f := range files {
		c, err := dsdconv.New(dsdOf(t, f), plan, dsdconv.Options{Hold: true})
		if err != nil {
			t.Fatal(err)
		}
		convs = append(convs, c)
	}
	var out []int32
	for i, c := range convs {
		for {
			got, err := readAll(t, c)
			out = append(out, got...)
			if err == nil {
				break
			}
			if !errors.Is(err, dsdconv.ErrWindow) {
				t.Fatal(err)
			}
			if i+1 < len(convs) {
				if err := c.Continue(convs[i+1]); err != nil {
					t.Fatalf("the oracle's chain does not continue into track %d: %v", i+1, err)
				}
			} else if err := c.End(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, c := range convs {
		_ = c.Close()
	}
	return out
}

// same fails the test unless got is want, saying where they first differ.
func same(t *testing.T, what string, got, want []int32) {
	t.Helper()
	if slices.Equal(got, want) {
		return
	}
	at := 0
	for at < len(got) && at < len(want) && got[at] == want[at] {
		at++
	}
	t.Fatalf("%s: %d samples, want %d; they first differ at sample %d (frame %d)", what, len(got), len(want), at, at/2)
}

// cat is the samples of each part, in order.
func cat(parts ...[]int32) []int32 {
	var out []int32
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// intp is a pointer to v.
func intp(v int) *int { return &v }

// pcmAltset is an altset of the bound PCM device listing f at rates in two
// channels with bits significant.
func pcmAltset(f format.ALSAFormat, bits int, rates ...int) alsa.Altset {
	return alsa.Altset{Interface: intp(1), Altset: intp(1), Formats: []string{string(f)}, Channels: intp(2), Rates: rates, Bits: intp(bits)}
}

// formatCode is f's ALSA format code (asound.h), as the fake's hardware
// lists formats.
var formatCode = map[format.ALSAFormat]uint32{format.S16LE: 2, format.S24LE: 6, format.S32LE: 10, format.S243LE: 32}

// rateName names a DSD rate.
func rateName(r int) string {
	return map[int]string{playbacktest.DSD64: "DSD64", playbacktest.DSD128: "DSD128", playbacktest.DSD256: "DSD256"}[r]
}

// occurrenceOf is the successor occurrence the engine handed entry off as
// (Prepare), the first it named.
func (d *dsdRig) occurrenceOf(entry uint32) engine.OccurrenceID {
	d.t.Helper()
	c := d.waitCall("the engine to hand off the entry", func(c call) bool { return c.kind == "prepare" && c.next != nil && c.next.Entry == entry })
	return c.next.ID
}

// preparedOcc waits until the backend has reported entry's successor
// occurrence Prepared, its open done, and returns it.
func (d *dsdRig) preparedOcc(entry uint32) engine.OccurrenceID {
	d.t.Helper()
	o := d.occurrenceOf(entry)
	d.waitEvents("the successor prepared", func(evs []event) bool { return count(evs, "prepared", o) > 0 })
	return o
}

// detailsOf are the Details the backend reported of o.
func (d *dsdRig) detailsOf(o engine.OccurrenceID) (engine.Details, bool) {
	for _, r := range d.rec.detailsList() {
		if r.o == o {
			return r.d, true
		}
	}
	return engine.Details{}, false
}

// convSteps records the steps of a converted occurrence's join the backend
// takes (playback.Seams.Converting), and holds the producer at one of them,
// the first of its kind, until letGo.
type convSteps struct {
	mu      sync.Mutex
	steps   []string
	changed chan struct{}
	holdAt  string
	held    chan struct{}
	gate    chan struct{}
	once    sync.Once
}

// newConvSteps is a record that holds the producer at the first step named
// holdAt ("decided" or "ended"; "" holds nothing).
func newConvSteps(t *testing.T, holdAt string) *convSteps {
	c := &convSteps{changed: make(chan struct{}), holdAt: holdAt, held: make(chan struct{}), gate: make(chan struct{})}
	t.Cleanup(c.letGo)
	return c
}

func (c *convSteps) seam(step string, o engine.OccurrenceID) {
	c.mu.Lock()
	c.steps = append(c.steps, fmt.Sprintf("%s %d", step, o))
	close(c.changed)
	c.changed = make(chan struct{})
	hold := c.holdAt != "" && step == c.holdAt
	if hold {
		c.holdAt = ""
	}
	c.mu.Unlock()
	if hold {
		close(c.held)
		<-c.gate
	}
}

func (c *convSteps) letGo() { c.once.Do(func() { close(c.gate) }) }

func (c *convSteps) ch() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.changed
}

// has reports whether step was taken for o.
func (c *convSteps) has(step string, o engine.OccurrenceID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.steps, fmt.Sprintf("%s %d", step, o))
}

// wait waits until step was taken for o.
func (c *convSteps) wait(t *testing.T, step string, o engine.OccurrenceID) {
	t.Helper()
	waitOn(t, fmt.Sprintf("the converted occurrence's %s step for %d", step, o), c.ch, func() bool { return c.has(step, o) })
}

// heldNow waits until the producer is held at the step asked.
func (c *convSteps) heldNow(t *testing.T) {
	t.Helper()
	select {
	case <-c.held:
	case <-time.After(alsapcm.Watchdog()):
		alsapcm.LogGoroutines(t)
		t.Fatalf("the producer did not reach the step it is to be held at within %v", alsapcm.Watchdog())
	}
}

// firstOcc is the occurrence the session's Start began with.
func (d *dsdRig) firstOcc() engine.OccurrenceID {
	d.t.Helper()
	c := d.waitCall("the session's start", func(c call) bool { return c.kind == "start" })
	return c.first
}

// TestAnUnknownDACPlaysDSDConverted (B728 B11; the design's sections 3.2 and
// 4.3): a DAC the profile database does not know, with no choice of its own,
// plays DSD64, DSD128 and DSD256 converted to its best target, 176.4 kHz /
// 24 bits in S24_3LE for the test DAC, through one PCM open and no DSD sink,
// the DAC given exactly the converter's samples; the set publishes no tuple
// and no reason, every rate converted. A known DAC whose first use is not
// acknowledged converts as well; a migrated explicit off refuses every rate,
// before any open, and so does a DAC that takes no conversion target, by the
// policy's word, conversion_not_offered.
func TestAnUnknownDACPlaysDSDConverted(t *testing.T) {
	for _, rate := range []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256} {
		t.Run(rateName(rate), func(t *testing.T) {
			d := newConvRig(t)
			a := track(rate, 0.25, noise(1))
			d.files.add("a", a)
			d.insert("a")
			d.k.Free()
			d.play()
			s := d.waitStopped(engine.ReasonEndOfQueue)
			if len(s.Skips()) != 0 || len(s.Faults()) != 0 {
				t.Fatalf("an unknown DAC's DSD did not play: skips %+v, faults %+v", s.Skips(), s.Faults())
			}
			if got := d.openFormats(); len(got) != 0 {
				t.Errorf("an unknown DAC's DSD asked DSD opens %v: it converts", got)
			}
			if v := d.k.View(); d.pcmOpens() != 1 || v.Opens != 1 || v.Committed != format.S243LE {
				t.Errorf("%d PCM opens asked, %d opens, %s committed; want one, in S24_3LE", d.pcmOpens(), v.Opens, v.Committed)
			}
			want, _ := converted(t, a, convPlan)
			same(t, "the DAC was given", unpack(t, d.k.Heard(), format.S243LE, 24), want)
			p := d.be.Policy()
			if len(p.Tuples) != 0 || p.Reason != "" || len(p.Converted) != 3 {
				t.Fatalf("the set published %+v, reason %q, converted %+v; want no tuple, no reason, three rates converted", p.Tuples, p.Reason, p.Converted)
			}
			for i, c := range p.Converted {
				if c.BitRate != []int{playbacktest.DSD64, playbacktest.DSD128, playbacktest.DSD256}[i] || c.Plan != convPlan || c.Packing != format.S243LE {
					t.Errorf("converted rate %d: %+v, want the rate at 176.4 kHz / 24 in S24_3LE", i, c)
				}
			}
		})
	}
	t.Run("a known DAC whose first use is not acknowledged", func(t *testing.T) {
		d := newDSDRig(t, dsdstate.Defaults())
		a := track(playbacktest.DSD64, 0.25, noise(1))
		d.files.add("a", a)
		d.insert("a")
		d.k.Free()
		d.play()
		d.waitStopped(engine.ReasonEndOfQueue)
		if got := d.openFormats(); len(got) != 0 || d.pcmOpens() != 1 {
			t.Errorf("a known DAC unacknowledged asked DSD opens %v and %d PCM opens; want it converted, one PCM open", got, d.pcmOpens())
		}
		want, _ := converted(t, a, convPlan)
		same(t, "the DAC was given", unpack(t, d.k.Heard(), format.S243LE, 24), want)
	})
	t.Run("a migrated explicit off", func(t *testing.T) {
		st := dsdstate.Defaults()
		st.DefaultMode = dsdstate.ModeOff
		d := newDSDRigWith(t, st, func(p *playback.DSDPolicy) { p.Profiles = nil })
		d.files.add("a", track(playbacktest.DSD64, 0.25, noise(1)))
		ids := d.insert("a")
		_ = d.eng.Play(bg)
		s := d.waitEnded()
		if sk, ok := s.Skipped(ids[0]); !ok || sk.Reason != "dsd_off_by_choice" {
			t.Errorf("an explicit off migrated: the entry was %+v (skipped %v), want refused as dsd_off_by_choice", sk, ok)
		}
		if v := d.k.View(); v.Opens != 0 || d.pcmOpens() != 0 {
			t.Errorf("an explicit off opened the DAC: %d opens, %d PCM opens asked", v.Opens, d.pcmOpens())
		}
	})
	t.Run("a DAC that takes no conversion target", func(t *testing.T) {
		d := newConvRig(t)
		// The 48 kHz family alone: no 176.4, 88.2 or 44.1 kHz PCM.
		d.box.SetAltsets(card, 0, pcmAltset(format.S243LE, 24, 48000, 96000, 192000))
		d.files.add("a", track(playbacktest.DSD64, 0.25, noise(1)))
		ids := d.insert("a")
		_ = d.eng.Play(bg)
		s := d.waitEnded()
		if sk, ok := s.Skipped(ids[0]); !ok || sk.Reason != "conversion_not_offered" {
			t.Errorf("a DAC with no conversion target: the entry was %+v (skipped %v), want refused as conversion_not_offered", sk, ok)
		}
		if v := d.k.View(); v.Opens != 0 || d.pcmOpens() != 0 {
			t.Errorf("a DAC with no conversion target was opened: %d opens, %d PCM opens asked", v.Opens, d.pcmOpens())
		}
		if p := d.be.Policy(); p.PlaysDSD() || p.Reason != "conversion_not_offered" {
			t.Errorf("a DAC with no conversion target published %+v (%q), want nothing playing, conversion_not_offered", p, p.Reason)
		}
	})
}

// TestConvertedDSDReachesTheDACPackedByTheTable (B728 B11; the design's
// section 4.3): the converted PCM reaches the DAC in the plan's packing, the
// first of the table's that carries the plan's bits at its rate on the DAC,
// each sample packed as PCM of that depth is: 24 bits in S24_3LE, S24_LE or
// S32_LE, 16 bits in S16_LE or widened into S32_LE. Neither the packing a PCM
// track of the same rate and depth took (the cache the PCM open offers first)
// nor the one the sink before it holds (a successor's inherited packing)
// chooses it, where the capability reading excludes them.
func TestConvertedDSDReachesTheDACPackedByTheTable(t *testing.T) {
	for _, c := range []struct {
		name    string
		altsets []alsa.Altset
		takes   []format.ALSAFormat
		plan    dsdconv.Plan
		packing format.ALSAFormat
	}{
		{"S24_3LE, the table's first", playbacktest.AllAltsets(), []format.ALSAFormat{format.S16LE, format.S32LE, format.S243LE}, convPlan, format.S243LE},
		{"S24_LE", []alsa.Altset{pcmAltset(format.S24LE, 24, 44100, 88200, 176400)}, []format.ALSAFormat{format.S24LE}, convPlan, format.S24LE},
		{"S32_LE with 24 significant bits", []alsa.Altset{pcmAltset(format.S32LE, 24, 44100, 88200, 176400)}, []format.ALSAFormat{format.S32LE}, convPlan, format.S32LE},
		{"16 bits in S16_LE", []alsa.Altset{pcmAltset(format.S16LE, 16, 44100, 88200, 176400)}, []format.ALSAFormat{format.S16LE}, dsdconv.Plan{RateHz: 176400, ValidBits: 16}, format.S16LE},
		{"16 bits widened into S32_LE", []alsa.Altset{pcmAltset(format.S32LE, 16, 88200)}, []format.ALSAFormat{format.S32LE}, dsdconv.Plan{RateHz: 88200, ValidBits: 16}, format.S32LE},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newConvRig(t)
			d.box.SetAltsets(card, 0, c.altsets...)
			var codes []uint32
			for _, f := range c.takes {
				codes = append(codes, formatCode[f])
			}
			d.k.HardwareFormats(codes...)
			a := track(playbacktest.DSD64, 0.25, noise(1))
			d.files.add("a", a)
			d.insert("a")
			d.k.Free()
			d.play()
			d.waitStopped(engine.ReasonEndOfQueue)
			if v := d.k.View(); v.Opens != 1 || v.Committed != c.packing {
				t.Fatalf("%d opens, %s committed; want one open in %s", v.Opens, v.Committed, c.packing)
			}
			want, _ := converted(t, a, c.plan)
			same(t, "the DAC was given", unpack(t, d.k.Heard(), c.packing, c.plan.ValidBits), want)
		})
	}

	// The capability reading excludes S32_LE at 24 bits (its altset says 16
	// significant), which the DAC's driver takes all the same, and lists
	// S24_3LE: the plan is S24_3LE.
	excluding := []alsa.Altset{pcmAltset(format.S32LE, 16, 44100, 88200, 176400), pcmAltset(format.S243LE, 24, 44100, 88200, 176400)}
	t.Run("a cached packing the reading excludes", func(t *testing.T) {
		d := newConvRig(t)
		d.box.SetAltsets(card, 0, excluding...)
		// A 24-bit track at 176.4 kHz first, on a DAC that takes S32_LE alone
		// then: it is offered S24_3LE and S24_LE, refused, and takes S32_LE,
		// which the backend keeps to offer that rate and depth first.
		d.k.HardwareFormats(formatCode[format.S32LE])
		pcm := pattern(1, 4410, 24)
		d.files.add("p", wav(176400, 24, pcm))
		d.insert("p")
		d.k.Free()
		d.play()
		d.waitStopped(engine.ReasonEndOfQueue)
		if v := d.k.View(); v.Committed != format.S32LE {
			t.Fatalf("the PCM track committed %s, want S32_LE", v.Committed)
		}
		d.k.HardwareFormats(formatCode[format.S243LE], formatCode[format.S32LE])
		a := track(playbacktest.DSD64, 0.25, noise(1))
		d.files.add("a", a)
		d.insert("a")
		before := d.k.View().Opens
		ids := d.eng.Snapshot()
		next, _ := ids.IDAt(1)
		if err := d.eng.PlayID(bg, next); err != nil {
			t.Fatal(err)
		}
		d.waitSnap("the converted track played", func(s *engine.Snapshot) bool {
			return s.State() == engine.Stopped && s.LastStop().Reason == engine.ReasonEndOfQueue && s.TrackCount() == 2
		})
		v := d.k.View()
		if v.Opens != before+1 || v.Committed != format.S243LE {
			t.Fatalf("the converted track: %d opens after %d, %s committed; want one open in S24_3LE, the plan's, never the cached S32_LE", v.Opens, before, v.Committed)
		}
		want, _ := converted(t, a, convPlan)
		same(t, "the converted track's open was given", unpack(t, d.k.Given()[v.OpenAt[len(v.OpenAt)-1]:], format.S243LE, 24), want)
	})
	t.Run("an inherited packing the reading excludes", func(t *testing.T) {
		d := newConvRig(t)
		d.box.SetAltsets(card, 0, excluding...)
		d.k.HardwareFormats(formatCode[format.S32LE])
		pcm := pattern(1, 4410, 24)
		a := track(playbacktest.DSD64, 0.25, noise(1))
		d.files.add("p", wav(176400, 24, pcm))
		d.files.add("a", a)
		ids := d.insert("p", "a")
		d.play()
		// The PCM track is offered S24_3LE and S24_LE, refused, and opens in
		// S32_LE.
		d.k.Await(t, "the PCM track's open in S32_LE", func(v alsapcm.KernelView) bool { return v.Committed == format.S32LE })
		// The DAC takes S24_3LE too from here on: the converted successor's
		// sink opens in it, where the PCM track's S32_LE, which the successor
		// would inherit at that rate and depth, is the reading's to exclude.
		d.k.HardwareFormats(formatCode[format.S243LE], formatCode[format.S32LE])
		o := d.preparedOcc(ids[1])
		d.k.Free()
		d.waitStopped(engine.ReasonEndOfQueue)
		v := d.k.View()
		if v.Committed != format.S243LE || len(v.OpenAt) < 2 {
			t.Fatalf("%d opens, the last committed %s; want the converted one's last, in S24_3LE", v.Opens, v.Committed)
		}
		for _, e := range d.rec.events() {
			if e.kind == "prepared" && e.o == o && e.join.Gapless {
				t.Errorf("the converted successor was prepared gapless in the PCM track's S32_LE sink")
			}
		}
		given := d.k.Given()
		pcmAt, convAt := v.OpenAt[len(v.OpenAt)-2], v.OpenAt[len(v.OpenAt)-1]
		same(t, "the PCM track's open was given", unpack(t, given[pcmAt:convAt], format.S32LE, 24), pcm)
		want, _ := converted(t, a, convPlan)
		same(t, "the converted track's open was given", unpack(t, given[convAt:], format.S243LE, 24), want)
	})
}

// TestConvertedTracksJoinContinuouslyAtOneRate (B728 B11; the design's
// section 4.7): two converted tracks at one DSD rate under one plan, which
// dsdconv.Continuous takes, share one converter chain in one sink: the DAC is
// given exactly the two tracks' bits converted as one stream, at 24 bits one
// converter over both, at 16 bits the chain whose dither starts again at the
// successor's first frame, the predecessor's frames, its join window's
// included, those one converter gives; each occurrence's length its own. A
// successor that settles only once the sink's lookahead is at its floor
// abuts: the window ends zero-padded, and the successor's converter starts
// afresh.
func TestConvertedTracksJoinContinuouslyAtOneRate(t *testing.T) {
	for _, c := range []struct {
		name    string
		altsets []alsa.Altset
		takes   format.ALSAFormat
		plan    dsdconv.Plan
	}{
		{"24 bits", playbacktest.AllAltsets(), format.S243LE, convPlan},
		{"16 bits", []alsa.Altset{pcmAltset(format.S16LE, 16, 44100)}, format.S16LE, dsdconv.Plan{RateHz: 44100, ValidBits: 16}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := newConvRig(t)
			d.box.SetAltsets(card, 0, c.altsets...)
			d.k.HardwareFormats(formatCode[c.takes])
			a, b := track(playbacktest.DSD64, 0.5, noise(1)), track(playbacktest.DSD64, 0.5, noise(2))
			d.files.add("a", a)
			d.files.add("b", b)
			ids := d.insert("a", "b")
			d.play()
			ob := d.preparedOcc(ids[1])
			d.k.Free()
			d.waitStopped(engine.ReasonEndOfQueue)
			if v := d.k.View(); v.Opens != 1 || v.Drains != 1 {
				t.Errorf("%d opens and %d drains; want the two tracks in one sink", v.Opens, v.Drains)
			}
			got := unpack(t, d.k.Heard(), c.takes, c.plan.ValidBits)
			same(t, "the DAC was given", got, chained(t, c.plan, a, b))
			one, _ := converted(t, joined(playbacktest.DSD64, 0.5, noise(1), 0.5, noise(2)), c.plan)
			frames := int(0.5*float64(playbacktest.DSD64)) / (playbacktest.DSD64 / c.plan.RateHz)
			if c.plan.ValidBits == 24 {
				same(t, "the DAC was given, against one converter over both tracks' bits", got, one)
			} else {
				same(t, "the first track's frames, its window's included, against one converter over both tracks' bits", got[:2*frames], one[:2*frames])
			}
			oa := d.firstOcc()
			for _, o := range []engine.OccurrenceID{oa, ob} {
				if det, ok := d.detailsOf(o); !ok || det.Frames != int64(frames) {
					t.Errorf("occurrence %d's Details %+v (reported %v), want %d frames, its own", o, det, ok, frames)
				}
			}
			for _, e := range d.rec.events() {
				if e.kind == "prepared" && e.o == ob && !e.join.Gapless {
					t.Errorf("the continuous successor was prepared %+v, want gapless", e.join)
				}
			}
		})
	}
	t.Run("a successor that settles after the floor abuts", func(t *testing.T) {
		steps := newConvSteps(t, "")
		d := newConvRig(t, withSteps(steps))
		a, b := track(playbacktest.DSD64, 0.5, noise(1)), track(playbacktest.DSD64, 0.5, noise(2))
		d.files.add("a", a)
		d.files.add("b", b)
		held, release := d.files.holdOpen("b")
		t.Cleanup(release)
		d.insert("a", "b")
		d.play()
		select {
		case <-held:
		case <-time.After(alsapcm.Watchdog()):
			t.Fatalf("b's open did not begin within %v", alsapcm.Watchdog())
		}
		oa := d.firstOcc()
		// Every frame of a but its window in the sink: the DAC plays only what
		// the producer has given it.
		steps.wait(t, "holding", oa)
		d.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
		// 450 ms of a's 500 played: what the sink holds of a, all but its
		// window, falls below the 100 ms floor.
		d.k.Play(t, 450*time.Millisecond)
		if steps.has("decided", oa) {
			t.Fatal("a's window was decided before the lookahead fell to its floor")
		}
		d.tick()
		steps.wait(t, "ended", oa)
		release()
		d.k.Free()
		d.waitStopped(engine.ReasonEndOfQueue)
		wantA, _ := converted(t, a, convPlan)
		wantB, _ := converted(t, b, convPlan)
		same(t, "the DAC was given", unpack(t, d.k.Heard(), format.S243LE, 24), cat(wantA, wantB))
		if v := d.k.View(); v.Opens != 1 {
			t.Errorf("%d opens: an abutting successor follows in the same sink", v.Opens)
		}
	})
}

// TestDSD64ThenDSD128ConvertedShareTheSink (B728 B11; the design's section
// 4.7, Argon's case): DSD64 then DSD128, both converted to 176.4 kHz / 24 bits
// in S24_3LE, then PCM of that transport, are one PCM transport: they follow
// in one sink, abutting, with no drain, no reopen and no PREPARE between, each
// what it is alone, the converted ones zero-padded.
func TestDSD64ThenDSD128ConvertedShareTheSink(t *testing.T) {
	d := newConvRig(t)
	a, b := track(playbacktest.DSD64, 0.25, noise(1)), track(playbacktest.DSD128, 0.25, noise(2))
	pcm := pattern(3, 8820, 24)
	d.files.add("a", a)
	d.files.add("b", b)
	d.files.add("c", wav(176400, 24, pcm))
	ids := d.insert("a", "b", "c")
	d.play()
	ob := d.preparedOcc(ids[1])
	oc := d.preparedOcc(ids[2])
	d.k.Free()
	d.waitStopped(engine.ReasonEndOfQueue)
	if v := d.k.View(); v.Opens != 1 || v.Drains != 1 || v.Prepares != 1 {
		t.Errorf("%d opens, %d drains, %d PREPAREs; want the three in one sink, its open's and its end's alone", v.Opens, v.Drains, v.Prepares)
	}
	wantA, _ := converted(t, a, convPlan)
	wantB, _ := converted(t, b, convPlan)
	same(t, "the DAC was given", unpack(t, d.k.Heard(), format.S243LE, 24), cat(wantA, wantB, pcm))
	for _, e := range d.rec.events() {
		if e.kind == "prepared" && (e.o == ob || e.o == oc) && !e.join.Gapless {
			t.Errorf("occurrence %d was prepared %+v, want gapless: one PCM transport", e.o, e.join)
		}
	}
}

// TestDeletingOrReplacingAPreparedConvertedSuccessor (B728 B11; the design's
// section 4.7): a, then b, which dsdconv.Continuous joins to a, b's first
// bits in a's join window; b is deleted, or replaced by a successor that
// abuts (DSD128) or one that is continuous (DSD64), at three moments. Before
// a's window is submitted, a goes back to its window point and nothing
// reaches the sink; after it is submitted and before the kernel has it, b's
// own frames in the sink or none of them, the cut goes to the window's first
// frame, and the window is submitted again, computed for what follows now.
// Either way the DAC is given what the oracle dsdconv.Continuous chooses
// gives: a alone, zero-padded, then the replacement, or a and a continuous
// replacement as one chain; nothing of b. Once the kernel has a frame of the
// window, the cut is refused, nothing dropped, and the engine stops playback
// when b is heard, as for any late cut: the DAC was given a's frames as b's
// bits made them, then b's start.
func TestDeletingOrReplacingAPreparedConvertedSuccessor(t *testing.T) {
	a, b := track(playbacktest.DSD64, 0.25, noise(1)), track(playbacktest.DSD64, 3, noise(2))
	c64, c128 := track(playbacktest.DSD64, 0.25, noise(3)), track(playbacktest.DSD128, 0.25, noise(4))
	variants := []struct {
		name string
		c    []byte
	}{{"deleted", nil}, {"replaced by one that abuts", c128}, {"replaced by one that is continuous", c64}}
	// The oracle for a then c, nothing between: chosen by the one rule.
	oracle := func(t *testing.T, c []byte) []int32 {
		if c == nil {
			w, _ := converted(t, a, convPlan)
			return w
		}
		fa := format.NativeDSD{BitRateHz: playbacktest.DSD64, Channels: 2, Frames: int64(0.25 * float64(playbacktest.DSD64))}
		fc := dsdOf(t, c).Format()
		if dsdconv.Continuous(fa, convPlan, fc, convPlan) {
			return chained(t, convPlan, a, c)
		}
		wa, _ := converted(t, a, convPlan)
		wc, _ := converted(t, c, convPlan)
		return cat(wa, wc)
	}
	// setup queues a, b and the variant's c, and plays. A producer the steps
	// hold is let go before the rig's teardown, which cleanups run last
	// registered first, so a test that fails while it is held ends with
	// nothing left running.
	setup := func(t *testing.T, steps *convSteps, c []byte) (*dsdRig, []uint32) {
		d := newConvRig(t, withSteps(steps))
		t.Cleanup(steps.letGo)
		d.files.add("a", a)
		d.files.add("b", b)
		refs := []string{"a", "b"}
		if c != nil {
			d.files.add("c", c)
			refs = append(refs, "c")
		}
		ids := d.insert(refs...)
		d.play()
		return d, ids
	}
	// finish plays the rest out and holds the DAC's bytes to want.
	finish := func(t *testing.T, d *dsdRig, want []int32) {
		d.k.Free()
		d.waitStopped(engine.ReasonEndOfQueue)
		same(t, "the DAC was given", unpack(t, d.k.Heard(), format.S243LE, 24), want)
		if v := d.k.View(); v.Opens != 1 {
			t.Errorf("%d opens: a cut in time needs no reopen", v.Opens)
		}
	}
	for _, v := range variants {
		t.Run("before its window is submitted/"+v.name, func(t *testing.T) {
			steps := newConvSteps(t, "decided")
			d, ids := setup(t, steps, v.c)
			ob := d.occurrenceOf(ids[1])
			steps.heldNow(t)
			if err := d.eng.Delete(bg, ids[1]); err != nil {
				t.Fatal(err)
			}
			steps.wait(t, "cut", ob)
			steps.letGo()
			finish(t, d, oracle(t, v.c))
		})
		t.Run("after its window is submitted, before the kernel has it/"+v.name, func(t *testing.T) {
			steps := newConvSteps(t, "")
			d, ids := setup(t, steps, v.c)
			ob := d.occurrenceOf(ids[1])
			d.waitEvents("b's first frames in the sink", func(evs []event) bool { return count(evs, "submitted", ob) > 0 })
			if err := d.eng.Delete(bg, ids[1]); err != nil {
				t.Fatal(err)
			}
			steps.wait(t, "cut", ob)
			finish(t, d, oracle(t, v.c))
		})
		t.Run("after its window is submitted, none of b's frames submitted/"+v.name, func(t *testing.T) {
			steps := newConvSteps(t, "ended")
			d, ids := setup(t, steps, v.c)
			ob := d.occurrenceOf(ids[1])
			steps.heldNow(t)
			if err := d.eng.Delete(bg, ids[1]); err != nil {
				t.Fatal(err)
			}
			steps.wait(t, "cut", ob)
			steps.letGo()
			finish(t, d, oracle(t, v.c))
		})
		t.Run("once the kernel has its window/"+v.name, func(t *testing.T) {
			steps := newConvSteps(t, "")
			d, ids := setup(t, steps, v.c)
			ob := d.occurrenceOf(ids[1])
			d.waitEvents("b's first frames in the sink", func(evs []event) bool { return count(evs, "submitted", ob) > 0 })
			d.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
			// 60 ms played of a's 250: the kernel's buffer, 200 ms, holds a's
			// end, its window's frames, and b's start.
			d.k.Play(t, 60*time.Millisecond)
			if err := d.eng.Delete(bg, ids[1]); err != nil {
				t.Fatal(err)
			}
			steps.wait(t, "cut", ob)
			d.k.Free()
			d.tickUntilStopped(engine.ReasonRemovedAudible)
			whole := chained(t, convPlan, a, b)
			fa := 2 * int(0.25*float64(playbacktest.DSD64)) / 16
			got := unpack(t, d.k.Heard(), format.S243LE, 24)
			if len(got) <= fa || len(got) > len(whole) {
				t.Fatalf("the DAC was given %d samples; want a's %d, its window b's, then b's start", len(got), fa)
			}
			same(t, "the DAC was given", got, whole[:len(got)])
		})
	}
}

// parkingSource is a DSD source whose reads past after bytes a channel wait
// for their context's end, which they answer: a source read the converter
// makes inside ReadFrames, on the session's life.
type parkingSource struct {
	decode.DSDDecoder
	after  int64
	read   int64
	parked chan struct{}
	once   sync.Once
	ended  chan error
}

func (p *parkingSource) ReadDSD(ctx context.Context, dst [][]byte) (int, error) {
	if p.read >= p.after {
		p.once.Do(func() { close(p.parked) })
		<-ctx.Done()
		select {
		case p.ended <- ctx.Err():
		default:
		}
		return 0, ctx.Err()
	}
	n, err := p.DSDDecoder.ReadDSD(ctx, dst)
	p.read += int64(n)
	return n, err
}

// TestStopDuringConversionIsBounded (B728 B11; the design's section 4.7): a
// Stop while the producer is inside the converter's read is the PCM sink's
// Drop, at once, and the session's release ends that read through its
// context, which reaches ReadFrames and the source under it: the release is
// confirmed, the engine stopped, every goroutine joined and every content
// closed, and nothing drained.
func TestStopDuringConversionIsBounded(t *testing.T) {
	src := &parkingSource{after: 64 << 10, parked: make(chan struct{}), ended: make(chan error, 1)}
	d := newConvRig(t, func(o *rigOptions) {
		if o.seams == nil {
			o.seams = &playback.Seams{}
		}
		o.seams.DSDSource = func(ref engine.Reference, s decode.DSDDecoder) decode.DSDDecoder {
			if ref.String() != "a" {
				return s
			}
			src.DSDDecoder = s
			return src
		}
	})
	d.files.add("a", track(playbacktest.DSD64, 2, noise(1)))
	d.insert("a")
	d.play()
	select {
	case <-src.parked:
	case <-time.After(alsapcm.Watchdog()):
		t.Fatalf("the converter's read did not reach the source's wait within %v", alsapcm.Watchdog())
	}
	if err := d.eng.Stop(bg); err != nil {
		t.Fatal(err)
	}
	s := d.waitSnap("stopped and released", func(s *engine.Snapshot) bool {
		_, unconfirmed := s.ReleaseUnconfirmed()
		return s.State() == engine.Stopped && !unconfirmed
	})
	if len(s.Faults()) != 0 {
		t.Errorf("a Stop during conversion faulted: %+v", s.Faults())
	}
	select {
	case err := <-src.ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the converter's read ended with %v, want the session's life's end", err)
		}
	case <-time.After(alsapcm.Watchdog()):
		t.Fatal("the converter's read never saw the release")
	}
	d.rig.stop()
	if v := d.k.View(); v.Drains != 0 || v.Open {
		t.Errorf("after the Stop: %d drains, open %v; want the sink dropped, not drained", v.Drains, v.Open)
	}
}

// TestTheClaimsAreThePlans (B728 B11; the design's sections 4.3 and 4.8): a
// converted occurrence's claims are read from its plan, never from the
// 176.4 kHz / 24-bit case: a 44.1 kHz, 16-bit DAC is said to get 16 bits at
// 44.1 kHz, a 20 kHz passband and TPDF dither, in Details (codec DSD, the
// plan's rate and bits, the frames of its PCM, so the duration is the DSD's,
// never lossless), in the observation (transport converted, the source's
// DSD rate, the filter, the gain, the samples it saturated) and in the set's
// converted rates; a 176.4 kHz, 24-bit DAC 24 bits, 30 kHz and no dither. The
// DAC's bytes are the plan's, and the conversion's clipping is said once.
func TestTheClaimsAreThePlans(t *testing.T) {
	for _, c := range []struct {
		name     string
		altsets  []alsa.Altset
		takes    format.ALSAFormat
		plan     dsdconv.Plan
		passband int
		dither   string
	}{
		{"a 44.1 kHz, 16-bit DAC", []alsa.Altset{pcmAltset(format.S16LE, 16, 44100)}, format.S16LE, dsdconv.Plan{RateHz: 44100, ValidBits: 16}, 20000, dsdconv.DitherTPDF},
		{"a 176.4 kHz, 24-bit DAC", playbacktest.AllAltsets(), format.S243LE, convPlan, 30000, dsdconv.DitherNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			var logged bytes.Buffer
			var logMu sync.Mutex
			d := newConvRig(t, func(o *rigOptions) { o.log = slog.New(slog.NewTextHandler(&lockedWriter{w: &logged, mu: &logMu}, nil)) })
			d.box.SetAltsets(card, 0, c.altsets...)
			d.k.HardwareFormats(formatCode[c.takes])
			a := track(playbacktest.DSD64, 0.25, overmodulated(1, 20000, 4000))
			d.files.add("a", a)
			d.insert("a")
			d.play()
			oa := d.firstOcc()
			want, clipped := converted(t, a, c.plan)
			if clipped == 0 {
				t.Fatal("the fixture saturates nothing: the test pins no count")
			}
			d.k.Free()
			d.waitStopped(engine.ReasonEndOfQueue)
			frames := int64(0.25*float64(playbacktest.DSD64)) / int64(playbacktest.DSD64/c.plan.RateHz)
			det, ok := d.detailsOf(oa)
			wantDet := engine.Details{Frames: frames, RateHz: c.plan.RateHz, Bits: c.plan.ValidBits, Channels: 2, Codec: "DSD", Lossless: false}
			if !ok || det != wantDet {
				t.Errorf("the Details reported %+v (%v), want %+v", det, ok, wantDet)
			}
			if got := time.Duration(det.Frames) * time.Second / time.Duration(det.RateHz); got != 250*time.Millisecond {
				t.Errorf("the Details' length is %v, want the DSD's 250 ms", got)
			}
			_, live := d.be.Facts()
			wantConv := playback.ConvertedPlayback{FromBitRate: playbacktest.DSD64, Filter: dsdconv.FilterName, PassbandHz: c.passband, Dither: c.dither, GainDB: 0, ClippedSamples: clipped}
			switch {
			case live == nil || live.Converted == nil:
				t.Fatalf("the observation %+v carries no plan", live)
			case live.Transport != playback.TransportConverted || live.Format != string(c.takes) || live.RateHz != c.plan.RateHz || live.ValidBits != c.plan.ValidBits || live.LogicalRate != 0:
				t.Errorf("the observation %+v, want converted %s at %d Hz from %d bits, no logical rate", live, c.takes, c.plan.RateHz, c.plan.ValidBits)
			case *live.Converted != wantConv:
				t.Errorf("the observation's plan %+v, want %+v", *live.Converted, wantConv)
			}
			p := d.be.Policy()
			for _, r := range p.Converted {
				if r.Plan != c.plan || r.Packing != c.takes || r.Plan.PassbandHz() != c.passband || r.Plan.Dither() != c.dither {
					t.Errorf("the set converts %d to %+v in %s (passband %d, dither %s), want %+v in %s", r.BitRate, r.Plan, r.Packing, r.Plan.PassbandHz(), r.Plan.Dither(), c.plan, c.takes)
				}
			}
			same(t, "the DAC was given", unpack(t, d.k.Heard(), c.takes, c.plan.ValidBits), want)
			logMu.Lock()
			lines := strings.Count(logged.String(), "saturated samples at full scale")
			logMu.Unlock()
			if lines != 1 {
				t.Errorf("the conversion's clipping was said %d times, want once:\n%s", lines, logged.String())
			}
		})
	}
}

// TestASeekInAConvertedTrackIsUnsupported (B728 B11; the design's section
// 4.7): seeking within converted DSD stays unsupported, as within native DSD
// and DoP. The engine knows the track for DSD by its Details' codec, which a
// converted occurrence's are, and refuses an absolute and a relative seek
// with seek_unsupported, changing nothing: the track plays on in its one
// sink and the DAC is given every converted frame once.
func TestASeekInAConvertedTrackIsUnsupported(t *testing.T) {
	d := newConvRig(t)
	a := track(playbacktest.DSD64, 0.5, noise(1))
	d.files.add("a", a)
	d.insert("a")
	d.play()
	d.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	d.k.Play(t, 50*time.Millisecond)
	d.tick()
	d.waitSnap("a heard, its Details known", func(s *engine.Snapshot) bool { tr, ok := s.Track(); return ok && tr.HasDetails })
	if err := d.eng.SeekAbsolute(bg, 0); engine.ReasonOf(err) != engine.ReasonSeekUnsupported {
		t.Errorf("an absolute seek in a converted track: %v, want %s", err, engine.ReasonSeekUnsupported)
	}
	if err := d.eng.SeekRelative(bg, -1); engine.ReasonOf(err) != engine.ReasonSeekUnsupported {
		t.Errorf("a relative seek in a converted track: %v, want %s", err, engine.ReasonSeekUnsupported)
	}
	d.k.Free()
	d.waitStopped(engine.ReasonEndOfQueue)
	want, _ := converted(t, a, convPlan)
	same(t, "the DAC was given", unpack(t, d.k.Heard(), format.S243LE, 24), want)
	if v := d.k.View(); v.Opens != 1 {
		t.Errorf("%d opens: a refused seek starts no session", v.Opens)
	}
}

// lockedWriter is a writer under a mutex, for a log the backend's goroutines
// write.
type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// TestAConvertedTrackIsNotLossless (B728 B11; the design's section 4.8): the
// Details of a converted occurrence, which OpenHome's Info reads as lossless
// 0, say codec DSD and never lossless, while a PCM track's beside it say its
// codec and lossless; the engine's track heard carries them.
func TestAConvertedTrackIsNotLossless(t *testing.T) {
	d := newConvRig(t)
	d.files.add("p", wav(176400, 24, pattern(1, 4410, 24)))
	d.files.add("a", track(playbacktest.DSD64, 0.25, noise(1)))
	ids := d.insert("p", "a")
	d.play()
	op := d.firstOcc()
	oa := d.preparedOcc(ids[1])
	d.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	// 50 ms played: p's 25 ms, then into a.
	d.k.Play(t, 50*time.Millisecond)
	d.tick()
	s := d.waitSnap("a heard", func(s *engine.Snapshot) bool {
		tr, ok := s.Track()
		return ok && tr.Occurrence == oa && tr.HasDetails
	})
	if tr, _ := s.Track(); tr.Details.Lossless || tr.Details.Codec != "DSD" {
		t.Errorf("the engine's track heard, the converted one, has Details %+v: want DSD, not lossless", tr.Details)
	}
	d.k.Free()
	d.waitStopped(engine.ReasonEndOfQueue)
	if det, ok := d.detailsOf(op); !ok || !det.Lossless || det.Codec != "WAV" {
		t.Errorf("the PCM track's Details %+v, want WAV, lossless", det)
	}
	if det, ok := d.detailsOf(oa); !ok || det.Lossless || det.Codec != "DSD" {
		t.Errorf("the converted track's Details %+v, want DSD, not lossless", det)
	}
}

// TestAConversionWithdrawnOrRetargetedIsReconciled (B728 B11; the design's
// section 3.5; binding note 1a): a converted occurrence is the effective
// policy's as native and DoP are, apart from its transport. Conversion
// withdrawn (an off written) while one plays revokes it at once, its PCM sink
// dropped and the session faulting; while one is prepared, the session playing
// PCM, it is handed off again and refused by the set's word, the PCM playing
// on. Its target changed (the DAC's altsets now stop at 88.2 kHz) while one
// plays revokes it; while one is prepared it is handed off again and plays at
// the new target, in a sink of its own.
func TestAConversionWithdrawnOrRetargetedIsReconciled(t *testing.T) {
	off := dsdstate.Defaults()
	off.DefaultMode = dsdstate.ModeOff
	retarget := func(d *dsdRig) {
		d.box.SetAltsets(card, 0, pcmAltset(format.S243LE, 24, 44100, 88200))
	}
	t.Run("withdrawn while it plays", func(t *testing.T) {
		d := newConvRig(t)
		d.files.add("a", track(playbacktest.DSD64, 2, noise(1)))
		d.insert("a")
		d.playedOut(100*time.Millisecond, "a")
		d.tick()
		d.waitSnap("a heard", func(s *engine.Snapshot) bool { _, ok := s.Track(); return ok })
		d.store.Set(off)
		d.monitorTickThatRevokes()
		s := d.tickUntilStopped(engine.ReasonFault)
		if f := s.Faults(); len(f) != 1 || f[0].Reason != "native_revoked" {
			t.Errorf("the faults %+v, want the converted occurrence revoked", f)
		}
		if p := d.be.Policy(); len(p.Converted) != 0 || p.Reason != "dsd_off_by_choice" {
			t.Errorf("the set published %+v (%q), want nothing converted, dsd_off_by_choice", p.Converted, p.Reason)
		}
	})
	t.Run("retargeted while it plays", func(t *testing.T) {
		d := newConvRig(t)
		d.files.add("a", track(playbacktest.DSD64, 2, noise(1)))
		d.insert("a")
		d.playedOut(100*time.Millisecond, "a")
		d.tick()
		d.waitSnap("a heard", func(s *engine.Snapshot) bool { _, ok := s.Track(); return ok })
		retarget(d)
		d.monitorTickThatRevokes()
		s := d.tickUntilStopped(engine.ReasonFault)
		if f := s.Faults(); len(f) != 1 || f[0].Reason != "native_revoked" {
			t.Errorf("the faults %+v, want the converted occurrence revoked", f)
		}
	})
	t.Run("withdrawn while it is prepared", func(t *testing.T) {
		d := newConvRig(t)
		pcm := pattern(1, 22050, 16)
		d.files.add("p", wav(44100, 16, pcm))
		d.files.add("a", track(playbacktest.DSD64, 0.25, noise(1)))
		ids := d.insert("p", "a")
		d.play()
		oa := d.preparedOcc(ids[1])
		d.store.Set(off)
		d.be.RefreshDSD(true)
		d.waitEvents("a handed off again", func(evs []event) bool { return count(evs, "replan", oa) > 0 })
		d.k.Free()
		s := d.waitEnded()
		if sk, ok := s.Skipped(ids[1]); !ok || sk.Reason != "dsd_off_by_choice" {
			t.Errorf("the converted successor was %+v (skipped %v), want it refused as dsd_off_by_choice", sk, ok)
		}
		if len(s.Faults()) != 0 {
			t.Errorf("the change faulted the PCM: %+v", s.Faults())
		}
		same(t, "the DAC was given", unpack(t, d.k.Heard(), format.S16LE, 16), pcm)
	})
	t.Run("retargeted while it is prepared", func(t *testing.T) {
		d := newConvRig(t)
		pcm := pattern(1, 22050, 16)
		a := track(playbacktest.DSD64, 0.25, noise(1))
		d.files.add("p", wav(44100, 16, pcm))
		d.files.add("a", a)
		ids := d.insert("p", "a")
		d.play()
		oa := d.preparedOcc(ids[1])
		retarget(d)
		d.be.RefreshDSD(true)
		d.waitEvents("a handed off again", func(evs []event) bool { return count(evs, "replan", oa) > 0 })
		d.k.Free()
		s := d.waitStopped(engine.ReasonEndOfQueue)
		if len(s.Skips()) != 0 || len(s.Faults()) != 0 {
			t.Errorf("the change skipped %+v or faulted %+v", s.Skips(), s.Faults())
		}
		v := d.k.View()
		if v.Opens != 2 || v.Committed != format.S243LE {
			t.Fatalf("%d opens, the last %s; want the PCM's and the converted one's at its new target", v.Opens, v.Committed)
		}
		given := d.k.Given()
		same(t, "the PCM's open was given", unpack(t, given[:v.OpenAt[1]], format.S16LE, 16), pcm)
		want, _ := converted(t, a, dsdconv.Plan{RateHz: 88200, ValidBits: 24})
		same(t, "the converted occurrence's open was given", unpack(t, given[v.OpenAt[1]:], format.S243LE, 24), want)
	})
}

// TestAConvertedPausesReopenChecksTheDACItWasConvertedFor (B728 B11; binding
// note 1a): the PCM sink a converted occurrence plays in checks every node it
// opens as the DAC the set the occurrence was admitted under was computed
// for, as a native sink does, the reopen after its pause's release included,
// whether the sink was opened for it or for the PCM track it joined. Another
// firmware in the bound unit by then is refused before a frame
// (wrong_device), the session left paused with its frames; once the DAC the
// set was computed for is back, the resume reopens and the DAC is given every
// frame once.
func TestAConvertedPausesReopenChecksTheDACItWasConvertedFor(t *testing.T) {
	for _, c := range []struct {
		name string
		pcm  []int32
	}{{"in a sink opened for it", nil}, {"in the sink of the PCM track it joined", pattern(1, 4410, 24)}} {
		t.Run(c.name, func(t *testing.T) {
			d := newConvRig(t, func(o *rigOptions) { o.pauseRelease = time.Second })
			a := track(playbacktest.DSD64, 1, noise(1))
			d.files.add("a", a)
			refs := []string{"a"}
			if c.pcm != nil {
				d.files.add("p", wav(176400, 24, c.pcm))
				refs = []string{"p", "a"}
			}
			ids := d.insert(refs...)
			d.play()
			d.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
			d.k.Play(t, 100*time.Millisecond)
			d.tick()
			d.waitSnap("a heard", func(s *engine.Snapshot) bool { tr, ok := s.Track(); return ok && tr.Entry == ids[len(ids)-1] })
			if err := d.eng.Pause(bg); err != nil {
				t.Fatalf("pause: %v", err)
			}
			d.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
			d.releaseDevice()
			other := playbacktest.DACUnit()
			other.BCDDevice = "0200"
			d.box.SetUnit(card, other)
			before := d.k.View()
			err := d.eng.Play(bg)
			var re *playback.ResumeError
			if !errors.As(err, &re) || re.Code != "wrong_device" {
				t.Fatalf("a resume onto another firmware: %v; want a ResumeError, wrong_device", err)
			}
			if v := d.k.View(); v.Open || v.Opens != before.Opens+1 || v.GivenBytes != before.GivenBytes {
				t.Fatalf("the refused node: open %v, %d opens (had %d), %d bytes given (had %d); want opened, closed, never written to",
					v.Open, v.Opens, before.Opens, v.GivenBytes, before.GivenBytes)
			}
			if s := d.eng.Snapshot(); s.State() != engine.Paused {
				t.Fatalf("after the refused reopen: %s, want paused", s.State())
			}
			d.box.SetUnit(card, playbacktest.DACUnit())
			if err := d.eng.Play(bg); err != nil {
				t.Fatalf("resume on the DAC the set was computed for: %v", err)
			}
			d.k.Free()
			d.waitStopped(engine.ReasonEndOfQueue)
			want, _ := converted(t, a, convPlan)
			same(t, "the DAC was given", unpack(t, d.k.Heard(), format.S243LE, 24), cat(c.pcm, want))
			if v := d.k.View(); v.Opens != 3 {
				t.Errorf("%d opens; want one, the refused reopen and the resume's", v.Opens)
			}
		})
	}
}
