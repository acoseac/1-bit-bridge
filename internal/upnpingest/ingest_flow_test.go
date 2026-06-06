package upnpingest

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/upnp"
)

// --- stub SOAP dispatcher backed by a route table ---

type soapRoute struct {
	action   string
	body     string
	consumed bool
}

type stubSOAP struct {
	mu       sync.Mutex
	routes   []soapRoute
	requests []string
}

func newStubSOAP() *stubSOAP { return &stubSOAP{} }

func (s *stubSOAP) addRoute(action, body string) {
	s.routes = append(s.routes, soapRoute{action: action, body: body})
}

func (s *stubSOAP) Do(_ context.Context, req *http.Request) (*http.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Find the next pending route whose action matches the request's
	// SOAPAction header (allowing each action's queue to drain
	// independently). This keeps the test fixtures readable as
	// per-action lists.
	soapAction := strings.Trim(req.Header.Get("SOAPAction"), `"`)
	for i := range s.routes {
		if !s.routes[i].consumed && strings.HasSuffix(soapAction, "#"+s.routes[i].action) {
			body := s.routes[i].body
			s.routes[i].consumed = true
			s.requests = append(s.requests, soapAction)
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		}
	}
	return nil, fmt.Errorf("stubSOAP: no route for action %q", soapAction)
}

// --- helpers to build SOAP envelopes ---

func wrapBrowse(didl string, numReturned, totalMatches int) string {
	var esc strings.Builder
	_ = xml.EscapeText(&esc, []byte(didl))
	return `<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:BrowseResponse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<Result>` + esc.String() + `</Result>` +
		fmt.Sprintf(`<NumberReturned>%d</NumberReturned><TotalMatches>%d</TotalMatches><UpdateID>1</UpdateID>`,
			numReturned, totalMatches) +
		`</u:BrowseResponse></s:Body></s:Envelope>`
}

func wrapSystemUpdateID(id string) string {
	return `<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:GetSystemUpdateIDResponse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<Id>` + id + `</Id></u:GetSystemUpdateIDResponse>` +
		`</s:Body></s:Envelope>`
}

// patch: add 'consumed' field used by routing — done via re-declaring
// the field in the routes slice in-place. Go doesn't support after-the-
// fact struct mutation, so promote the route type.

// --- stub resolver ---

type stubResolver struct{ controlURL string }

func (r *stubResolver) ResolveControlURL(_ context.Context, _ config.UPnPUpstreamServerConfig) (string, error) {
	if r.controlURL == "" {
		return "", errors.New("no controlURL")
	}
	return r.controlURL, nil
}

// --- end-to-end ingest flow tests ---

func openIngestTestStore(t *testing.T) *manifest.Store {
	t.Helper()
	s, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestIngester_Run_WalksAndUpserts(t *testing.T) {
	// Tree: root(64) -> Music(64$0) -> Artist(64$0$0) -> Album(64$0$0$0) -> track
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
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<item id="64$0$0$0$0" parentID="64$0$0$0">`+
			`<dc:title>Track</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class>`+
			`<upnp:artist>Artist</upnp:artist><upnp:album>Album</upnp:album>`+
			`<upnp:originalTrackNumber>1</upnp:originalTrackNumber>`+
			`<res protocolInfo="http-get:*:audio/x-flac:*" size="999">http://h:8200/MediaItems/5.flac</res>`+
			`</item></DIDL-Lite>`, 1, 1))

	client := upnp.NewContentDirectoryClient(stub)
	store := openIngestTestStore(t)
	cfg := config.UPnPUpstreamConfig{
		Enabled: true,
		Servers: []config.UPnPUpstreamServerConfig{
			{Name: "2Go", UDN: "uuid:test", PathPrefix: "Chord 2Go"},
		},
	}
	ing, err := NewIngester(cfg, client, &stubResolver{controlURL: "http://h:8200/ctl/CD"}, store, nil)
	if err != nil {
		t.Fatalf("NewIngester: %v", err)
	}

	res, err := ing.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.PerServer) != 1 {
		t.Fatalf("per-server len = %d; want 1", len(res.PerServer))
	}
	pr := res.PerServer[0]
	if pr.Err != nil {
		t.Fatalf("per-server err: %v", pr.Err)
	}
	if pr.Walked != 1 || pr.Reaped != 0 {
		t.Fatalf("walked=%d reaped=%d; want 1/0", pr.Walked, pr.Reaped)
	}
	const wantPath = "Chord 2Go/Music/Artist/Album/01 - Track.flac"
	tr, err := store.GetTrack(context.Background(), wantPath)
	if err != nil {
		t.Fatalf("GetTrack: %v", err)
	}
	if tr == nil {
		t.Fatalf("track %q not in manifest", wantPath)
	}
	if tr.Codec != "FLAC" {
		t.Errorf("Codec = %q; want FLAC", tr.Codec)
	}
	if tr.Title != "Track" || tr.Artist != "Artist" || tr.Album != "Album" {
		t.Errorf("metadata: %+v", tr)
	}
	// Routing sidecar present + populated.
	rt, err := store.GetUPnPRouting(context.Background(), wantPath)
	if err != nil {
		t.Fatalf("GetUPnPRouting: %v", err)
	}
	if rt == nil {
		t.Fatal("routing row missing")
	}
	if rt.ServerUDN != "uuid:test" || rt.ObjectID != "64$0$0$0$0" ||
		rt.ResURL != "http://h:8200/MediaItems/5.flac" {
		t.Errorf("routing: %+v", rt)
	}
}

