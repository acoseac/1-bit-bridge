package library

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// warmWatchdog bounds a wait of these tests on a goroutine of theirs: a
// build of 25,000 rows under the race detector takes a second or two.
const warmWatchdog = time.Minute

// await waits for ch to close, or fails the test after warmWatchdog.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(warmWatchdog):
		t.Fatalf("waited %v for %s", warmWatchdog, what)
	}
}

// answer waits for an answer on ch, or fails the test after warmWatchdog.
func answer(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(warmWatchdog):
		t.Fatalf("waited %v for %s to answer", warmWatchdog, what)
		return nil
	}
}

// askedCtx is parent, for a lookup or a warm, telling the test when it is
// first asked for its Done: as the lookup or the warm waits for the
// resolver's turn, the first thing either waits for (pathsOf, warm). It is
// handed to the resolver itself, never as the parent of another context,
// whose making would ask it.
type askedCtx struct {
	context.Context
	once  sync.Once
	asked chan struct{}
}

func newAskedCtx(parent context.Context) *askedCtx {
	return &askedCtx{Context: parent, asked: make(chan struct{})}
}

func (c *askedCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.asked) })
	return c.Context.Done()
}

// TestALookupDuringTheWarmWaitsForItAndFindsTheIndexBuilt pins Warm's turn:
// a warm holds the resolver's turn for the whole build, so a lookup that
// comes while it builds (here once it has read half of 25,000 tracks) waits
// for it in the turn and then finds the index built. The library is read
// once, by the warm, in one build, and the lookup reads its own row alone. A
// second read of the library, a lookup's beside the warm, would be held
// until the warm has returned, so the counts tell the two apart.
func TestALookupDuringTheWarmWaitsForItAndFindsTheIndexBuilt(t *testing.T) {
	const n = 25_000
	r, s := syntheticResolver(t, n)
	parked, release, second := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var reads atomic.Int32
	s.hold = func(i int) {
		if i == 0 && reads.Add(1) == 2 {
			<-second
		}
		if i == n/2 && reads.Load() == 1 {
			close(parked)
			<-release
		}
	}
	warmed := make(chan error, 1)
	go func() { warmed <- r.Warm(context.Background()) }()
	await(t, parked, "the warm halfway through the library")
	ctx := newAskedCtx(context.Background())
	looked := make(chan error, 1)
	go func() { looked <- r.Member(ctx, Reference(syntheticPath(n-1)).String()) }()
	await(t, ctx.asked, "the lookup to wait for the turn")
	close(release)
	if err := answer(t, warmed, "the warm"); err != nil {
		t.Fatalf("the warm: %v", err)
	}
	close(second)
	if err := answer(t, looked, "the lookup"); err != nil {
		t.Fatalf("the lookup that came during the warm: %v, want the track found", err)
	}
	handed, gets := s.counts()
	if handed != n || r.ids.reads != n || r.ids.builds != 1 || gets != 1 {
		t.Errorf("the library handed over %d rows (%d into the index, %d builds) and %d tracks; want every row once, by the warm, in one build, and the lookup's own track", handed, r.ids.reads, r.ids.builds, gets)
	}
}

// failingStore is a library index whose first read of the paths fails once
// it has handed over failAfter rows, with an error that quotes a stored
// path, as a store's can; every read after it is the store's own.
type failingStore struct {
	*fakeStore
	failAfter int
	failed    atomic.Bool
}

func (s *failingStore) TrackPathsIndexedAfter(ctx context.Context, after int64, fn func(string, int64) error) error {
	if s.failed.Swap(true) {
		return s.fakeStore.TrackPathsIndexedAfter(ctx, after, fn)
	}
	n := 0
	return s.fakeStore.TrackPathsIndexedAfter(ctx, after, func(p string, at int64) error {
		if n == s.failAfter {
			return fmt.Errorf("disk I/O error reading the row of %s", p)
		}
		n++
		return fn(p, at)
	})
}

