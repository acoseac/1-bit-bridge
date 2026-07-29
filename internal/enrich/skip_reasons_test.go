package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestSkipReasonsAttributeTheRightCause is the reason this whole surface
// exists. "5,435 tracks are short of something" is the same number
// whether the library is genuinely unmatchable or the matcher is broken;
// "3,557 of them are no_mb_match" is the number that tells them apart.
//
// Two tracks, two distinct causes, one enricher run.
func TestSkipReasonsAttributeTheRightCause(t *testing.T) {
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Clean response, no candidates → the searchable track is a
		// genuine no-match rather than an error.
		_, _ = io.WriteString(w, `{"releases":[]}`)
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	// The "An Unknown Artist / CD 02" shape — nothing to search by.
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: "untagged.flac", Size: 1, ModTime: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Searchable, but MusicBrainz has nothing.
	if err := store.UpsertTrack(ctx, &manifest.Track{
		Path: "obscure.flac", Size: 1, ModTime: time.Now(),
		Artist: "Ducu Bertzi", Album: "Dor de duca",
	}); err != nil {
		t.Fatal(err)
	}

	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, filepath.Join(dir, "artwork"))
	defer startEnricherForTest(e, 3*time.Second)()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.skipped.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.skipped.Load(); got < 2 {
		t.Fatalf("only %d tracks skipped, want 2", got)
	}

	reasons := e.SkipReasons()
	if reasons[skipReasonNoSearchTerms] != 1 {
		t.Errorf("%s = %d, want 1 (the untagged track)",
			skipReasonNoSearchTerms, reasons[skipReasonNoSearchTerms])
	}
	if reasons[skipReasonNoMBMatch] != 1 {
		t.Errorf("%s = %d, want 1 (the searchable-but-absent track)",
			skipReasonNoMBMatch, reasons[skipReasonNoMBMatch])
	}
	if reasons[skipReasonMBError] != 0 {
		t.Errorf("%s = %d, want 0 — a clean empty response is not an error",
			skipReasonMBError, reasons[skipReasonMBError])
	}
}

// TestSkipReasonsKeysStayBounded guards the cardinality contract. The map
// keys must be the fixed skipReason* set — never a formatted error
// string, which would mint a fresh key per distinct upstream message and
// make the map grow without bound on a flaky network.
func TestSkipReasonsKeysStayBounded(t *testing.T) {
	// Every response is a persistent 400, so every track lands on the
	// mb_error path with a DIFFERENT body.
	var n int
	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"distinct message `+string(rune('a'+n%26))+`"}`)
	}))
	defer mbSrv.Close()
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer caaSrv.Close()

	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for _, a := range []string{"Alpha", "Beta", "Gamma", "Delta", "Epsilon"} {
		if err := store.UpsertTrack(ctx, &manifest.Track{
			Path: a + ".flac", Size: 1, ModTime: time.Now(),
			Artist: a, Album: a + " Album",
		}); err != nil {
			t.Fatal(err)
		}
	}

	e := NewEnricher(store, NewMusicBrainzClient(mbSrv.URL, "t", nil),
		NewCoverArtClient(caaSrv.URL, "t", nil), nil, filepath.Join(dir, "artwork"))
	defer startEnricherForTest(e, 3*time.Second)()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.skipped.Load() < 5 {
		time.Sleep(10 * time.Millisecond)
	}

	reasons := e.SkipReasons()
	for k := range reasons {
		switch k {
		case skipReasonNoSearchTerms, skipReasonNoMBMatch, skipReasonMBError:
		default:
			t.Errorf("unbounded skip-reason key %q — the variable part of a "+
				"failure must ride the log line, not the map key", k)
		}
	}
	if len(reasons) > 3 {
		t.Errorf("skipReasons has %d keys, want at most the 3 constants: %v", len(reasons), reasons)
	}
}

// TestSkipReasonsIsACopy — the accessor hands callers a snapshot, so an
// admin handler ranging over it can't race the enricher goroutine or
// mutate the enricher's own tally.
func TestSkipReasonsIsACopy(t *testing.T) {
	e := &Enricher{}
	e.skipReasonsMu.Lock()
	e.skipReasons = map[string]int64{skipReasonNoMBMatch: 3}
	e.skipReasonsMu.Unlock()

	got := e.SkipReasons()
	got[skipReasonNoMBMatch] = 999
	got["injected"] = 1

	again := e.SkipReasons()
	if again[skipReasonNoMBMatch] != 3 {
		t.Errorf("caller mutation leaked into the enricher: %d", again[skipReasonNoMBMatch])
	}
	if _, ok := again["injected"]; ok {
		t.Error("caller-injected key leaked into the enricher")
	}
}