func TestIngester_Run_SkipsWhenSystemUpdateIDMatchesAndWithinBackstop(t *testing.T) {
	stub := newStubSOAP()
	stub.addRoute("GetSystemUpdateID", wrapSystemUpdateID("42"))
	// No Browse routes — the test expects the skip-gate to NOT call Browse.

	client := upnp.NewContentDirectoryClient(stub)
	store := openIngestTestStore(t)
	cfg := config.UPnPUpstreamConfig{
		Enabled: true,
		Servers: []config.UPnPUpstreamServerConfig{
			{Name: "2Go", UDN: "uuid:test"},
		},
	}
	idStore := newMemoryUpdateIDStore()
	idStore.Set("uuid:test", "42", time.Now().Add(-1*time.Hour))

	ing, err := NewIngester(cfg, client, &stubResolver{controlURL: "http://h/ctl/CD"}, store, idStore)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ing.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.PerServer[0].Skipped {
		t.Fatalf("expected Skipped=true; got %+v", res.PerServer[0])
	}
}

func TestIngester_Run_DisabledIsNoop(t *testing.T) {
	stub := newStubSOAP() // no routes; calling anything would error
	client := upnp.NewContentDirectoryClient(stub)
	store := openIngestTestStore(t)
	cfg := config.UPnPUpstreamConfig{Enabled: false}
	ing, err := NewIngester(cfg, client, &stubResolver{controlURL: "http://h/ctl/CD"}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ing.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.PerServer) != 0 {
		t.Fatalf("disabled run should produce no per-server results; got %+v", res.PerServer)
	}
}

func TestIngester_Run_ReapsStaleRowsAfterSuccessfulWalk(t *testing.T) {
	// Pre-seed two routing rows for the same server with old timestamps;
	// the walk yields exactly one track that overlaps one of them. The
	// other must be reaped.
	store := openIngestTestStore(t)
	const olderPath = "Chord 2Go/Music/Old.flac"
	const livePath = "Chord 2Go/Music/Artist/Album/01 - Live.flac"
	tNow := time.Now().UTC()
	tOld := tNow.Add(-1 * time.Hour)
	for _, p := range []string{olderPath, livePath} {
		if err := store.UpsertTrack(context.Background(), &manifest.Track{Path: p, Size: 1, ModTime: tOld}); err != nil {
			t.Fatal(err)
		}
		if err := store.UpsertUPnPRouting(context.Background(), &manifest.UPnPRouting{
			SourcePath: p, ServerUDN: "uuid:test", ObjectID: "x",
			ResURL: "http://h/x.flac", LastSeenAt: tOld,
		}); err != nil {
			t.Fatal(err)
		}
	}

	stub := newStubSOAP()
	stub.addRoute("GetSystemUpdateID", wrapSystemUpdateID("0"))
	// Walker yields ONLY the "Live" track — the "Old" path must drop.
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
	stub.addRoute("Browse", wrapBrowse(
		`<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">`+
			`<item id="x1" parentID="64$0$0$0">`+
			`<dc:title>Live</dc:title><upnp:class>object.item.audioItem.musicTrack</upnp:class>`+
			`<upnp:artist>Artist</upnp:artist><upnp:album>Album</upnp:album>`+
			`<upnp:originalTrackNumber>1</upnp:originalTrackNumber>`+
			`<res protocolInfo="http-get:*:audio/x-flac:*" size="1">http://h/MediaItems/1.flac</res>`+
			`</item></DIDL-Lite>`, 1, 1))

	client := upnp.NewContentDirectoryClient(stub)
	cfg := config.UPnPUpstreamConfig{
		Enabled: true,
		Servers: []config.UPnPUpstreamServerConfig{
			{Name: "2Go", UDN: "uuid:test", PathPrefix: "Chord 2Go"},
		},
	}
	ing, err := NewIngester(cfg, client, &stubResolver{controlURL: "http://h:8200/ctl/CD"}, openStoreFromExisting(t, store), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Reuse the same store (NewIngester takes a *Store; we constructed
	// it above so the seeded rows survive).
	ing = mustReplaceStore(ing, store)

	res, err := ing.Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.PerServer[0].Walked != 1 {
		t.Fatalf("walked = %d; want 1", res.PerServer[0].Walked)
	}
	if res.PerServer[0].Reaped != 1 {
		t.Fatalf("reaped = %d; want 1", res.PerServer[0].Reaped)
	}
	// olderPath gone, livePath present.
	if got, _ := store.GetTrack(context.Background(), olderPath); got != nil {
		t.Errorf("old track should be reaped")
	}
	if got, _ := store.GetTrack(context.Background(), livePath); got == nil {
		t.Errorf("live track should remain")
	}
}

// --- test infra helpers ---

// openStoreFromExisting is a no-op shim so the test can pass the
// already-open store into NewIngester. Kept as a helper so its purpose
// is self-documenting.
func openStoreFromExisting(_ *testing.T, s *manifest.Store) *manifest.Store { return s }

// mustReplaceStore is a test seam: we already constructed an Ingester
// with a store handle; this swap is purely defensive against any future
// change that might require a different wire-up. Today it's an identity.
func mustReplaceStore(i *Ingester, s *manifest.Store) *Ingester {
	i.store = s
	return i
}
