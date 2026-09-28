package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
	"github.com/acoseac/1-bit-bridge/internal/urlquery"
)

// A rendition records the version of its source it was made from
// (track_variants.source_mtime_ns / source_size), and three readers ask
// whether it is still current: the auto-optimize sweeper and the album
// gain's dsd_peaks against the TRACK ROW, the serve path against the file
// on disk. Every writer has to stamp on the clock those readers share, or
// between a change to a file and the scan that reads it each writer's
// rendition is one the other side calls stale (backlog B24).

// committingQueue stands in for transcode.Pool behind the adapter and the
// sweeper. Every job it accepts is committed at once, the way
// Pool.processJob commits one: a sidecar at the spec's own path and a
// VariantRow stamped with the spec's source facts, a DSD rendition's peak
// included. A test then sees the rows and files a render leaves, without
// sox.
type committingQueue struct {
	store *manifest.Store
	mu    sync.Mutex
	done  []string // "<source> <variant id>", one per committed render
}

func (q *committingQueue) Enqueue(spec transcode.JobSpec) error {
	sidecar := spec.SidecarPath()
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
		return err
	}
	body := []byte("rendition of " + spec.SourceLibraryRel)
	if err := os.WriteFile(sidecar, body, 0o644); err != nil {
		return err
	}
	row := manifest.VariantRow{
		SourcePath: spec.SourceLibraryRel, VariantID: spec.VariantID(), SidecarPath: sidecar,
		Format: "flac", SampleRate: spec.TargetSampleRate, BitsPerSample: spec.TargetBits,
		SizeBytes: int64(len(body)), SourceMTimeNS: spec.SourceMTimeNS, SourceSize: spec.SourceSize,
		CreatedAt: time.Now().UnixNano(),
	}
	if profile := spec.DSDPeakProfile(); profile != "" {
		gain, peak := 3.0, -4.0
		row.AppliedGainDB, row.TruePeakDBTP, row.PeakProfile = &gain, &peak, profile
	}
	if err := q.store.UpsertVariant(context.Background(), row); err != nil {
		return err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.done = append(q.done, spec.SourceLibraryRel+" "+spec.VariantID())
	return nil
}

// since returns the renders committed after the first n.
func (q *committingQueue) since(n int) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]string(nil), q.done[n:]...)
}

func (q *committingQueue) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.done)
}

const (
	stampPCM         = "Fixture/PCM/01.flac"
	stampDSD         = "Fixture/DSD/01.dsf"
	stampPCMCompact  = "optimized-v2-48000-16"
	stampDSDCompact  = "optimized-dsd-" + transcode.DSDRenditionSchemaVersion + "-44100-16"
	stampDSDFaithful = "pcm-" + transcode.DSDRenditionSchemaVersion + "-176400-24"
)

// stampBridge is the rendition half of a bridge, wired over one store as
// runServe wires it: the adapter POST /v1/upscale hands a request to, the
// auto-optimize sweeper, and GET /v1/download, with committingQueue in the
// pool's place.
type stampBridge struct {
	store   *manifest.Store
	libDir  string
	queue   *committingQueue
	adapter *upscaleEnqueuerAdapter
	sweeper *autoOptimizeSweeper
	url     string
	token   string
}

// newStampBridge is newEmptyStampBridge over a library holding a hi-res
// FLAC and a DSD64 file, both scanned.
func newStampBridge(t *testing.T) *stampBridge {
	t.Helper()
	b := newEmptyStampBridge(t)
	rate, bits := 96000.0, 24
	b.seed(t, stampPCM, &manifest.Track{Codec: "FLAC", SampleRate: &rate, BitsPerSample: &bits})
	dsdRate, dsdBits, channels, duration := 2822400.0, 1, 2, 3.0
	b.seed(t, stampDSD, &manifest.Track{Codec: "DSF", SampleRate: &dsdRate, BitsPerSample: &dsdBits,
		Channels: &channels, Duration: &duration})
	return b
}

