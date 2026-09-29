package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestAStaleRenditionIsRenderedAgainWhenADownloadFindsItsRowCurrent: the
// case backlog B82 was filed for. A client asked for two renditions (the
// PCM file's CarPlay tier, the DSD file's faithful tier), both files were
// retagged, and a scan read the change, so each rendition is stale against
// its row as well as its file. With auto-optimize off (the default) nothing
// rendered them again on main at 6bc4605a: the downloads answered 410
// variant_stale and rendered nothing ("the stale downloads rendered []"),
// and a batch counts the tracks covered.
//
// The download that finds such a rendition stale queues a render of it
// through the on-demand path, stamped with the row the scan wrote, so the
// next download is served. Then nothing more happens: a served rendition
// is never stale, so no later download renders anything.
func TestAStaleRenditionIsRenderedAgainWhenADownloadFindsItsRowCurrent(t *testing.T) {
	b := newStampBridge(t)
	if n := b.request(t, stampPCM, "optimize"); n != 1 {
		t.Fatalf("the CarPlay request queued %d jobs, want 1", n)
	}
	if n := b.request(t, stampDSD, "pcm"); n != 1 {
		t.Fatalf("the faithful request queued %d jobs, want 1", n)
	}
	renditions := b.queue.since(0)
	sort.Strings(renditions)
	b.wantServed(t, "after the requests", renditions)
	if lanes := b.queue.lanesSince(0); !slices.Equal(lanes, []bool{false, false}) {
		t.Fatalf("the client's requests went to the lanes %v (true: background), want the foreground one", lanes)
	}

	b.change(t, stampPCM)
	b.change(t, stampDSD)
	b.scanReads(t, stampPCM, nil)
	b.scanReads(t, stampDSD, nil)

	stale := []struct{ rel, variant string }{{stampPCM, stampPCMCompact}, {stampDSD, stampDSDFaithful}}
	before := b.queue.count()
	for _, r := range stale {
		if code := b.download(t, r.rel, r.variant); code != http.StatusGone {
			t.Fatalf("GET %s %s after the scan read its retag = %d, want 410 variant_stale", r.rel, r.variant, code)
		}
	}
	got := b.queue.since(before)
	sort.Strings(got)
	if !slices.Equal(got, renditions) {
		t.Fatalf("the stale downloads rendered %v, want each stale rendition again: %v\n"+
			"with auto-optimize off nothing else renders them, and the download answers 410 for ever", got, renditions)
	}
	// Nobody waits on these renders (each download has played the source),
	// so they take the background lane the sweep's take, never the one a
	// CarPlay plug-in waits in: a library retagged at once would queue its
	// renditions ahead of it (CodeRabbit on #1097).
	if lanes := b.queue.lanesSince(before); !slices.Equal(lanes, []bool{true, true}) {
		t.Errorf("the stale downloads' renders went to the lanes %v (true: background), want the background one", lanes)
	}
	b.wantServed(t, "after the stale downloads", renditions)

	settled := b.queue.count()
	for range 3 {
		for _, r := range stale {
			if code := b.download(t, r.rel, r.variant); code != http.StatusOK {
				t.Errorf("GET %s %s once rendered again = %d, want 200", r.rel, r.variant, code)
			}
		}
	}
	if extra := b.queue.since(settled); len(extra) != 0 {
		t.Errorf("served downloads rendered %v more, want nothing: a rendition stamped with its row is never stale", extra)
	}
}

// TestAStaleDownloadWhoseRowIsBehindRendersAgainAfterItsRescan: the same
// download before any scan read the retag. B53 made it ask for a rescan of
// the file's directory; the rescan writes the row, and the render that
// download wanted is queued then, stamped with the version the rescan read,
// rather than on the next play. On main at 6bc4605a the rescan ran and
// nothing rendered the rendition in the 10 s after it.
func TestAStaleDownloadWhoseRowIsBehindRendersAgainAfterItsRescan(t *testing.T) {
	b := newEmptyStampBridge(t)
	abs, scanner := b.mintScannedDSF(t, stampDSD)
	rescanned := b.startRescans(t, scanner)
	if n := b.request(t, stampDSD, "pcm"); n != 1 {
		t.Fatalf("the faithful request queued %d jobs, want 1", n)
	}
	b.wantServed(t, "after the request", b.queue.since(0))

	later := time.Now().Add(time.Minute).Truncate(time.Second)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	before := b.queue.count()
	if code := b.download(t, stampDSD, stampDSDFaithful); code != http.StatusGone {
		t.Fatalf("GET after the retag = %d, want 410", code)
	}
	select {
	case <-rescanned:
	case <-time.After(10 * time.Second):
		t.Fatal("the stale download's rescan did not run within 10 s")
	}
	deadline := time.Now().Add(10 * time.Second)
	for b.queue.count() == before {
		if time.Now().After(deadline) {
			t.Fatalf("nothing rendered the stale rendition within 10 s of the rescan that read its retag")
		}
		time.Sleep(10 * time.Millisecond)
	}
	b.wantServed(t, "after the rescan", b.queue.since(before))
	row, err := b.store.GetVariant(t.Context(), stampDSD, stampDSDFaithful)
	if err != nil || row == nil {
		t.Fatalf("GetVariant: %v, %v", row, err)
	}
	if row.SourceMTimeNS != later.UnixNano() {
		t.Errorf("the rendition records mtime %d, want the version the rescan read, %d", row.SourceMTimeNS, later.UnixNano())
	}
}

