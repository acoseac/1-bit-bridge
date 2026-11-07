package manifest

// The golden manifests of the client format gate (backlog B489): one library
// of every WavPack and Monkey's Audio shape the bridge indexes, beside a
// FLAC, as the manifest serves it to a client that declares nothing and to
// one that declares both formats, full and as a delta after a deletion. The
// files are in testdata/b489; BRIDGE_UPDATE_B489_GOLDENS=1 writes them from
// what the bridge serves, never by hand, and the iOS mirror's fixtures are
// copied from them.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/formatgate"
)

// goldenTime is every fixture file's and folder's mtime, so the goldens do
// not move with the clock.
var goldenTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// writeB489GoldenLibrary lays the goldens' library out under lib: a lossless
// WavPack file, a hybrid one with its correction file and one without, a
// DSD one, a float one, a file a WavPack reader refuses, a Monkey's Audio
// file of each layout and one of a version the reader does not take, and a
// FLAC, every file and folder at goldenTime.
func writeB489GoldenLibrary(t *testing.T, lib string) {
	t.Helper()
	tags := func(album, title, track string) []apeItem {
		return []apeItem{apeText("Title", title), apeText("Artist", "Glasswing"), apeText("Album", album),
			apeText("Track", track), apeText("Year", "2024")}
	}
	at := func(rel string) string { return filepath.Join(lib, filepath.FromSlash(rel)) }
	hybrid := wvSpec{rate: 44100, bytes: 2, channels: 2, hybrid: true, total: 300000, samples: 4096}
	writeWavPack(t, at("Glasswing/Packed/01 Lossless.wv"), stdWavPack(), tags("Packed", "Lossless", "1/6"))
	writeWavPack(t, at("Glasswing/Packed/02 Hybrid.wv"), hybrid, tags("Packed", "Hybrid", "2/6"))
	writeFixtureFile(t, at("Glasswing/Packed/02 Hybrid.wvc"), wvcFile(hybrid))
	writeWavPack(t, at("Glasswing/Packed/03 Hybrid Alone.wv"), hybrid, tags("Packed", "Hybrid Alone", "3/6"))
	writeWavPack(t, at("Glasswing/Packed/04 DSD.wv"),
		wvSpec{rate: 44100, bytes: 1, dsd: true, dsdPower: 3, channels: 2, total: 352800 * 2}, tags("Packed", "DSD", "4/6"))
	writeWavPack(t, at("Glasswing/Packed/05 Float.wv"),
		wvSpec{rate: 48000, bytes: 4, channels: 2, float: true, total: 48000}, tags("Packed", "Float", "5/6"))
	writeFixtureFile(t, at("Glasswing/Packed/06 Malformed.wv"), []byte("wvpk and nothing a decoder takes"))
	writeAPE(t, at("Glasswing/Monkey/01 Monkey.ape"), stdAPE(), tags("Monkey", "Monkey", "1/3"))
	old := stdAPE()
	old.version = 3950
	old.formatFlags = fxAPEFlagSeekElems | fxAPEFlagPeakLevel
	writeAPE(t, at("Glasswing/Monkey/02 Old Monkey.ape"), old, tags("Monkey", "Old Monkey", "2/3"))
	unsupported := stdAPE()
	unsupported.version = 4000
	writeAPE(t, at("Glasswing/Monkey/03 Unsupported.ape"), unsupported, tags("Monkey", "Unsupported", "3/3"))
	if err := os.MkdirAll(at("Glasswing/Plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeMinimalFLAC(t, at("Glasswing/Plain/01 Plain.flac"), 44100, 16,
		map[string]string{"TITLE": "Plain", "ARTIST": "Glasswing", "ALBUM": "Plain", "TRACKNUMBER": "1"})
	if err := filepath.WalkDir(lib, func(p string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(p, goldenTime, goldenTime)
	}); err != nil {
		t.Fatal(err)
	}
}

// goldenManifest is m as the goldens hold it: what the clock, the temporary
// directory and the run decide (generatedAt, libraryRoots, a track's
// firstIndexedAt) left out, indented.
func goldenManifest(t *testing.T, raw []byte) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	delete(m, "generatedAt")
	delete(m, "libraryRoots")
	if tracks, ok := m["tracks"].([]any); ok {
		for _, tr := range tracks {
			if row, ok := tr.(map[string]any); ok {
				delete(row, "firstIndexedAt")
			}
		}
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// requireGolden compares got with testdata/b489/name, or writes it there
// under BRIDGE_UPDATE_B489_GOLDENS.
func requireGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", "b489", name)
	if os.Getenv("BRIDGE_UPDATE_B489_GOLDENS") != "" {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (BRIDGE_UPDATE_B489_GOLDENS=1 writes it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from what the bridge serves now:\n%s", p, got)
	}
}

// TestTheGoldenManifestOfEachView: the manifest a client that declares no
// format is served, and the one a client that declares both is served, full
// and as a delta after a WavPack file is deleted, are the goldens. The first
// holds the FLAC alone and no tombstone of a gated path; the second every
// WavPack and Monkey's Audio row, a refused file's (the malformed WavPack
// file, the unsupported version) with no codec, the hybrid one's correction
// file on its row, and the deleted file's tombstone. Neither holds the
// correction file as a track.
func TestTheGoldenManifestOfEachView(t *testing.T) {
	ctx := context.Background()
	lib := filepath.Join(t.TempDir(), "Music")
	writeB489GoldenLibrary(t, lib)
	store := openTestStore(t)
	scanner := NewScanner([]string{lib}, store, "")
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	views := []struct {
		name string
		view formatgate.Set
	}{{"undeclared", formatgate.Set{}}, {"declared", formatgate.All()}}
	for _, v := range views {
		var buf bytes.Buffer
		if err := WriteManifest(ctx, &buf, store, []string{lib}, time.Time{}, v.view); err != nil {
			t.Fatal(err)
		}
		requireGolden(t, "manifest_full_"+v.name+".json", goldenManifest(t, buf.Bytes()))
	}

	since := time.Now().UTC()
	time.Sleep(2 * time.Millisecond)
	if err := os.Remove(filepath.Join(lib, "Glasswing", "Packed", "03 Hybrid Alone.wv")); err != nil {
		t.Fatal(err)
	}
	scanner.SetDeleteThreshold(1)
	if _, err := scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		var buf bytes.Buffer
		if err := WriteManifest(ctx, &buf, store, []string{lib}, since, v.view); err != nil {
			t.Fatal(err)
		}
		requireGolden(t, "manifest_delta_"+v.name+".json", goldenManifest(t, buf.Bytes()))
	}
}