// TestAWarmThatFailsLeavesTheNextLookupToBuild pins a warm whose read of the
// index fails (here halfway through 25,000 tracks, with an error that quotes
// a stored path): Warm answers a failure, not a cancellation, that names no
// path, and logs it once, at Warn, by the error's class and never its text;
// the index keeps what the warm took whole, and the turn is let go, so the
// next lookup, within its own bound, goes on from where the warm stopped,
// reading the rest of the library and the commit it stopped inside again,
// and finds the track; the index has been built once.
func TestAWarmThatFailsLeavesTheNextLookupToBuild(t *testing.T) {
	const n = 25_000
	rec := loggingtest.Record(t)
	s := syntheticStore(n)
	r, err := New(Config{Store: &failingStore{fakeStore: s, failAfter: n / 2}, Paths: fakePaths{}})
	if err != nil {
		t.Fatal(err)
	}
	err = r.Warm(context.Background())
	if err == nil || errors.Is(err, ErrCancelled) {
		t.Fatalf("a warm whose read failed: %v, want a failure", err)
	}
	if strings.Contains(err.Error(), "Artist") {
		t.Errorf("the warm's failure names a path: %v", err)
	}
	lines := rec.Lines(warmFailedMessage)
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN ") || !strings.Contains(lines[0], "class=other") {
		t.Fatalf("the failure logged %q, want once, at Warn, with its class", lines)
	}
	if strings.Contains(lines[0], "Artist") || strings.Contains(lines[0], "disk I/O") {
		t.Errorf("the line carries the error's text: %s", lines[0])
	}
	first, _ := s.counts()
	ctx, cancel := context.WithTimeout(context.Background(), warmWatchdog)
	defer cancel()
	if err := r.Member(ctx, Reference(syntheticPath(n-1)).String()); err != nil {
		t.Fatalf("the lookup after a failed warm: %v, want the track found", err)
	}
	// The warm took n/2 rows; the store handed it the one it failed on too.
	total, _ := s.counts()
	if left := n - n/2; total-first > left+500 {
		t.Errorf("the lookup after the failed warm read %d rows, more than the %d left and one commit's 500", total-first, left)
	}
	if r.ids.builds != 1 {
		t.Errorf("%d builds, want one", r.ids.builds)
	}
}

// TestAWarmEndsWithItsContext pins Warm's context, the player's in serve. A
// warm whose context ends while it builds (here once it has read half of
// 25,000 tracks) stops there, answers ErrCancelled and logs at Info that it
// stopped, no failure; it keeps what it took whole and lets go of the turn,
// so the next lookup goes on from where it stopped and finds the track. A
// warm whose context ends while it waits for the turn, which a lookup holds,
// stops at once and reads nothing.
func TestAWarmEndsWithItsContext(t *testing.T) {
	const n = 25_000
	rec := loggingtest.Record(t)
	r, s := syntheticResolver(t, n)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parked := make(chan struct{})
	s.hold = func(i int) {
		if i == n/2 {
			close(parked)
			<-ctx.Done()
		}
	}
	warmed := make(chan error, 1)
	go func() { warmed <- r.Warm(ctx) }()
	await(t, parked, "the warm halfway through the library")
	cancel()
	if err := answer(t, warmed, "the warm"); !errors.Is(err, ErrCancelled) {
		t.Fatalf("a warm whose context ended: %v, want ErrCancelled", err)
	}
	if lines := rec.Lines(warmStoppedMessage); len(lines) != 1 || !strings.HasPrefix(lines[0], "INFO ") {
		t.Errorf("the stop logged %q, want once, at Info", lines)
	}
	if failures := rec.Failures(); len(failures) != 0 {
		t.Errorf("a stopped warm logged a failure: %q", failures)
	}
	s.hold = nil
	first, _ := s.counts()
	lctx, lcancel := context.WithTimeout(context.Background(), warmWatchdog)
	defer lcancel()
	if err := r.Member(lctx, Reference(syntheticPath(n-1)).String()); err != nil {
		t.Fatalf("the lookup after a stopped warm: %v, want the track found", err)
	}
	total, _ := s.counts()
	if total-first > n-first+500 || r.ids.builds != 1 {
		t.Errorf("the lookup after the stopped warm read %d rows (%d builds); want at most the %d left and one commit's 500, in the one build", total-first, r.ids.builds, n-first)
	}

	// A lookup holds the turn, held in its read of the library; a warm waits
	// for it and its context ends.
	r, s = syntheticResolver(t, 1_000)
	held, letGo := make(chan struct{}), make(chan struct{})
	s.hold = func(i int) {
		if i == 0 {
			close(held)
			<-letGo
		}
	}
	looked := make(chan error, 1)
	go func() { looked <- r.Member(context.Background(), Reference(syntheticPath(999)).String()) }()
	await(t, held, "the lookup's read of the library")
	live, stop := context.WithCancel(context.Background())
	defer stop()
	wctx := newAskedCtx(live)
	warmed = make(chan error, 1)
	go func() { warmed <- r.Warm(wctx) }()
	await(t, wctx.asked, "the warm to wait for the turn")
	stop()
	if err := answer(t, warmed, "the warm waiting for the turn"); !errors.Is(err, ErrCancelled) {
		t.Fatalf("a warm whose context ended as it waited for the turn: %v, want ErrCancelled", err)
	}
	close(letGo)
	if err := answer(t, looked, "the lookup"); err != nil {
		t.Fatalf("the lookup that held the turn: %v", err)
	}
	if handed, _ := s.counts(); handed != 1_000 {
		t.Errorf("the library handed over %d rows, want the lookup's 1,000 alone", handed)
	}
}

