package listeningverify

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// synthetic are the limits the small sets these tests build are held to: a
// second-order loop, so the noise bounds are far looser than production's,
// and the channel, level, frequency and framing checks just as strict.
var synthetic = limits{silence: -40, hf: map[int]float64{2822400: 10}, hfSpread: 6, line: 80, level: 0.05, frequency: 0.01, absent: -50, run: 64, boundary: -10}

const synthRate = 2822400

// synthProgramme is a 2 s programme: 0.6 s silence, 0.3 s left 1 kHz, 0.1 s
// silence, 0.3 s right 2 kHz, silence; 10 ms raised-cosine ramps.
func synthProgramme(channel int, t float64) float64 {
	burst := func(start, end, f float64) float64 {
		if t < start || t >= end {
			return 0
		}
		env := 1.0
		if in := t - start; in < 0.01 {
			env = 0.5 * (1 - math.Cos(math.Pi*in/0.01))
		} else if out := end - t; out < 0.01 {
			env = 0.5 * (1 - math.Cos(math.Pi*out/0.01))
		}
		return env * math.Sin(2*math.Pi*f*(t-start))
	}
	if channel == 0 {
		return burst(0.6, 0.9, 1000)
	}
	return burst(1.0, 1.3, 2000)
}

// synthDSD modulates the programme with a second-order 1-bit loop, NTF
// (1 - z^-1)^2, dithered: canonical bytes per channel, bit 7 earliest.
func synthDSD(bits int) [2][]byte {
	var out [2][]byte
	seed := uint64(0x9e3779b97f4a7c15)
	next := func() float64 {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		return float64(seed>>11) / (1 << 53)
	}
	for c := 0; c < 2; c++ {
		out[c] = make([]byte, (bits+7)/8)
		e1, e2 := 0.0, 0.0
		for k := 0; k < bits; k++ {
			u := 0.5 * math.Pow(10, -1.5) * synthProgramme(c, float64(k)/synthRate)
			v := u - 2*e1 + e2 // y = u + (1 - z^-1)^2 e
			y := -1.0
			if v+0.1*(next()-next()) >= 0 {
				y = 1
				out[c][k>>3] |= 0x80 >> uint(k&7)
			}
			e2, e1 = e1, y-v
		}
	}
	return out
}

