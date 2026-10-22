package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/dsn"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestServeGivesARowIndexedBeforeSampleFormatItsKind boots the real serve
// over a store whose rows predate tracks.sample_format (backlog B513). The
// store's open-time fill gives the ALAC row its kind from its codec; the
// header pass serve runs reads the float WAV's header through the API's own
// resolver, with no rescan of the file. The paired phone's manifest then
// carries both, and /v1/health advertises sampleFormat. The package tests
// pin the fill and the pass over a store; only a boot sees serve start the
// pass and hand it the resolver.
func TestServeGivesARowIndexedBeforeSampleFormatItsKind(t *testing.T) {
	const floatRel, alacRel = "Artist/Album/01 Float.wav", "Artist/Album/02 Lossless.m4a"
	passed := make(chan manifest.SampleFormatBackfill, 8)
	b := startConsoleBridge(t, "", func(lib string) {
		copySampleFormatFixture(t, "wav/float.wav", filepath.Join(lib, filepath.FromSlash(floatRel)))
		copySampleFormatFixture(t, "m4a/itunes_alac.m4a", filepath.Join(lib, filepath.FromSlash(alacRel)))
		indexBeforeSampleFormat(t, lib, filepath.Join(filepath.Dir(lib), "data"))
	}, func(o *serveOpts) {
		o.sampleFormatPassed = func(r manifest.SampleFormatBackfill) {
			select {
			case passed <- r:
			default:
			}
		}
	})
	waitForSampleFormatGain(t, passed, b.stderr)

	token := mintedToken(t, b)
	status, body := getBytesWithToken(t, b, "/v1/manifest", token)
	if status != http.StatusOK {
		t.Fatalf("GET /v1/manifest = %d: %s", status, body)
	}
	var m struct {
		Tracks []map[string]any `json:"tracks"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, tr := range m.Tracks {
		if p, ok := tr["path"].(string); ok {
			got[p] = tr["sampleFormat"]
		}
	}
	for rel, want := range map[string]string{floatRel: manifest.SampleFormatFloat, alacRel: manifest.SampleFormatInteger} {
		if got[rel] != want {
			t.Errorf("the manifest serves %s with sampleFormat %#v, want %q", rel, got[rel], want)
		}
	}

	var health api.HealthResponse
	getJSON(t, b.phone, b.apiBase+"/v1/health", "", &health)
	if !slices.Contains(health.Features, "sampleFormat") {
		t.Errorf("/v1/health does not advertise sampleFormat: %v", health.Features)
	}
}

// TestTheSampleFormatPassRunsAtBootAndAfterEveryScan: the loop serve starts
// runs one header pass at once (a scan that finished before serve registered
// its post-scan hook sends no nudge) and one per nudge after it, and returns
// when its context ends.
func TestTheSampleFormatPassRunsAtBootAndAfterEveryScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	nudge := make(chan struct{}, 1)
	ran := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSampleFormatBackfill(ctx, nudge, func(context.Context) { ran <- struct{}{} })
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	giveUp := serveGiveUp(t)
	wait := func(what string) {
		t.Helper()
		select {
		case <-ran:
		case <-giveUp:
			t.Fatalf("no pass ran %s", what)
		}
	}
	wait("at boot")
	nudge <- struct{}{}
	wait("after a scan's nudge")
	cancel()
	select {
	case <-done:
	case <-giveUp:
		t.Fatal("the loop did not return once its context ended")
	}
	if len(ran) != 0 {
		t.Fatalf("%d passes ran with no nudge", len(ran))
	}
}

// TestServeHandsTheSampleFormatPassThePostScanNudge reads runServe's wiring:
// the pass's nudge is one of postScanNudges, which the single post-scan hook
// pokes after every successful scan, and the loop runs on scanCtx, joined
// through bgWriters before the store closes. A nudge registered with a hook
// of its own would unhook the sweepers (SetPostScanHook replaces).
func TestServeHandsTheSampleFormatPassThePostScanNudge(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	for _, want := range []string{
		"postScanNudges = append(postScanNudges, sampleFormatNudge)",
		"runSampleFormatBackfill(scanCtx, sampleFormatNudge,",
	} {
		if n := strings.Count(body, want); n != 1 {
			t.Errorf("main.go holds %q %d times, want once", want, n)
		}
	}
	idx := strings.Index(body, "runSampleFormatBackfill(scanCtx, sampleFormatNudge,")
	if idx < 0 {
		return
	}
	window := body[max(0, idx-400):idx]
	if !strings.Contains(window, "bgWriters.Add(1)") || !strings.Contains(window, "defer bgWriters.Done()") {
		t.Error("the sample-format pass is not joined through bgWriters, so it can write after the store closes")
	}
}

// copySampleFormatFixture copies the manifest package's testdata file rel to
// dst, making dst's directory.
func copySampleFormatFixture(t *testing.T, rel, dst string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "manifest", "testdata", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// indexBeforeSampleFormat indexes lib into the store serve opens in dataDir,
// then rewinds every row to what a store from before tracks.sample_format
// holds, the column NULL. The edit is made with serve not running, on a
// connection of the test's own with the store's pragmas, the
// local-test-fixture skill's route: internal/manifest, imported here,
// registers unicode_lower.
func indexBeforeSampleFormat(t *testing.T, lib, dataDir string) {
	t.Helper()
	dbPath := manifest.DefaultDBPath(dataDir)
	store, err := manifest.OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = manifest.NewScanner([]string{lib}, store, "").Scan(context.Background())
	if cerr := store.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatalf("index the library: %v", err)
	}
	db, err := sql.Open("sqlite", dsn.File(dbPath, "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`UPDATE tracks SET sample_format = NULL`)
	var n int64
	if err == nil {
		n, err = res.RowsAffected()
	}
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	if err != nil || n != 2 {
		t.Fatalf("rewind the rows: %d rows, %v", n, err)
	}
}

// waitForSampleFormatGain waits for a header pass that gave a row its kind.
func waitForSampleFormatGain(t *testing.T, passed <-chan manifest.SampleFormatBackfill, stderr *safeBuffer) {
	t.Helper()
	giveUp := serveGiveUp(t)
	for {
		select {
		case r := <-passed:
			if r.Gained > 0 {
				return
			}
		case <-giveUp:
			t.Fatalf("no header pass gave a row its kind before the test's deadline; stderr=%s", stderr.String())
		}
	}
}
