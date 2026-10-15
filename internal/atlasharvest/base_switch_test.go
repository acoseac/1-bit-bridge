package atlasharvest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/baseurl"
)

// Backlog B169: what a tick computed against one Atlas base must not land on
// another. SetCredential to a new base resets the results cursor and the
// last-submit stamp. A results page or a library submit still in flight
// against the old base must leave both as that reset left them: the old base's
// cursor would hide the new base's results below it, and the old submit's
// stamp would hold the new base's first submit for a whole submit interval.

// switchMidRequest returns a handler body that re-provisions state against
// newBase while the request is in flight, as a credential push from the app
// landing during a tick would.
func switchMidRequest(t *testing.T, state *StateStore, newBase string) {
	t.Helper()
	if err := state.SetCredential("new-token", newBase, time.Now().Add(time.Hour)); err != nil {
		t.Errorf("SetCredential to the new base: %v", err)
	}
}

func TestAResultsPageFromTheOldBaseLeavesTheNewBasesCursorAlone(t *testing.T) {
	state := mustOpenState(t, filepath.Join(t.TempDir(), "state.json"))
	newAtlas := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(resultsResponse{})
	}))
	defer newAtlas.Close()
	oldAtlas := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/atlas/harvest/results" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switchMidRequest(t, state, newAtlas.URL)
		_ = json.NewEncoder(w).Encode(resultsResponse{
			Results:    []resultItem{{MBID: "a1", Status: "done", Found: true, Bio: "Bio A", Cursor: 500}},
			NextCursor: 500,
		})
	}))
	defer oldAtlas.Close()
	if err := state.SetCredential("old-token", oldAtlas.URL, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	c := &Client{State: state, MBIDs: &fakeMBIDs{}, Sink: &fakeSink{}, HTTP: oldAtlas.Client()}
	if err := c.pollResults(context.Background()); err != nil {
		t.Fatalf("pollResults: %v", err)
	}

	st := state.Snapshot()
	if want := baseurl.CredentialBase(newAtlas.URL); st.AtlasBaseURL != want {
		t.Fatalf("base = %q, want the new %q (premise)", st.AtlasBaseURL, want)
	}
	if st.ResultCursor != 0 {
		t.Errorf("cursor = %d after the old base's page landed on the new base, want 0: "+
			"the new base's results below it would never be fetched", st.ResultCursor)
	}
}

func TestASubmitToTheOldBaseLeavesTheNewBasesFirstSubmitDue(t *testing.T) {
	state := mustOpenState(t, filepath.Join(t.TempDir(), "state.json"))
	var newSubmits atomic.Int32
	newAtlas := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/atlas/harvest/submit":
			newSubmits.Add(1)
			_ = json.NewEncoder(w).Encode(submitResponse{Accepted: 1})
		default:
			_ = json.NewEncoder(w).Encode(resultsResponse{})
		}
	}))
	defer newAtlas.Close()
	oldAtlas := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/atlas/harvest/submit":
			switchMidRequest(t, state, newAtlas.URL)
			_ = json.NewEncoder(w).Encode(submitResponse{Accepted: 1})
		default:
			_ = json.NewEncoder(w).Encode(resultsResponse{})
		}
	}))
	defer oldAtlas.Close()
	if err := state.SetCredential("old-token", oldAtlas.URL, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	c := &Client{State: state, MBIDs: &fakeMBIDs{ids: []string{"a1"}}, Sink: &fakeSink{}, HTTP: oldAtlas.Client()}
	c.tick(context.Background())

	st := state.Snapshot()
	if want := baseurl.CredentialBase(newAtlas.URL); st.AtlasBaseURL != want {
		t.Fatalf("base = %q, want the new %q (premise)", st.AtlasBaseURL, want)
	}
	if !st.LastSubmitAt.IsZero() {
		t.Errorf("last submit = %v after the old base's submit, want zero: "+
			"the new base's first library submit would wait a whole interval", st.LastSubmitAt)
	}
	c.tick(context.Background())
	if got := newSubmits.Load(); got != 1 {
		t.Errorf("the next tick posted %d submits to the new base, want 1", got)
	}
}
