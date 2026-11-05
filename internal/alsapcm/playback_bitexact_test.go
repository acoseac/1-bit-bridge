package alsapcm_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
)

// goldens are PR 1's PCM fixtures that decode, from both manifests: their
// samples' hashes were made by reference tools (flac, ffmpeg, afconvert),
// never by the bridge's decoder.
func goldens(t *testing.T) []golden {
	t.Helper()
	dir := filepath.Join("..", "player", "decode", "testdata")
	var out []golden
	var pcm []struct {
		File          string `json:"file"`
		RateHz        int    `json:"rate_hz"`
		ValidBits     int    `json:"valid_bits"`
		Frames        int64  `json:"frames"`
		SamplesSHA256 string `json:"samples_sha256"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "goldens", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &pcm); err != nil {
		t.Fatal(err)
	}
	for _, g := range pcm {
		out = append(out, golden{file: filepath.Join("goldens", g.File), rate: g.RateHz, bits: g.ValidBits, frames: g.Frames, sha: g.SamplesSHA256})
	}
	var alac struct {
		Fixtures []struct {
			File          string `json:"file"`
			RateHz        int    `json:"rate_hz"`
			ValidBits     int    `json:"valid_bits"`
			Frames        int64  `json:"frames"`
			SamplesSHA256 string `json:"samples_sha256"`
			Refusal       string `json:"refusal"`
		} `json:"fixtures"`
	}
	b, err = os.ReadFile(filepath.Join(dir, "alac-goldens", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &alac); err != nil {
		t.Fatal(err)
	}
	for _, g := range alac.Fixtures {
		out = append(out, golden{file: filepath.Join("alac-goldens", g.File), rate: g.RateHz, bits: g.ValidBits, frames: g.Frames, sha: g.SamplesSHA256, refusal: g.Refusal})
	}
	for i := range out {
		data, err := os.ReadFile(filepath.Join(dir, out[i].file))
		if err != nil {
			t.Fatal(err)
		}
		out[i].data = data
		out[i].ref = "golden:" + filepath.Base(out[i].file)
	}
	return out
}

// TestPlaybackIsBitExactThroughTheEngine pins the backend's purpose: every
// PR 1 golden, FLAC, WAV, AIFF, AIFC and ALAC, played end to end through the
// engine's Run, the backend and the real sink, for each packing a DAC may
// take for its depth (S16_LE; S24_3LE, S24_LE and S32_LE shifted left eight
// for 24 bits; S32_LE), reaches the fake DAC as samples whose hash is the
// reference tools': nothing resampled, dithered, scaled or dropped, and the
// bytes a packing leaves zero zero. A DAC that takes the depth's first
// packing gets it; one that takes only a later one gets that one.
func TestPlaybackIsBitExactThroughTheEngine(t *testing.T) {
	for _, g := range goldens(t) {
		if g.refusal != "" {
			continue
		}
		for i, f := range map[int][]format.ALSAFormat{16: {format.S16LE}, 24: {format.S243LE, format.S24LE, format.S32LE}, 32: {format.S32LE}}[g.bits] {
			t.Run(filepath.Base(g.file)+"/"+string(f), func(t *testing.T) {
				r := newRig(t)
				r.k.Formats(f)
				r.k.Free()
				r.files.add(g.ref, g.data)
				r.insert(g.ref)
				r.play()
				r.waitStopped(engine.ReasonEndOfQueue)
				// Each packing offered before the one the DAC takes is one open
				// its refine refused, closed with nothing written.
				if v := r.k.View(); v.Committed != f || v.Opens != i+1 {
					t.Fatalf("the DAC committed %s in %d opens, want %s in %d", v.Committed, v.Opens, f, i+1)
				}
				got := unpack(t, r.k.Heard(), f, g.bits)
				if int64(len(got)) != 2*g.frames || sumOf(got) != g.sha {
					t.Fatalf("the DAC heard %d samples hashing %s; want %d samples, the reference tools' %s", len(got), sumOf(got), 2*g.frames, g.sha)
				}
			})
		}
	}
}

// TestPlaybackPrefersTheDepthsOwnContainer pins the packing's order: a DAC
// that takes every packing of 24 bits gets S24_3LE, the container as wide as
// the samples.
func TestPlaybackPrefersTheDepthsOwnContainer(t *testing.T) {
	r := newRig(t)
	r.k.Formats(format.S16LE, format.S243LE, format.S24LE, format.S32LE)
	r.k.Free()
	r.files.add("a", wav(48000, 24, pattern(1, 3000, 24)))
	r.insert("a")
	r.play()
	r.waitStopped(engine.ReasonEndOfQueue)
	if v := r.k.View(); v.Committed != format.S243LE {
		t.Errorf("24-bit content went out as %s, want S24_3LE", v.Committed)
	}
}

// TestPlaybackJoinsOneTransportWithNothingBetween pins the gapless join: two
// entries of one transport, through the engine, reach the DAC as the two
// decodes concatenated, with nothing between, in one open of the device,
// with no drain, reopen or PREPARE between them, and the successor is
// reported Prepared gapless.
func TestPlaybackJoinsOneTransportWithNothingBetween(t *testing.T) {
	gs := goldens(t)
	pick := func(name string) golden {
		for _, g := range gs {
			if filepath.Base(g.file) == name {
				return g
			}
		}
		t.Fatalf("no golden %s", name)
		return golden{}
	}
	a, b := pick("decorrelated_16bit_44100.flac"), pick("extrema_16bit_44100.wav")
	r := newRig(t)
	r.k.Formats(format.S16LE)
	r.k.Free()
	r.files.add(a.ref, a.data)
	r.files.add(b.ref, b.data)
	r.insert(a.ref, b.ref)
	r.play()
	r.waitStopped(engine.ReasonEndOfQueue)
	got := unpack(t, r.k.Heard(), format.S16LE, 16)
	if int64(len(got)) != 2*(a.frames+b.frames) {
		t.Fatalf("the DAC heard %d samples, want %d: the two entries and nothing between", len(got), 2*(a.frames+b.frames))
	}
	if sumOf(got[:2*a.frames]) != a.sha || sumOf(got[2*a.frames:]) != b.sha {
		t.Fatalf("the DAC heard %s then %s; want %s then %s", sumOf(got[:2*a.frames]), sumOf(got[2*a.frames:]), a.sha, b.sha)
	}
	if v := r.k.View(); v.Opens != 1 || v.Drains != 1 || v.Prepares != 1 {
		t.Errorf("between two gapless entries: %d opens, %d drains, %d PREPAREs; want the open's and the end's alone", v.Opens, v.Drains, v.Prepares)
	}
	evs := r.rec.events()
	for _, e := range evs {
		if e.kind == "prepared" && (!e.join.Gapless || len(e.join.Differs) != 0) {
			t.Errorf("the successor was prepared %+v, want gapless", e.join)
		}
	}
	if count(evs, "prepared", 0) != 1 || count(evs, "audible", 0) != 2 || count(evs, "finished", 0) != 1 {
		t.Errorf("events %s; want one prepared, two audible, finished", kinds(evs))
	}
}

// TestPlaybackReopensForAnotherTransport pins the honest gap: an entry of
// another rate and depth follows through a drain, a close and an open of the
// device for its transport within the session; its join names what differs;
// each open's bytes are exactly its own entry's, so no byte of one entry
// went out in the other's open.
func TestPlaybackReopensForAnotherTransport(t *testing.T) {
	r := newRig(t)
	r.k.Formats(format.S16LE, format.S243LE)
	r.k.Free()
	a, b := pattern(1, 6000, 16), pattern(2, 7000, 24)
	r.files.add("a", wav(44100, 16, a))
	r.files.add("b", wav(48000, 24, b))
	r.insert("a", "b")
	r.play()
	r.waitStopped(engine.ReasonEndOfQueue)
	v := r.k.View()
	if v.Opens != 2 || v.Closes != 2 || len(v.OpenAt) != 2 {
		t.Fatalf("%d opens and %d closes, want two of each", v.Opens, v.Closes)
	}
	given := r.k.Given()
	first, second := given[:v.OpenAt[1]], given[v.OpenAt[1]:]
	if got := unpack(t, first, format.S16LE, 16); sumOf(got) != sumOf(a) {
		t.Errorf("the first open was given %d samples, not exactly entry a's %d", len(got), len(a))
	}
	if got := unpack(t, second, format.S243LE, 24); sumOf(got) != sumOf(b) {
		t.Errorf("the second open was given %d samples, not exactly entry b's %d", len(got), len(b))
	}
	if v.Drains != 2 {
		t.Errorf("%d drains, want one before the reopen and one at the end", v.Drains)
	}
	var joined bool
	for _, e := range r.rec.events() {
		if e.kind == "prepared" {
			joined = true
			if e.join.Gapless || len(e.join.Differs) != 3 {
				t.Errorf("b was prepared %+v; want not gapless, the rate, the format and the valid bits named", e.join)
			}
		}
	}
	if !joined {
		t.Errorf("b was never reported prepared: %s", kinds(r.rec.events()))
	}
}

// TestPlaybackPlaysSixteenBitsOnADACWithoutS16LE pins B735: a DAC whose
// stream offers S32_LE and no S16_LE, as the Mytek Brooklyn DAC+'s does
// (S32_LE, SPECIAL and DSD_U32_BE: formats 10, 31 and 52), plays 16-bit
// content through the engine's Run as 16 valid bits in S32_LE, each sample
// shifted left sixteen bits with its low two bytes zero, once the packings
// before it (S16_LE, S24_3LE, S24_LE) were each refused at the open and
// closed unwritten; and what the backend says of the open, the note serve
// logs and the facts GET /v1/outputs serves, is S32_LE from 16 valid bits.
// Before B735 the table packed 16 bits as S16_LE alone, and the entry was
// refused as transport_not_supported.
func TestPlaybackPlaysSixteenBitsOnADACWithoutS16LE(t *testing.T) {
	r := newRig(t)
	r.k.HardwareFormats(10, 31, 52)
	r.k.Free()
	// A negative sample whose two bytes differ, a positive one and the
	// extremes lead, so the DAC's first bytes can be read off one by one.
	samples := append([]int32{-0x1235, 0x1234, -32768, 32767}, pattern(1, 3000, 16)...)
	r.files.add("cd", wav(44100, 16, samples))
	ids := r.insert("cd")
	if err := r.eng.Play(bg); err != nil {
		sk, _ := r.eng.Snapshot().Skipped(ids[0])
		t.Fatalf("play: %v; the 16-bit entry was refused, %s: %s", err, sk.Reason, sk.Detail)
	}
	s := r.waitSnap("playback to stop", func(s *engine.Snapshot) bool {
		return s.State() == engine.Stopped && s.LastStop().Reason != ""
	})
	if sk, ok := s.Skipped(ids[0]); ok {
		t.Fatalf("the 16-bit entry was refused, %s: %s", sk.Reason, sk.Detail)
	}
	if got := s.LastStop(); got.Reason != engine.ReasonEndOfQueue {
		t.Fatalf("playback stopped for %s (%s); want end_of_queue", got.Reason, got.Detail)
	}
	if v := r.k.View(); v.Committed != format.S32LE || v.Opens != 4 {
		t.Errorf("the DAC committed %s in %d opens; want S32_LE in 4, S16_LE, S24_3LE and S24_LE each refused first", v.Committed, v.Opens)
	}
	heard := r.k.Heard()
	want := []byte{
		0x00, 0x00, 0xcb, 0xed, // -0x1235, which is 0xedcb
		0x00, 0x00, 0x34, 0x12, // 0x1234
		0x00, 0x00, 0x00, 0x80, // -32768
		0x00, 0x00, 0xff, 0x7f, // 32767
	}
	if len(heard) < len(want) || !bytes.Equal(heard[:len(want)], want) {
		t.Fatalf("the DAC's first two frames are % x; want % x", heard[:min(len(heard), len(want))], want)
	}
	if got := unpack(t, heard, format.S32LE, 16); sumOf(got) != sumOf(samples) {
		t.Errorf("the DAC heard %d samples hashing %s; want the %d written, %s", len(got), sumOf(got), len(samples), sumOf(samples))
	}
	var opened *playback.Note
	for drained := false; !drained; {
		select {
		case n := <-r.notes:
			if n.Kind == playback.NoteOpened {
				opened = &n
			}
		default:
			drained = true
		}
	}
	if opened == nil || opened.Params.Format != format.S32LE || opened.Transport.Format != format.S32LE || opened.Transport.ValidBits != 16 {
		t.Errorf("the open was noted as %+v; want S32_LE committed, from 16 valid bits", opened)
	}
	if _, live := r.be.Facts(); live == nil || live.Format != string(format.S32LE) || live.ValidBits != 16 || live.Transport != playback.TransportPCM {
		t.Errorf("the playback facts are %+v; want S32_LE from 16 valid bits, pcm", live)
	}
}
