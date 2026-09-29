package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestAStaleDownloadRescansItsSourceAndRendersItAgain: the case the rescan
// on a stale download exists for. The auto-optimize sweep pre-generated a
// rendition, then the file was retagged. A phone that keeps the rendition
// listed asks for it on every play, gets 410 variant_stale, plays the
// source, and asks for no new one. Measured on main at 6dfba62c: five
// downloads after the retag each answered 410, the row still recorded the
// old mtime three seconds later, and a sweep then rendered nothing (the file
// had changed since its scan), so the rendition answered 410 until the
// periodic scan, six hours by default.
//
// Since B53 the first stale download asks for a rescan of the file's
// directory, and since B82 the render it wanted is queued once that rescan
// has read the change, so the next download is served whether or not the
// sweep runs. The sweep, nudged by the same rescan, then finds the
// rendition fresh and renders nothing more. Once the row is current a stale
// download asks for no rescan, since a rescan could not change it, and a
// download that answers 200 asks for nothing at all.
//
// Named …StaleDownloadRescansItsSourceSoTheSweepRendersItAgain until B82:
// the render the sweep made is the download's own now.
func TestAStaleDownloadRescansItsSourceAndRendersItAgain(t *testing.T) {
	b := newEmptyStampBridge(t)
	abs, scanner := b.mintScannedDSF(t, stampDSD)
	rescanned := b.startRescans(t, scanner)

	b.sweep(t)
	if got := b.queue.since(0); len(got) != 1 {
		t.Fatalf("the first sweep rendered %v, want the DSF's compact tier", got)
	}
	if code := b.download(t, stampDSD, stampDSDCompact); code != http.StatusOK || b.rescansWaiting() != 0 {
		t.Fatalf("before the retag: GET %d with %d rescans waiting, want 200 and none", code, b.rescansWaiting())
	}

	later := time.Now().Add(time.Minute).Truncate(time.Second)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	before := b.queue.count()
	if code := b.download(t, stampDSD, stampDSDCompact); code != http.StatusGone {
		t.Fatalf("GET after the retag = %d, want 410", code)
	}
	b.awaitRowAt(t, stampDSD, later, "a download found the rendition stale")
	select {
	case <-rescanned:
	case <-time.After(10 * time.Second):
		t.Fatal("the rescan did not signal: runServe queues the waiting renders and nudges the auto-optimize sweep there")
	}
	if got := b.queue.since(before); len(got) != 1 {
		t.Fatalf("the rescan's step rendered %v, want the compact tier the stale download asked for", got)
	}
	if code := b.download(t, stampDSD, stampDSDCompact); code != http.StatusOK {
		t.Fatalf("GET after the rescan = %d, want 200: the stale download's render is stamped with the row the rescan wrote", code)
	}

	settled := b.queue.count()
	b.sweep(t)
	if got := b.queue.since(settled); len(got) != 0 {
		t.Errorf("the sweep after the rescan rendered %v, want nothing: the rendition is fresh again", got)
	}
	for range 3 {
		if code := b.download(t, stampDSD, stampDSDCompact); code != http.StatusOK {
			t.Errorf("GET = %d, want 200", code)
		}
	}
	if n := b.rescansWaiting(); n != 0 {
		t.Errorf("%d rescans queued after the rendition was served again, want none", n)
	}
}

