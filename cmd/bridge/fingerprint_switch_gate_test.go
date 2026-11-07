package main

import (
	"context"
	"errors"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/admin"
)

// What a fingerprint pass does with what reaches it after enrich.enabled is
// switched off (backlog B617). The pass's context asks the switch once an
// hour in these tests (switchEvery), so only AcoustID's client, which asks
// the switch at its send and at its answer, and the sweep's writes, which
// ask it again, can stop the pass.

// switchAcoustIDNoMatch is AcoustID's clean answer for a fingerprint it does
// not know.
const switchAcoustIDNoMatch = `{"status":"ok","results":[]}`

// sweepEnded waits for the sweeper's pass under way to end.
func sweepEnded(t *testing.T, status *sweepStatus[admin.FingerprintSweepCounts]) {
	t.Helper()
	eventually(t, func() bool {
		running, _, lastEnd, _, _ := status.snapshot()
		return !running && !lastEnd.IsZero()
	}, "the sweep to end")
}

// verdicts reads what the store keeps of AcoustID's answers: the files with a
// no-match verdict and the files whose match was accepted.
func (f *sweepSwitchFixture) verdicts(t *testing.T) (noMatch, matched []string) {
	t.Helper()
	ctx := context.Background()
	nm, err := f.store.FreshAcoustIDNoMatches(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for p := range nm {
		noMatch = append(noMatch, p)
	}
	m, err := f.store.AcoustIDMatchedPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for p := range m {
		matched = append(matched, p)
	}
	return noMatch, matched
}

// flipAtBodyEOF is a transport under AcoustID's client that runs at once when
// it has read the body of an answer to its end: after the client's gate took
// the answer, before the sweep writes what it says.
type flipAtBodyEOF struct {
	base http.RoundTripper
	at   func()
}

func (f *flipAtBodyEOF) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := f.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	resp.Body = &bodyEOFHook{ReadCloser: resp.Body, at: f.at}
	return resp, nil
}

type bodyEOFHook struct {
	io.ReadCloser
	at func()
}

func (h *bodyEOFHook) Read(p []byte) (int, error) {
	n, err := h.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		h.at()
	}
	return n, err
}

