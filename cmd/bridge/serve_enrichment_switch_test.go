package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/atlasharvest"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// enrich.enabled and enrich.deezerEnabled in serve (backlog B617): what the
// fingerprint path does with enrichment off, and that serve wires the
// switches into everything that asks the upstreams, and into nothing else.

// switchCounter is a server that counts and logs the requests it answers.
type switchCounter struct {
	srv  *httptest.Server
	mu   sync.Mutex
	log  []string
	hold func(r *http.Request)
}

// newSwitchCounter starts a plain-HTTP counting server answering with answer.
func newSwitchCounter(t *testing.T, answer http.HandlerFunc) *switchCounter {
	t.Helper()
	c := &switchCounter{}
	c.srv = httptest.NewServer(c.handler(answer))
	t.Cleanup(c.srv.Close)
	return c
}

// handler logs a request, lets hold decide when it is answered, and answers
// nothing once the client has given up (build-ci-tests.md, B526).
func (c *switchCounter) handler(answer http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		c.mu.Lock()
		c.log = append(c.log, r.Method+" "+r.URL.RequestURI())
		hold := c.hold
		c.mu.Unlock()
		if hold != nil {
			hold(r)
			if r.Context().Err() != nil {
				panic(http.ErrAbortHandler)
			}
		}
		answer(w, r)
	})
}

func (c *switchCounter) requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.log...)
}

func (c *switchCounter) count() int { return len(c.requests()) }

func (c *switchCounter) asked(prefix string) int {
	n := 0
	for _, r := range c.requests() {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (c *switchCounter) setHold(hold func(r *http.Request)) {
	c.mu.Lock()
	c.hold = hold
	c.mu.Unlock()
}

const (
	switchAcoustID   = "9ff43b6a-4f16-427c-93c2-92307ca505e0"
	switchArtistMBID = "b10bbbfc-cf9e-42e0-be17-e2c3e1d2600d"
	// switchDuration is the length the candidates' rows carry, which the
	// canned fingerprint and the AcoustID answer agree with.
	switchDuration = 243.55
)

// switchAcoustIDMatch is an AcoustID answer the gate accepts for a track of
// switchDuration seconds by "Some Artist".
var switchAcoustIDMatch = fmt.Sprintf(`{"status":"ok","results":[{"id":%q,"score":0.98,"recordings":[`+
	`{"id":"c0ffee00-0000-4000-8000-000000000001","title":"T","sources":10,"duration":243,`+
	`"artists":[{"id":%q,"name":"Some Artist"}]}]}]}`, switchAcoustID, switchArtistMBID)

// sweepSwitchFixture is a fingerprint sweeper over a store and a root of its
// own, whose candidates are rows the enricher has stamped with no artist
// MBID, and an AcoustID that counts what it is asked and matches each.
type sweepSwitchFixture struct {
	store    *manifest.Store
	acoustid *switchCounter
	sweeper  *fingerprintSweeper
}

func newSweepSwitchFixture(t *testing.T, names ...string) *sweepSwitchFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	// t.Cleanup, not defer, so the loop's drain runs first.
	t.Cleanup(func() { _ = store.Close() })
	dur := switchDuration
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte("audio"), 0o644); err != nil {
			t.Fatal(err)
		}
		tr := &manifest.Track{Path: name, Size: 5, ModTime: time.Now(), Duration: &dur, Artist: "Some Artist"}
		if err := store.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
		if err := store.MarkEnriched(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	f := &sweepSwitchFixture{store: store}
	f.acoustid = newSwitchCounter(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, switchAcoustIDMatch)
	})
	f.sweeper = &fingerprintSweeper{
		store:     store,
		resolver:  bridgefs.New([]string{root}),
		client:    acoustid.NewClient(f.acoustid.srv.URL, "k", "ua", f.acoustid.srv.Client()),
		cache:     acoustid.NewCache(16),
		workers:   1,
		maxPerRun: 10,
		length:    time.Minute,
		compute: func(context.Context, string, time.Duration) (acoustid.Fingerprint, error) {
			return acoustid.Fingerprint{Value: "AQABz0mUaEkSRZEG", Duration: switchDuration, DistinctB64: 40}, nil
		},
	}
	return f
}

