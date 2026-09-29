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

// TestAStaleDownloadRescansItsSourceSoTheSweepRendersItAgain: the case the
// rescan on a stale download exists for. The auto-optimize sweep
// pre-generated a rendition, then the file was retagged. The phone asks
// for that rendition on every play, gets 410 variant_stale, plays the
// source, and never asks for a new one: the manifest lists the family.
// Measured on main at 6dfba62c: five downloads after the retag each
// answered 410, the row still recorded the old mtime three seconds later,
// and a sweep then rendered nothing (the file had changed since its scan),
// so the rendition answered 410 until the periodic scan, six hours by
// default.
//
// Now the first stale download asks for a rescan of the file's directory,
// the rescan writes the row and nudges the sweep, and the sweep renders
// the rendition again from the version the scan read. Once the row is
// current a stale download asks for nothing more, since a rescan could not
// change it. A download that answers 200 asks for nothing either.
func TestAStaleDownloadRescansItsSourceSoTheSweepRendersItAgain(t *testing.T) {
	b := newEmptyStampBridge(t)
	abs, scanner := b.mintScannedDSF(t, stampDSD)
	wrote := b.startRescans(t, scanner)

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
	if code := b.download(t, stampDSD, stampDSDCompact); code != http.StatusGone {
		t.Fatalf("GET after the retag = %d, want 410", code)
	}
	b.awaitRowAt(t, stampDSD, later, "a download found the rendition stale")
	select {
	case <-wrote:
	case <-time.After(10 * time.Second):
		t.Fatal("the rescan that wrote the row did not signal: runServe nudges the auto-optimize sweep there")
	}

	if code := b.download(t, stampDSD, stampDSDCompact); code != http.StatusGone {
		t.Fatalf("GET before the sweep = %d, want 410: the rendition is still the old version's", code)
	}
	if n := b.rescansWaiting(); n != 0 {
		t.Errorf("%d rescans queued by a stale download whose row is current, want none: a rescan changes nothing", n)
	}

	before := b.queue.count()
	b.sweep(t)
	if got := b.queue.since(before); len(got) != 1 {
		t.Fatalf("the sweep after the rescan rendered %v, want the compact tier again", got)
	}
	if code := b.download(t, stampDSD, stampDSDCompact); code != http.StatusOK {
		t.Errorf("GET after the sweep = %d, want 200", code)
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
	wrote := b.startRescans(t, scanner)

	later := time.Now().Add(time.Minute).Truncate(time.Second)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	if n := b.request(t, folded, "pcm"); n != 0 {
		t.Fatalf("the request for the changed file queued %d jobs, want the refusal", n)
	}
	select {
	case <-wrote:
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
	r.request("")
	if len(resolved) != 0 || len(r.waiting) != 0 {
		t.Errorf("an empty path resolved %q and queued %d scans, want neither: it names no file, "+
			"and its directory resolves to the whole root", resolved, len(r.waiting))
	}
	r.request("01.flac")
	if len(r.waiting) != 1 || r.waiting[0].abs != filepath.FromSlash("/lib") {
		t.Errorf("a file at the root queued %+v, want one scan of the root", r.waiting)
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
	h := newStaleRenditionRescan(lookup, func(rel string) { asked = append(asked, rel) })
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
		h.observe(context.Background(), clientPath, info)
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

	// A full table of directories asked for within the minute drops a new
	// one; past the minute the oldest are forgotten and it is asked for.
	h.asked = map[string]time.Time{}
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