// TestAnAcoustIDAnswerAfterTheFlipIsNotKept: what AcoustID says of a file's
// first lookup is not kept when enrich.enabled is switched off before the
// answer arrives, which the client's gate refuses as it arrives, or after
// the client took it and before the sweep writes it, which the write's own
// ask of the switch refuses. Either refusal stops the pass, so the next file
// is not looked up, and nothing is cached, persisted or re-queued, a no-match
// included, which would have kept the file from every sweep for 30 days.
// Switched on again, the next pass asks about the same files and keeps what
// AcoustID says (backlog B617).
func TestAnAcoustIDAnswerAfterTheFlipIsNotKept(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		match        bool
	}{
		{name: "a match", answer: switchAcoustIDMatch, match: true},
		{name: "a no-match", answer: switchAcoustIDNoMatch},
	} {
		for _, when := range []string{"before it arrives", "once it is taken"} {
			t.Run(tc.name+", the switch off "+when, func(t *testing.T) {
				f := newSweepSwitchFixtureAnswering(t, tc.answer, "a.flac", "b.flac")
				f.sweeper.switchEvery = time.Hour
				var on atomic.Bool
				on.Store(true)
				status := &sweepStatus[admin.FingerprintSweepCounts]{}
				nudge := make(chan struct{}, 1)
				if when == "before it arrives" {
					holding, release := make(chan struct{}), make(chan struct{})
					var n atomic.Int64
					f.acoustid.setHold(func(r *http.Request) {
						if n.Add(1) != 1 {
							return
						}
						close(holding)
						select {
						case <-release:
						case <-r.Context().Done():
						}
					})
					runSwitchSweeper(t, f.sweeper, func() bool { return true }, on.Load, nudge, status)
					select {
					case <-holding:
					case <-time.After(10 * time.Second):
						t.Fatal("the sweep's first lookup never reached AcoustID")
					}
					on.Store(false)
					close(release)
				} else {
					var once sync.Once
					flip := &flipAtBodyEOF{base: f.acoustid.srv.Client().Transport, at: func() { once.Do(func() { on.Store(false) }) }}
					f.sweeper.client = acoustid.NewClient(f.acoustid.srv.URL, "k", "ua", &http.Client{Transport: flip}, onSwitches)
					runSwitchSweeper(t, f.sweeper, func() bool { return true }, on.Load, nudge, status)
					eventually(t, func() bool { return !on.Load() }, "the first answer to be read")
				}
				sweepEnded(t, status)

				if got := f.acoustid.requests(); len(got) != 1 {
					t.Errorf("AcoustID was asked %d time(s), want the first lookup alone:\n%s", len(got), strings.Join(got, "\n"))
				}
				if n := f.sweeper.cache.Len(); n != 0 {
					t.Errorf("the pass the switch stopped cached %d outcome(s)", n)
				}
				if noMatch, matched := f.verdicts(t); len(noMatch)+len(matched) != 0 {
					t.Errorf("the pass the switch stopped persisted no-match %v and match %v", noMatch, matched)
				}
				for _, name := range []string{"a.flac", "b.flac"} {
					if f.requeued(t, name) {
						t.Errorf("the pass the switch stopped re-queued %s", name)
					}
				}

				on.Store(true)
				nudge <- struct{}{}
				if tc.match {
					eventually(t, func() bool { return f.requeued(t, "a.flac") && f.requeued(t, "b.flac") },
						"both files to be resolved and re-queued once enrichment is on")
				} else {
					eventually(t, func() bool { noMatch, _ := f.verdicts(t); return len(noMatch) == 2 },
						"both no-matches to be kept once enrichment is on")
				}
				if got := f.acoustid.count(); got != 3 {
					t.Errorf("AcoustID was asked %d time(s), want 3: the refused lookup, then both files again:\n%s",
						got, strings.Join(f.acoustid.requests(), "\n"))
				}
			})
		}
	}
}

// TestAReQueueAfterTheFlipIsNotMade: a pass whose answers were all taken
// and written while enrich.enabled was on, switched off after them with
// nothing sent since (its second file's decode fails, after the flip), asks
// the switch again at its re-queue: it re-queues nothing, and forgets the
// match it cached and did not hand over. Switched on again, the next pass
// looks the file up again and re-queues it (backlog B617).
func TestAReQueueAfterTheFlipIsNotMade(t *testing.T) {
	f := newSweepSwitchFixture(t, "a.flac", "b.flac")
	f.sweeper.switchEvery = time.Hour
	var on atomic.Bool
	on.Store(true)
	decode := f.sweeper.compute
	var decodes atomic.Int64
	f.sweeper.compute = func(ctx context.Context, abs string, length time.Duration) (acoustid.Fingerprint, error) {
		if decodes.Add(1) == 2 {
			on.Store(false)
			return acoustid.Fingerprint{}, errors.New("a decode that fails")
		}
		return decode(ctx, abs, length)
	}
	status := &sweepStatus[admin.FingerprintSweepCounts]{}
	nudge := make(chan struct{}, 1)
	runSwitchSweeper(t, f.sweeper, func() bool { return true }, on.Load, nudge, status)
	eventually(t, func() bool { return decodes.Load() >= 2 }, "the sweep to decode its second file")
	sweepEnded(t, status)

	if got := f.acoustid.count(); got != 1 {
		t.Errorf("AcoustID was asked %d time(s), want the first file's lookup alone", got)
	}
	for _, name := range []string{"a.flac", "b.flac"} {
		if f.requeued(t, name) {
			t.Errorf("the pass the switch stopped re-queued %s", name)
		}
	}
	if n := f.sweeper.cache.Len(); n != 0 {
		t.Errorf("the pass the switch stopped left %d outcome(s) cached", n)
	}

	on.Store(true)
	nudge <- struct{}{}
	eventually(t, func() bool { return f.requeued(t, "a.flac") && f.requeued(t, "b.flac") },
		"both files to be resolved and re-queued once enrichment is on")
}