func newEmptyStampBridge(t *testing.T) *stampBridge {
	t.Helper()
	dir := t.TempDir()
	b := &stampBridge{libDir: filepath.Join(dir, "library")}
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	b.store, b.queue = store, &committingQueue{store: store}

	on := func() bool { return true }
	caps := func() transcode.DSDRenderCaps { return transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true} }
	variantsDir := func() string { return filepath.Join(dir, "variants") }
	resolver := bridgefs.New([]string{b.libDir})
	b.adapter = &upscaleEnqueuerAdapter{
		pool: b.queue, store: store, resolver: resolver, cfg: &config.Config{},
		outputDir: variantsDir, dsdCaps: caps,
		tempDir: func() string { return filepath.Join(dir, "scratch") },
	}
	b.sweeper = &autoOptimizeSweeper{
		store: store, resolver: resolver, enqueue: b.queue.Enqueue, enabled: on,
		outputDir: variantsDir, dsdCaps: caps,
		maxPerSweep:  func() int { return 100 },
		minFreeBytes: func() int64 { return 0 },
		diskFree:     func(string) (int64, error) { return 1 << 50, nil },
	}

	tokens, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatalf("auth.OpenStore: %v", err)
	}
	if b.token, _, err = tokens.Mint("stamp probe"); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	provider := manifest.NewProvider(store, nil)
	srv := api.New(&config.Config{LibraryRoots: []string{b.libDir}}, tokens, provider, "stamp-fingerprint").
		WithUpscale(on, &variantStoreAdapter{provider: provider, store: store, variantsDir: variantsDir}).
		WithCarPlayOptimize(on).
		WithDSDRender(on).
		WithUpscaleEnqueuer(b.adapter)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	b.url = hs.URL
	return b
}

// seed writes a source file and its row as a scan leaves them: the row's
// size and mtime are the file's.
func (b *stampBridge) seed(t *testing.T, rel string, tr *manifest.Track) {
	t.Helper()
	abs := filepath.Join(b.libDir, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, bytes.Repeat([]byte{0x5a}, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	isDSD := tr.Codec == "DSF"
	tr.Path, tr.IsDSD = rel, &isDSD
	b.scanReads(t, rel, tr)
}

// scanReads writes the row a scan writes for rel: the file's size and mtime
// now, over the row's other facts (tr, or the stored row when tr is nil).
func (b *stampBridge) scanReads(t *testing.T, rel string, tr *manifest.Track) {
	t.Helper()
	if tr == nil {
		var err error
		if tr, err = b.store.LookupTrack(context.Background(), rel); err != nil || tr == nil {
			t.Fatalf("LookupTrack(%s): %v, %v", rel, tr, err)
		}
	}
	fi, err := os.Stat(filepath.Join(b.libDir, rel))
	if err != nil {
		t.Fatal(err)
	}
	tr.Size, tr.ModTime = fi.Size(), fi.ModTime()
	if err := b.store.UpsertTrack(context.Background(), tr); err != nil {
		t.Fatalf("UpsertTrack(%s): %v", rel, err)
	}
}

// change rewrites rel on disk the way a tagger does (new bytes, a new
// mtime) and leaves its row alone, as a library is between two scans. The
// mtime moves a minute, past the 2 s the serve path tolerates.
func (b *stampBridge) change(t *testing.T, rel string) {
	t.Helper()
	abs := filepath.Join(b.libDir, rel)
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("retagged")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
}

func (b *stampBridge) do(t *testing.T, method, target string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, b.url+target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	return resp
}

// request is a client asking for one rendition of rel (POST /v1/upscale).
// It returns how many jobs the bridge queued.
func (b *stampBridge) request(t *testing.T, rel, kind string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"path": rel, "kind": kind})
	resp := b.do(t, http.MethodPost, "/v1/upscale", body)
	defer resp.Body.Close()
	var got api.UpscaleResponse
	if resp.StatusCode != http.StatusAccepted || json.NewDecoder(resp.Body).Decode(&got) != nil {
		t.Fatalf("POST /v1/upscale %s %s: status %d", kind, rel, resp.StatusCode)
	}
	return got.Enqueued
}

// download is what GET /v1/download answers for one rendition.
func (b *stampBridge) download(t *testing.T, rel, variant string) int {
	t.Helper()
	resp := b.do(t, http.MethodGet, "/v1/download?path="+urlquery.Escape(rel)+"&variant="+urlquery.Escape(variant), nil)
	defer resp.Body.Close()
	return resp.StatusCode
}