// TestARescanIndexesNoSecondSpellingOfTheDirectory: the rescanner scans the
// directory the file's ROW names, resolved through the bridge's resolver,
// never the spelling a request arrived with. The scanner makes each row's
// path from the spelling of the directory it is handed, and a
// case-insensitive filesystem opens a case variant all the same, so on main
// at 6dfba62c a POST /v1/upscale naming a changed file in lower case left
// the rows [Fixture/DSD/01.dsf fixture/dsd/01.dsf]: the album a second
// time, in every paired device's manifest. (Today's app sends the
// manifest's own spelling; a script or any other client need not.) Skipped
// where the filesystem is case-sensitive, since the variant then names no
// file and the request is refused before anything is queued.
func TestARescanIndexesNoSecondSpellingOfTheDirectory(t *testing.T) {
	b := newEmptyStampBridge(t)
	abs, scanner := b.mintScannedDSF(t, stampDSD)
	folded := strings.ToLower(stampDSD)
	if _, err := os.Stat(filepath.Join(b.libDir, filepath.FromSlash(folded))); err != nil {
		t.Skipf("this filesystem is case-sensitive: %s does not open %s", folded, stampDSD)
	}
	rescanned := b.startRescans(t, scanner)

	later := time.Now().Add(time.Minute).Truncate(time.Second)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	if n := b.request(t, folded, "pcm"); n != 0 {
		t.Fatalf("the request for the changed file queued %d jobs, want the refusal", n)
	}
	select {
	case <-rescanned:
	case <-time.After(10 * time.Second):
		t.Fatal("no rescan wrote a row within 10 s of the refused request")
	}
	tracks, err := b.store.ListTracks(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, tr := range tracks {
		paths = append(paths, tr.Path)
		if tr.Path == stampDSD && tr.ModTime.UnixNano() != later.UnixNano() {
			t.Errorf("the row records %v, want the version the rescan read, %v", tr.ModTime, later)
		}
	}
	if !slices.Equal(paths, []string{stampDSD}) {
		t.Errorf("rows after the rescan: %v, want only %s: the rescan indexed the directory under the request's spelling", paths, stampDSD)
	}
}

// TestSourceRescannerRefusesAnEmptyPath: every rescan request funnels
// through sourceRescanner.request, and an empty path is refused there
// before anything resolves it. path.Dir("") is ".", which the resolver maps
// onto the library root, so it would have queued a walk of the whole root.
// The positive control is a file at the root, whose directory IS the root:
// that one is queued.
func TestSourceRescannerRefusesAnEmptyPath(t *testing.T) {
	var resolved []string
	r := newSourceRescanner(func(rel string) (string, error) {
		resolved = append(resolved, rel)
		return filepath.Join(filepath.FromSlash("/lib"), filepath.FromSlash(rel)), nil
	})
	if r.queue("") || len(resolved) != 0 || len(r.waiting) != 0 {
		t.Errorf("an empty path resolved %q and queued %d scans, want neither: it names no file, "+
			"and its directory resolves to the whole root", resolved, len(r.waiting))
	}
	if !r.queue("01.flac") || len(r.waiting) != 1 || r.waiting[0].abs != filepath.FromSlash("/lib") {
		t.Errorf("a file at the root queued %+v, want one scan of the root", r.waiting)
	}
	if !r.queue("02.flac") || len(r.waiting) != 1 {
		t.Errorf("a second file of a waiting directory: %d queued, want it reported as queued and folded into the one scan", len(r.waiting))
	}
}

// TestARescanDropsTheAlbumIndexBeforeItNudgesTheSweep: a rescan may have
// read a retag that moved a DSD track to another album, and the album-gain
// index is dropped only when a FULL scan lands, so it can be up to its
// two-minute TTL old. The renders a rescan's step queues (the ones stale
// downloads waited for) and the sweep it nudges render straight away, so
// the index goes first: a render queued ahead of it would record the gain of
// the track's old album-mates (CodeRabbit on #1093). The waiting renders go
// next, with the directory the rescan read, onto the foreground lane ahead
// of the sweep's background one (backlog B82), and the nudge last. Without
// an album gain or a stale-download hook wired the nudge still goes, and a
// nudge already pending does not block the rescanner.
func TestARescanDropsTheAlbumIndexBeforeItNudgesTheSweep(t *testing.T) {
	nudge := make(chan struct{}, 1)
	var order []string
	after := afterRescan(func() {
		if len(nudge) != 0 {
			order = append(order, "invalidated after the nudge")
			return
		}
		order = append(order, "invalidated")
	}, func(_ context.Context, dir string) {
		switch {
		case len(nudge) != 0:
			order = append(order, "rendered after the nudge")
		case len(order) == 0:
			order = append(order, "rendered before the index was dropped")
		default:
			order = append(order, "rendered "+dir)
		}
	}, nudge)
	after(context.Background(), "Album")
	if want := []string{"invalidated", "rendered Album"}; !slices.Equal(order, want) || len(nudge) != 1 {
		t.Fatalf("after a rescan: %v, %d nudges pending, want %v and then one nudge", order, len(nudge), want)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		after(context.Background(), "Album")
		afterRescan(nil, nil, nudge)(context.Background(), "Album")
	}()
	// The goroutine takes no context: the cancel is a formality the drain
	// asks for, and the drain waits for it to return.
	_, cancel := context.WithCancel(context.Background())
	drainLoopOnCleanup(t, cancel, done, "the after steps sent to a full nudge")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a nudge already pending blocked the rescanner's loop")
	}

	<-nudge
	afterRescan(nil, nil, nudge)(context.Background(), "Album")
	if len(nudge) != 1 {
		t.Errorf("with no album gain and no stale-download hook wired: %d nudges pending, want 1", len(nudge))
	}
}