// requeued says whether a sweep re-queued path for the enricher, which
// stamps enriched_at back to 0.
func (f *sweepSwitchFixture) requeued(t *testing.T, path string) bool {
	t.Helper()
	rows, err := f.store.UnenrichedTracks(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Path == path {
			return true
		}
	}
	return false
}

// runSwitchSweeper runs the sweeper's loop with gate and enrichmentOn, its
// settle delay a millisecond, until the test ends.
func runSwitchSweeper(t *testing.T, s *fingerprintSweeper, gate, enrichmentOn func() bool, nudge chan struct{}, status *sweepStatus[admin.FingerprintSweepCounts]) {
	t.Helper()
	oldSettle := fingerprintSweeperSettleDelay
	fingerprintSweeperSettleDelay = time.Millisecond
	t.Cleanup(func() { fingerprintSweeperSettleDelay = oldSettle })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runFingerprintSweeper(ctx, s, gate, enrichmentOn, staticInterval(time.Hour), nudge, nil, status)
	}()
	drainLoopOnCleanup(t, cancel, done, "the fingerprint sweeper")
}

// eventually waits for cond, failing the test with what after 10s.
func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestFingerprintingStopsWithEnrichment: with fingerprinting switched on and
// enrich.enabled off, the fingerprint gate is closed, so no fingerprint goes
// to AcoustID and no row is re-queued for an enricher turn it would not get,
// and the Jobs card says why, "enrichment_off". With enrichment on again the
// same loop sweeps, asks AcoustID and re-queues what it resolved: the
// fixture can do what the switch stopped (backlog B617).
func TestFingerprintingStopsWithEnrichment(t *testing.T) {
	f := newSweepSwitchFixture(t, "a.flac")
	off := false
	var live atomic.Pointer[config.Config]
	live.Store(&config.Config{
		Fingerprint: config.FingerprintConfig{Enabled: true, APIKey: "k"},
		Enrich:      config.EnrichConfig{Enabled: &off},
	})
	// fpcalc answers, as far as the probe's cache knows: only enrichment
	// keeps the gate closed.
	ready, degraded := fingerprintGates(live.Load, &fingerprintToolchainCache{at: time.Now()})
	if ready() {
		t.Error("fingerprinting reads ready with enrichment off")
	}
	if got := degraded(); got != fingerprintEnrichmentOff {
		t.Errorf("degraded reason = %q, want %q", got, fingerprintEnrichmentOff)
	}
	status := &sweepStatus[admin.FingerprintSweepCounts]{}
	card := fingerprintStateClosure(func() bool { return live.Load().Fingerprint.Enabled }, ready, degraded, status)()
	if card == nil || !card.Enabled || card.Active || card.DegradedReason != fingerprintEnrichmentOff {
		t.Errorf("the Jobs card = %+v, want switched on, not active, degraded for enrichment_off", card)
	}

	var asked atomic.Int64
	gate := func() bool {
		asked.Add(1)
		return ready()
	}
	nudge := make(chan struct{}, 1)
	runSwitchSweeper(t, f.sweeper, gate, func() bool { return live.Load().Enrich.EffectiveEnabled() }, nudge, status)
	eventually(t, func() bool { return asked.Load() >= 1 }, "the boot pass to ask the gate")
	nudge <- struct{}{}
	eventually(t, func() bool { return asked.Load() >= 2 }, "the nudged pass to ask the gate")
	if got := f.acoustid.requests(); len(got) != 0 {
		t.Errorf("AcoustID was asked with enrichment off:\n%s", strings.Join(got, "\n"))
	}
	if f.requeued(t, "a.flac") {
		t.Error("a.flac was re-queued for the enricher with enrichment off")
	}

	live.Store(&config.Config{Fingerprint: config.FingerprintConfig{Enabled: true, APIKey: "k"}})
	nudge <- struct{}{}
	eventually(t, func() bool { return f.requeued(t, "a.flac") }, "the resolved row to be re-queued once enrichment is on")
	if f.acoustid.count() == 0 {
		t.Error("the sweep that re-queued a.flac asked AcoustID nothing")
	}
	if card := fingerprintStateClosure(func() bool { return true }, ready, degraded, status)(); !card.Active || card.DegradedReason != "" {
		t.Errorf("the Jobs card with enrichment on = %+v, want active", card)
	}
}

