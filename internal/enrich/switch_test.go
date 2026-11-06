package enrich

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestAnEnricherSwitchedOffAsksNoUpstream: with enrich.enabled off, the
// enricher asks MusicBrainz, the Cover Art Archive, Deezer and Atlas nothing,
// reads nothing of the store and stamps nothing, however long it runs
// (backlog B617). On main nothing turned it off: an enricher handed this
// gate went on asking every service about the two seeded tracks.
func TestAnEnricherSwitchedOffAsksNoUpstream(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, _ := switchEnricher(t, ups)
	seedSwitchTracks(t, store)

	var asked atomic.Int64
	e.WithEnabled(func() bool {
		asked.Add(1)
		return false
	})
	stop := runEnricher(t, e)
	// Five turns of the loop, or the first request: either is the enricher's
	// chance to ask.
	waitFor(t, func() bool { return asked.Load() >= 5 || ups.total() > 0 },
		"the enricher to ask its gate five times")
	stop()

	if got := ups.all(); len(got) != 0 {
		t.Errorf("an enricher switched off sent %d request(s):\n%s", len(got), strings.Join(got, "\n"))
	}
	waiting := unenrichedPaths(t, store)
	for _, tr := range switchTracks {
		if !waiting[tr.Path] {
			t.Errorf("%s was stamped by an enricher switched off", tr.Path)
		}
	}
	if done, skipped := e.Done(), e.skipped.Load(); done != 0 || skipped != 0 {
		t.Errorf("an enricher switched off counted %d done and %d skipped", done, skipped)
	}

	t.Run("it reads nothing of the store", func(t *testing.T) {
		e, store, _ := switchEnricher(t, newSwitchUpstreams(t))
		_ = store.Close() // any read of the store now fails, and Run says so
		var asked atomic.Int64
		e.WithEnabled(func() bool {
			asked.Add(1)
			return false
		})
		rec := loggingtest.Record(t)
		stop := runEnricher(t, e)
		waitFor(t, func() bool { return asked.Load() >= 5 || len(rec.Failures(msgListUnenriched)) > 0 },
			"the enricher to ask its gate five times")
		stop()
		if got := rec.Failures(msgListUnenriched); len(got) != 0 {
			t.Errorf("an enricher switched off listed the store's unenriched tracks:\n%s", strings.Join(got, "\n"))
		}
	})
}

// mainsRequests are the requests main's enricher sent the four services for
// switchTracks, in order for each service, captured from main's code
// (deterministic across runs).
var mainsRequests = []string{
	"musicbrainz GET /release/?query=release%3A%22Other+Album%22+AND+artist%3A%22Artist%22&fmt=json&limit=10",
	"musicbrainz GET /artist/?query=artist%3A%22Artist%22&fmt=json&limit=25",
	"coverart GET /release/" + switchSearchedMBID + "/front-1200",
	"coverart GET /release/" + switchReleaseMBID + "/front-1200",
	"deezer GET /search/artist?q=Artist&limit=5",
	"deezer GET /picture.jpg",
	"atlas GET /release/" + switchSearchedMBID + "/front-1200",
	"atlas GET /release/" + switchReleaseMBID + "/front-1200",
}