// TestAStaleDownloadRendersEveryNewVersionOfItsFileAgain: the minute a
// render waits before it is asked for again bounds the renders of ONE
// version of the file, the failed ones. A file retagged again is a new
// version, and its render is asked for at once. Measured on a real bridge
// with sox (ops/engineering-log.md, 2026-09-29): a retag, a scan and a
// download rendered the CarPlay tier again, then a second retag 30 s later
// answered 410 on 15 downloads over 30 s, because the render its rescan
// queued, and every download after it, was refused as asked for within the
// minute.
func TestAStaleDownloadRendersEveryNewVersionOfItsFileAgain(t *testing.T) {
	b := newEmptyStampBridge(t)
	abs, scanner := b.mintScannedDSF(t, stampDSD)
	rescanned := b.startRescans(t, scanner)
	if n := b.request(t, stampDSD, "pcm"); n != 1 {
		t.Fatalf("the faithful request queued %d jobs, want 1", n)
	}
	for i, what := range []string{"the first retag", "a second retag within the minute"} {
		later := time.Now().Add(time.Duration(i+1) * time.Minute).Truncate(time.Second)
		if err := os.Chtimes(abs, later, later); err != nil {
			t.Fatal(err)
		}
		before := b.queue.count()
		if code := b.download(t, stampDSD, stampDSDFaithful); code != http.StatusGone {
			t.Fatalf("%s: GET = %d, want 410", what, code)
		}
		select {
		case <-rescanned:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: the stale download's rescan did not run within 10 s", what)
		}
		if got := b.queue.since(before); len(got) != 1 {
			t.Fatalf("%s: the rescan's step rendered %v, want the faithful tier again", what, got)
		}
		if code := b.download(t, stampDSD, stampDSDFaithful); code != http.StatusOK {
			t.Errorf("%s: GET after the rescan = %d, want 200", what, code)
		}
	}
}

// TestAStaleDownloadRendersNothingForAKindThatIsSwitchedOff: a stale
// download renders a rendition again under the live gate of its kind, the
// one POST /v1/upscale reads, so it never renders what a client's request
// for the family would be refused. With the CarPlay kind switched off, the
// PCM file's stale compact tier renders nothing while the DSD file's
// faithful tier, whose gate is open, renders; switched back on, the next
// download renders it (a refusal on a gate spends no minute).
func TestAStaleDownloadRendersNothingForAKindThatIsSwitchedOff(t *testing.T) {
	b := newStampBridge(t)
	b.request(t, stampPCM, "optimize")
	b.request(t, stampDSD, "pcm")
	b.change(t, stampPCM)
	b.change(t, stampDSD)
	b.scanReads(t, stampPCM, nil)
	b.scanReads(t, stampDSD, nil)

	b.closed[transcode.JobKindOptimize].Store(true)
	before := b.queue.count()
	b.download(t, stampPCM, stampPCMCompact)
	b.download(t, stampDSD, stampDSDFaithful)
	want := []string{stampDSD + " " + stampDSDFaithful}
	if got := b.queue.since(before); !slices.Equal(got, want) {
		t.Fatalf("with the CarPlay kind off, the stale downloads rendered %v, want only the faithful tier %v", got, want)
	}

	b.closed[transcode.JobKindOptimize].Store(false)
	before = b.queue.count()
	if code := b.download(t, stampPCM, stampPCMCompact); code != http.StatusGone {
		t.Fatalf("GET of the compact tier = %d, want 410: nothing rendered it while its kind was off", code)
	}
	want = []string{stampPCM + " " + stampPCMCompact}
	if got := b.queue.since(before); !slices.Equal(got, want) {
		t.Errorf("once the CarPlay kind is back on, the next stale download rendered %v, want %v", got, want)
	}
}

