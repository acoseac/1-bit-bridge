package openhome

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/library"
)

// The confinement's tests (B470 PR 5a4): what an Insert's Uri may be, row by
// row, each refusal with its code; and the golden track ids taken from a
// library that holds their paths, through the player's own library resolver.

// confineRow is one row of the confinement matrix: a Uri, and the fault an
// Insert of it answers (0 for taken).
type confineRow struct {
	what  string
	uri   string
	fault int
}

// insertFault is the fault an Insert of uri answers through h, 0 when it is
// taken; a taken Insert's Read gives the Uri back byte for byte (round 3: as
// sent, its name in the case the controller wrote it).
func insertFault(t *testing.T, h *handler, uri string) int {
	t.Helper()
	r := invoke(t, h, playlist, "Insert", "AfterId", "0", "Uri", uri, "Metadata", "")
	if r.status == http.StatusOK {
		id := ui4Value(t, "NewId", r.values["NewId"])
		if id == 0 {
			t.Errorf("%s: taken with NewId 0", uri)
		}
		if got := read(t, h, playlist, "Read", "Id", u(id))["Uri"]; got != uri {
			t.Errorf("%s: Read gives %q back, want the Uri as sent", uri, got)
		}
		return 0
	}
	if r.status != http.StatusInternalServerError {
		t.Fatalf("%s: %d, want a response or a fault", uri, r.status)
	}
	return r.fault
}