// TestEveryRescanRunsItsAfterStepHoweverFewRowsItWrote: ScanSubtree counts
// the rows it committed, and a rescan whose file was deleted in the seconds
// before it ran deletes that row and counts none. That still changes an
// album's membership, so the step after the rescan (the album-gain index
// dropped, the waiting renders queued, the sweep nudged) runs for it too,
// with the library-relative directory the rescan read; until review round 4
// it ran only when the count was above zero (CodeRabbit on #1093). A rescan
// the shutdown interrupted runs nothing after it.
func TestEveryRescanRunsItsAfterStepHoweverFewRowsItWrote(t *testing.T) {
	r := newSourceRescanner(underRoot(filepath.FromSlash("/lib")))
	r.request("Deleted/01.dsf")
	after := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx, func(context.Context, string) (int, error) { return 0, nil },
			func(_ context.Context, dir string) { after <- dir })
	}()
	drainLoopOnCleanup(t, cancel, done, "the source rescanner")
	select {
	case dir := <-after:
		if dir != "Deleted" {
			t.Errorf("the after step was told of %q, want the directory the rescan read, library-relative: Deleted", dir)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a rescan that committed no row ran no after step: a deletion-only rescan left the album-gain index as it was")
	}

	stopped, stop := context.WithCancel(context.Background())
	defer stop()
	r2 := newSourceRescanner(underRoot(filepath.FromSlash("/lib")))
	r2.request("Album/01.dsf")
	var ran int
	r2.run(stopped, func(context.Context, string) (int, error) { stop(); return 1, nil },
		func(context.Context, string) { ran++ })
	if ran != 0 {
		t.Errorf("a rescan the shutdown interrupted ran its after step %d times, want none", ran)
	}
}

// fileStat is the os.FileInfo a download's freshness check hands the stale
// rendition hook: only size and mtime are read.
type fileStat struct {
	size  int64
	mtime time.Time
}

func (f fileStat) Name() string       { return "01.flac" }
func (f fileStat) Size() int64        { return f.size }
func (f fileStat) Mode() os.FileMode  { return 0o644 }
func (f fileStat) ModTime() time.Time { return f.mtime }
func (f fileStat) IsDir() bool        { return false }
func (f fileStat) Sys() any           { return nil }