// TestAWarmThatPanicsIsRecovered pins a panic in Warm (here the id seam's,
// as the build hashes a path): it is recovered, Warm answers a failure that
// names no path, the line carries the stack and never the panic's value,
// and the turn is let go: the next lookup builds the index and finds the
// track.
func TestAWarmThatPanicsIsRecovered(t *testing.T) {
	rec := loggingtest.Record(t)
	r, _ := syntheticResolver(t, 1_000)
	secret := "a value a panic may carry: " + syntheticPath(7)
	var panics atomic.Bool
	panics.Store(true)
	idOf := r.idOf
	r.idOf = func(p string) string {
		if panics.Load() {
			panic(secret)
		}
		return idOf(p)
	}
	err := r.Warm(context.Background())
	if err == nil || errors.Is(err, ErrCancelled) || strings.Contains(err.Error(), secret) {
		t.Fatalf("a warm that panicked: %v, want a failure that names no path", err)
	}
	if lines := rec.Lines(warmPanicMessage); len(lines) != 1 || !strings.Contains(lines[0], "stack=") {
		t.Errorf("the panic logged %q, want once, with its stack", lines)
	}
	if all := strings.Join(rec.All(), "\n"); strings.Contains(all, secret) {
		t.Errorf("the log carries the panic's value: %s", all)
	}
	panics.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), warmWatchdog)
	defer cancel()
	if err := r.Member(ctx, Reference(syntheticPath(999)).String()); err != nil {
		t.Fatalf("the lookup after a warm that panicked: %v, want the track found", err)
	}
}

// benchTrack is the i-th track of BenchmarkTheIndexBuild's library, its row
// about the size of a real library's: a path of about 80 bytes (an artist,
// an album with its year, a numbered title) and the tags a scan stores.
func benchTrack(i int) *manifest.Track {
	artist, album, title := fmt.Sprintf("Artist Name %04d", i/120), fmt.Sprintf("Album Title %05d", i/12), fmt.Sprintf("Track Title Number %06d", i)
	year, number, disc, bits := 1970+i%50, i%12+1, 1, 24
	duration, rate := 245.5, 96000.0
	return &manifest.Track{
		Path: fmt.Sprintf("%s/%s (%d)/%02d - %s.flac", artist, album, year, number, title),
		Size: int64(40_000_000 + i), ModTime: time.Unix(1_700_000_000+int64(i), 0),
		Title: title, Artist: artist, AlbumArtist: artist, Album: album, Genre: "Jazz",
		TrackNumber: &number, DiscNumber: &disc, Year: &year, Duration: &duration, SampleRate: &rate, BitsPerSample: &bits,
		MusicBrainzTrackID: fmt.Sprintf("0f8fad5b-d9cb-469f-a165-%012d", i), MusicBrainzAlbumID: fmt.Sprintf("7c9e6679-7425-40de-944b-%012d", i/12),
	}
}

// benchStore is a manifest store of n of benchTrack's tracks, written as a
// scan writes them, 500 to a commit, then closed and opened again, so it is
// read as a box reads its store at rest, its write-ahead log checkpointed.
func benchStore(b *testing.B, n int) *manifest.Store {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bridge.db")
	store, err := manifest.OpenStore(path)
	if err != nil {
		b.Fatal(err)
	}
	batch := make([]*manifest.Track, 0, 500)
	for i := range n {
		batch = append(batch, benchTrack(i))
		if len(batch) == cap(batch) || i == n-1 {
			if err := store.UpsertTrackBatch(context.Background(), batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	if err := store.Close(); err != nil {
		b.Fatal(err)
	}
	if store, err = manifest.OpenStore(path); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

// BenchmarkTheIndexBuild measures Warm's build of the index of the paths by
// their ids from the first row, over the manifest store (SQLite) of a
// synthetic library of 25,000 and of 100,000 tracks: what the first Insert
// after the player starts would spend of its budget on the build without
// the warm (B470 PR 5a4). The store is read as the page cache holds it once
// written; a box's first build after a boot may read it from its disk. To
// judge a box, run it there:
//
//	go test -run '^$' -bench TheIndexBuild ./internal/player/library/
func BenchmarkTheIndexBuild(b *testing.B) {
	for _, n := range []int{25_000, 100_000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			store := benchStore(b, n)
			ctx := context.Background()
			log := slog.New(slog.DiscardHandler)
			for b.Loop() {
				r, err := New(Config{Store: store, Paths: fakePaths{}, Log: log})
				if err != nil {
					b.Fatal(err)
				}
				if err := r.Warm(ctx); err != nil {
					b.Fatal(err)
				}
				if r.ids.paths != n {
					b.Fatalf("the index holds %d paths, want %d", r.ids.paths, n)
				}
			}
		})
	}
}
