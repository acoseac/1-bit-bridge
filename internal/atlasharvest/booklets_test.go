package atlasharvest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBookletSink is an in-memory BookletSink recording verdicts + tags.
type fakeBookletSink struct {
	universe  []string
	toCheck   []string // returned by BookletsToCheck
	toFetch   []BookletFetchItem
	available map[string]string // mbid → etag recorded available
	missed    map[string]int    // mbid → miss count
	tags      map[string]string // mbid → last stamped tag
	fetched   []string
	unavail   []string
	gcSeen    []string // universes DeleteBookletsNotIn saw
	gcOrphans []string // what it returns
}

func newFakeBookletSink() *fakeBookletSink {
	return &fakeBookletSink{
		available: map[string]string{},
		missed:    map[string]int{},
		tags:      map[string]string{},
	}
}

func (f *fakeBookletSink) DistinctAlbumReleaseMBIDs(context.Context) ([]string, error) {
	return f.universe, nil
}
func (f *fakeBookletSink) BookletsToCheck(_ context.Context, candidates []string, _ int) ([]string, error) {
	if f.toCheck != nil {
		return f.toCheck, nil
	}
	return candidates, nil
}
func (f *fakeBookletSink) UpsertBookletAvailability(_ context.Context, mbid string, available bool, etag string, _ int64) error {
	if available {
		f.available[mbid] = etag
	} else {
		f.missed[mbid]++
	}
	return nil
}
func (f *fakeBookletSink) SetBookletTagAndBumpIndex(_ context.Context, mbid, tag string) (int64, error) {
	f.tags[mbid] = tag
	return 1, nil
}
func (f *fakeBookletSink) BookletsToFetch(_ context.Context, limit int) ([]BookletFetchItem, error) {
	if limit < len(f.toFetch) {
		return f.toFetch[:limit], nil
	}
	return f.toFetch, nil
}
func (f *fakeBookletSink) MarkBookletFetched(_ context.Context, mbid string) error {
	f.fetched = append(f.fetched, mbid)
	return nil
}
func (f *fakeBookletSink) MarkBookletUnavailable(_ context.Context, mbid string) error {
	f.unavail = append(f.unavail, mbid)
	return nil
}
func (f *fakeBookletSink) DeleteBookletsNotIn(_ context.Context, universe []string) ([]string, error) {
	f.gcSeen = append(f.gcSeen, strings.Join(universe, ","))
	return f.gcOrphans, nil
}

// fakeBookletFiles is an in-memory BookletFileStore.
type fakeBookletFiles struct {
	mu      sync.Mutex
	files   map[string][]byte
	removed []string
}

func newFakeBookletFiles() *fakeBookletFiles {
	return &fakeBookletFiles{files: map[string][]byte{}}
}

func (f *fakeBookletFiles) WriteBooklet(mbid string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[mbid] = b
	return nil
}

func (f *fakeBookletFiles) RemoveBooklet(mbid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, mbid)
	f.removed = append(f.removed, mbid)
	return nil
}

// bookletTestClient builds a Client whose harvest legs are quiet (recent
// submit, drained results) so ticks exercise only the booklet loops.
func bookletTestClient(t *testing.T, atlasURL string, sink *fakeBookletSink, files *fakeBookletFiles) *Client {
	t.Helper()
	state := mustOpenState(t, filepath.Join(t.TempDir(), "state.json"))
	if err := state.SetCredential("test-token", atlasURL, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := state.SetLastSubmit(time.Now()); err != nil {
		t.Fatal(err)
	}
	c := &Client{State: state, MBIDs: &fakeMBIDs{}, Sink: &fakeSink{}, Booklets: sink}
	if files != nil {
		c.BookletFiles = files
	}
	return c
}

// quietResults answers the poll leg with an empty page.
func quietResults(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path == "/v1/atlas/harvest/results" {
		_ = json.NewEncoder(w).Encode(resultsResponse{})
		return true
	}
	return false
}

func TestBookletCheckCycleRecordsVerdictsAndStampsTags(t *testing.T) {
	const (
		relAvail = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		relMiss  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	)
	pdf := append([]byte("%PDF-1.4\n"), bytes.Repeat([]byte("x"), 64)...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if quietResults(w, r) {
			return
		}
		switch {
		case r.URL.Path == "/v1/atlas/harvest/booklets/check":
			var body bookletsCheckRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(bookletsCheckResponse{Booklets: []bookletsCheckItem{
				{MBID: relAvail, Etag: "etag-1", Bytes: int64(len(pdf))},
			}})
		case strings.HasPrefix(r.URL.Path, "/v1/atlas/release/") && strings.HasSuffix(r.URL.Path, "/booklet"):
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(pdf)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	sink := newFakeBookletSink()
	sink.universe = []string{relAvail, relMiss}
	sink.toFetch = []BookletFetchItem{{ReleaseMBID: relAvail, Etag: "etag-1"}}
	files := newFakeBookletFiles()
	c := bookletTestClient(t, srv.URL, sink, files)

	c.tick(context.Background())

	if got := sink.available[relAvail]; got != "etag-1" {
		t.Errorf("available[%s] = %q, want etag-1", relAvail, got)
	}
	if sink.missed[relMiss] != 1 {
		t.Errorf("missed[%s] = %d, want 1", relMiss, sink.missed[relMiss])
	}
	if sink.tags[relAvail] != "etag-1" {
		t.Errorf("tag stamped = %q, want etag-1", sink.tags[relAvail])
	}
	if _, tagged := sink.tags[relMiss]; tagged {
		t.Error("miss row got a tag stamp, want none")
	}
	// The fetch sweep downloaded the available PDF + marked it.
	if !bytes.Equal(files.files[relAvail], pdf) {
		t.Errorf("fetched bytes = %d, want %d", len(files.files[relAvail]), len(pdf))
	}
	if len(sink.fetched) != 1 || sink.fetched[0] != relAvail {
		t.Errorf("fetched marks = %v", sink.fetched)
	}
	// GC ran over the same universe.
	if len(sink.gcSeen) != 1 || sink.gcSeen[0] != relAvail+","+relMiss {
		t.Errorf("gc universes = %v", sink.gcSeen)
	}
	// The check stamp persisted → a second tick doesn't re-check.
	if st := c.State.Snapshot(); st.LastBookletCheckAt.IsZero() {
		t.Error("LastBookletCheckAt not stamped after a successful cycle")
	}
}

func TestBookletCheckToleratesPreBookletAtlas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if quietResults(w, r) {
			return
		}
		w.WriteHeader(http.StatusNotFound) // old Atlas: no booklets endpoint
	}))
	defer srv.Close()

	sink := newFakeBookletSink()
	sink.universe = []string{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}
	c := bookletTestClient(t, srv.URL, sink, nil)
	c.tick(context.Background())

	// No verdicts, no credential wipe, and the stamp advanced so the next
	// attempt waits a full interval instead of retrying every tick.
	if len(sink.available) != 0 || len(sink.missed) != 0 {
		t.Errorf("verdicts recorded against a 404 endpoint: %v / %v", sink.available, sink.missed)
	}
	st := c.State.Snapshot()
	if st.Token == "" {
		t.Error("credential was wiped by a pre-booklet Atlas 404 — must only happen on 401/403")
	}
	if st.LastBookletCheckAt.IsZero() {
		t.Error("LastBookletCheckAt not stamped on endpoint-missing")
	}
}