// TestEnrichmentIsOnWhereTheConfigSaysNothing: a config that says nothing of
// enrich.enabled or enrich.deezerEnabled enriches as main did. Through gates
// read from it, the enricher sends the four services the requests main's
// sent for the same tracks, and a load and a save of that config write
// neither key, so the file stays one an older binary loads (backlog B617).
func TestEnrichmentIsOnWhereTheConfigSaysNothing(t *testing.T) {
	cfgPath := writeSwitchConfig(t)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	ups := newSwitchUpstreams(t)
	e, store, _ := switchEnricher(t, ups)
	e.WithEnabled(cfg.Enrich.EffectiveEnabled).WithDeezerEnabled(cfg.Enrich.EffectiveDeezerEnabled)
	seedSwitchTracks(t, store)
	stop := runEnricher(t, e)
	waitFor(t, func() bool { return len(unenrichedPaths(t, store)) == 0 }, "both tracks to be enriched")
	stop()
	if got := ups.all(); !slices.Equal(got, mainsRequests) {
		t.Errorf("a config that says nothing sent:\n%s\nwant main's requests:\n%s",
			strings.Join(got, "\n"), strings.Join(mainsRequests, "\n"))
	}

	if err := config.Edit(cfgPath, func(c *config.Config) (bool, error) {
		c.LibraryName = "Saved Again"
		return true, nil
	}); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Enrich map[string]any `yaml:"enrich"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"enabled", "deezerEnabled"} {
		if v, ok := doc.Enrich[key]; ok {
			t.Errorf("a load and a save of a config that said nothing wrote enrich.%s: %v", key, v)
		}
	}
}

// writeSwitchConfig writes a config that loads and says nothing of
// enrichment, and returns its path.
func writeSwitchConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "libraryName: Switch\nlibraryRoots:\n  - " + yamlQuoted(lib) + "\ndataDir: " + yamlQuoted(filepath.Join(dir, "data")) + "\n"
	path := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// yamlQuoted writes s as a single-quoted YAML scalar, which keeps a Windows
// path's backslashes as they are.
func yamlQuoted(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// TestASwitchOffStopsAnEnrichmentInFlightAndStampsNothing: turning the
// switch off cancels the request the enricher is waiting on, within a
// second, and the pass it belonged to takes the path a shutdown takes: no
// stamp, no skip counted, no line, the row left at enriched_at = 0, and no
// request after it (backlog B617).
func TestASwitchOffStopsAnEnrichmentInFlightAndStampsNothing(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, _ := switchEnricher(t, ups)
	seedSwitchTracks(t, store)
	var on atomic.Bool
	on.Store(true)
	var asked atomic.Int64
	e.WithEnabled(func() bool {
		asked.Add(1)
		return on.Load()
	})

	held := make(chan struct{})
	abandoned := make(chan struct{})
	var first sync.Once
	ups.mb.setHold(func(r *http.Request) {
		isFirst := false
		first.Do(func() { isFirst = true })
		if !isFirst {
			return
		}
		close(held)
		select {
		case <-r.Context().Done():
			close(abandoned)
		case <-time.After(10 * time.Second):
		}
	})
	rec := loggingtest.Record(t)
	stop := runEnricher(t, e)
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("the enricher sent MusicBrainz no request")
	}
	before := ups.total()

	on.Store(false)
	select {
	case <-abandoned:
	case <-time.After(5 * time.Second):
		t.Fatal("the request held at MusicBrainz was not cancelled within 5s of the switch-off")
	}
	waitFor(t, func() bool { return len(rec.Lines(msgEnrichmentSwitchedOff)) > 0 },
		"the enricher to say it was switched off")
	parked := asked.Load()
	waitFor(t, func() bool { return asked.Load() >= parked+3 }, "three turns of the loop after the switch-off")
	stop()

	if got := ups.total(); got != before {
		t.Errorf("%d request(s) after the switch-off:\n%s", got-before, strings.Join(ups.all(), "\n"))
	}
	waiting := unenrichedPaths(t, store)
	for _, tr := range switchTracks {
		if !waiting[tr.Path] {
			t.Errorf("%s was stamped although the switch stopped its pass", tr.Path)
		}
	}
	if done, skipped := e.Done(), e.skipped.Load(); done != 0 || skipped != 0 {
		t.Errorf("a pass the switch stopped counted %d done and %d skipped", done, skipped)
	}
	if got := e.SkipReasons(); len(got) != 0 {
		t.Errorf("a pass the switch stopped was given skip reasons: %v", got)
	}
	for _, msg := range []string{"enrichment skipped", msgMarkEnriched, msgMarkSkipped, "MB search"} {
		if got := rec.Lines(msg); len(got) != 0 {
			t.Errorf("a pass the switch stopped logged %q:\n%s", msg, strings.Join(got, "\n"))
		}
	}
}

// TestASwitchOnResumesTheQueuedRows: rows that waited while the switch was
// off are enriched, with what the upstreams answer, once it is on: off
// stamped none of them, as skipped or otherwise (backlog B617).
func TestASwitchOnResumesTheQueuedRows(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, _ := switchEnricher(t, ups)
	seedSwitchTracks(t, store)
	var on atomic.Bool
	var asked atomic.Int64
	e.WithEnabled(func() bool {
		asked.Add(1)
		return on.Load()
	})
	runEnricher(t, e)
	waitFor(t, func() bool { return asked.Load() >= 5 }, "five turns of the loop while the switch is off")
	if got := unenrichedPaths(t, store); len(got) != len(switchTracks) {
		t.Fatalf("%d of %d rows still wait while the switch is off", len(got), len(switchTracks))
	}

	on.Store(true)
	waitFor(t, func() bool { return len(unenrichedPaths(t, store)) == 0 },
		"the rows to be enriched once the switch is on")
	for _, tr := range switchTracks {
		got, err := store.GetTrack(context.Background(), tr.Path)
		if err != nil {
			t.Fatal(err)
		}
		if got.ArtistMBID != switchArtistMBID || got.ArtworkMBID == "" {
			t.Errorf("%s: artist %q, artwork %q, want the artist and the cover the upstreams gave",
				tr.Path, got.ArtistMBID, got.ArtworkMBID)
		}
	}
	if skipped := e.skipped.Load(); skipped != 0 {
		t.Errorf("%d row(s) were skipped", skipped)
	}
}

// TestDeezerOffAsksDeezerNothingAndStillFetchesCovers: with
// enrich.deezerEnabled off and enrich.enabled on, Deezer is asked nothing,
// and MusicBrainz and the covers go on as before: each track is stamped with
// its artist and its cover, and none with a portrait. The answer is not a
// miss, so the artist is not negative-cached and nothing is logged about it
// (backlog B617).
func TestDeezerOffAsksDeezerNothingAndStillFetchesCovers(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, artworkDir := switchEnricher(t, ups)
	seedSwitchTracks(t, store)
	e.WithDeezerEnabled(func() bool { return false })
	rec := loggingtest.Record(t)
	stop := runEnricher(t, e)
	waitFor(t, func() bool { return len(unenrichedPaths(t, store)) == 0 }, "both tracks to be enriched")
	stop()

	if got := ups.deezer.requests(); len(got) != 0 {
		t.Errorf("Deezer switched off was asked:\n%s", strings.Join(got, "\n"))
	}
	if ups.mb.count() == 0 || ups.caa.count() == 0 {
		t.Errorf("MusicBrainz asked %d time(s), the Cover Art Archive %d: both must go on with Deezer off",
			ups.mb.count(), ups.caa.count())
	}
	for _, mbid := range []string{switchReleaseMBID, switchSearchedMBID} {
		if !fileExists(ArtworkCachePath(artworkDir, mbid, DefaultCoverSize)) {
			t.Errorf("no cover cached for release %s", mbid)
		}
	}
	for _, tr := range switchTracks {
		got, err := store.GetTrack(context.Background(), tr.Path)
		if err != nil {
			t.Fatal(err)
		}
		if got.ArtistMBID != switchArtistMBID {
			t.Errorf("%s: artist %q, want %q", tr.Path, got.ArtistMBID, switchArtistMBID)
		}
	}
	if fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
		t.Error("a portrait was cached with Deezer off")
	}
	if e.deezerNegCache.Has(switchArtistMBID) {
		t.Error("the artist was negative-cached although Deezer was never asked")
	}
	if got := rec.Lines("artist image"); len(got) != 0 {
		t.Errorf("a portrait Deezer was not asked for was logged:\n%s", strings.Join(got, "\n"))
	}
}

// recoveredArtistMBID is the artist the fingerprint fallback names for the
// untagged track of TestAPortraitComesBackAfterDeezerOnAndRetryMissing.
const recoveredArtistMBID = "55555555-5555-4555-8555-555555555555"

// fixedAcoustic is a fingerprint verdict source answering from a map.
type fixedAcoustic map[string]AcousticMatch

func (f fixedAcoustic) LookupPath(p string) (AcousticMatch, bool) {
	m, ok := f[p]
	return m, ok
}

// TestAPortraitComesBackAfterDeezerOnAndRetryMissing: a track enriched while
// Deezer was off has no portrait, and gets one once Deezer is on and "Retry
// missing" queues it again (its artist-image facet,
// ResetEnrichedByArtistMBIDs), on the same enricher: the off answer was not
// negative-cached for the process's life. Both callers of the portrait fetch
// are driven: the text path's (resolveArtist) and the fingerprint fallback's
// (fetchRecoveredArtistImage), whose untagged track takes its artist from
// the verdict (backlog B617).
func TestAPortraitComesBackAfterDeezerOnAndRetryMissing(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, store, artworkDir := switchEnricher(t, ups)
	seedSwitchTracks(t, store)
	const untagged = "Unknown/01.flac"
	tr := manifest.Track{Path: untagged, Size: 1, ModTime: time.Now()}
	if err := store.UpsertTrack(context.Background(), &tr); err != nil {
		t.Fatal(err)
	}
	e.WithAcousticFallback(fixedAcoustic{untagged: {ArtistMBID: recoveredArtistMBID, ArtistName: "Recovered"}})

	var deezerOn atomic.Bool
	e.WithDeezerEnabled(deezerOn.Load)
	stop := runEnricher(t, e)
	waitFor(t, func() bool { return len(unenrichedPaths(t, store)) == 0 }, "every track to be enriched with Deezer off")
	if got := ups.deezer.count(); got != 0 {
		t.Fatalf("Deezer switched off was asked %d time(s)", got)
	}
	portraits := []string{ArtistImagePath(artworkDir, switchArtistMBID), ArtistImagePath(artworkDir, recoveredArtistMBID)}
	for _, p := range portraits {
		if fileExists(p) {
			t.Fatalf("%s was cached with Deezer off", filepath.Base(p))
		}
	}

	deezerOn.Store(true)
	if _, err := store.ResetEnrichedByArtistMBIDs(context.Background(), []string{switchArtistMBID, recoveredArtistMBID}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return fileExists(portraits[0]) && fileExists(portraits[1]) },
		"both portraits once Deezer is on and Retry missing queued their tracks")
	stop()
}

// TestDeezerOffAfterASearchSendsNoFetch: Deezer switched off while its
// search is in flight sends no image request after it: the search, already
// sent, ends by itself, and the fetch is asked of the switch first (backlog
// B617).
func TestDeezerOffAfterASearchSendsNoFetch(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, _, artworkDir := switchEnricher(t, ups)
	var deezerOn atomic.Bool
	deezerOn.Store(true)
	e.WithDeezerEnabled(deezerOn.Load)
	searching := make(chan struct{})
	release := make(chan struct{})
	ups.deezer.setHold(func(r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/search/") {
			return
		}
		close(searching)
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	})

	type result struct {
		ok  bool
		err error
	}
	answered := make(chan result, 1)
	go func() {
		ok, err := e.ensureArtistImageCached(context.Background(), switchArtistMBID, "Artist")
		answered <- result{ok, err}
	}()
	select {
	case <-searching:
	case <-time.After(5 * time.Second):
		t.Fatal("Deezer's search was never sent")
	}
	deezerOn.Store(false)
	close(release)
	var got result
	select {
	case got = <-answered:
	case <-time.After(10 * time.Second):
		t.Fatal("the portrait fetch did not answer")
	}
	if got.ok || !errors.Is(got.err, errDeezerOff) {
		t.Errorf("ok=%v err=%v, want errDeezerOff", got.ok, got.err)
	}
	if reqs := ups.deezer.requests(); len(reqs) != 1 || !strings.HasPrefix(reqs[0], "GET /search/artist") {
		t.Errorf("Deezer was asked %v, want the search alone", reqs)
	}
	if fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
		t.Error("a portrait was cached after Deezer was switched off")
	}
}

// TestCancelWhenOffCancelsOnceItsSwitchIsOff: the context CancelWhenOff
// returns ends when its predicate answers false, asked every interval, or
// when its parent ends; a nil predicate is never off, and one that answers
// false at the start is cancelled already.
func TestCancelWhenOffCancelsOnceItsSwitchIsOff(t *testing.T) {
	const every = 2 * time.Millisecond
	t.Run("off at the start", func(t *testing.T) {
		ctx, release := cancelWhenOff(context.Background(), func() bool { return false }, every)
		defer release()
		if ctx.Err() == nil {
			t.Error("a context whose switch is off at the start is not cancelled")
		}
	})
	t.Run("a nil switch", func(t *testing.T) {
		parent, cancel := context.WithCancel(context.Background())
		ctx, release := cancelWhenOff(parent, nil, every)
		defer release()
		if ctx.Err() != nil {
			t.Fatal("a nil switch cancelled the context")
		}
		cancel()
		<-ctx.Done()
	})
	t.Run("a switch-off", func(t *testing.T) {
		var on atomic.Bool
		on.Store(true)
		ctx, release := cancelWhenOff(context.Background(), on.Load, every)
		defer release()
		if ctx.Err() != nil {
			t.Fatal("cancelled while the switch is on")
		}
		on.Store(false)
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("not cancelled within 5s of the switch-off")
		}
	})
}

// TestTheEnricherSaysWhenItsSwitchesFlip: one Info line at the start for a
// switch that is off, and one at each flip the enricher sees.
func TestTheEnricherSaysWhenItsSwitchesFlip(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, _, _ := switchEnricher(t, ups)
	var on, deezer atomic.Bool
	var asked atomic.Int64
	e.WithEnabled(func() bool {
		asked.Add(1)
		return on.Load()
	}).WithDeezerEnabled(deezer.Load)
	rec := loggingtest.Record(t)
	runEnricher(t, e)
	turns := func(n int64, what string) {
		t.Helper()
		from := asked.Load()
		waitFor(t, func() bool { return asked.Load() >= from+n }, what)
	}
	turns(3, "three turns with both switches off")
	on.Store(true)
	turns(3, "three turns after enrichment was switched on")
	deezer.Store(true)
	turns(3, "three turns after Deezer was switched on")
	on.Store(false)
	deezer.Store(false)
	turns(3, "three turns after both were switched off")

	for _, c := range []struct {
		msg  string
		want int
	}{
		{msgEnrichmentOff, 1}, {msgDeezerOff, 1},
		{msgEnrichmentSwitchedOn, 1}, {msgDeezerSwitchedOn, 1},
		{msgEnrichmentSwitchedOff, 1}, {msgDeezerSwitchedOff, 1},
	} {
		if got := rec.Lines(c.msg); len(got) != c.want {
			t.Errorf("%q logged %d time(s), want %d", c.msg, len(got), c.want)
		}
	}
}
