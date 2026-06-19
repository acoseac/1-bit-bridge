package atlasharvest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

type fakeMBIDs struct {
	ids            []string
	releaseIDs     []string
	textReleaseIDs []string
	calls          int
}

func (f *fakeMBIDs) DistinctArtistMBIDs(context.Context) ([]string, error) {
	f.calls++
	return f.ids, nil
}

func (f *fakeMBIDs) DistinctReleaseMBIDs(context.Context) ([]string, error) {
	return f.releaseIDs, nil
}

func (f *fakeMBIDs) DistinctReleaseTextMBIDs(context.Context) ([]string, error) {
	return f.textReleaseIDs, nil
}

func mustOpenState(t *testing.T, path string) *StateStore {
	t.Helper()
	s, err := OpenStateStore(path)
	if err != nil {
		t.Fatalf("OpenStateStore: %v", err)
	}
	return s
}

type fakeSink struct {
	stored         []ArtistMeta
	storedReleases []ReleaseMeta
}

func (f *fakeSink) UpsertArtistMeta(_ context.Context, m ArtistMeta) error {
	f.stored = append(f.stored, m)
	return nil
}

func (f *fakeSink) UpsertReleaseMeta(_ context.Context, m ReleaseMeta) error {
	f.storedReleases = append(f.storedReleases, m)
	return nil
}

// One tick submits the library + drains the results, storing bios (done) and
// tombstones (exhausted) and advancing the cursor.
func TestClientSubmitAndPoll(t *testing.T) {
	var gotSubmit []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/atlas/harvest/submit":
			var body submitRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotSubmit = append(gotSubmit, body.MBIDs...)
			_ = json.NewEncoder(w).Encode(submitResponse{Accepted: len(body.MBIDs)})
		case "/v1/atlas/harvest/results":
			if r.URL.Query().Get("since") == "0" {
				_ = json.NewEncoder(w).Encode(resultsResponse{
					Results: []resultItem{
						{MBID: "a1", Status: "done", Found: true, Bio: "Bio A", Genres: []string{"jazz"}, Source: "lastfm", SourceURL: "https://last.fm/a", Cursor: 1},
						{MBID: "a2", Status: "exhausted", Found: false, Cursor: 2},
						{MBID: "r1", Kind: "release", Status: "done", Found: true, Description: "About this album", RecordLabel: "Blue Note", Genres: []string{"jazz"}, Source: "wikipedia", SourceURL: "https://en.wikipedia.org/wiki/x", Cursor: 3},
						{MBID: "r2", Kind: "release", Status: "done", Found: true, Cursor: 4},                                                                                         // cover-only (no text) → must NOT be stored
						{MBID: "r3", Kind: "release_text", Status: "done", Found: true, Description: "Local-art album", Source: "lastfm", SourceURL: "https://last.fm/r3", Cursor: 5}, // text-only → stored, but NOT cover-pending
					},
					NextCursor: 5,
				})
				return
			}
			_ = json.NewEncoder(w).Encode(resultsResponse{NextCursor: 2})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	state, err := OpenStateStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("OpenStateStore: %v", err)
	}
	if err := state.SetCredential("test-token", srv.URL, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}

	sink := &fakeSink{}
	c := &Client{State: state, MBIDs: &fakeMBIDs{ids: []string{"a1", "a2"}}, Sink: sink}
	c.tick(context.Background())

	if len(gotSubmit) != 2 {
		t.Fatalf("submitted %d artist MBIDs, want 2", len(gotSubmit))
	}
	if len(sink.stored) != 2 {
		t.Fatalf("stored %d results, want 2", len(sink.stored))
	}
	if got := sink.stored[0]; got.MBID != "a1" || !got.Found || got.Bio != "Bio A" || got.Source != "lastfm" {
		t.Errorf("result[0] = %+v", got)
	}
	if got := sink.stored[1]; got.MBID != "a2" || got.Found {
		t.Errorf("result[1] = %+v, want a2 tombstone (found=false)", got)
	}
	// Album text (Phase D): r1 (release, has text) + r3 (release_text) are stored;
	// the cover-only release (r2, no description/label/genre) leaves no overlay row.
	storedByMBID := map[string]ReleaseMeta{}
	for _, m := range sink.storedReleases {
		storedByMBID[m.MBID] = m
	}
	if len(sink.storedReleases) != 2 {
		t.Fatalf("stored %d release metas, want 2 (r1 + r3; cover-only r2 skipped)", len(sink.storedReleases))
	}
	if got := storedByMBID["r1"]; got.Description != "About this album" || got.RecordLabel != "Blue Note" || got.Source != "wikipedia" {
		t.Errorf("r1 release meta = %+v", got)
	}
	if got := storedByMBID["r3"]; got.Description != "Local-art album" || got.Source != "lastfm" {
		t.Errorf("r3 (release_text) meta = %+v", got)
	}
	// Only cover-bearing "release" results feed the cover sweep; the text-only
	// "release_text" (r3) must NOT be queued for a cover refetch.
	pending := state.PendingCoversSnapshot()
	if _, ok := pending["r3"]; ok {
		t.Error("release_text r3 must NOT be added to pending covers")
	}
	if _, ok := pending["r1"]; !ok {
		t.Error("release r1 should be queued for cover refetch")
	}
	if c := state.Snapshot().ResultCursor; c != 5 {
		t.Errorf("cursor = %d, want 5", c)
	}
}