// TestALookupAfterTheFlipIsRefusedAtItsSend: enrich.enabled switched off
// while the pass decodes its second file (so after the first file's answer
// was taken and kept) stops the second lookup at AcoustID's client, before
// anything else has asked the switch. The pass re-queues nothing, and forgets
// the first file's match it did not hand over, which kept the file out of
// every later pass in this process; and it records none of it, since a
// match's provenance is written only with its re-queue (round 3). Switched on
// again, both files are looked up and re-queued, each with its provenance
// (backlog B617).
func TestALookupAfterTheFlipIsRefusedAtItsSend(t *testing.T) {
	f := newSweepSwitchFixture(t, "a.flac", "b.flac")
	f.sweeper.switchEvery = time.Hour
	var on atomic.Bool
	on.Store(true)
	decode := f.sweeper.compute
	var decodes atomic.Int64
	f.sweeper.compute = func(ctx context.Context, abs string, length time.Duration) (acoustid.Fingerprint, error) {
		if decodes.Add(1) == 2 {
			on.Store(false)
		}
		return decode(ctx, abs, length)
	}
	status := &sweepStatus[admin.FingerprintSweepCounts]{}
	nudge := make(chan struct{}, 1)
	runSwitchSweeper(t, f.sweeper, func() bool { return true }, on.Load, nudge, status)
	eventually(t, func() bool { return decodes.Load() >= 2 }, "the sweep to decode its second file")
	sweepEnded(t, status)

	if got := f.acoustid.requests(); len(got) != 1 {
		t.Errorf("AcoustID was asked %d time(s), want the first file's lookup alone:\n%s", len(got), strings.Join(got, "\n"))
	}
	if n := f.sweeper.cache.Len(); n != 0 {
		t.Errorf("the pass the switch stopped left %d outcome(s) cached", n)
	}
	if _, matched := f.verdicts(t); len(matched) != 0 {
		t.Errorf("the pass the switch stopped recorded the provenance of %v: a match is recorded only with its re-queue", matched)
	}
	for _, name := range []string{"a.flac", "b.flac"} {
		if f.requeued(t, name) {
			t.Errorf("the pass the switch stopped re-queued %s", name)
		}
	}

	on.Store(true)
	nudge <- struct{}{}
	eventually(t, func() bool { return f.requeued(t, "a.flac") && f.requeued(t, "b.flac") },
		"both files to be resolved and re-queued once enrichment is on")
	if got := f.acoustid.count(); got != 3 {
		t.Errorf("AcoustID was asked %d time(s), want 3: the first file, then both again:\n%s",
			got, strings.Join(f.acoustid.requests(), "\n"))
	}
	if _, matched := f.verdicts(t); len(matched) != 2 {
		t.Errorf("the provenance once both are re-queued = %v, want both files", matched)
	}
}