func (b *stampBridge) sweep(t *testing.T) {
	t.Helper()
	if counts := b.sweeper.sweepOnce(context.Background()); counts == nil {
		t.Fatal("the sweep failed")
	}
}

// wantServed fails for each render in done that GET /v1/download refuses:
// a rendition the bridge made from the file on disk and then calls stale is
// a render nobody can play.
func (b *stampBridge) wantServed(t *testing.T, when string, done []string) {
	t.Helper()
	for _, d := range done {
		var rel, variant string
		if _, err := fmt.Sscan(d, &rel, &variant); err != nil {
			t.Fatal(err)
		}
		if code := b.download(t, rel, variant); code != http.StatusOK {
			t.Errorf("%s: GET /v1/download for %s answers %d, want 200 — the bridge rendered it and calls it stale", when, d, code)
		}
	}
}

// wantFreshPeak fails unless the peak on record for rel under the tier's
// profile is one the album gain reads as current (manifest.FreshDSDPeaks,
// judged against the row). A stale one is re-measured, a full decode, by
// every render of an album-mate.
func (b *stampBridge) wantFreshPeak(t *testing.T, when, rel string, kind transcode.JobKind, rate int) {
	t.Helper()
	profile := transcode.DSDPeakProfileFor(kind, rate, "-v")
	fresh, err := b.store.FreshDSDPeaks(context.Background(), profile, []string{rel})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fresh[rel]; !ok {
		t.Errorf("%s: no fresh %q peak for %s — the render recorded one the album survey re-measures", when, profile, rel)
	}
}

// alternate runs rounds of a client asking for the CarPlay tier of the
// FLAC and the faithful tier of the DSF, then a sweep. It fails for each
// render a step makes that GET /v1/download refuses, and for a faithful
// render whose peak the album gain reads as stale.
func (b *stampBridge) alternate(t *testing.T, phase string, rounds int) {
	t.Helper()
	for round := 1; round <= rounds; round++ {
		when := fmt.Sprintf("%s, round %d", phase, round)
		seen := b.queue.count()
		b.request(t, stampPCM, "optimize")
		if b.request(t, stampDSD, "pcm") > 0 {
			b.wantFreshPeak(t, when+", after the faithful render", stampDSD, transcode.JobKindPCMRender, 176400)
		}
		b.wantServed(t, when+", after the requests", b.queue.since(seen))
		seen = b.queue.count()
		b.sweep(t)
		b.wantServed(t, when+", after the sweep", b.queue.since(seen))
	}
}

// delta is the paths a device syncing since `since` receives.
func (b *stampBridge) delta(t *testing.T, since time.Time) []string {
	t.Helper()
	tracks, err := b.store.ListServedTracks(context.Background(), &since)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, tr := range tracks {
		out = append(out, tr.Path)
	}
	sort.Strings(out)
	return out
}

