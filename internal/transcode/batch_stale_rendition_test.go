package transcode

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
	"github.com/acoseac/1-bit-bridge/internal/flactest"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/google/uuid"
)

// renderedLibrary is a library on disk, scanned by the real scanner, whose
// batches run on a real Coordinator and Pool. The pool's runner stands in
// for sox and commits each job the way Pool.processJob commits one, so a
// rendition's row records the version of its source the batch stamped
// (the track row's).
type renderedLibrary struct {
	store   *manifest.Store
	root    string
	scanner *manifest.Scanner
	coord   *Coordinator
	out     string

	mu        sync.Mutex
	completed []string // "<source> <variant id>", one per committed job
	done      chan struct{}
}

func newRenderedLibrary(t *testing.T, files map[string][]byte) *renderedLibrary {
	t.Helper()
	l := &renderedLibrary{store: openTempStoreForBatch(t), root: t.TempDir(), out: t.TempDir(),
		done: make(chan struct{}, 64)}
	for rel, body := range files {
		abs := filepath.Join(l.root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if body == nil {
			if _, err := dsdtone.MintDSF(abs, dsdtone.Tone{RateHz: 2822400, Seconds: 1, AmplitudeDBFS: -6}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(abs, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l.scanner = manifest.NewScanner([]string{l.root}, l.store, filepath.Join(t.TempDir(), "artwork"))
	l.scan(t)

	p := NewPool(l.store, 1, 64)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	p.runner = func(ctx context.Context, spec JobSpec) (RunResult, error) {
		return RunResult{SizeBytes: 4096}, nil
	}
	c, err := NewCoordinator(p, l.store, t.TempDir(), nil, bridgefs.New([]string{l.root}).ResolveChecked)
	if err != nil {
		t.Fatal(err)
	}
	c.WithDSDRender(capsFn(dsdCapsOn))
	p.SetOnJobComplete(func(path, variantID string, sampleRate, bitsPerSample int, durationSeconds float64, batchID uuid.UUID, completedAt time.Time) {
		c.OnJobComplete(path, variantID, sampleRate, bitsPerSample, durationSeconds, batchID, completedAt)
		l.mu.Lock()
		l.completed = append(l.completed, path+" "+variantID)
		l.mu.Unlock()
		l.done <- struct{}{}
	})
	p.SetOnJobFailed(func(path, variantID, errMsg string, durationSeconds float64, batchID uuid.UUID, failedAt time.Time) {
		c.OnJobFailed(path, variantID, errMsg, durationSeconds, batchID, failedAt)
		t.Errorf("job %s %s failed: %s", path, variantID, errMsg)
		l.done <- struct{}{}
	})
	l.coord = c
	return l
}

// scan runs the real scanner over the library, as the periodic scan does.
func (l *renderedLibrary) scan(t *testing.T) {
	t.Helper()
	if _, err := l.scanner.Scan(context.Background()); err != nil {
		t.Fatalf("Scan: %v", err)
	}
}

// submit runs one batch and waits for every job it enqueued to finish.
// It returns the result and the renders the batch made.
func (l *renderedLibrary) submit(t *testing.T, kind string) (*SubmitResult, []string) {
	t.Helper()
	l.mu.Lock()
	before := len(l.completed)
	l.mu.Unlock()
	var (
		res *SubmitResult
		err error
	)
	switch kind {
	case "upscale":
		res, err = l.coord.Submit(context.Background(), "Album", 192000, 24, l.out)
	case "optimize":
		res, err = l.coord.SubmitOptimize(context.Background(), "Album", l.out)
	case "pcm":
		res, err = l.coord.SubmitPCMRender(context.Background(), "Album", l.out)
	}
	if err != nil {
		t.Fatalf("submit %s: %v", kind, err)
	}
	for i := 0; i < res.EnqueuedCount; i++ {
		select {
		case <-l.done:
		case <-time.After(30 * time.Second):
			t.Fatalf("submit %s: job %d of %d did not finish in 30 s", kind, i+1, res.EnqueuedCount)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	rendered := append([]string(nil), l.completed[before:]...)
	sort.Strings(rendered)
	return res, rendered
}

// TestABatchRendersARenditionThatARetagAndAScanMadeStale is backlog B100:
// a batch counts a track covered only while one of its renditions of the
// kind is FRESH, recording the version of the file its row records. Each
// kind renders the album, two files are retagged (new bytes, a new mtime)
// and the real scanner reads the change, so their rows' stamps move; the
// next batch renders those two again, and only those, under the new stamp,
// and a third batch renders nothing.
//
// Measured on main before the fix (B82's record, 2026-09-29): after the
// retag and the scan the batch answered `enqueuedCount: 0, alreadyCovered:
// N` (a projection's HasVariant was ANY rendition of the kind), so the stale
// renditions stayed 410 until a download healed them.
func TestABatchRendersARenditionThatARetagAndAScanMadeStale(t *testing.T) {
	flac := flactest.Stream(96000, 2, 24, flactest.Block, 3)
	for _, tc := range []struct {
		kind string
		// renders are the renditions the first batch makes; retagged the
		// ones the retags make stale, which the second batch renders again.
		renders, retagged []string
	}{
		{"upscale",
			[]string{"Album/01.flac upscaled-v2-192000-24", "Album/02.flac upscaled-v2-192000-24"},
			[]string{"Album/01.flac upscaled-v2-192000-24"}},
		{"optimize",
			[]string{"Album/01.flac optimized-v2-48000-16", "Album/02.flac optimized-v2-48000-16",
				"Album/03.dsf " + stampDSDCompactID, "Album/04.dsf " + stampDSDCompactID},
			[]string{"Album/01.flac optimized-v2-48000-16", "Album/03.dsf " + stampDSDCompactID}},
		{"pcm",
			[]string{"Album/03.dsf " + stampDSDFaithfulID, "Album/04.dsf " + stampDSDFaithfulID},
			[]string{"Album/03.dsf " + stampDSDFaithfulID}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			lib := newRenderedLibrary(t, map[string][]byte{
				"Album/01.flac": flac, "Album/02.flac": flac,
				"Album/03.dsf": nil, "Album/04.dsf": nil,
			})

			res, rendered := lib.submit(t, tc.kind)
			if !slices.Equal(rendered, tc.renders) {
				t.Fatalf("the first batch rendered %v, want %v", rendered, tc.renders)
			}
			if res.EnqueuedCount != len(tc.renders) || res.AlreadyCovered != 0 {
				t.Fatalf("the first batch: enqueued %d, alreadyCovered %d, want %d and 0",
					res.EnqueuedCount, res.AlreadyCovered, len(tc.renders))
			}

			// A retag, then the scan that reads it: each row's stamp moves.
			before := map[string]*manifest.TrackStat{}
			for _, rel := range []string{"Album/01.flac", "Album/03.dsf"} {
				before[rel] = trackStat(t, lib.store, rel)
				retag(t, filepath.Join(lib.root, filepath.FromSlash(rel)))
			}
			lib.scan(t)
			for rel, was := range before {
				if now := trackStat(t, lib.store, rel); now.MTimeNS == was.MTimeNS || now.Size == was.Size {
					t.Fatalf("%s: the scan left the row at (%d, %d), want the retag's version", rel, now.MTimeNS, now.Size)
				}
			}

			res, rendered = lib.submit(t, tc.kind)
			if !slices.Equal(rendered, tc.retagged) {
				t.Errorf("after the retag and the scan the batch rendered %v, want %v: a rendition the "+
					"retag made stale answers 410, so a batch that counts it covered leaves it so", rendered, tc.retagged)
			}
			if want := len(tc.renders) - len(tc.retagged); res.AlreadyCovered != want || res.EnqueuedCount != len(tc.retagged) {
				t.Errorf("after the retag: enqueued %d, alreadyCovered %d, want %d and %d",
					res.EnqueuedCount, res.AlreadyCovered, len(tc.retagged), want)
			}
			// What the batch rendered records the version the scan read.
			for _, d := range tc.retagged {
				rel, id := splitRender(d)
				v, err := lib.store.GetVariant(context.Background(), rel, id)
				if err != nil || v == nil {
					t.Fatalf("GetVariant(%s, %s): %v, %v", rel, id, v, err)
				}
				if st := trackStat(t, lib.store, rel); v.SourceMTimeNS != st.MTimeNS || v.SourceSize != st.Size {
					t.Errorf("%s: the rendition records (%d, %d), want its row's (%d, %d)",
						d, v.SourceMTimeNS, v.SourceSize, st.MTimeNS, st.Size)
				}
			}

			// And no loop: fresh again, every rendition is covered.
			res, rendered = lib.submit(t, tc.kind)
			if len(rendered) != 0 || res.AlreadyCovered != len(tc.renders) {
				t.Errorf("a third batch rendered %v (alreadyCovered %d), want nothing rendered and all %d covered",
					rendered, res.AlreadyCovered, len(tc.renders))
			}
		})
	}
}

// The DSD renditions' current ids at a DSD64 source (2.8224 MHz: the
// 44.1 kHz family).
const (
	stampDSDCompactID  = manifest.VariantKindPrefixOptimizedDSD + "-" + DSDRenditionSchemaVersion + "-44100-16"
	stampDSDFaithfulID = "pcm-" + DSDRenditionSchemaVersion + "-176400-24"
)

func trackStat(t *testing.T, s *manifest.Store, rel string) *manifest.TrackStat {
	t.Helper()
	st, err := s.GetTrackStat(context.Background(), rel)
	if err != nil || st == nil {
		t.Fatalf("GetTrackStat(%s): %v, %v", rel, st, err)
	}
	return st
}

// splitRender splits a "<source> <variant id>" record; no path here holds a
// space.
func splitRender(d string) (rel, id string) {
	rel, id, _ = strings.Cut(d, " ")
	return rel, id
}
