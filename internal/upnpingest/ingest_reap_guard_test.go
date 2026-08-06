package upnpingest

import (
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

// emptyDIDL is a well-formed but EMPTY DIDL-Lite payload — the shape a
// MiniDLNA-class server serves for a container while its database rebuilds,
// and the shape a still-answering server serves for its root when the share
// underneath it has unmounted.
const emptyDIDL = `<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" ` +
	`xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/"></DIDL-Lite>`

// seedRoutedTracks pre-seeds n routed rows for serverUDN with an old
// last_seen_at, so a subsequent walk's reconcile sweep would consider every
// one of them stale.
func seedRoutedTracks(t *testing.T, store *manifest.Store, serverUDN string, paths ...string) {
	t.Helper()
	tOld := time.Now().UTC().Add(-1 * time.Hour)
	for _, p := range paths {
		if err := store.UpsertTrack(context.Background(),
			&manifest.Track{Path: p, Size: 1, ModTime: tOld}); err != nil {
			t.Fatalf("UpsertTrack(%s): %v", p, err)
		}
		if err := store.UpsertUPnPRouting(context.Background(), &manifest.UPnPRouting{
			SourcePath: p, ServerUDN: serverUDN, ObjectID: "x",
			ResURL: "http://h/x.flac", LastSeenAt: tOld,
		}); err != nil {
			t.Fatalf("UpsertUPnPRouting(%s): %v", p, err)
		}
	}
}

func guardTestConfig() config.UPnPUpstreamConfig {
	return config.UPnPUpstreamConfig{
		Enabled: true,
		Servers: []config.UPnPUpstreamServerConfig{
			{Name: "2Go", UDN: "uuid:test", PathPrefix: "Chord 2Go"},
		},
	}
}

// TestIngester_Run_EmptyRootBrowse_DoesNotReap is the regression gate for
// the unguarded reap: a container that Browses EMPTY without an error
// terminates BrowseAll (client.go's `pageLen == 0` break), so the walker
// yields nothing, stats.Truncated stays false and the walk error is nil —
// and the reconcile sweep would then delete every routed row for that
// server with NONE of the debounce the filesystem scanner's missing_count
// pass provides.
//
// The fixture reproduces the documented upstream misbehaviour exactly: the
// server reports NumberReturned=1 / TotalMatches=15283 while serving an
// EMPTY page (pager.go: "MiniDLNA can report an inaccurate TotalMatches
// while its DB builds"). The SystemUpdateID gate FORCES a walk during such
// a rebuild, because the ID moved — so this is the reachable steady state,
// not a contrived one.
func TestIngester_Run_EmptyRootBrowse_DoesNotReap(t *testing.T) {
	store := openIngestTestStore(t)
	const pathA = "Chord 2Go/Music/Artist/Album/01 - One.flac"
	const pathB = "Chord 2Go/Music/Artist/Album/02 - Two.flac"
	seedRoutedTracks(t, store, "uuid:test", pathA, pathB)

	stub := newStubSOAP()
	stub.addRoute("GetSystemUpdateID", wrapSystemUpdateID("0"))
	// Root browses EMPTY while claiming to hold 15283 items.
	stub.addRoute("Browse", wrapBrowse(emptyDIDL, 1, 15283))

	ing, err := NewIngester(guardTestConfig(), upnp.NewContentDirectoryClient(stub),
		&stubResolver{controlURL: "http://h:8200/ctl/CD"}, store, nil)
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}

	res, err := ing.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pr := res.PerServer[0]
	// The walk genuinely succeeded — that is precisely what makes this
	// dangerous. If this assertion ever fails the fixture stopped
	// reproducing the bug and the test below proves nothing.
	if pr.Err != nil {
		t.Fatalf("walk err = %v; want nil (fixture must reproduce an ERROR-FREE empty walk)", pr.Err)
	}
	if pr.Walked != 0 {
		t.Fatalf("walked = %d; want 0 (fixture must reproduce a ZERO-track walk)", pr.Walked)
	}
	if pr.Reaped != 0 {
		t.Fatalf("reaped = %d; want 0 — an implausibly empty walk must NOT reap", pr.Reaped)
	}
	for _, p := range []string{pathA, pathB} {
		got, err := store.GetTrack(context.Background(), p)
		if err != nil {
			t.Fatalf("GetTrack(%s): %v", p, err)
		}
		if got == nil {
			t.Errorf("routed track %q was reaped after an error-free EMPTY walk (data loss)", p)
		}
	}
}