// TestASweepInFlightStopsWithEnrichment: a fingerprint sweep under way stops
// when enrich.enabled is switched off, as at a shutdown: an fpcalc held at
// the switch-off is killed, a lookup held there is abandoned, no request
// follows, and what the sweep resolved before is not re-queued (backlog
// B617).
func TestASweepInFlightStopsWithEnrichment(t *testing.T) {
	for _, held := range []string{"a held fpcalc", "a held lookup"} {
		t.Run(held, func(t *testing.T) {
			f := newSweepSwitchFixture(t, "a.flac", "b.flac")
			var on atomic.Bool
			on.Store(true)
			holding := make(chan struct{})
			cancelled := make(chan struct{})
			hold := func(ctx context.Context) {
				close(holding)
				select {
				case <-ctx.Done():
					close(cancelled)
				case <-time.After(10 * time.Second):
				}
			}
			wantLookups := 1
			if held == "a held fpcalc" {
				answer := f.sweeper.compute
				f.sweeper.compute = func(ctx context.Context, abs string, length time.Duration) (acoustid.Fingerprint, error) {
					if filepath.Base(abs) == "b.flac" {
						hold(ctx)
						return acoustid.Fingerprint{}, ctx.Err()
					}
					return answer(ctx, abs, length)
				}
			} else {
				wantLookups = 2
				var n atomic.Int64
				f.acoustid.setHold(func(r *http.Request) {
					if n.Add(1) == 2 {
						hold(r.Context())
					}
				})
			}
			status := &sweepStatus[admin.FingerprintSweepCounts]{}
			runSwitchSweeper(t, f.sweeper, func() bool { return true }, on.Load, nil, status)
			select {
			case <-holding:
			case <-time.After(10 * time.Second):
				t.Fatal("the sweep never reached b.flac")
			}
			on.Store(false)
			select {
			case <-cancelled:
			case <-time.After(5 * time.Second):
				t.Fatal("what the sweep held was not stopped within 5s of the switch-off")
			}
			eventually(t, func() bool {
				running, _, lastEnd, _, _ := status.snapshot()
				return !running && !lastEnd.IsZero()
			}, "the sweep to end")
			if got := f.acoustid.count(); got != wantLookups {
				t.Errorf("AcoustID was asked %d time(s), want %d:\n%s", got, wantLookups, strings.Join(f.acoustid.requests(), "\n"))
			}
			if f.requeued(t, "a.flac") {
				t.Error("the sweep the switch stopped re-queued a.flac")
			}
		})
	}
}

// serveSwitchMirror is a counting MusicBrainz and Cover Art Archive mirror:
// MusicBrainz knows one artist and no release, so the enricher goes on to
// the artist's portrait; the Cover Art Archive has no cover.
func serveSwitchMirror(t *testing.T) *switchCounter {
	t.Helper()
	return newSwitchCounter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/ws/2/artist"):
			fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":"Artist"}]}`, switchArtistMBID)
		case strings.HasPrefix(r.URL.Path, "/ws/2/release"):
			_, _ = io.WriteString(w, `{"releases":[]}`)
		default:
			http.NotFound(w, r)
		}
	})
}