// TestAChangedFileIsNotRenderedUntilItsRowIsReRead drives the loop B24 was
// filed for through the real entry points: POST /v1/upscale into the
// adapter, the auto-optimize sweep, and GET /v1/download. Both sources
// change on disk and the scanner does not run, then a client asks for a
// rendition and the sweeper sweeps, three times over. Measured on the
// code before the fix: twelve renders, a manifest delta for both tracks on
// every step, and the download answering 200 after each request and 410
// after each sweep, because the request stamped the file on disk and the
// sweep the row.
//
// A render can only record one version, and while the row is behind the
// file every version is stale to one of the readers. So none starts: the
// request is refused and the sweep passes the files over, until the scan
// reads them. Then each rendition renders once, and every one serves.
func TestAChangedFileIsNotRenderedUntilItsRowIsReRead(t *testing.T) {
	b := newStampBridge(t)

	b.sweep(t)
	if got := b.queue.since(0); len(got) != 2 {
		t.Fatalf("the first sweep rendered %v, want the two compact tiers", got)
	}
	b.wantServed(t, "after the first sweep", b.queue.since(0))

	b.change(t, stampPCM)
	b.change(t, stampDSD)
	changed, before := time.Now(), b.queue.count()
	b.alternate(t, "the rows behind the files", 3)
	if got := b.queue.since(before); len(got) != 0 {
		t.Errorf("%d renders over three rounds of a request and a sweep while the row was behind the file: %v\n"+
			"want none: a render then records a version the sweeper or the serve path calls stale, "+
			"and the other writer renders it again", len(got), got)
	}
	if got := b.delta(t, changed); len(got) != 0 {
		t.Errorf("those rounds pushed a manifest delta for %v to every paired device, want none", got)
	}

	b.scanReads(t, stampPCM, nil)
	b.scanReads(t, stampDSD, nil)
	before = b.queue.count()
	b.alternate(t, "after the scan", 3)
	got := b.queue.since(before)
	sort.Strings(got)
	want := []string{stampDSD + " " + stampDSDCompact, stampDSD + " " + stampDSDFaithful, stampPCM + " " + stampPCMCompact}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("after the scan read both files: renders %v, want each rendition once: %v", got, want)
	}
	b.wantServed(t, "after the scan", want)
	b.wantFreshPeak(t, "after the scan", stampDSD, transcode.JobKindOptimize, 44100)
	b.wantFreshPeak(t, "after the scan", stampDSD, transcode.JobKindPCMRender, 176400)
}

// TestARefusedRequestRescansTheFileSoTheNextOneRenders: the refusal costs a
// client one play of the source, not every play until the periodic scan
// (six hours by default). It queues a rescan of the file's directory,
// through sourceRescanner over a real scanner as runServe wires them, and
// the next request finds the row current and renders a rendition stamped
// with the version the scan read.
func TestARefusedRequestRescansTheFileSoTheNextOneRenders(t *testing.T) {
	b := newEmptyStampBridge(t)
	abs := filepath.Join(b.libDir, stampDSD)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := dsdtone.MintDSF(abs, dsdtone.Tone{RateHz: 2822400, Seconds: 1, AmplitudeDBFS: -6}); err != nil {
		t.Fatal(err)
	}
	scanner := manifest.NewScanner([]string{b.libDir}, b.store, filepath.Join(t.TempDir(), "artwork"))
	if _, err := scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	rescanner := newSourceRescanner()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		rescanner.run(ctx, scanner.ScanSubtree)
	}()
	drainLoopOnCleanup(t, cancel, done, "the source rescanner")
	b.adapter.rescan = rescanner.request

	later := time.Now().Add(time.Minute).Truncate(time.Second)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	if n := b.request(t, stampDSD, "pcm"); n != 0 {
		t.Fatalf("the request for the changed file queued %d jobs, want the refusal", n)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		tr, err := b.store.LookupTrack(context.Background(), stampDSD)
		if err != nil {
			t.Fatal(err)
		}
		if tr != nil && tr.ModTime.UnixNano() == later.UnixNano() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the row still records %v 10 s after the refused request, want %v: nothing rescanned the file", tr.ModTime, later)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := b.request(t, stampDSD, "pcm"); n != 1 {
		t.Fatalf("the request after the rescan queued %d jobs, want the render", n)
	}
	b.wantServed(t, "after the rescan", b.queue.since(0))
	row, err := b.store.GetVariant(context.Background(), stampDSD, stampDSDFaithful)
	if err != nil || row == nil {
		t.Fatalf("GetVariant: %v, %v", row, err)
	}
	if row.SourceMTimeNS != later.UnixNano() {
		t.Errorf("the rendition records mtime %d, want the version the rescan read, %d", row.SourceMTimeNS, later.UnixNano())
	}
}