// TestIngester_Run_ImplausiblyPartialWalk_DoesNotReap covers the mid-tree
// flavour of the same bug: an album container that browses empty leaves the
// REST of the walk looking clean, so the walk returns a partial library with
// no error at all. Three of four tracks would be reaped with no debounce.
func TestIngester_Run_ImplausiblyPartialWalk_DoesNotReap(t *testing.T) {
	store := openIngestTestStore(t)
	seeded := []string{
		"Chord 2Go/Music/Artist/Album/01 - One.flac",
		"Chord 2Go/Music/Artist/Album/02 - Two.flac",
		"Chord 2Go/Music/Artist/Album/03 - Three.flac",
		"Chord 2Go/Music/Artist/Album/04 - Four.flac",
	}
	seedRoutedTracks(t, store, "uuid:test", seeded...)

	stub := newStubSOAP()
	stub.addRoute("GetSystemUpdateID", wrapSystemUpdateID("0"))
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<container id="64$0" parentID="64"><dc:title>Music</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`+
			`</DIDL-Lite>`, 1, 1))
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<container id="64$0$0" parentID="64$0"><dc:title>Artist</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`+
			`</DIDL-Lite>`, 1, 1))
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<container id="64$0$0$0" parentID="64$0$0"><dc:title>Album</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>`+
			`</DIDL-Lite>`, 1, 1))
	// The album serves ONE of its four tracks, then the page ends.
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<item id="x1" parentID="64$0$0$0"><dc:title>One</dc:title>`+
			`<upnp:class>object.item.audioItem.musicTrack</upnp:class>`+
			`<upnp:artist>Artist</upnp:artist><upnp:album>Album</upnp:album>`+
			`<upnp:originalTrackNumber>1</upnp:originalTrackNumber>`+
			`<res protocolInfo="http-get:*:audio/x-flac:*" size="1">http://h/MediaItems/1.flac</res></item>`+
			`</DIDL-Lite>`, 1, 1))

	ing, err := NewIngester(guardTestConfig(), upnp.NewContentDirectoryClient(stub),
		&stubResolver{controlURL: "http://h:8200/ctl/CD"}, store, nil)
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}

	res, err := ing.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	pr := res.PerServer[0]
	if pr.Err != nil {
		t.Fatalf("walk err = %v; want nil (fixture must reproduce an ERROR-FREE partial walk)", pr.Err)
	}
	if pr.Walked != 1 {
		t.Fatalf("walked = %d; want 1 (fixture must reproduce a PARTIAL walk)", pr.Walked)
	}
	if pr.Reaped != 0 {
		t.Fatalf("reaped = %d; want 0 — a walk holding 1 of 4 stored tracks must NOT reap", pr.Reaped)
	}
	for _, p := range seeded[1:] {
		got, err := store.GetTrack(context.Background(), p)
		if err != nil {
			t.Fatalf("GetTrack(%s): %v", p, err)
		}
		if got == nil {
			t.Errorf("routed track %q was reaped after an implausibly PARTIAL walk (data loss)", p)
		}
	}
}

// TestIngester_Run_ImplausibleWalkReapsAfterGrace proves the guard is a
// DELAY, not a permanent stall: a genuinely-emptied upstream that keeps
// reporting the same shape past implausibleWalkGrace does eventually get
// reconciled. Without this the operator's only recourse for a real bulk
// deletion would be removing the server from config.
func TestIngester_Run_ImplausibleWalkReapsAfterGrace(t *testing.T) {
	store := openIngestTestStore(t)
	const pathA = "Chord 2Go/Music/Artist/Album/01 - One.flac"
	const pathB = "Chord 2Go/Music/Artist/Album/02 - Two.flac"
	seedRoutedTracks(t, store, "uuid:test", pathA, pathB)

	stub := newStubSOAP()
	// Two full walk cycles, both serving an empty root.
	for range 2 {
		stub.addRoute("GetSystemUpdateID", wrapSystemUpdateID("0"))
		stub.addRoute("Browse", wrapBrowse(emptyDIDL, 0, 0))
	}

	ing, err := NewIngester(guardTestConfig(), upnp.NewContentDirectoryClient(stub),
		&stubResolver{controlURL: "http://h:8200/ctl/CD"}, store, nil)
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}

	base := time.Now().UTC()
	res, err := ing.Run(context.Background(), Options{Now: func() time.Time { return base }})
	if err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	if res.PerServer[0].Reaped != 0 {
		t.Fatalf("run #1 reaped = %d; want 0 — the FIRST implausible walk must never reap",
			res.PerServer[0].Reaped)
	}

	// Same shape, one grace window later: the upstream has been consistent
	// for long enough that this IS its library.
	later := base.Add(implausibleWalkGrace + time.Minute)
	res, err = ing.Run(context.Background(), Options{Now: func() time.Time { return later }})
	if err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if res.PerServer[0].Reaped != 2 {
		t.Fatalf("run #2 reaped = %d; want 2 — the guard must expire, not stall forever",
			res.PerServer[0].Reaped)
	}
	for _, p := range []string{pathA, pathB} {
		if got, _ := store.GetTrack(context.Background(), p); got != nil {
			t.Errorf("track %q survived the post-grace reconcile", p)
		}
	}
}

