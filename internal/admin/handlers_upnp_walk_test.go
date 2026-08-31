package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUPnPWalkSnapshotReportsOnlyALiveWalk pins the shape the SSE gate
// keys on.
//
// `walking` false is what stops the event publishing on the 500ms tick,
// so an unwired bridge or a finished walk must report exactly that — not
// a stale key, and not a half-filled row.
func TestUPnPWalkSnapshotReportsOnlyALiveWalk(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// Unwired: the feature is off, and the event must never fire.
	if got := srv.getUPnPWalkSnapshot(); got.Walking || got.SourceID != "" {
		t.Errorf("unwired bridge reports %+v, want an empty status", got)
	}

	srv.deps.UPnPWalkProgress = func() UPnPWalkStatus {
		return UPnPWalkStatus{Key: testUpstreamKey, Walking: true, Items: 4200}
	}
	got := srv.getUPnPWalkSnapshot()
	if !got.Walking || got.Items != 4200 {
		t.Fatalf("live walk reports %+v", got)
	}
	// The facet id, not the routing key: it is what the page's rows, the
	// sidebar and the sources event all carry, so the client can match a
	// frame to a row it already rendered.
	if got.SourceID != upstreamSourceID() {
		t.Errorf("sourceId = %q, want the facet id %q", got.SourceID, upstreamSourceID())
	}

	// A finished walk reports nothing, even though the key is still known.
	srv.deps.UPnPWalkProgress = func() UPnPWalkStatus {
		return UPnPWalkStatus{Key: testUpstreamKey, Walking: false, Items: 9999}
	}
	if got := srv.getUPnPWalkSnapshot(); got.Walking || got.Items != 0 {
		t.Errorf("finished walk reports %+v, want an empty status", got)
	}

	w, _ := playerGet(t, srv, "/api/upnp/walk")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/upnp/walk = %d", w.Code)
	}
	var body upnpWalkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Walking {
		t.Error("the REST twin disagrees with the snapshot it wraps")
	}
}

// TestUPnPServerRowsCarryTheFacetID pins the join the live frame needs.
//
// The page renders its rows from /api/upnp/servers and the progress
// arrives on a separate event; without a shared id the client would have
// to match on the server's NAME, which is operator-editable and stops
// matching after a rename.
func TestUPnPServerRowsCarryTheFacetID(t *testing.T) {
	js := readFile(t, filepath.Join("static", "app.js"))
	if !strings.Contains(js, `data-source-id="${escapeHTML(s.sourceId`) {
		t.Error("the configured row no longer carries the facet id; a live walk " +
			"frame could not be matched to it")
	}
	fn := extractJSFunction(t, js, "applyUpnpWalk")
	if !strings.Contains(fn, "row.dataset.sourceId === data.sourceId") {
		t.Error("applyUpnpWalk no longer matches on the facet id")
	}
	// The transition is recorded BEFORE anything can return early: a frame
	// arriving while the list is still loading would otherwise leave it
	// unrecorded, and the closing frame would find no rising edge to fall
	// from — so the refresh never runs.
	// The boundary is the first RETURN, not the first DOM access. Checking
	// "before the loop" passes against a version that returns early on an
	// empty row list and records the state afterwards — which is exactly
	// the bug, and exactly what that looser form let through.
	// Comments stripped first. This repo comments densely and names the
	// identifiers it discusses — the docblock on this very function says
	// "before anything can return early", and a raw scan reads that prose
	// as the return it is warning about. The sibling parity tests strip
	// for the same reason; reusing their helper rather than a third copy.
	fn = stripJSComments(fn)
	prev := strings.Index(fn, "upnpWasWalking = walking")
	if prev < 0 {
		t.Fatal("applyUpnpWalk no longer records the walking state at all")
	}
	if ret := strings.Index(fn, "return"); ret >= 0 && ret < prev {
		t.Error("applyUpnpWalk can return before recording the walking state; a " +
			"frame arriving while the list is still loading would lose the " +
			"transition, and the closing frame would find no rising edge to fall from")
	}
	// And the refetch happens once, on the falling edge, gated on the
	// container — this handler runs on EVERY page, and only the UPnP page
	// has a list to refresh.
	if !strings.Contains(fn, "wasWalking && !walking") ||
		!strings.Contains(fn, `getElementById("upnp-configured-list")`) ||
		!strings.Contains(fn, "loadUpnpConfigured()") {
		t.Error("applyUpnpWalk does not refresh the list once on the falling edge; " +
			"\"Last walk\" and \"Routed tracks\" would stay stale until a reload")
	}
}

