package admin

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
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
	// The falling edge refetches once, so the counts the walk just changed
	// land without a reload — and ONCE, not per frame.
	if !strings.Contains(fn, "if (upnpWasWalking && !walking) void loadUpnpConfigured();") {
		t.Error("applyUpnpWalk does not refresh the list when a walk ends; " +
			"\"Last walk\" and \"Routed tracks\" would stay stale until a reload")
	}
}

// TestUPnPWalkEventIsGatedOnAWalkBeingInFlight pins that this event does
// not turn the 500ms tick into a publish-every-tick.
//
// It rides the fast tick only because its snapshot is atomic reads. The
// sources event beside it looks like the natural home and is not: that
// one issues a COUNT(*) per configured upstream, which is exactly why it
// sits on the 30s tick.
func TestUPnPWalkEventIsGatedOnAWalkBeingInFlight(t *testing.T) {
	src := readFile(t, "handlers_events.go")
	i := strings.Index(src, "case <-fastTk.C:")
	if i < 0 {
		t.Fatal("no fast tick in the SSE loop")
	}
	end := strings.Index(src[i:], "case <-medTk.C:")
	if end < 0 {
		t.Fatal("could not bound the fast-tick case")
	}
	fast := src[i : i+end]
	if !strings.Contains(fast, "publishUPnPWalk()") {
		t.Fatal("the walk event does not ride the fast tick; a progress counter " +
			"that updates every 30s is not progress")
	}
	// The whole statement, brace included. A substring check on the
	// condition alone passes against `walking || wasWalking || true`,
	// which is precisely the mutation this is here to catch — verified by
	// making it and watching the loose form stay green.
	if !strings.Contains(fast, "if walking || wasWalking {") {
		t.Error("the walk publish is not gated on a walk being in flight plus one " +
			"final frame; an idle bridge would publish on every 500ms tick, and " +
			"without the latch the last frame would say \"walking\" forever")
	}
}
