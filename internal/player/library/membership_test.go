package library

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// The golden track ids of two paths: the first 8 bytes of SHA-256("\x00" +
// the path's UTF-8), computed outside Go (B470 PR 5a4's brief), which 1-bit's
// DLNATrackIDHasher derives from the manifest path too.
const (
	goldenPath    = "Artist/Album (2020)/01 Track One.flac"
	goldenID      = "b5b4c8dc9764772e"
	goldenUnicode = "Ünïcödé/Ålbum/02 – Song.flac"
	goldenUniID   = "9ec9973104172764"
)

// member is Member's answer for id: nil, or the refusal's reason.
func member(t *testing.T, r *Resolver, id string) engine.Reason {
	t.Helper()
	err := r.Member(context.Background(), id)
	if err == nil {
		return ""
	}
	var u *engine.Unplayable
	if !errors.As(err, &u) {
		t.Fatalf("Member(%s): %v (%T), want nil or an *engine.Unplayable", id, err, err)
	}
	return u.Reason
}

// TestTheGoldenIDsNameTheirTracks pins the id a controller sends against the
// path the index stores, with ids computed outside the bridge, never through
// its own hash: a library holding each path takes its id, and the URI that
// names it opens it.
func TestTheGoldenIDsNameTheirTracks(t *testing.T) {
	f := newLibraryFixture(t)
	f.add(goldenPath, []byte("one"))
	f.add(goldenUnicode, []byte("two"))
	for _, tc := range []struct{ id, data string }{{goldenID, "one"}, {goldenUniID, "two"}} {
		if got := member(t, f.r, tc.id); got != "" {
			t.Errorf("Member(%s): %s, want the track", tc.id, got)
		}
		ref, err := engine.NewReference("http://192.168.1.20:7790/dlna/file/" + tc.id)
		if err != nil {
			t.Fatal(err)
		}
		c, err := f.open(ref)
		if err != nil {
			t.Fatalf("the URI of %s: %v", tc.id, err)
		}
		if got := read(t, c); string(got) != tc.data {
			t.Errorf("the URI of %s read %q, want %q", tc.id, got, tc.data)
		}
	}
}