// TestTheConfinementMatrix pins what an Insert takes: the canonical URI of a
// track of the bridge's own, on a host the bridge advertises in /v1/health or
// binds for DLNA, on 7790 or dlna.listenAddress's port; 605 for a Uri past
// 128 bytes whatever its shape; 600, Argument Value Invalid, for every other
// shape, a host or port that is not the bridge's, a host no controller on the
// LAN can reach the bridge by (loopback, link-local with or without a zone,
// a tailnet's, localhost), a paired alias the bridge never advertised, and a
// track id the library does not serve now (no row, routed, an SACD image's
// virtual track, outside the roots); 501 for a library that cannot answer.
// Nothing is resolved: a name is compared as a string, its ASCII case aside,
// and a host with a byte that is not ASCII is 600 whatever Unicode folds it
// to (round 2: U+0130 lower-cases to i and the Kelvin sign to k, so
// lower-cased first they read as names the bridge advertises). A refusal
// never reaches the engine, and a Uri refused for its host or its shape never
// asks the library; a Uri taken is stored as sent (round 3), its name in the
// case the controller wrote it.
func TestTheConfinementMatrix(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	id := goldenID
	at := func(host string) string { return "http://" + host + "/dlna/file/" + id }
	rows := []confineRow{
		{"the LAN address health advertises and DLNA binds", at("192.168.1.5:7790"), 0},
		{"a second interface DLNA binds", at("10.0.0.5:7790"), 0},
		{"dlna.listenAddress's port", at("192.168.1.5:7800"), 0},
		{"a ULA health advertises", at("[fd00::5]:7790"), 0},
		{"a .local name health advertises", at("bridge.local:7790"), 0},
		{"that name in capitals", at("Bridge.LOCAL:7790"), 0},
		{"a second name health advertises, in capitals", at("KITCHEN.Local:7790"), 0},
		{"a custom endpoint's name", at("music.example.com:7790"), 0},
		{"another track of the library's", "http://192.168.1.5:7790/dlna/file/" + trackB, 0},

		{"a dotted capital I (U+0130), which lower-cases to i", at("brİdge.local:7790"), 600},
		{"the Kelvin sign (U+212A), which lower-cases to k", at("Kitchen.local:7790"), 600},
		{"a long s (U+017F), which folds to s", at("muſic.example.com:7790"), 600},
		{"a fullwidth letter", at("ｂridge.local:7790"), 600},
		{"a byte that is not ASCII in the port", at("192.168.1.5:７７９０"), 600},

		{"an address the bridge neither advertises nor binds", at("192.168.1.99:7790"), 600},
		{"a port no track is served on", at("192.168.1.5:7801"), 600},
		{"the API's port", at("192.168.1.5:7788"), 600},
		{"a paired alias the bridge never advertised", at("nas.home.arpa:7790"), 600},
		{"a name that spells an advertised address", at("192-168-1-5.nip.io:7790"), 600},
		{"a tailnet address health advertises", at("100.101.102.103:7790"), 600},
		{"a tailnet name health advertises", at("bridge.tail1234.ts.net:7790"), 600},
		{"loopback", at("127.0.0.1:7790"), 600},
		{"IPv6 loopback", at("[::1]:7790"), 600},
		{"localhost", at("localhost:7790"), 600},
		{"a link-local IPv6 literal", at("[fe80::5]:7790"), 600},
		{"a link-local IPv6 literal with a zone", at("[fe80::5%25eth0]:7790"), 600},
		{"a link-local IPv4 literal", at("169.254.7.5:7790"), 600},
		{"the unspecified address", at("0.0.0.0:7790"), 600},
		{"a multicast address", at("239.255.255.250:7790"), 600},

		{"HTTP in capitals", "HTTP://192.168.1.5:7790/dlna/file/" + id, 600},
		{"https", "https://192.168.1.5:7790/dlna/file/" + id, 600},
		{"a zero-padded port", at("192.168.1.5:07790"), 600},
		{"an empty port", at("192.168.1.5:"), 600},
		{"no port", at("192.168.1.5"), 600},
		{"a zero-padded address", at("192.168.001.005:7790"), 600},
		{"an abbreviated address", at("192.168.1:7790"), 600},
		{"an IPv6 literal not in its canonical form", at("[fd00:0::5]:7790"), 600},
		{"an IPv4-mapped IPv6 literal", at("[::ffff:192.168.1.5]:7790"), 600},
		{"a trailing dot", at("bridge.local.:7790"), 600},
		{"user information", at("u@192.168.1.5:7790"), 600},
		{"a query", at("192.168.1.5:7790") + "?x=1", 600},
		{"a fragment", at("192.168.1.5:7790") + "#x", 600},
		{"percent-encoding in the path", "http://192.168.1.5:7790/dlna/file/%62" + id[1:], 600},
		{"percent-encoding in the host", at("192.168.1.%35:7790"), 600},
		{"a trailing slash", at("192.168.1.5:7790") + "/", 600},
		{"a rendition's path", at("192.168.1.5:7790") + "/variant-flac", 600},
		{"another route", "http://192.168.1.5:7790/dlna/art/" + id, 600},
		{"an id in capitals", "http://192.168.1.5:7790/dlna/file/" + strings.ToUpper(id), 600},
		{"a short id", "http://192.168.1.5:7790/dlna/file/" + id[1:], 600},
		{"a bare id", id, 600},
		{"a UPnP server's res", "http://192.168.1.77:8200/MediaItems/22.flac", 600},
		{"empty", "", 600},

		{"129 bytes", "http://192.168.1.5:7790/dlna/file/" + id + strings.Repeat("x", 129-len(at("192.168.1.5:7790"))), 605},
		{"129 bytes of anything", strings.Repeat("a", 129), 605},
	}
	want := 0
	for _, row := range rows {
		if row.fault == 0 {
			want++
		}
	}
	for _, row := range rows {
		p.lib.mu.Lock()
		p.lib.asked = nil
		p.lib.mu.Unlock()
		if got := insertFault(t, h, row.uri); got != row.fault {
			t.Errorf("%s (%s): fault %d, want %d", row.what, row.uri, got, row.fault)
		}
		p.lib.mu.Lock()
		asked := slices.Clone(p.lib.asked)
		p.lib.mu.Unlock()
		if row.fault != 0 && len(asked) != 0 {
			t.Errorf("%s: the library was asked %v for a Uri refused for its host or shape", row.what, asked)
		}
	}
	if n := len(p.eng.Snapshot().IDs()); n != want {
		t.Errorf("the queue holds %d entries, want the %d taken", n, want)
	}

	for _, tc := range []struct {
		why   error
		fault int
	}{
		{&engine.Unplayable{Reason: library.ReasonNotIndexed}, 600},
		{&engine.Unplayable{Reason: library.ReasonRouted}, 600},
		{&engine.Unplayable{Reason: library.ReasonSACDVirtual}, 600},
		{&engine.Unplayable{Reason: library.ReasonOutsideLibrary}, 600},
		{&engine.Unplayable{Reason: library.ReasonAmbiguous}, 501},
		{&engine.Unplayable{Reason: library.ReasonIndexUnreadable}, 501},
		{&engine.Unplayable{Reason: library.ReasonResolverFailed}, 501},
		{library.ErrCancelled, 501},
		{errors.New("the index went away"), 501},
	} {
		p.lib.refuse(trackC, tc.why)
		if got := insertFault(t, h, "http://192.168.1.5:7790/dlna/file/"+trackC); got != tc.fault {
			t.Errorf("a track the library refuses with %v: fault %d, want %d", tc.why, got, tc.fault)
		}
	}
	h.library = nil
	if got := insertFault(t, h, at("192.168.1.5:7790")); got != 501 {
		t.Errorf("no library: fault %d, want 501", got)
	}
	h.library, h.hosts = p.lib, nil
	if got := insertFault(t, h, at("192.168.1.5:7790")); got != 600 {
		t.Errorf("no hosts: fault %d, want 600", got)
	}
}