// TestServeWiresTheEnrichmentGate boots the real serve over a library of two
// tagged tracks, with its MusicBrainz and Cover Art Archive bases at a
// counting mirror and Deezer's at another (serveOpts.deezerBase). With
// enrich.enabled false in the file, or BRIDGE_ENRICH_ENABLED=false, neither
// sees a request through turns of the enricher after the scan, the manifest a
// paired device reads carries no enrichmentProgress, and the portrait of an
// artist whose track waits for the enricher is 404 no_image where it would be
// 202 pending (a row seeded before the boot, whose file is gone and which the
// boot's scan counts missing once); with enrich.deezerEnabled false the
// enricher asks the mirror and Deezer sees nothing; and with both on, both are
// asked and the manifest carries the block, so the seams reach the clients
// and serve hands the switch to the manifest and the artwork routes (backlog
// B617).
func TestServeWiresTheEnrichmentGate(t *testing.T) {
	persistentMedium(t)
	const waiting = "66666666-6666-4666-8666-666666666666"
	type leg struct {
		name      string
		enrich    string
		env       map[string]string
		mirrorOff bool // the mirror must see nothing
	}
	for _, l := range []leg{
		{name: "enrich.enabled false", enrich: "  enabled: false\n", mirrorOff: true},
		{name: "BRIDGE_ENRICH_ENABLED=false", env: map[string]string{"BRIDGE_ENRICH_ENABLED": "false"}, mirrorOff: true},
		{name: "enrich.deezerEnabled false", enrich: "  deezerEnabled: false\n"},
		{name: "both on"},
	} {
		t.Run(l.name, func(t *testing.T) {
			for k, v := range l.env {
				t.Setenv(k, v)
			}
			fill := func(lib string) {
				for _, name := range []string{"01 One.flac", "02 Two.flac"} {
					writeHiResFLACFixture(t, filepath.Join(lib, "Artist", "Album", name))
				}
				if l.mirrorOff {
					seedAWaitingArtist(t, filepath.Join(filepath.Dir(lib), "data"), waiting)
				}
			}
			mirror := serveSwitchMirror(t)
			deezer := newSwitchCounter(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"data":[]}`)
			})
			var asked atomic.Int64
			yaml := "enrich:\n  musicbrainzBaseURL: " + mirror.srv.URL + "/ws/2\n  coverArtBaseURL: " + mirror.srv.URL + "/caa\n" + l.enrich
			b := startConsoleBridge(t, yaml, fill, func(o *serveOpts) {
				o.deezerBase = deezer.srv.URL
				o.enricherPoll = 20 * time.Millisecond
				o.enricherAsked = func(bool) { asked.Add(1) }
			})
			waitForTracksIndexed(t, b.console, b.adminBase, 2, b.stderr)
			turns := func(n int64) {
				t.Helper()
				from := asked.Load()
				eventually(t, func() bool { return asked.Load() >= from+n }, fmt.Sprintf("%d more turns of the enricher", n))
			}
			mint := pairViaAdmin(t, t.Context(), b.console, b.adminBase+"/api/tokens",
				`{"name":"enrichment switch"}`, http.StatusCreated, b.stderr)
			token := linkQueryItems(t, mint.PairURL)["token"]
			if token == "" {
				t.Fatalf("the console's pairing link carries no token: %s", mint.PairURL)
			}
			var m map[string]json.RawMessage
			getJSON(t, b.phone, b.apiBase+"/v1/manifest", token, &m)
			_, progress := m["enrichmentProgress"]
			switch {
			case l.mirrorOff:
				turns(5)
				if got := mirror.requests(); len(got) != 0 {
					t.Errorf("the mirror was asked with enrichment off:\n%s", strings.Join(got, "\n"))
				}
				if got := deezer.requests(); len(got) != 0 {
					t.Errorf("Deezer was asked with enrichment off:\n%s", strings.Join(got, "\n"))
				}
				if progress {
					t.Errorf("the manifest carries enrichmentProgress with enrichment off: %s", m["enrichmentProgress"])
				}
				status, body := getBytesWithToken(t, b, "/v1/artist-image/"+waiting, token)
				if status != http.StatusNotFound || !strings.Contains(string(body), `"no_image"`) {
					t.Errorf("the waiting artist's portrait answered %d %s with enrichment off, want 404 no_image", status, body)
				}
			case l.enrich != "":
				eventually(t, func() bool { return mirror.asked("GET /ws/2/artist") > 0 }, "the enricher to ask the mirror for the artist")
				turns(5)
				if got := deezer.requests(); len(got) != 0 {
					t.Errorf("Deezer was asked with enrich.deezerEnabled false:\n%s", strings.Join(got, "\n"))
				}
				if !progress {
					t.Error("the manifest carries no enrichmentProgress with enrichment on")
				}
			default:
				eventually(t, func() bool { return deezer.count() > 0 }, "Deezer to be asked for the artist's portrait")
				if mirror.count() == 0 {
					t.Error("the mirror was asked nothing with enrichment on")
				}
				if !progress {
					t.Error("the manifest carries no enrichmentProgress with enrichment on")
				}
			}
		})
	}
}

// seedAWaitingArtist puts in the store of dataDir, before serve opens it, a
// track whose artist MBID is artist and which waits for the enricher: its file
// is not in the library, and the boot's scan counts it missing once, below
// the default three that delete a row.
func seedAWaitingArtist(t *testing.T, dataDir, artist string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	row := &manifest.Track{Path: "Gone/Album/01.flac", Size: 1, ModTime: time.Unix(1, 0),
		Artist: "Gone", Album: "Album", ArtistMBID: artist}
	if err := store.UpsertTrack(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestTheHarvestRunsWithEnrichmentOff: enrich.enabled stops the enricher's
// lookups and nothing of the Atlas harvest, its own opt-in to a service the
// operator chose. Booted with enrichment off and the harvest on, serve's
// harvest submits the library's release, polls for results and refetches a
// pending premium cover from Atlas, while the enrichment mirror is asked
// nothing (backlog B617).
func TestTheHarvestRunsWithEnrichmentOff(t *testing.T) {
	persistentMedium(t)
	const release = "77777777-7777-4777-8777-777777777777"
	atlas := &switchCounter{}
	atlas.srv = httptest.NewTLSServer(atlas.handler(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/atlas/harvest/submit":
			_, _ = io.WriteString(w, `{"accepted":1,"rejected":0}`)
		case r.URL.Path == "/v1/atlas/harvest/results":
			_, _ = io.WriteString(w, `{"results":[],"nextCursor":0}`)
		case strings.HasPrefix(r.URL.Path, "/release/"+release+"/front-"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(atlas.srv.Close)
	mirror := serveSwitchMirror(t)

	fill := func(lib string) {
		path := filepath.Join(lib, "Artist", "Album", "01 One.flac")
		writeFLACFixture(t, path, "ARTIST=Artist", "ALBUM=Album", "TITLE=One", "MUSICBRAINZ_ALBUMID="+release)
		// The harvest's first tick runs as serve starts, maybe before the
		// scan: its credential, the pending cover and the track's row are
		// in the data dir before the boot, so the tick finds the release.
		dataDir := filepath.Join(filepath.Dir(lib), "data")
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			t.Fatal(err)
		}
		state, err := atlasharvest.OpenStateStore(harvestStatePath(dataDir))
		if err != nil {
			t.Fatal(err)
		}
		if err := state.SetCredential("bh-token", atlas.srv.URL, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := state.AddPendingCovers([]string{release}); err != nil {
			t.Fatal(err)
		}
		store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		row := &manifest.Track{Path: "Artist/Album/01 One.flac", Size: info.Size(), ModTime: info.ModTime(),
			Artist: "Artist", Album: "Album", MusicBrainzAlbumID: release}
		if err := store.UpsertTrack(context.Background(), row); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
	yaml := "atlas:\n  enabled: true\n  harvestEnabled: true\n" +
		"enrich:\n  enabled: false\n  musicbrainzBaseURL: " + mirror.srv.URL + "/ws/2\n  coverArtBaseURL: " + mirror.srv.URL + "/caa\n"
	var asked atomic.Int64
	b := startConsoleBridge(t, yaml, fill, func(o *serveOpts) {
		o.harvestHTTP = atlas.srv.Client()
		o.enricherPoll = 20 * time.Millisecond
		o.enricherAsked = func(bool) { asked.Add(1) }
	})
	eventually(t, func() bool {
		return atlas.asked("POST /v1/atlas/harvest/submit") > 0 &&
			atlas.asked("GET /v1/atlas/harvest/results") > 0 &&
			atlas.asked("GET /release/"+release+"/front-") > 0
	}, "the harvest to submit, poll and refetch the pending cover")
	waitForTracksIndexed(t, b.console, b.adminBase, 1, b.stderr)
	from := asked.Load()
	eventually(t, func() bool { return asked.Load() >= from+5 }, "five turns of the enricher")
	if got := mirror.requests(); len(got) != 0 {
		t.Errorf("the enrichment mirror was asked with enrichment off:\n%s", strings.Join(got, "\n"))
	}
}