// TestAStaleDownloadRendersNothingForAFileWhoseRendersKeepFailing: a render
// that fails writes no row, so the rendition stays stale and every play of
// the track would render it again. Once the failures suppress the file (the
// transcode-failure suppression every automatic path honours), a stale
// download renders nothing.
func TestAStaleDownloadRendersNothingForAFileWhoseRendersKeepFailing(t *testing.T) {
	b := newStampBridge(t)
	b.request(t, stampPCM, "optimize")
	b.change(t, stampPCM)
	b.scanReads(t, stampPCM, nil)
	tr, err := b.store.LookupTrack(t.Context(), stampPCM)
	if err != nil || tr == nil {
		t.Fatalf("LookupTrack: %v, %v", tr, err)
	}
	for range 3 {
		if err := b.store.RecordVariantFailure(t.Context(), stampPCM, tr.Size, tr.ModTime.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	before := b.queue.count()
	if code := b.download(t, stampPCM, stampPCMCompact); code != http.StatusGone {
		t.Fatalf("GET = %d, want 410", code)
	}
	if got := b.queue.since(before); len(got) != 0 {
		t.Errorf("a stale download of a suppressed file rendered %v, want nothing", got)
	}
}

// TestAStaleRenditionOfAnOlderSchemaIsRenderedAsTheCurrentOne: the render a
// stale download asks for is the one a client's request for the family
// would make, the family's CURRENT id for the source. A DSD compact tier
// minted under the `v1` schema cannot be rendered again as itself: the stale
// download renders the `v2` one, which the app, taking the newest of a
// family, plays. The `v1` row stays (nothing reaps a rendition whose file
// exists), stale for ever, and its later downloads render nothing: the
// family is fresh.
func TestAStaleRenditionOfAnOlderSchemaIsRenderedAsTheCurrentOne(t *testing.T) {
	b := newStampBridge(t)
	const v1 = "optimized-dsd-v1-44100-16"
	body := []byte("a v1 rendition")
	sidecar := filepath.Join(filepath.Dir(b.libDir), "variants", "old", "01."+v1+".flac")
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, body, 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := b.store.LookupTrack(t.Context(), stampDSD)
	if err != nil || old == nil {
		t.Fatalf("LookupTrack: %v, %v", old, err)
	}
	if err := b.store.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: stampDSD, VariantID: v1, SidecarPath: sidecar, Format: "flac",
		SampleRate: 44100, BitsPerSample: 16, SizeBytes: int64(len(body)),
		SourceMTimeNS: old.ModTime.UnixNano(), SourceSize: old.Size, CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if code := b.download(t, stampDSD, v1); code != http.StatusOK {
		t.Fatalf("GET of the v1 rendition before the retag = %d, want 200", code)
	}

	b.change(t, stampDSD)
	b.scanReads(t, stampDSD, nil)
	clock := time.Now()
	b.heal.now = func() time.Time { return clock }
	before := b.queue.count()
	if code := b.download(t, stampDSD, v1); code != http.StatusGone {
		t.Fatalf("GET of the v1 rendition after the scan read the retag = %d, want 410", code)
	}
	want := []string{stampDSD + " " + stampDSDCompact}
	if got := b.queue.since(before); !slices.Equal(got, want) {
		t.Fatalf("the stale v1 download rendered %v, want the family's current id %v", got, want)
	}
	b.wantServed(t, "after the render", want)

	for range 3 {
		clock = clock.Add(2 * staleRenditionRescanEvery)
		if code := b.download(t, stampDSD, v1); code != http.StatusGone {
			t.Errorf("GET of the v1 rendition = %d, want 410: it is still the old version's", code)
		}
	}
	if got := b.queue.since(before + 1); len(got) != 0 {
		t.Errorf("later downloads of the stale v1 rendition rendered %v, want nothing: the family is fresh", got)
	}
}

// TestRenditionKindOfNamesTheKindThatRendersEachFamily: the three families
// by their prefix, the DSD compact tier sharing the CarPlay kind's, and
// nothing for an id that names no family this bridge renders.
func TestRenditionKindOfNamesTheKindThatRendersEachFamily(t *testing.T) {
	for _, c := range []struct {
		id   string
		kind transcode.JobKind
		ok   bool
	}{
		{"optimized-v2-48000-16", transcode.JobKindOptimize, true},
		{"optimized-v1-44100-16", transcode.JobKindOptimize, true},
		{"optimized-dsd-v2-44100-16", transcode.JobKindOptimize, true},
		{"pcm-v2-176400-24", transcode.JobKindPCMRender, true},
		{"pcm-v1-192000-24", transcode.JobKindPCMRender, true},
		{"upscaled-v2-192000-24", transcode.JobKindUpscale, true},
		{"upscaled-v1-176400-24", transcode.JobKindUpscale, true},
		{"", "", false},
		{"optimized", "", false},
		{"pcmv2-176400-24", "", false},
		{"dsd-v2-44100-16", "", false},
	} {
		kind, ok := renditionKindOf(c.id)
		if kind != c.kind || ok != c.ok {
			t.Errorf("renditionKindOf(%q) = (%q, %v), want (%q, %v)", c.id, kind, ok, c.kind, c.ok)
		}
	}
}

// TestAStaleRerenderGoesThroughTheKindsGateAndTheSuppression drives
// staleRerender.rerender alone: the kind read off the id, its live gate,
// the demo refusal, the suppression, and only then the enqueue, with the
// kind and the row's path.
func TestAStaleRerenderGoesThroughTheKindsGateAndTheSuppression(t *testing.T) {
	var enqueued []string
	open := map[transcode.JobKind]bool{transcode.JobKindOptimize: true, transcode.JobKindPCMRender: true, transcode.JobKindUpscale: true}
	suppressed, suppressedErr := false, error(nil)
	r := staleRerender{
		enqueue: func(kind transcode.JobKind, rel string) error {
			enqueued = append(enqueued, string(kind)+" "+rel)
			return nil
		},
		open:       func(kind transcode.JobKind) bool { return open[kind] },
		suppressed: func(context.Context, string) (bool, error) { return suppressed, suppressedErr },
	}
	try := func(what, id string, wantErr error, want []string) {
		t.Helper()
		enqueued = nil
		err := r.rerender(t.Context(), "A/01.flac", id)
		if (wantErr == nil && err != nil) || (wantErr != nil && !errors.Is(err, wantErr)) {
			t.Errorf("%s: err = %v, want %v", what, err, wantErr)
		}
		if !slices.Equal(enqueued, want) {
			t.Errorf("%s: enqueued %v, want %v", what, enqueued, want)
		}
	}
	try("the CarPlay tier", "optimized-v2-48000-16", nil, []string{"optimize A/01.flac"})
	try("the DSD compact tier", "optimized-dsd-v2-44100-16", nil, []string{"optimize A/01.flac"})
	try("the faithful tier", "pcm-v2-176400-24", nil, []string{"pcm A/01.flac"})
	try("an upscale", "upscaled-v2-192000-24", nil, []string{"upscale A/01.flac"})
	try("an id of no family", "bogus-v2-1-1", errRerenderInactive, nil)

	open[transcode.JobKindPCMRender] = false
	try("the faithful tier, its kind off", "pcm-v2-176400-24", errRerenderInactive, nil)
	try("the CarPlay tier, its kind on", "optimized-v2-48000-16", nil, []string{"optimize A/01.flac"})
	open[transcode.JobKindPCMRender] = true

	suppressed = true
	try("a suppressed file", "optimized-v2-48000-16", errRerenderSuppressed, nil)
	suppressed, suppressedErr = false, errors.New("database is locked")
	enqueued = nil
	if err := r.rerender(t.Context(), "A/01.flac", "optimized-v2-48000-16"); err == nil || len(enqueued) != 0 {
		t.Errorf("a failed suppression read: err %v, enqueued %v, want the error and nothing enqueued", err, enqueued)
	}
	suppressedErr = nil

	r.demo = true
	try("a demo bridge", "optimized-v2-48000-16", errRerenderInactive, nil)
	r.demo = false
	r.open = nil
	try("no gates wired", "optimized-v2-48000-16", errRerenderInactive, nil)
}

// TestAStaleDownloadAsksForARenderOncePerMinuteAndWaitsForItsRescan drives
// staleRenditionHeal alone, over a fake clock. Once the row is current a
// stale download asks for the render at once, and again for the same version
// of the file only after a minute (a failed render is tried once a minute,
// not on every GET), unless nothing was tried: the pool's queue full, or the
// kind switched off. A new version of the file is asked for at once. While
// the row is behind, the render waits for a rescan of its directory that
// brings the row level, through rescans that do not (another directory's
// rescan takes nothing), for at most staleRenditionWaitMax. A rescan that
// left a waiting file behind keeps its directory's minute; one that brought
// every waiting file level frees it. A file that changed again between the
// check here and the enqueue's (errSourceAheadOfRow) waits for a rescan, and
// is waiting before the heal asks for it (CodeRabbit on #1097: the enqueue's
// own request came first, and a quick rescan could have gone by).
func TestAStaleDownloadAsksForARenderOncePerMinuteAndWaitsForItsRescan(t *testing.T) {
	rowTime := time.Unix(1_700_000_000, 0)
	rows := map[string]*manifest.Track{}
	row := func(rel string, mtime time.Time) {
		rows[rel] = &manifest.Track{Path: rel, Size: 4096, ModTime: mtime}
	}
	at := func(mtime time.Time) fileStat { return fileStat{size: 4096, mtime: mtime} }
	current := at(rowTime)
	behind := fileStat{size: 4104, mtime: rowTime.Add(time.Minute)}
	onDisk := map[string]os.FileInfo{}
	var rescans, renders []string
	var renderErr error
	var h *staleRenditionHeal
	var waitingAtRequest []bool
	lookup := func(_ context.Context, rel string) (*manifest.Track, error) { return rows[rel], nil }
	h = newStaleRenditionHeal(lookup,
		func(rel string) bool {
			rescans = append(rescans, rel)
			h.mu.Lock()
			_, waiting := h.waiting[rel+"\x00optimized-v2-48000-16"]
			h.mu.Unlock()
			waitingAtRequest = append(waitingAtRequest, waiting)
			return true
		},
		func(_ context.Context, rel, variantID string) error {
			renders = append(renders, rel+" "+variantID)
			return renderErr
		},
		func(rel string) (os.FileInfo, error) {
			if fi, ok := onDisk[rel]; ok {
				return fi, nil
			}
			return nil, os.ErrNotExist
		})
	now := time.Unix(1_800_000_000, 0)
	h.now = func() time.Time { return now }
	const v, v2 = "optimized-v2-48000-16", "pcm-v2-176400-24"
	step := func(what string, act func(), wantRescans, wantRenders []string) {
		t.Helper()
		rescans, renders = nil, nil
		act()
		if !slices.Equal(rescans, wantRescans) || !slices.Equal(renders, wantRenders) {
			t.Errorf("%s: rescans %v and renders %v, want %v and %v", what, rescans, renders, wantRescans, wantRenders)
		}
	}
	observe := func(rel, id string, info os.FileInfo) func() {
		return func() { h.observe(t.Context(), rel, id, info) }
	}
	rescanned := func(dir string) func() { return func() { h.rescanned(t.Context(), dir) } }

	row("A/01.flac", rowTime)
	step("row current", observe("A/01.flac", v, current), nil, []string{"A/01.flac " + v})
	step("again within the minute", observe("A/01.flac", v, current), nil, nil)
	step("another rendition of the file", observe("A/01.flac", v2, current), nil, []string{"A/01.flac " + v2})
	now = now.Add(staleRenditionRescanEvery)
	step("a minute on (the render failed)", observe("A/01.flac", v, current), nil, []string{"A/01.flac " + v})
	retagged := rowTime.Add(time.Hour)
	row("A/01.flac", retagged)
	step("a new version of the file, within the minute", observe("A/01.flac", v, at(retagged)), nil, []string{"A/01.flac " + v})
	row("A/01.flac", rowTime)

	now = now.Add(staleRenditionRescanEvery)
	renderErr = fmt.Errorf("pool: %w", api.ErrUpscaleQueueFull)
	step("the pool's queue full", observe("A/01.flac", v, current), nil, []string{"A/01.flac " + v})
	step("again: nothing was queued", observe("A/01.flac", v, current), nil, []string{"A/01.flac " + v})
	renderErr = errRerenderInactive
	step("the kind switched off", observe("A/01.flac", v, current), nil, []string{"A/01.flac " + v})
	renderErr = nil
	step("switched back on", observe("A/01.flac", v, current), nil, []string{"A/01.flac " + v})
	renderErr = api.ErrUpscaleIneligible
	now = now.Add(staleRenditionRescanEvery)
	step("refused as ineligible", observe("A/01.flac", v, current), nil, []string{"A/01.flac " + v})
	step("again within the minute", observe("A/01.flac", v, current), nil, nil)
	renderErr = nil

	row("B/01.flac", rowTime)
	row("B/02.flac", rowTime)
	row("C/01.flac", rowTime)
	step("row behind", observe("B/01.flac", v, behind), []string{"B/01.flac"}, nil)
	step("another file of the directory, within the minute", observe("B/02.flac", v, behind), nil, nil)
	step("another directory's rescan", rescanned("C"), nil, nil)
	onDisk["B/01.flac"] = current
	onDisk["B/02.flac"] = behind
	step("the rescan of B: 01's row caught up, 02's did not", rescanned("B"), nil, []string{"B/01.flac " + v})
	if _, kept := h.asked["B"]; !kept {
		t.Error("a rescan that left 02 behind freed B's rescan minute: a file that never comes level would have every download ask again")
	}
	onDisk["B/02.flac"] = current
	step("a later rescan of B, with no download between: 02 was kept", rescanned("B"), nil, []string{"B/02.flac " + v})
	step("01 changed again: the level rescan freed the minute", observe("B/01.flac", v, behind), []string{"B/01.flac"}, nil)

	row("E/01.flac", rowTime)
	step("E behind", observe("E/01.flac", v, behind), []string{"E/01.flac"}, nil)
	now = now.Add(staleRenditionWaitMax)
	step("a rescan of E past the wait, still behind: dropped", rescanned("E"), nil, nil)
	onDisk["E/01.flac"] = current
	step("a rescan of E that brings it level: nothing waits", rescanned("E"), nil, nil)

	row("D/01.flac", rowTime)
	onDisk["D/01.flac"] = current
	renderErr = errSourceAheadOfRow
	waitingAtRequest = nil
	step("the file changed again before the enqueue", observe("D/01.flac", v, current),
		[]string{"D/01.flac"}, []string{"D/01.flac " + v})
	if !slices.Equal(waitingAtRequest, []bool{true}) {
		t.Errorf("the heal asked for the rescan with the render waiting %v, want [true]: a rescan that ran before the wait "+
			"was recorded would leave the render for the next download", waitingAtRequest)
	}
	renderErr = nil
	reread := rowTime.Add(2 * time.Hour)
	row("D/01.flac", reread)
	onDisk["D/01.flac"] = at(reread)
	step("the rescan reads the new version", rescanned("D"), nil, []string{"D/01.flac " + v})

	// No re-render wired (the rescans alone): no render waits.
	h2 := newStaleRenditionHeal(lookup, func(string) bool { return true }, nil, nil)
	h2.observe(t.Context(), "B/01.flac", v, behind)
	h2.observe(t.Context(), "A/01.flac", v, current)
	if len(h2.waiting) != 0 {
		t.Errorf("with no re-render wired, %d renders wait, want none", len(h2.waiting))
	}
}

// TestAtMostACapOfRendersWaitForARescan: the renders waiting for rescans are
// bounded like the rescan queue itself. Past the cap a new one is dropped
// (its next download renders it once the row is current), unless some have
// waited longer than staleRenditionWaitMax, which are forgotten first.
func TestAtMostACapOfRendersWaitForARescan(t *testing.T) {
	h := newStaleRenditionHeal(nil, nil, func(context.Context, string, string) error { return nil }, nil)
	now := time.Unix(1_800_000_000, 0)
	h.now = func() time.Time { return now }
	const v = "optimized-v2-48000-16"
	for i := 0; i < sourceRescanQueueCap; i++ {
		h.await(fmt.Sprintf("D%04d", i), fmt.Sprintf("D%04d/01.flac", i), v)
	}
	late := "Late/01.flac\x00" + v
	h.await("Late", "Late/01.flac", v)
	if _, ok := h.waiting[late]; ok || len(h.waiting) != sourceRescanQueueCap {
		t.Errorf("past the cap: %d waiting, Late waiting %v, want the cap and Late dropped", len(h.waiting), ok)
	}
	h.await("D0000", "D0000/01.flac", v)
	if len(h.waiting) != sourceRescanQueueCap {
		t.Errorf("a render already waiting, asked for again at the cap: %d waiting, want %d", len(h.waiting), sourceRescanQueueCap)
	}
	now = now.Add(staleRenditionWaitMax)
	h.await("Late", "Late/01.flac", v)
	if _, ok := h.waiting[late]; !ok || len(h.waiting) > sourceRescanQueueCap {
		t.Errorf("past the wait: %d waiting, Late waiting %v, want the old ones forgotten and Late kept", len(h.waiting), ok)
	}
}