// TestTheHostsAreTheInventoryNow pins the hosts as the inventory gives them at
// each Insert: an address health advertises first that the DLNA listener does
// not bind (a docker0 bridge) is the bridge's own and is taken; once health
// stops advertising an address, it is refused, unless DLNA still binds it,
// and once neither does, it is refused; and a link-local address is refused
// even when health advertises it (a custom endpoint can) and DLNA binds it.
func TestTheHostsAreTheInventoryNow(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	var mu sync.Mutex
	advertised := []string{"https://172.17.0.1:7788", "https://192.168.1.5:7788", "https://[fe80::5%25eth0]:7788", "https://169.254.7.5:7788"}
	served := map[netip.Addr]bool{netip.MustParseAddr("192.168.1.5"): true, netip.MustParseAddr("fe80::5"): true}
	h.hosts = Inventory{
		Advertised: func() []string {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(advertised)
		},
		Served: func(a netip.Addr) bool {
			mu.Lock()
			defer mu.Unlock()
			return served[a]
		},
		Ports: []uint16{DefaultTrackPort},
	}
	uri := func(host string) string { return "http://" + host + ":7790/dlna/file/" + goldenID }
	for _, host := range []string{"172.17.0.1", "192.168.1.5"} {
		if got := insertFault(t, h, uri(host)); got != 0 {
			t.Errorf("%s, advertised: fault %d, want taken", host, got)
		}
	}
	for _, host := range []string{"[fe80::5]", "[fe80::5%25eth0]", "169.254.7.5"} {
		if got := insertFault(t, h, uri(host)); got != 600 {
			t.Errorf("%s, link-local: fault %d, want 600", host, got)
		}
	}
	mu.Lock()
	advertised = advertised[1:2]
	mu.Unlock()
	if got := insertFault(t, h, uri("172.17.0.1")); got != 600 {
		t.Errorf("an address health no longer advertises: fault %d, want 600", got)
	}
	mu.Lock()
	advertised = nil
	mu.Unlock()
	if got := insertFault(t, h, uri("192.168.1.5")); got != 0 {
		t.Errorf("an address DLNA still binds: fault %d, want taken", got)
	}
	mu.Lock()
	served = map[netip.Addr]bool{}
	mu.Unlock()
	if got := insertFault(t, h, uri("192.168.1.5")); got != 600 {
		t.Errorf("an address neither advertises nor binds now: fault %d, want 600", got)
	}
}

// TestAnAdvertisedNameIsComparedAsASCII pins the inventory's side of a name's
// comparison (round 2): the host health advertises is lower-cased as ASCII,
// byte by byte, and never folded by Unicode's rules, so a name it advertises
// with the Kelvin sign (U+212A, which folds to k) or a long s (U+017F, which
// folds to s) names no Uri's ASCII host, nor one with a dotted capital I
// (U+0130); one it advertises in ASCII capitals names its lower-case
// spelling, and a Uri that spells it in capitals too.
func TestAnAdvertisedNameIsComparedAsASCII(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	h.hosts = Inventory{
		Advertised: func() []string {
			return []string{"https://Kitchen.local:7788", "https://muſic.example.com", "https://brİdge.lan:7788",
				"https://Bridge.LOCAL:7788"}
		},
		Ports: []uint16{DefaultTrackPort},
	}
	uri := func(host string) string { return "http://" + host + ":7790/dlna/file/" + goldenID }
	for _, tc := range []struct {
		host  string
		fault int
	}{
		{"kitchen.local", 600},
		{"music.example.com", 600},
		{"bridge.lan", 600},
		{"bridge.local", 0},
		{"BRIDGE.local", 0},
	} {
		if got := insertFault(t, h, uri(tc.host)); got != tc.fault {
			t.Errorf("%s: fault %d, want %d", tc.host, got, tc.fault)
		}
	}
}