// TestSourceRescannerQueuesADirectoryOnceAtATime: requests for the files
// of one directory queue one scan of it; a request for a directory whose
// scan has already started queues another, since the file may have changed
// after the walk passed it; and a full queue drops a request rather than
// block the HTTP request that made it.
func TestSourceRescannerQueuesADirectoryOnceAtATime(t *testing.T) {
	full := newSourceRescanner()
	filled := make(chan struct{})
	go func() {
		defer close(filled)
		for i := 0; i < sourceRescanQueueCap+5; i++ {
			full.request(fmt.Sprintf("/lib/D%03d/01.flac", i), fmt.Sprintf("D%03d/01.flac", i))
		}
	}()
	select {
	case <-filled:
	case <-time.After(5 * time.Second):
		t.Fatal("a request blocked on the full queue: it runs inside an HTTP request, and must drop instead")
	}
	if len(full.queue) != sourceRescanQueueCap || len(full.pending) != sourceRescanQueueCap {
		t.Errorf("queue %d, pending %d after %d directories, want both at the cap %d",
			len(full.queue), len(full.pending), sourceRescanQueueCap+5, sourceRescanQueueCap)
	}

	r := newSourceRescanner()
	scanned := make(chan string, 4)
	hold := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx, func(ctx context.Context, dir string) (int, error) {
			scanned <- dir
			select {
			case <-hold:
			case <-ctx.Done():
			}
			return 0, nil
		})
	}()
	drainLoopOnCleanup(t, cancel, done, "the source rescanner")

	next := func(what string) string {
		t.Helper()
		select {
		case dir := <-scanned:
			return dir
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s within 5 s", what)
			return ""
		}
	}
	r.request("/lib/A/01.flac", "A/01.flac")
	r.request("/lib/A/02.flac", "A/02.flac")
	if got := next("scan"); got != "/lib/A" {
		t.Fatalf("scanned %q, want /lib/A", got)
	}
	r.request("/lib/A/01.flac", "A/01.flac")
	r.request("/lib/A/03.flac", "A/03.flac")
	close(hold)
	if got := next("second scan, for the requests made during the first"); got != "/lib/A" {
		t.Fatalf("second scan of %q, want /lib/A again", got)
	}
	r.mu.Lock()
	queued, pending := len(r.queue), len(r.pending)
	r.mu.Unlock()
	if queued != 0 || pending != 0 {
		t.Errorf("after the second scan started: %d queued, %d pending, want none — four requests for one directory, "+
			"two before its scan started and two during it, are two scans", queued, pending)
	}
}

// TestAutoOptimizeSweepPassesOverAFileThatChangedSinceItsScan: neither
// pass renders a file whose size or mtime is no longer its row's, since the
// rendition would record a version the serve path refuses. The sweep
// counts it, and once the scan has read the file the next sweep renders
// it, stamped with the version the scan recorded.
func TestAutoOptimizeSweepPassesOverAFileThatChangedSinceItsScan(t *testing.T) {
	f := newAutoOptimizeFixture(t)
	f.sweeper.dsdCaps = func() transcode.DSDRenderCaps { return transcode.DSDRenderCaps{Enabled: true, DecodeDSD: true} }
	seedSchemaMoveLibrary(t, f)
	later := time.Unix(1700000060, 0)
	changed := []string{"A/DSD/01.dsf", "A/DSD/03.dsf"}
	for _, rel := range changed {
		if err := os.Chtimes(filepath.Join(f.libDir, rel), later, later); err != nil {
			t.Fatal(err)
		}
	}
	counts := sweepOnceOrFail(t, f)
	if got := sweptPaths(f); len(got) != 0 {
		t.Errorf("swept %v while the rows were behind the files, want nothing", got)
	}
	// 03's compact tier, and both files' faithful tiers.
	if counts.ChangedSinceScan != 3 {
		t.Errorf("ChangedSinceScan = %d, want 3", counts.ChangedSinceScan)
	}

	for _, rel := range changed {
		tr, err := f.store.LookupTrack(context.Background(), rel)
		if err != nil || tr == nil {
			t.Fatalf("LookupTrack(%s): %v, %v", rel, tr, err)
		}
		tr.ModTime = later
		if err := f.store.UpsertTrack(context.Background(), tr); err != nil {
			t.Fatal(err)
		}
	}
	counts = sweepOnceOrFail(t, f)
	v := transcode.DSDRenditionSchemaVersion
	got := sweptJobsByID(f)
	checkSweptJobIDs(t, f, got, []string{
		"A/DSD/01.dsf optimized-dsd-" + v + "-44100-16",
		"A/DSD/03.dsf optimized-dsd-" + v + "-48000-16",
		"A/DSD/01.dsf pcm-" + v + "-176400-24",
		"A/DSD/03.dsf pcm-" + v + "-192000-24",
	})
	if counts.ChangedSinceScan != 0 {
		t.Errorf("ChangedSinceScan = %d after the scan, want 0", counts.ChangedSinceScan)
	}
	for id, spec := range got {
		if spec.SourceMTimeNS != later.UnixNano() {
			t.Errorf("%s records mtime %d, want the row's %d", id, spec.SourceMTimeNS, later.UnixNano())
		}
	}
}