func TestWalkLooksImplausible(t *testing.T) {
	cases := []struct {
		name          string
		walked        int
		baseline      int
		baselineKnown bool
		want          bool
	}{
		// The baseline query failed: we cannot see how much we are about
		// to delete, so the guard must fail CLOSED rather than be disarmed
		// by exactly the failure that hides the damage.
		{"baseline unknown, walk empty", 0, 0, false, true},
		{"baseline unknown, walk full", 500, 0, false, true},
		// Nothing stored: nothing to protect, nothing for the sweep to
		// delete. This is the normal first-ingest shape.
		{"first ingest", 15283, 0, true, false},
		{"empty walk, empty baseline", 0, 0, true, false},
		// The catastrophic shape.
		{"empty walk, non-empty baseline", 0, 15283, true, true},
		{"empty walk, single stored row", 0, 1, true, true},
		// Partial shapes around the threshold.
		{"walk holds a tenth", 1528, 15283, true, true},
		{"walk just under half", 7000, 15283, true, true},
		{"walk exactly half", 7642, 15283, true, false},
		{"walk holds nearly all", 15282, 15283, true, false},
		// Ordinary churn must stay reapable.
		{"one track deleted upstream", 15282, 15283, true, false},
		{"upstream grew", 20000, 15283, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := walkLooksImplausible(c.walked, c.baseline, c.baselineKnown); got != c.want {
				t.Errorf("walkLooksImplausible(%d, %d, %v) = %v; want %v",
					c.walked, c.baseline, c.baselineKnown, got, c.want)
			}
		})
	}
}

// TestReapAuthorized_GraceWindow pins the bookkeeping: the first implausible
// walk always refuses, further ones refuse until the grace elapses, and a
// plausible walk in between RESETS the window (so an upstream that recovers
// and later degrades again gets a fresh full grace, not a stale one).
func TestReapAuthorized_GraceWindow(t *testing.T) {
	ing := &Ingester{implausibleSince: make(map[string]time.Time)}
	base := time.Now().UTC()

	if ok, _ := ing.reapAuthorized("srv", 0, 100, true, base); ok {
		t.Fatal("first implausible walk authorised a reap")
	}
	if ok, elapsed := ing.reapAuthorized("srv", 0, 100, true, base.Add(time.Hour)); ok {
		t.Fatalf("reap authorised %v into the grace window", elapsed)
	}
	if ok, _ := ing.reapAuthorized("srv", 0, 100, true, base.Add(implausibleWalkGrace)); !ok {
		t.Fatal("reap still refused at the grace boundary")
	}
	// The window resets once granted, so the next degradation gets a full
	// fresh grace rather than reaping immediately.
	if ok, _ := ing.reapAuthorized("srv", 0, 100, true, base.Add(implausibleWalkGrace+time.Second)); ok {
		t.Fatal("window not reset after the grace expired")
	}

	// A plausible walk clears the window outright.
	if ok, _ := ing.reapAuthorized("other", 0, 100, true, base); ok {
		t.Fatal("first implausible walk authorised a reap for 'other'")
	}
	if ok, _ := ing.reapAuthorized("other", 100, 100, true, base.Add(time.Minute)); !ok {
		t.Fatal("plausible walk refused a reap")
	}
	if _, tracked := ing.implausibleSince["other"]; tracked {
		t.Error("plausible walk left the server marked implausible")
	}
	// ...so the NEXT implausible walk starts a fresh grace window.
	if ok, _ := ing.reapAuthorized("other", 0, 100, true, base.Add(2*time.Minute)); ok {
		t.Fatal("post-recovery implausible walk reaped without a fresh grace window")
	}
}

// TestReapAuthorized_BackwardsClockDelaysReap pins the safe direction under
// an NTP step: a clock that jumps backwards must never authorise a reap
// early. (It also must not permanently wedge — the window is re-armed by
// the next plausible walk.)
func TestReapAuthorized_BackwardsClockDelaysReap(t *testing.T) {
	ing := &Ingester{implausibleSince: make(map[string]time.Time)}
	base := time.Now().UTC()
	if ok, _ := ing.reapAuthorized("srv", 0, 100, true, base); ok {
		t.Fatal("first implausible walk authorised a reap")
	}
	if ok, _ := ing.reapAuthorized("srv", 0, 100, true, base.Add(-24*time.Hour)); ok {
		t.Fatal("backwards clock authorised a reap")
	}
}