// Atlas rejecting the token wipes the credential so the app re-provisions.
func TestClientTokenRejectedClearsCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	state := mustOpenState(t, filepath.Join(t.TempDir(), "s.json"))
	_ = state.SetCredential("dead-token", srv.URL, time.Now().Add(time.Hour))
	c := &Client{State: state, MBIDs: &fakeMBIDs{ids: []string{"a1"}}, Sink: &fakeSink{}}

	c.tick(context.Background())

	if tok := state.Snapshot().Token; tok != "" {
		t.Errorf("token = %q, want cleared after 401", tok)
	}
}

// A no-credential tick is a cheap no-op (doesn't call the sources or sink).
func TestClientNoCredentialIsNoOp(t *testing.T) {
	state := mustOpenState(t, filepath.Join(t.TempDir(), "s.json"))
	sink := &fakeSink{}
	source := &fakeMBIDs{ids: []string{"a1"}}
	c := &Client{State: state, MBIDs: source, Sink: sink}

	c.tick(context.Background())

	if len(sink.stored) != 0 {
		t.Errorf("stored %d with no credential, want 0", len(sink.stored))
	}
	if source.calls != 0 {
		t.Errorf("MBID source called %d times with no credential, want 0", source.calls)
	}
}

func TestStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.json")
	s := mustOpenState(t, p)
	if err := s.SetCredential("tok", "https://atlas.example", time.Unix(100, 0).UTC()); err != nil {
		t.Fatalf("SetCredential: %v", err)
	}
	if err := s.SetCursor(5); err != nil {
		t.Fatalf("SetCursor: %v", err)
	}

	s2, err := OpenStateStore(p)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := s2.Snapshot()
	if got.Token != "tok" || got.ResultCursor != 5 || got.AtlasBaseURL != "https://atlas.example" {
		t.Errorf("roundtrip = %+v", got)
	}
}

// Re-provisioning a DIFFERENT Atlas resets the sync position (fresh library scope).
func TestStateCredentialChangeResetsCursorOnNewAtlas(t *testing.T) {
	s := mustOpenState(t, filepath.Join(t.TempDir(), "s.json"))
	_ = s.SetCredential("t1", "https://atlas-a", time.Time{})
	_ = s.SetCursor(9)
	_ = s.SetCredential("t2", "https://atlas-b", time.Time{}) // different host
	if c := s.Snapshot().ResultCursor; c != 0 {
		t.Errorf("cursor = %d after switching Atlas, want 0", c)
	}
}