// TestAStaleDownloadAsksForARescanOnlyWhileItsRowIsBehindAndOncePerMinute:
// the phone asks for a stale rendition on every play, every paired device
// does, and a player issues range requests, so the download path must not
// turn each GET into a rescan. It asks only while the row is behind the file
// (once a scan has read the change, a rescan changes nothing), at most once
// per directory per minute (a directory whose rescan does not bring its row
// level, a file still being written or one the scan cannot read, would
// otherwise keep the rescanner on it for as long as GETs arrive), naming the
// file by its row's path whatever spelling the GET used, and for at most
// sourceRescanQueueCap directories at once.
func TestAStaleDownloadAsksForARescanOnlyWhileItsRowIsBehindAndOncePerMinute(t *testing.T) {
	rowTime := time.Unix(1_700_000_000, 0)
	rows := map[string]*manifest.Track{}
	lookup := func(_ context.Context, rel string) (*manifest.Track, error) {
		if rel == "Broken/01.flac" {
			return nil, errors.New("database is locked")
		}
		return rows[rel], nil
	}
	var asked []string
	queueFull := false
	// No re-render wired: this test is about the rescans alone.
	h := newStaleRenditionHeal(lookup, func(rel string) bool {
		asked = append(asked, rel)
		return !queueFull
	}, nil, nil)
	now := time.Unix(1_800_000_000, 0)
	h.now = func() time.Time { return now }
	row := func(rel string) *manifest.Track {
		tr := &manifest.Track{Path: rel, Size: 4096, ModTime: rowTime}
		rows[rel] = tr
		return tr
	}
	row("Album/01.flac")
	rows["album/01.flac"] = rows["Album/01.flac"] // the store's case-folded lookup
	row("Album/02.flac")
	row("Other/01.flac")
	current := fileStat{size: 4096, mtime: rowTime}
	behind := fileStat{size: 4104, mtime: rowTime.Add(time.Minute)}
	observe := func(clientPath string, info os.FileInfo) []string {
		t.Helper()
		asked = nil
		h.observe(context.Background(), clientPath, "optimized-v2-48000-16", info)
		return asked
	}
	for _, step := range []struct {
		what       string
		clientPath string
		info       os.FileInfo
		advance    time.Duration
		want       []string
	}{
		{"the row is current", "Album/01.flac", current, 0, nil},
		{"no row", "Gone/01.flac", behind, 0, nil},
		{"a failed lookup", "Broken/01.flac", behind, 0, nil},
		{"the row is behind, asked by another spelling", "album/01.flac", behind, 0, []string{"Album/01.flac"}},
		{"again within the minute", "Album/01.flac", behind, 30 * time.Second, nil},
		{"another file of the directory within the minute", "Album/02.flac", behind, 0, nil},
		{"another directory", "Other/01.flac", behind, 0, []string{"Other/01.flac"}},
		{"a minute after the first", "Album/02.flac", behind, 30 * time.Second, []string{"Album/02.flac"}},
	} {
		now = now.Add(step.advance)
		if got := observe(step.clientPath, step.info); !slices.Equal(got, step.want) {
			t.Errorf("%s: asked for %v, want %v", step.what, got, step.want)
		}
	}

	// A request the rescanner dropped (its queue full) spends no minute:
	// the next download asks again, and only a queued one is debounced.
	row("Full/01.flac")
	queueFull = true
	for _, what := range []string{"the queue is full", "again while it is still full"} {
		if got := observe("Full/01.flac", behind); !slices.Equal(got, []string{"Full/01.flac"}) {
			t.Errorf("%s: asked for %v, want the directory: nothing was queued, so nothing is debounced", what, got)
		}
	}
	queueFull = false
	if got := observe("Full/01.flac", behind); !slices.Equal(got, []string{"Full/01.flac"}) {
		t.Errorf("once there is room: asked for %v, want the directory", got)
	}
	if got := observe("Full/01.flac", behind); got != nil {
		t.Errorf("after a queued request, within the minute: asked for %v, want nothing", got)
	}

	// A full table of directories asked for within the minute drops a new
	// one; past the minute the oldest are forgotten and it is asked for.
	h.asked = recentKeys{}
	for i := 0; i < sourceRescanQueueCap; i++ {
		row(fmt.Sprintf("D%04d/01.flac", i))
		if got := observe(fmt.Sprintf("D%04d/01.flac", i), behind); len(got) != 1 {
			t.Fatalf("directory %d of %d: asked for %v, want it", i, sourceRescanQueueCap, got)
		}
	}
	row("Late/01.flac")
	if got := observe("Late/01.flac", behind); got != nil {
		t.Errorf("a directory past %d asked for within the minute: asked for %v, want it dropped", sourceRescanQueueCap, got)
	}
	now = now.Add(staleRenditionRescanEvery)
	if got := observe("Late/01.flac", behind); !slices.Equal(got, []string{"Late/01.flac"}) {
		t.Errorf("past the minute: asked for %v, want the directory", got)
	}
	if len(h.asked) > sourceRescanQueueCap {
		t.Errorf("%d directories remembered, want at most %d", len(h.asked), sourceRescanQueueCap)
	}
}