// TestAnInsertStoresItsURIAsSent pins what an Insert keeps (round 3): the Uri
// as the controller sent it, its name in the case it was written in, once the
// confinement, which compares it in its canonical form, has taken it. Read,
// ReadList, the occurrence the engine hands the backend and Info's Track give
// that spelling, and so do the queue file and Read and ReadList after a
// restart. 1-bit matches Read's Uri byte for byte against what it inserted
// (OpenHomeWake.decide), so a spelling changed in storage loses its cast on a
// relaunch; round 2 stored the canonical form, and round 3 restored this.
func TestAnInsertStoresItsURIAsSent(t *testing.T) {
	p := newTestPlayer(t)
	h := p.handler("")
	const sent = "http://Bridge.LOCAL:7790/dlna/file/" + goldenID
	id := newID(t, h, 0, sent, "meta")
	reads := func(when string) {
		t.Helper()
		if got := read(t, h, playlist, "Read", "Id", u(id))["Uri"]; got != sent {
			t.Errorf("%s: Read gives %q, want the Uri as sent, %q", when, got, sent)
		}
		if list := entries(t, h, u(id)); len(list.Entries) != 1 || list.Entries[0].URI != sent {
			t.Errorf("%s: ReadList gives %+v, want the one entry's Uri as sent, %q", when, list.Entries, sent)
		}
	}
	reads("as inserted")
	read(t, h, playlist, "Play")
	s := p.be.session(t, 1)
	if got := s.first.Ref.String(); got != sent {
		t.Errorf("the occurrence handed to the backend names %q, want %q", got, sent)
	}
	s.hear(s.first.ID, nil)
	p.heardNow(s.first.ID)
	if got := read(t, h, info, "Track")["Uri"]; got != sent {
		t.Errorf("Info's Track gives %q, want %q", got, sent)
	}
	read(t, h, playlist, "Stop")
	p.restart()
	h = p.handler("")
	reads("after a restart")
	raw, err := os.ReadFile(filepath.Join(p.dir, p.queue))
	must(t, "read the queue file", err)
	if !strings.Contains(string(raw), sent) {
		t.Errorf("the queue file holds %s, want the Uri as sent", raw)
	}
}

// TestOneTrackUnderTwoSpellingsIsTwoEntries pins one track inserted twice,
// its host spelled two ways (round 3): two entries with two IDs, each keeping
// its own spelling, each opening the track's file through the library
// resolver (an open parses its stored form, the host not looked at), and each
// played as an occurrence of its own entry and spelling, Info's Track naming
// that spelling while it is heard. Nothing in the engine, the backend or the
// library is keyed on a stored Uri, so neither spelling stands in for the
// other; and after a restart both are there as they were, and both open.
func TestOneTrackUnderTwoSpellingsIsTwoEntries(t *testing.T) {
	p := newTestPlayer(t)
	lib := newRealLibrary(t)
	lib.add("Artist/Album (2020)/01 Track One.flac", []byte("one"))
	h := p.handler("")
	h.library = lib.r
	spellings := []string{"http://Bridge.LOCAL:7790/dlna/file/" + goldenID, "http://bridge.local:7790/dlna/file/" + goldenID}
	var ids []uint32
	for _, uri := range spellings {
		after := uint32(0)
		if len(ids) > 0 {
			after = ids[len(ids)-1]
		}
		ids = append(ids, newID(t, h, after, uri, ""))
	}
	if ids[0] == ids[1] {
		t.Fatalf("both spellings were given the ID %d", ids[0])
	}
	opens := func(when string) {
		t.Helper()
		for i, id := range ids {
			got := read(t, h, playlist, "Read", "Id", u(id))["Uri"]
			if got != spellings[i] {
				t.Errorf("%s: Read(%d) gives %q, want %q", when, id, got, spellings[i])
			}
			ref, err := engine.NewReference(got)
			must(t, "a stored Uri as a reference", err)
			if err := lib.opens(ref); err != nil {
				t.Errorf("%s: %q does not open: %v", when, got, err)
			}
		}
		if list := entries(t, h, u(ids[0])+" "+u(ids[1])); len(list.Entries) != 2 || list.Entries[0].URI != spellings[0] || list.Entries[1].URI != spellings[1] {
			t.Errorf("%s: ReadList gives %+v, want each entry with its own spelling", when, list.Entries)
		}
	}
	opens("as inserted")
	for i, id := range ids {
		read(t, h, playlist, "SeekId", "Value", u(id))
		s := p.be.session(t, i+1)
		if s.first.Entry != id || s.first.Ref.String() != spellings[i] {
			t.Errorf("SeekId(%d) started entry %d naming %q, want entry %d naming %q", id, s.first.Entry, s.first.Ref.String(), id, spellings[i])
		}
		s.hear(s.first.ID, nil)
		p.heardNow(s.first.ID)
		if got := read(t, h, info, "Track")["Uri"]; got != spellings[i] {
			t.Errorf("Info's Track while entry %d is heard: %q, want %q", id, got, spellings[i])
		}
	}
	read(t, h, playlist, "Stop")
	p.restart()
	h = p.handler("")
	h.library = lib.r
	opens("after a restart")
}