func TestBookletFetch404FlipsUnavailableAndClearsTag(t *testing.T) {
	const rel = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if quietResults(w, r) {
			return
		}
		if r.URL.Path == "/v1/atlas/harvest/booklets/check" {
			_ = json.NewEncoder(w).Encode(bookletsCheckResponse{})
			return
		}
		w.WriteHeader(http.StatusNotFound) // booklet gone upstream
	}))
	defer srv.Close()

	sink := newFakeBookletSink()
	sink.toFetch = []BookletFetchItem{{ReleaseMBID: rel, Etag: "etag-1"}}
	files := newFakeBookletFiles()
	c := bookletTestClient(t, srv.URL, sink, files)
	c.tick(context.Background())

	if len(sink.unavail) != 1 || sink.unavail[0] != rel {
		t.Errorf("unavailable marks = %v, want [%s]", sink.unavail, rel)
	}
	if tag, ok := sink.tags[rel]; !ok || tag != "" {
		t.Errorf("tag = (%q, %v), want cleared (empty stamp)", tag, ok)
	}
	if len(sink.fetched) != 0 {
		t.Errorf("fetched marks = %v, want none", sink.fetched)
	}
}

func TestBookletFetchRefusesOversizedPDF(t *testing.T) {
	const rel = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if quietResults(w, r) {
			return
		}
		if r.URL.Path == "/v1/atlas/harvest/booklets/check" {
			_ = json.NewEncoder(w).Encode(bookletsCheckResponse{})
			return
		}
		// Stream cap+2 bytes so the LimitedReader detects the overrun.
		w.Header().Set("Content-Type", "application/pdf")
		chunk := bytes.Repeat([]byte("y"), 1<<20)
		total := 0
		for total <= maxBookletBytes+1 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			total += len(chunk)
		}
	}))
	defer srv.Close()

	sink := newFakeBookletSink()
	sink.toFetch = []BookletFetchItem{{ReleaseMBID: rel, Etag: "e"}}
	files := newFakeBookletFiles()
	c := bookletTestClient(t, srv.URL, sink, files)
	c.tick(context.Background())

	if len(sink.fetched) != 0 {
		t.Errorf("oversized PDF was marked fetched")
	}
	files.mu.Lock()
	_, present := files.files[rel]
	files.mu.Unlock()
	if present {
		t.Error("oversized PDF left a partial file in the store, want removed")
	}
}

func TestNudgeBookletFetchPrioritizes(t *testing.T) {
	const (
		relTapped = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		relQueued = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	)
	var served []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if quietResults(w, r) {
			return
		}
		if r.URL.Path == "/v1/atlas/harvest/booklets/check" {
			_ = json.NewEncoder(w).Encode(bookletsCheckResponse{})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/atlas/release/") {
			parts := strings.Split(r.URL.Path, "/")
			served = append(served, parts[4])
			_, _ = w.Write([]byte("%PDF-1.4"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	sink := newFakeBookletSink()
	sink.toFetch = []BookletFetchItem{{ReleaseMBID: relQueued, Etag: "e"}}
	files := newFakeBookletFiles()
	c := bookletTestClient(t, srv.URL, sink, files)
	c.NudgeBookletFetch(relTapped)
	c.tick(context.Background())

	if len(served) < 2 || served[0] != relTapped {
		t.Errorf("serve order = %v, want the nudged MBID first", served)
	}
}