// TestAFingerprintPassRunsOnTheSwitchsContext pins what the behavioural
// tests above cannot see, since each sets switchEvery: runFingerprintSweeper
// runs each pass on enrichgate.CancelWhenOff over enrich.enabled at the
// sweeper's switchEvery, and nothing outside a test sets that field, so a
// production pass asks the switch at enrichgate.Interval, once a second
// (enrichgate's own tests pin the interval and its bound) (backlog B617).
func TestAFingerprintPassRunsOnTheSwitchsContext(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var calls []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || goToolIgnores(name) {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Name.Name != "runFingerprintSweeper" || n.Body == nil {
					return true
				}
				ast.Inspect(n.Body, func(m ast.Node) bool {
					if call, ok := m.(*ast.CallExpr); ok && types.ExprString(call.Fun) == "enrichgate.CancelWhenOff" {
						calls = append(calls, types.ExprString(call))
					}
					return true
				})
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "switchEvery" {
						t.Errorf("%s sets switchEvery: %s", fset.Position(n.Pos()), types.ExprString(lhs))
					}
				}
			case *ast.KeyValueExpr:
				if key, ok := n.Key.(*ast.Ident); ok && key.Name == "switchEvery" {
					t.Errorf("%s sets switchEvery in a literal", fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	want := "enrichgate.CancelWhenOff(ctx, enrichmentOn, s.switchEvery)"
	if len(calls) != 1 || calls[0] != want {
		t.Errorf("runFingerprintSweeper's calls of enrichgate.CancelWhenOff = %q, want %q alone", calls, want)
	}
}

// TestServeBuildsEveryClientOverTheLiveSwitches pins what serve's behavioural
// tests cannot see, since the enricher's batch, the sweep's pass and the
// cover adapter carry the switch on their contexts as well: every client
// runServe builds of an upstream enrich.enabled governs holds the live
// switches, enrichmentOn, and Deezer's deezerOn beside it, each read from the
// live config, so a client fails closed whatever context it is handed
// (backlog B617, round 3).
func TestServeBuildsEveryClientOverTheLiveSwitches(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"enrich.NewMusicBrainzClient":   "enrichgate.Enrichment(enrichmentOn)",
		"enrich.NewCoverArtClient":      "enrichgate.Enrichment(enrichmentOn)",
		"enrich.NewDeezerClient":        "enrichgate.Deezer(enrichmentOn, deezerOn)",
		"enrich.NewITunesClient":        "enrichgate.Enrichment(enrichmentOn)",
		"enrich.NewAtlasPremiumFetcher": "enrichgate.Enrichment(enrichmentOn)",
		"acoustid.NewClient":            "enrichgate.Enrichment(enrichmentOn)",
	}
	wantSwitch := map[string]string{
		"enrichmentOn": "func() bool { return liveCfg().Enrich.EffectiveEnabled() }",
		"deezerOn":     "func() bool { return liveCfg().Enrich.EffectiveDeezerEnabled() }",
	}
	built := map[string]int{}
	switches := map[string][]string{}
	found := false
	for _, d := range file.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "runServe" || fd.Body == nil {
			continue
		}
		found = true
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				fun := types.ExprString(n.Fun)
				sw, listed := want[fun]
				if !listed {
					return true
				}
				built[fun]++
				if len(n.Args) == 0 {
					t.Errorf("%s: %s is called with no argument", fset.Position(n.Pos()), fun)
					return true
				}
				if got := types.ExprString(n.Args[len(n.Args)-1]); got != sw {
					t.Errorf("%s: %s is built over %s, want %s", fset.Position(n.Pos()), fun, got, sw)
				}
			case *ast.AssignStmt:
				for i, lhs := range n.Lhs {
					if id, ok := lhs.(*ast.Ident); ok && i < len(n.Rhs) {
						if _, ok := wantSwitch[id.Name]; ok {
							var b strings.Builder
							if err := format.Node(&b, fset, n.Rhs[i]); err != nil {
								t.Fatal(err)
							}
							switches[id.Name] = append(switches[id.Name], strings.Join(strings.Fields(b.String()), " "))
						}
					}
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("found no runServe in main.go: the pin reads nothing")
	}
	for _, fun := range []string{"enrich.NewMusicBrainzClient", "enrich.NewCoverArtClient", "enrich.NewDeezerClient",
		"enrich.NewAtlasPremiumFetcher", "acoustid.NewClient"} {
		if built[fun] == 0 {
			t.Errorf("runServe builds no %s: the pin reads nothing of it", fun)
		}
	}
	for name, rhs := range wantSwitch {
		if got := switches[name]; len(got) != 1 || got[0] != rhs {
			t.Errorf("runServe's %s = %q, want %q alone", name, got, rhs)
		}
	}
}