// TestMembershipIsTheOpensChecksShortOfTheFile pins Member: a track of the
// library's inside a root configured now is a member, whatever is at its path
// (it opens nothing: the open of an occurrence checks the file); an id no row
// has, a row an upstream server routes, an SACD image's virtual track, a root
// no longer configured and two rows with one id are each refused as an open
// refuses them; and what is no id at all is not_indexed.
func TestMembershipIsTheOpensChecksShortOfTheFile(t *testing.T) {
	f := newLibraryFixture(t)
	opened := 0
	f.r.cfg.Seams = &Seams{BeforeOpen: func() { opened++ }}
	ref := f.add("Artist/Album/01 a.flac", []byte("a"))
	if got := member(t, f.r, ref.String()); got != "" {
		t.Fatalf("an indexed track: %s", got)
	}
	gone := f.index("Artist/Album/02 nothing here.flac")
	if got := member(t, f.r, gone.String()); got != "" {
		t.Fatalf("an indexed track with no file at its path: %s, want a member (the open checks the file)", got)
	}
	if opened != 0 {
		t.Fatalf("Member opened %d files", opened)
	}
	if got := member(t, f.r, Reference("never/indexed.flac").String()); got != ReasonNotIndexed {
		t.Errorf("an id no row has: %s", got)
	}
	routed := f.add("2go/Album/x.flac", []byte("x"))
	if err := f.store.UpsertUPnPRouting(context.Background(), &manifest.UPnPRouting{
		SourcePath: "2go/Album/x.flac", ServerUDN: "uuid:2go", ObjectID: "1", ResURL: "http://192.0.2.1/x.flac", LastSeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if got := member(t, f.r, routed.String()); got != ReasonRouted {
		t.Errorf("a routed track: %s", got)
	}
	virtual := f.index("Music/A.iso/st/01.dff")
	if got := member(t, f.r, virtual.String()); got != ReasonSACDVirtual {
		t.Errorf("an SACD image's virtual track: %s", got)
	}
	c, err := f.open(virtual)
	f.refusedWith("an SACD image's virtual track", c, err, ReasonSACDVirtual)
	for _, id := range []string{"", "B5B4C8DC9764772E", "b5b4c8dc9764772", "http://192.168.1.20:7790/dlna/file/" + goldenID} {
		if got := member(t, f.r, id); got != ReasonNotIndexed {
			t.Errorf("Member(%q): %s, want not_indexed", id, got)
		}
	}
	second := filepath.Join(t.TempDir(), "Second")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	f.paths.SetRoots([]string{second, filepath.Join(t.TempDir(), "Third")})
	if got := member(t, f.r, ref.String()); got != ReasonOutsideLibrary {
		t.Errorf("a track whose root is no longer configured: %s", got)
	}
	f.paths.SetRoots([]string{f.root})
	f.r.idOf = func(string) string { return "00000000000000aa" }
	f.r.ids.reset()
	if got := member(t, f.r, "00000000000000aa"); got != ReasonAmbiguous {
		t.Errorf("two rows with one id: %s", got)
	}
}

// TestBothReferenceFormsOpenAndTheHostIsNotChecked pins the stored forms: an
// entry's reference is the bare track id (serve's form before PR 5a4) or the
// canonical URI as a controller inserted it, and both open the track; the URI's
// host and port are not looked at (an address the bridge never had opens, so a
// queue keeps playing after the bridge's address changes); a near miss of the
// canonical form is refused at the open, as a hand-edited queue file's would
// be.
func TestBothReferenceFormsOpenAndTheHostIsNotChecked(t *testing.T) {
	f := newLibraryFixture(t)
	id := f.add("a.flac", []byte("the track")).String()
	for _, s := range []string{id, "http://192.168.1.20:7790/dlna/file/" + id, "http://203.0.113.9:1/dlna/file/" + id,
		"http://[fe80::1]:7790/dlna/file/" + id, "http://Bridge.LOCAL:7790/dlna/file/" + id} {
		ref, err := engine.NewReference(s)
		if err != nil {
			t.Fatal(err)
		}
		c, err := f.open(ref)
		if err != nil {
			t.Fatalf("the reference %s: %v", s, err)
		}
		if got := read(t, c); string(got) != "the track" {
			t.Errorf("the reference %s read %q", s, got)
		}
	}
	for _, s := range []string{"http://192.168.1.20:07790/dlna/file/" + id, "HTTP://192.168.1.20:7790/dlna/file/" + id,
		"http://192.168.1.20:7790/dlna/file/" + id + "/", "http://192.168.1.20:7790/dlna/file/" + id + "?x",
		"http://[::ffff:192.168.1.20]:7790/dlna/file/" + id, "http://192.168.001.020:7790/dlna/file/" + id} {
		ref, err := engine.NewReference(s)
		if err != nil {
			t.Fatal(err)
		}
		c, err := f.open(ref)
		f.refusedWith("the near miss "+s, c, err, ReasonNotIndexed)
	}
}

// TestATrackAScanAddsIsFoundAtOnce pins the index's freshness against the
// real store: built by a first lookup, it finds a track upserted after it at
// the next, and a deleted one is refused at the next; a deleted and re-indexed
// one is found again.
func TestATrackAScanAddsIsFoundAtOnce(t *testing.T) {
	f := newLibraryFixture(t)
	a := f.add("a.flac", []byte("a"))
	if got := member(t, f.r, a.String()); got != "" {
		t.Fatalf("a: %s", got)
	}
	b := f.add("b.flac", []byte("b"))
	if got := member(t, f.r, b.String()); got != "" {
		t.Fatalf("a track indexed after the index was built: %s, want found at once", got)
	}
	if err := f.store.DeleteTrack(context.Background(), "a.flac"); err != nil {
		t.Fatal(err)
	}
	if got := member(t, f.r, a.String()); got != ReasonNotIndexed {
		t.Fatalf("a deleted track: %s, want not_indexed", got)
	}
	f.index("a.flac")
	if got := member(t, f.r, a.String()); got != "" {
		t.Fatalf("a deleted track indexed again: %s", got)
	}
}

// fakeStore is a library index of a synthetic library, as the manifest store
// answers it: rows by path, each stamped past every earlier write, read in
// stamp order; and the rows it hands over counted, the resolver's work.
type fakeStore struct {
	mu    sync.Mutex
	rows  map[string]int64
	stamp int64
	// handed counts the rows TrackPathsIndexedAfter handed over, and gets
	// the rows GetTrack read; hold, when set, is called before each row is
	// handed over (a test cancels there).
	handed, gets int
	hold         func(n int)
}

func newFakeStore() *fakeStore { return &fakeStore{rows: map[string]int64{}} }

// put writes the paths in one commit: one stamp, past every earlier one.
func (s *fakeStore) put(paths ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stamp++
	for _, p := range paths {
		s.rows[p] = s.stamp
	}
}

func (s *fakeStore) remove(paths ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range paths {
		delete(s.rows, p)
	}
}

func (s *fakeStore) TrackPathsIndexedAfter(ctx context.Context, after int64, fn func(string, int64) error) error {
	s.mu.Lock()
	type row struct {
		p  string
		at int64
	}
	var rows []row
	for p, at := range s.rows {
		if at > after {
			rows = append(rows, row{p, at})
		}
	}
	hold := s.hold
	s.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].at < rows[j].at || rows[i].at == rows[j].at && rows[i].p < rows[j].p
	})
	for i, r := range rows {
		if hold != nil {
			hold(i)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		s.handed++
		s.mu.Unlock()
		if err := fn(r.p, r.at); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeStore) GetTrack(_ context.Context, path string) (*manifest.Track, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	if _, ok := s.rows[path]; !ok {
		return nil, nil
	}
	return &manifest.Track{Path: path}, nil
}

func (s *fakeStore) GetUPnPRouting(context.Context, string) (*manifest.UPnPRouting, error) {
	return nil, nil
}

// counts is what the store has handed over and read so far.
func (s *fakeStore) counts() (handed, gets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handed, s.gets
}

// syntheticPath is the i-th path of the synthetic library.
func syntheticPath(i int) string {
	return fmt.Sprintf("Artist %03d/Album %03d/%02d Track.flac", i/1000, i/10%100, i%10)
}

// syntheticResolver is a resolver over a synthetic library of n tracks.
func syntheticResolver(t *testing.T, n int) (*Resolver, *fakeStore) {
	t.Helper()
	s := newFakeStore()
	paths := make([]string, n)
	for i := range paths {
		paths[i] = syntheticPath(i)
	}
	// As a scan writes: batches of 500, a stamp each.
	for i := 0; i < n; i += 500 {
		s.put(paths[i:min(i+500, n)]...)
	}
	r, err := New(Config{Store: s, Paths: fakePaths{}})
	if err != nil {
		t.Fatal(err)
	}
	return r, s
}

// fakePaths maps every stored path onto one root.
type fakePaths struct{}

func (fakePaths) Resolve(p string) (string, error) { return filepath.Join("/music", p), nil }
func (fakePaths) Roots() []string                  { return []string{"/music"} }

// TestTheIndexCostsTheLibraryOnceAndASeekAfter pins the membership check's
// cost over a large synthetic library (25,000 tracks): New reads nothing; the
// first lookup reads every row once, the initialization; then a lookup of
// another track reads no row but its own, a hundred lookups of ids no track has
// read none (no rescan per miss), a track added afterwards is found by reading
// it alone, a deleted one is refused by reading its own row, and found again,
// once indexed again, by reading it alone.
func TestTheIndexCostsTheLibraryOnceAndASeekAfter(t *testing.T) {
	const n = 25_000
	r, s := syntheticResolver(t, n)
	if handed, gets := s.counts(); handed != 0 || gets != 0 {
		t.Fatalf("New read %d rows and %d tracks, want none", handed, gets)
	}
	step := func(what string, id string, want engine.Reason, handedWant, getsWant int) {
		t.Helper()
		h0, g0 := s.counts()
		if got := member(t, r, id); got != want {
			t.Fatalf("%s: %q, want %q", what, got, want)
		}
		h1, g1 := s.counts()
		if h1-h0 != handedWant || g1-g0 != getsWant {
			t.Fatalf("%s read %d rows and %d tracks, want %d and %d", what, h1-h0, g1-g0, handedWant, getsWant)
		}
	}
	step("the first lookup", Reference(syntheticPath(7)).String(), "", n, 1)
	step("a lookup of another track", Reference(syntheticPath(24_999)).String(), "", 0, 1)
	for i := 0; i < 100; i++ {
		step("a lookup of an id no track has", Reference(fmt.Sprintf("absent/%d.flac", i)).String(), ReasonNotIndexed, 0, 0)
	}
	s.put("New/Album/01 added.flac")
	step("a track added after the index was built", Reference("New/Album/01 added.flac").String(), "", 1, 1)
	s.remove(syntheticPath(42))
	step("a deleted track", Reference(syntheticPath(42)).String(), ReasonNotIndexed, 0, 1)
	step("the deleted track again", Reference(syntheticPath(42)).String(), ReasonNotIndexed, 0, 0)
	s.put(syntheticPath(42))
	step("the deleted track indexed again", Reference(syntheticPath(42)).String(), "", 1, 1)
	if r.ids.builds != 1 {
		t.Fatalf("%d builds, want the first alone", r.ids.builds)
	}
}

// TestTheIndexIsBuiltAgainOncePastItsSlack pins the bound on what the index
// holds of deleted rows: a library replaced whole (every track deleted and as
// many others indexed) leaves the old paths in the index until it has grown
// past twice the rows its last build read and staleSlack, and the next lookup
// then builds it again from the first row, so it never holds more than a few
// times the library.
func TestTheIndexIsBuiltAgainOncePastItsSlack(t *testing.T) {
	const n = 1_000
	r, s := syntheticResolver(t, n)
	if got := member(t, r, Reference(syntheticPath(0)).String()); got != "" {
		t.Fatal(got)
	}
	for round := 1; r.ids.builds == 1; round++ {
		if round > 10 {
			t.Fatalf("the index holds %d paths for %d built, past %d, and was not built again", r.ids.paths, r.ids.built, 2*r.ids.built+staleSlack)
		}
		for i := 0; i < n; i++ {
			s.remove(syntheticPath(i + (round-1)*n))
		}
		var next []string
		for i := 0; i < n; i++ {
			next = append(next, syntheticPath(i+round*n))
		}
		s.put(next...)
		if got := member(t, r, Reference(next[0]).String()); got != "" {
			t.Fatalf("round %d: %s", round, got)
		}
	}
	if r.ids.paths > 2*n || r.ids.built != n {
		t.Fatalf("after the build again: %d paths, %d built, want the %d of the library now", r.ids.paths, r.ids.built, n)
	}
}

// TestACancelledBuildGoesOnWhereItStopped pins the index's cancellation: a
// lookup whose context ends while the index is built answers ErrCancelled,
// refusing nothing, and keeps what it read whole; the next lookup reads only
// the rest (and the commit it stopped inside again), and finds the track.
func TestACancelledBuildGoesOnWhereItStopped(t *testing.T) {
	const n = 25_000
	r, s := syntheticResolver(t, n)
	ctx, cancel := context.WithCancel(context.Background())
	s.hold = func(i int) {
		if i == n/2 {
			cancel()
		}
	}
	if err := r.Member(ctx, Reference(syntheticPath(n-1)).String()); !errors.Is(err, ErrCancelled) {
		t.Fatalf("a lookup cancelled mid-build: %v, want ErrCancelled", err)
	}
	first, _ := s.counts()
	s.hold = nil
	if got := member(t, r, Reference(syntheticPath(n-1)).String()); got != "" {
		t.Fatalf("the lookup after: %s", got)
	}
	total, _ := s.counts()
	if total-first > n-first+500 {
		t.Fatalf("the lookup after the cancelled one read %d rows, more than the %d left and one commit's 500", total-first, n-first)
	}
	if r.ids.builds != 1 {
		t.Fatalf("%d builds, want one", r.ids.builds)
	}
}

// TestAScanChangingRowsBesideTheLookups pins the index under a scan that
// writes as lookups run (under -race): a track indexed before a lookup began
// and never deleted is found by it, every track the scan added is found once
// it has written, and a deleted one is not found once it is deleted.
func TestAScanChangingRowsBesideTheLookups(t *testing.T) {
	const n, writes = 5_000, 2_000
	r, s := syntheticResolver(t, n)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			s.put(fmt.Sprintf("Scan/%d.flac", i))
			s.remove(syntheticPath(1_000 + i))
		}
	}()
	for i := 0; i < 500; i++ {
		if got := member(t, r, Reference(syntheticPath(i)).String()); got != "" {
			t.Fatalf("a track indexed before the scan began: %s", got)
		}
	}
	wg.Wait()
	for _, i := range []int{0, writes / 2, writes - 1} {
		if got := member(t, r, Reference(fmt.Sprintf("Scan/%d.flac", i)).String()); got != "" {
			t.Fatalf("a track the scan added: %s", got)
		}
		if got := member(t, r, Reference(syntheticPath(1_000+i)).String()); got != ReasonNotIndexed {
			t.Fatalf("a track the scan deleted: %s", got)
		}
	}
}