// writeDSF writes a DSF file of canonical channels: LSB-first unless msb,
// declaring bits per sample declared (1 or 8) whatever the storage, the
// channels swapped when swap.
func writeDSF(t *testing.T, path string, ch [2][]byte, bits int, msb bool, declared uint32, swap bool) {
	t.Helper()
	nbytes := (bits + 7) / 8
	blocks := (nbytes + 4095) / 4096
	data := make([]byte, blocks*8192)
	for c := 0; c < 2; c++ {
		src := ch[c]
		if swap {
			src = ch[1-c]
		}
		for i := 0; i < nbytes; i++ {
			v := src[i]
			if !msb {
				v = bitrev(v)
			}
			data[(i/4096)*8192+c*4096+i%4096] = v
		}
	}
	var b bytes.Buffer
	le32 := func(v uint32) { _ = binary.Write(&b, binary.LittleEndian, v) }
	le64 := func(v uint64) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("DSD ")
	le64(28)
	le64(uint64(92 + len(data)))
	le64(0)
	b.WriteString("fmt ")
	le64(52)
	for _, v := range []uint32{1, 0, 2, 2, synthRate, declared} {
		le32(v)
	}
	le64(uint64(bits))
	le32(4096)
	le32(0)
	b.WriteString("data")
	le64(uint64(12 + len(data)))
	b.Write(data)
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeWAV writes the programme's 32-bit WAVE_FORMAT_EXTENSIBLE reference.
func writeWAV(t *testing.T, path string, rate, frames int) {
	t.Helper()
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	data := frames * 8
	b.WriteString("RIFF")
	w(uint32(60 + data))
	b.WriteString("WAVEfmt ")
	w(uint32(40))
	w(uint16(0xFFFE))
	w(uint16(2))
	w(uint32(rate))
	w(uint32(rate * 8))
	w(uint16(8))
	w(uint16(32))
	w(uint16(22))
	w(uint16(32))
	w(uint32(3))
	b.Write([]byte{1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xAA, 0, 0x38, 0x9B, 0x71})
	b.WriteString("data")
	w(uint32(data))
	for k := 0; k < frames; k++ {
		for c := 0; c < 2; c++ {
			w(int32(math.Round(math.Pow(10, -1.5) * math.Ldexp(1, 31) * synthProgramme(c, float64(k)/float64(rate)))))
		}
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// synthManifest is the manifest of a set of one LR DSF and its reference,
// hashes given.
func synthManifest(dsdHash, refHash string) map[string]any {
	return map[string]any{
		"schemaVersion": 1,
		"calibration": map[string]any{"reference": "a second-order test loop, never a listening asset", "peakDBFS": -30,
			"leadingSilenceMS": 1000, "trailingSilenceMS": 1000, "attestation": qualification.CalibrationPhrase},
		"fixtures": []any{
			map[string]any{"ID": "dsd", "Path": "dsd.dsf", "SHA256": dsdHash, "Container": "DSF", "Order": "LR", "PCMReference": "ref", "Rate": synthRate, "Channels": 2, "Frames": 2 * synthRate},
			map[string]any{"ID": "ref", "Path": "ref.wav", "SHA256": refHash, "Container": "WAV", "Order": "LR", "PCMReference": "", "Rate": 44100, "Channels": 2, "Frames": 2 * 44100},
		},
	}
}

// synthSet is a small asset directory: one LR DSF and its reference, the
// manifest that names them and runs.json with one matrix of the DSF; the
// DSF written LSB-first, unless msb, declaring the bits per sample it is
// written in unless declared says otherwise, its channels swapped when swap.
type synthSet struct {
	msb, swap bool
	declared  uint32
	// step, when set, makes the left channel's last 10 ms (3528 bytes,
	// one analysis block) 0xEE: density three quarters, a DC step at the
	// file's end, no long run, and no tone in the block's bins.
	step bool
}

func (s synthSet) build(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	assets := filepath.Join(dir, "assets")
	if err := os.Mkdir(assets, 0o700); err != nil {
		t.Fatal(err)
	}
	bits := 2 * synthRate
	declared := s.declared
	if declared == 0 {
		declared = 1
		if s.msb {
			declared = 8
		}
	}
	ch := synthDSD(bits)
	if s.step {
		for i := len(ch[0]) - 3528; i < len(ch[0]); i++ {
			ch[0][i] = 0xEE
		}
	}
	writeDSF(t, filepath.Join(assets, "dsd.dsf"), ch, bits, s.msb, declared, s.swap)
	writeWAV(t, filepath.Join(assets, "ref.wav"), 44100, 2*44100)
	hash := func(name string) string {
		b, err := os.ReadFile(filepath.Join(assets, name))
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%x", sha256.Sum256(b))
	}
	put := func(name string, v any) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put("manifest.json", synthManifest(hash("dsd.dsf"), hash("ref.wav")))
	put("runs.json", map[string]any{"runs": []run{{Manifest: "manifest.json", Transport: "native", Format: "DSD_U32_BE", Rate: synthRate, Scenario: "matrix", Fixtures: []string{"dsd"}}}})
	return dir
}

func has(problems []string, want string) bool {
	for _, p := range problems {
		if strings.Contains(p, want) {
			return true
		}
	}
	return false
}

// TestTheCheckPassesASoundSet: a fixture whose channels, levels, timing and
// framing match its reference and its manifest passes, and the report names
// what it measured.
func TestTheCheckPassesASoundSet(t *testing.T) {
	t.Parallel()
	report, problems := checkDir(synthSet{}.build(t), synthetic)
	if len(problems) != 0 {
		t.Fatalf("a sound set was refused: %q", problems)
	}
	for _, want := range []string{
		"manifest.json: 2 fixtures; schema, ReadManifest and CheckAssets pass",
		"dsd.dsf: 5644800 bits a channel at 2822400, bridge decoder and independent reader agree; order LR",
		"dsd.dsf against ref.wav",
		"boundaries of dsd (0x69, the joins, 0x69)",
		"runs.json: 1 runs hold to dsd-test's fixture-list rules",
	} {
		if !has(report, want) {
			t.Fatalf("the report lacks %q: %q", want, report)
		}
	}
}

// TestTheCheckCatchesASwappedChannelPair is the negative control: an LR
// fixture whose channels were swapped, its manifest and reference still
// saying LR, is caught as RL.
func TestTheCheckCatchesASwappedChannelPair(t *testing.T) {
	t.Parallel()
	_, problems := checkDir(synthSet{swap: true}.build(t), synthetic)
	for _, want := range []string{`manifest.json dsd (dsd.dsf): decoded channel order "RL", the manifest says "LR"`, `the DSD's channel order is "RL", its reference's "LR"`} {
		if !has(problems, want) {
			t.Fatalf("a swapped fixture is not caught as %q: %q", want, problems)
		}
	}
}

// TestTheCheckCatchesTheWrongBitOrder: LSB-first bytes declared MSB-first
// decode as noise, not the reference's tones.
func TestTheCheckCatchesTheWrongBitOrder(t *testing.T) {
	t.Parallel()
	_, problems := checkDir(synthSet{declared: 8}.build(t), synthetic)
	if !has(problems, "against the reference's") {
		t.Fatalf("a fixture of the wrong bit order passed: %q", problems)
	}
}

// TestTheCheckCatchesAStepAtAFilesEnd: a file that ends off its own mean
// clicks where the sink joins it to its 0x69 tail, and the boundary check
// says so, when nothing else in the fixture is wrong.
func TestTheCheckCatchesAStepAtAFilesEnd(t *testing.T) {
	t.Parallel()
	_, problems := checkDir(synthSet{step: true}.build(t), synthetic)
	if len(problems) != 1 || !has(problems, "the boundaries of dsd: boundary 1 holds") {
		t.Fatalf("a step at the file's end is not the one problem caught: %q", problems)
	}
}

// TestTheSchemaRefusesWhatTheLoaderTakes: the qualification loader reads a
// manifest with a key the schema does not allow and a hash in capitals; the
// schema refuses both, and both refuse a third channel. checkDir runs the two
// on every manifest (TestTheCheckPassesASoundSet).
func TestTheSchemaRefusesWhatTheLoaderTakes(t *testing.T) {
	schema, err := loadSchema("manifest-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("ab", 32)
	fixture := func(m map[string]any) map[string]any { return m["fixtures"].([]any)[0].(map[string]any) }
	for _, c := range []struct {
		name   string
		mutate func(map[string]any)
		want   string
		// loaderTakes is whether qualification.ReadManifest reads it.
		loaderTakes bool
	}{
		{"nothing changed", func(map[string]any) {}, "", true},
		{"an unknown key", func(m map[string]any) { fixture(m)["Gain"] = 1 }, "has Gain, which the schema does not allow", true},
		{"a hash in capitals", func(m map[string]any) { fixture(m)["SHA256"] = strings.ToUpper(hash) }, "does not match ^[0-9a-f]{64}$", true},
		{"three channels", func(m map[string]any) { fixture(m)["Channels"] = 3 }, "Channels: is not 2", false},
		{"no Order", func(m map[string]any) { delete(fixture(m), "Order") }, "lacks Order", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := synthManifest(hash, hash)
			c.mutate(m)
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			v, err := decodeJSON(b)
			if err != nil {
				t.Fatal(err)
			}
			problems := validate(schema, v, "manifest")
			if (c.want == "") != (len(problems) == 0) || (c.want != "" && !has(problems, c.want)) {
				t.Fatalf("the schema's verdict on %s: %q", c.name, problems)
			}
			if _, err := qualification.ReadManifest(bytes.NewReader(b)); (err == nil) != c.loaderTakes {
				t.Fatalf("ReadManifest on %s: %v", c.name, err)
			}
		})
	}
}

// TestTheRunsAreHeldToTheHarnessRules: a DoP run whose fixture's reference
// is not at the carrier, a matrix that lists a WAV fixture, a DoP contact of
// two fixtures and a lifecycle with no DSD fixture are each refused as
// dsd-test refuses them, and the matrix of the DSF alone is taken. checkDir
// runs every run of runs.json through it (TestTheCheckPassesASoundSet).
func TestTheRunsAreHeldToTheHarnessRules(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	b, err := json.Marshal(synthManifest(hash, hash))
	if err != nil {
		t.Fatal(err)
	}
	m, err := qualification.ReadManifest(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	native := run{Manifest: "manifest.json", Transport: "native", Format: "DSD_U32_BE", Rate: synthRate}
	dop := run{Manifest: "manifest.json", Transport: "dop", Format: "S32_LE", Rate: synthRate}
	with := func(r run, scenario string, fixtures ...string) run {
		r.Scenario, r.Fixtures = scenario, fixtures
		return r
	}
	for _, c := range []struct {
		name string
		r    run
		want string
	}{
		{"the matrix of the DSF", with(native, "matrix", "dsd"), ""},
		{"a DoP reference off its carrier", with(dop, "matrix", "dsd"), "DoP requires same-carrier PCM reference"},
		{"a WAV in a matrix", with(native, "matrix", "dsd", "ref"), "fixture-list tuple mismatch: ref"},
		{"a DoP contact of two", with(dop, "level-check", "dsd", "dsd"), "DoP contact requires exactly one fixture"},
		{"a lifecycle of PCM alone", with(native, "backend-lifecycle", "ref"), "backend lifecycle requires native execution"},
	} {
		t.Run(c.name, func(t *testing.T) {
			problems := checkRun(c.r, m)
			if (c.want == "") != (len(problems) == 0) || (c.want != "" && !has(problems, c.want)) {
				t.Fatalf("%s: %q", c.name, problems)
			}
		})
	}
}