// realLibrary is the player's library resolver over a store of its own and a
// root holding the test's files, as serve wires it.
type realLibrary struct {
	t     *testing.T
	root  string
	store *manifest.Store
	r     *library.Resolver
}

func newRealLibrary(t *testing.T) *realLibrary {
	t.Helper()
	root := filepath.Join(t.TempDir(), "Music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	r, err := library.New(library.Config{Store: store, Paths: bridgefs.New([]string{root})})
	if err != nil {
		t.Fatal(err)
	}
	return &realLibrary{t: t, root: root, store: store, r: r}
}

// add writes data at rel below the root and indexes it.
func (l *realLibrary) add(rel string, data []byte) {
	l.t.Helper()
	p := filepath.Join(l.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		l.t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		l.t.Fatal(err)
	}
	if err := l.store.UpsertTrack(context.Background(), &manifest.Track{Path: rel, Size: int64(len(data))}); err != nil {
		l.t.Fatal(err)
	}
}

// opens is what the backend makes of an occurrence's open through the
// resolver (playback's mapping): nil when it opens, the resolver's
// *engine.Unplayable, or missing_file for any other error.
func (l *realLibrary) opens(ref engine.Reference) error {
	c, err := l.r.Open(context.Background(), ref)
	if err == nil {
		return c.Closer.Close()
	}
	var u *engine.Unplayable
	if errors.As(err, &u) {
		return u
	}
	return &engine.Unplayable{Reason: engine.ReasonMissing}
}

// TestTheGoldenURIsAreTakenFromTheLibrary pins the confinement against the
// library's own index with track ids computed outside the bridge, never
// through its own hash: a library holding "Artist/Album (2020)/01 Track
// One.flac" takes http://<host>:7790/dlna/file/b5b4c8dc9764772e, and one
// holding "Ünïcödé/Ålbum/02 – Song.flac" (those UTF-8 bytes) takes
// 9ec9973104172764, each read back as inserted and opened by the resolver at
// the occurrence's open; a track id the library has no row for, a routed
// track and an SACD image's virtual track are 600.
func TestTheGoldenURIsAreTakenFromTheLibrary(t *testing.T) {
	p := newTestPlayer(t)
	lib := newRealLibrary(t)
	lib.add("Artist/Album (2020)/01 Track One.flac", []byte("one"))
	lib.add("Ünïcödé/Ålbum/02 – Song.flac", []byte("two"))
	lib.add("2go/Album/x.flac", []byte("x"))
	if err := lib.store.UpsertUPnPRouting(context.Background(), &manifest.UPnPRouting{
		SourcePath: "2go/Album/x.flac", ServerUDN: "uuid:2go", ObjectID: "1", ResURL: "http://192.0.2.1/x.flac", LastSeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := lib.store.UpsertTrack(context.Background(), &manifest.Track{Path: "Music/A.iso/st/01.dff", Size: 1}); err != nil {
		t.Fatal(err)
	}
	h := p.handler("")
	h.library = lib.r
	for _, id := range []string{"b5b4c8dc9764772e", "9ec9973104172764"} {
		uri := "http://192.168.1.5:7790/dlna/file/" + id
		if got := insertFault(t, h, uri); got != 0 {
			t.Fatalf("%s: fault %d, want taken", uri, got)
		}
		ref, err := engine.NewReference(uri)
		if err != nil {
			t.Fatal(err)
		}
		if err := lib.opens(ref); err != nil {
			t.Errorf("%s does not open: %v", uri, err)
		}
	}
	for _, rel := range []string{"never/indexed.flac", "2go/Album/x.flac", "Music/A.iso/st/01.dff"} {
		uri := "http://192.168.1.5:7790/dlna/file/" + library.Reference(rel).String()
		if got := insertFault(t, h, uri); got != 600 {
			t.Errorf("%s: fault %d, want 600", rel, got)
		}
	}
	if n := len(p.eng.Snapshot().IDs()); n != 2 {
		t.Errorf("the queue holds %d entries, want the two golden tracks", n)
	}
}