// TestUPnPWalkEventFollowsTheWalk drives the REAL SSE stream and asserts
// on the frames it publishes, rather than on the shape of the source.
//
// It covers three things a string match cannot: that an idle bridge
// publishes nothing after the initial snapshot (the event rides the 500ms
// tick, so an ungated publish would be four frames a second forever);
// that a walk in flight does publish; and — the race — that a walk which
// ENDS between the initial snapshot and the first tick still gets its
// closing frame. Without that last one the client's progress line stays
// up for the life of the connection.
func TestUPnPWalkEventFollowsTheWalk(t *testing.T) {
	srv, _, _ := newTestServer(t)

	var mu sync.Mutex
	walking := true
	items := int64(7)
	srv.deps.UPnPWalkProgress = func() UPnPWalkStatus {
		mu.Lock()
		defer mu.Unlock()
		return UPnPWalkStatus{Key: testUpstreamKey, Walking: walking, Items: items}
	}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	initial := readFrames(t, resp.Body, 11, 3*time.Second)
	var opening *upnpWalkResponse
	for _, f := range initial {
		if f.event != "upnpwalk" {
			continue
		}
		var got upnpWalkResponse
		if err := json.Unmarshal([]byte(f.data), &got); err != nil {
			t.Fatalf("decode upnpwalk: %v", err)
		}
		opening = &got
	}
	if opening == nil || !opening.Walking || opening.Items != 7 {
		t.Fatalf("initial snapshot carried %+v, want the walk in flight", opening)
	}

	// End the walk. This is the race: it finishes after the initial
	// snapshot said "walking" and before the first fast tick.
	mu.Lock()
	walking = false
	mu.Unlock()

	post := readFrames(t, resp.Body, 1, 5*time.Second)
	var closing *upnpWalkResponse
	for _, f := range post {
		if f.event != "upnpwalk" {
			continue
		}
		var got upnpWalkResponse
		if err := json.Unmarshal([]byte(f.data), &got); err != nil {
			t.Fatalf("decode upnpwalk: %v", err)
		}
		closing = &got
	}
	if closing == nil {
		t.Fatalf("no closing frame after the walk ended; the client's progress "+
			"line would stay up for the life of the connection (got %d frames: %v)",
			len(post), post)
	}
	if closing.Walking {
		t.Errorf("closing frame still says walking: %+v", closing)
	}
}

// TestUPnPWalkEventIsSilentOnAnIdleBridge is the other half: the event
// rides the 500ms tick, so an ungated publish would be four frames a
// second on a bridge doing nothing.
func TestUPnPWalkEventIsSilentOnAnIdleBridge(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.deps.UPnPWalkProgress = func() UPnPWalkStatus { return UPnPWalkStatus{} }

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := readFrames(t, resp.Body, 11, 3*time.Second); len(got) != 11 {
		t.Fatalf("initial snapshot incomplete: %d frames", len(got))
	}
	// ~4 fast ticks. Diff suppression alone would not save an ungated
	// publish here, because the first one would already have been sent.
	for _, f := range readFrames(t, resp.Body, 1, 2*time.Second) {
		if f.event == "upnpwalk" {
			t.Errorf("idle bridge published an upnpwalk frame: %s", f.data)
		}
	}
}
