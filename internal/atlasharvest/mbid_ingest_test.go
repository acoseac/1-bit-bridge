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

// TestPollResultsDoesNotQueueAMalformedReleaseMBID pins the ingest half of
// the path-traversal fix.
//
// The results page is the upstream's, and the bridge never checks that a
// returned MBID is one it submitted — so the upstream chooses what lands
// in PendingCovers, which is PERSISTED and has no age-based eviction. The
// cover sweep then makes that value the leading component of
// ArtworkCachePath's filepath.Join, whose writer does
// os.MkdirAll(filepath.Dir(path)).
//
// Drives the real pollResults against a real server rather than calling
// the predicate, because the predicate passing proves nothing about
// whether the loop consults it.
func TestPollResultsDoesNotQueueAMalformedReleaseMBID(t *testing.T) {
	const traversal = "../../../../tmp/pwned"
	const good = "9f9e9d9c-9b9a-4998-9796-959493929190"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/atlas/harvest/results" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(resultsResponse{
			Results: []resultItem{
				{Kind: "release", MBID: traversal, Found: true},
				{Kind: "release", MBID: good, Found: true},
			},
			NextCursor: 2,
		})
	}))
	defer srv.Close()

	state := mustOpenState(t, filepath.Join(t.TempDir(), "s.json"))
	if err := state.SetCredential("test-token", srv.URL, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	c := &Client{State: state, MBIDs: &fakeMBIDs{}, Sink: &fakeSink{}}

	if err := c.pollResults(context.Background()); err != nil {
		t.Fatalf("pollResults: %v", err)
	}

	pending := state.Snapshot().PendingCovers
	if _, queued := pending[traversal]; queued {
		t.Error("a traversing MBID was queued for cover refetch and persisted")
	}
	// Positive control in the same page: the filter must reject the bad
	// entry WITHOUT dropping the good one beside it. A filter that
	// queues nothing would pass the assertion above for the wrong
	// reason.
	if _, queued := pending[good]; !queued {
		t.Error("a well-formed MBID on the same page was dropped")
	}
}

// TestIsValidMBIDShape is the predicate's own truth table, kept small and
// anchored: the whole security property is that the alphabet a UUID draws
// from makes traversal impossible, so the cases that matter are the ones
// carrying a separator or a dot.
func TestIsValidMBIDShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"canonical", "9f9e9d9c-9b9a-4998-9796-959493929190", true},
		{"uppercase hex", "9F9E9D9C-9B9A-4998-9796-959493929190", true},
		{"empty", "", false},
		{"traversal", "../../../../tmp/pwned", false},
		{"traversal dressed as a uuid", "../9e9d9c-9b9a-4998-9796-95949392919", false},
		{"absolute path", "/etc/cron.d/x", false},
		{"windows separator", `..\..\x`, false},
		{"trailing slash", "9f9e9d9c-9b9a-4998-9796-959493929190/", false},
		{"embedded newline", "9f9e9d9c-9b9a-4998-9796-959493929190\n", false},
		{"too short", "9f9e9d9c-9b9a-4998-9796-9594939291", false},
		{"non-hex", "zzzzzzzz-9b9a-4998-9796-959493929190", false},
	} {
		if got := isValidMBID(tc.in); got != tc.want {
			t.Errorf("%s: isValidMBID(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}