// TestTheCLIRendersOnlyAFileItsRowStillDescribes: `bridge optimize`,
// `render` and `upscale` stamp the row, as every writer does, and pass
// over a file that changed after its scan, `--force` or not, telling the
// operator to scan first. The album gain still measures such a file as an
// album-mate, stamped with its row.
func TestTheCLIRendersOnlyAFileItsRowStillDescribes(t *testing.T) {
	store, resolver, track := renderCLIFixture(t, renderSource{rel: "A/DSD/01.dsf", codec: "DSF",
		rateHz: 2822400, isDSD: true, durationSec: 300, channels: 2})
	if err := store.UpsertTrack(context.Background(), &track); err != nil {
		t.Fatal(err)
	}
	p := runUpscaleParams{targetRateFlag: "auto", targetBits: 24, quality: transcode.QualityVeryHigh,
		workers: 1, kind: transcode.JobKindPCMRender, dsdCaps: cliCapsDSD, tempDir: "/scratch/render"}
	rowStamped := func(what string, spec transcode.JobSpec) {
		t.Helper()
		if spec.SourceMTimeNS != track.ModTime.UnixNano() || spec.SourceSize != track.Size {
			t.Errorf("%s records (%d, %d), want the row's (%d, %d)", what,
				spec.SourceMTimeNS, spec.SourceSize, track.ModTime.UnixNano(), track.Size)
		}
	}

	c, counters, _ := classifyWith(t, store, resolver, track, transcode.JobKindPCMRender, cliCapsDSD)
	if c == nil || !c.needsRun || counters.changedSinceScan != 0 {
		t.Fatalf("a scanned file: candidate %v, needs a run %v, changedSinceScan %d, want one to render",
			c != nil, c != nil && c.needsRun, counters.changedSinceScan)
	}
	rowStamped("a scanned file's spec", c.spec)

	abs, err := resolver.Resolve(track.Path)
	if err != nil {
		t.Fatal(err)
	}
	later := track.ModTime.Add(time.Minute)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	forced := p
	forced.force = true
	var counters2 upscaleSkipCounters
	c, exit := classifyUpscaleTrack(context.Background(), io.Discard, store, resolver, track, forced, &counters2)
	if exit != 0 || c == nil {
		t.Fatalf("a changed file under --force: candidate %v, exit %d, want it listed", c != nil, exit)
	}
	if c.needsRun || counters2.changedSinceScan != 1 {
		t.Errorf("a changed file under --force: needsRun %v, changedSinceScan %d, want it listed and not run",
			c.needsRun, counters2.changedSinceScan)
	}
	rowStamped("a changed file's spec", c.spec)

	var stdout, stderr bytes.Buffer
	dry := p
	dry.dryRun = true
	if code := runUpscaleBatch(context.Background(), &stdout, &stderr, store, &config.Config{}, resolver, dry); code != 0 {
		t.Fatalf("dry run exit %d; stderr=%q", code, stderr.String())
	}
	for _, line := range []string{"SKIP (changed on disk since the last scan)", "changed on disk since the last scan (run `bridge scan`"} {
		if !bytes.Contains(stdout.Bytes(), []byte(line)) {
			t.Errorf("dry run output does not say %q:\n%s", line, stdout.String())
		}
	}

	like := transcode.JobSpec{Kind: transcode.JobKindPCMRender, Quality: transcode.QualityVeryHigh, TargetSampleRate: 176400}
	mate, err := cliAlbumMateSpec(store, resolver, p)(context.Background(), track.Path, like)
	if err != nil {
		t.Fatalf("the album gain could not measure the changed file as an album-mate: %v", err)
	}
	rowStamped("the album-mate spec", mate)
}
