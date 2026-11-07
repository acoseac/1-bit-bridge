package admin

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// enrichSwitchRows are the two settings rows of backlog B617: the field the
// console sends, the key bridge.yaml holds, and the value the running config
// resolves it to.
var enrichSwitchRows = []struct {
	field, key string
	resolved   func(config.EnrichConfig) bool
}{
	{"enrichEnabled", "enabled", config.EnrichConfig.EffectiveEnabled},
	{"enrichDeezerEnabled", "deezerEnabled", config.EnrichConfig.EffectiveDeezerEnabled},
}

// enrichKeysOnDisk reads the enrich block of the bridge.yaml at path as the
// file holds it, key by key.
func enrichKeysOnDisk(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Enrich map[string]any `yaml:"enrich"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("bridge.yaml does not parse: %v", err)
	}
	return doc.Enrich
}

// TestTheEnrichmentSwitchesSaveLiveAndWriteOnlyOff drives the real settings
// PATCH for enrichEnabled and enrichDeezerEnabled (backlog B617). Each answers
// live and the running config follows it; the file holds the key as false
// once it is switched off and holds no key once it is on again, so the file
// goes back to one an older binary loads; GET /api/settings shows the
// resolved value; and where the control plane owns the field the save is
// refused whole and changes nothing, as its siblings' are
// (TestManagedSettingsAreRefused).
func TestTheEnrichmentSwitchesSaveLiveAndWriteOnlyOff(t *testing.T) {
	for _, row := range enrichSwitchRows {
		t.Run(row.field, func(t *testing.T) {
			srv, _, cfgPath := newTestServer(t)
			h := srv.Handler()
			running := func() bool { return row.resolved(srv.deps.CfgHolder.Load().Enrich) }
			shown := func() any {
				t.Helper()
				var got map[string]any
				if code := doJSON(t, h, "GET", "/api/settings", nil, &got); code != 200 {
					t.Fatalf("GET /api/settings: status %d", code)
				}
				return got[row.field]
			}
			save := func(on bool) settingsPatchResponse {
				t.Helper()
				var resp settingsPatchResponse
				if code := doJSON(t, h, "PATCH", "/api/settings", map[string]any{row.field: on}, &resp); code != 200 {
					t.Fatalf("PATCH %s=%v: status %d", row.field, on, code)
				}
				return resp
			}
			onDisk := func() (any, bool) {
				v, ok := enrichKeysOnDisk(t, cfgPath)[row.key]
				return v, ok
			}

			if !running() || shown() != true {
				t.Fatalf("a config that says nothing: running %v, shown %v; want on", running(), shown())
			}
			if v, ok := onDisk(); ok {
				t.Fatalf("the fixture's file already holds enrich.%s: %v", row.key, v)
			}

			resp := save(false)
			if got := resp.Fields[row.field].Status; got != applyLive {
				t.Errorf("off: %s answered %q, want %q", row.field, got, applyLive)
			}
			if resp.RestartRequired {
				t.Error("off: the save asked for a restart; the switch is live")
			}
			if running() {
				t.Error("off: the running config still has it on")
			}
			if v, ok := onDisk(); !ok || v != false {
				t.Errorf("off: the file holds enrich.%s = %v (present %v), want false", row.key, v, ok)
			}
			if got := shown(); got != false {
				t.Errorf("off: GET /api/settings shows %v, want false", got)
			}
			if got := save(false).Fields[row.field].Status; got != applyUnchanged {
				t.Errorf("off again: answered %q, want %q", got, applyUnchanged)
			}

			resp = save(true)
			if got := resp.Fields[row.field].Status; got != applyLive {
				t.Errorf("on: %s answered %q, want %q", row.field, got, applyLive)
			}
			if !running() {
				t.Error("on: the running config still has it off")
			}
			if v, ok := onDisk(); ok {
				t.Errorf("on: the file still holds enrich.%s = %v; on is written as no key", row.key, v)
			}
			if got := shown(); got != true {
				t.Errorf("on: GET /api/settings shows %v, want true", got)
			}

			if err := srv.deps.CfgHolder.Update(srv.deps.CfgPath, func(c *config.Config) error {
				c.Deployment.ManagedSettings = []string{row.field}
				return nil
			}); err != nil {
				t.Fatalf("arrange the managed field: %v", err)
			}
			var refusal map[string]string
			if code := doJSON(t, h, "PATCH", "/api/settings",
				map[string]any{row.field: false, "libraryName": "Renamed"}, &refusal); code != 403 {
				t.Errorf("managed: status %d, want 403", code)
			}
			if refusal["error"] != "managed-setting" {
				t.Errorf("managed: the refusal's code is %q, want managed-setting", refusal["error"])
			}
			if !running() {
				t.Error("managed: the refused save switched it off")
			}
			if v, ok := onDisk(); ok {
				t.Errorf("managed: the refused save wrote enrich.%s = %v", row.key, v)
			}
			if got := srv.deps.CfgHolder.Load().LibraryName; got == "Renamed" {
				t.Error("managed: the free field beside the managed one was saved; a refusal is whole")
			}
		})
	}
}

// The retry fixture's rows, in one album folder, each one a retry with
// enrichment on moves: retryGapPath is enriched and has no cover (the miss
// predicate's artwork arm), so ResetEnrichedMisses queues it again, and it
// holds a fingerprint no-match marker, which the fingerprint clear removes;
// retryWholePath is enriched with all three MBIDs, and its artist has no
// portrait on disk, so the artist-image facet alone queues it again.
const (
	retryFolder      = "Album"
	retryGapPath     = "Album/01.flac"
	retryWholePath   = "Album/02.flac"
	retryArtistMBID  = "dddddddd-1111-4111-8111-111111111111"
	retryReleaseMBID = "eeeeeeee-1111-4111-8111-111111111111"
	retryNoBookMBID  = "eeeeeeee-2222-4222-8222-222222222222"
)

// retrySwitchFixture is a console over the retry rows, with the calls of the
// two facets that leave no row behind counted. The artist-image facet's read
// of the portraits is not one of them: the library-wide answer's card reads
// the same set for its coverage, so that facet is seen by the row it queues.
type retrySwitchFixture struct {
	srv      *Server
	forgets  atomic.Int32 // FingerprintForget, the fingerprint clear's
	harvests atomic.Int32 // HarvestForceSubmit, the harvest's facet
}

// newRetrySwitchFixture seeds the retry rows and sets the switches with
// configure, as a save would leave the running config.
func newRetrySwitchFixture(t *testing.T, configure func(*config.Config)) *retrySwitchFixture {
	t.Helper()
	srv, _, _ := newTestServer(t)
	f := &retrySwitchFixture{srv: srv}
	ctx := context.Background()
	gap := &manifest.Track{Path: retryGapPath, Size: 1, ModTime: time.Unix(1, 0),
		ArtistMBID: retryArtistMBID, MusicBrainzAlbumID: retryReleaseMBID}
	whole := &manifest.Track{Path: retryWholePath, Size: 1, ModTime: time.Unix(1, 0),
		ArtworkMBID: retryReleaseMBID, ArtistMBID: retryArtistMBID, MusicBrainzAlbumID: retryReleaseMBID}
	for _, tr := range []*manifest.Track{gap, whole} {
		if err := srv.deps.Manifest.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
		if err := srv.deps.Manifest.MarkEnriched(ctx, tr); err != nil {
			t.Fatal(err)
		}
	}
	if err := srv.deps.Manifest.SetAcoustIDNoMatch(ctx, retryGapPath, 1, 1); err != nil {
		t.Fatal(err)
	}
	srv.deps.ArtistImages = func() (map[string]string, error) { return map[string]string{}, nil }
	srv.deps.FingerprintForget = func(string) int { f.forgets.Add(1); return 0 }
	srv.deps.HarvestForceSubmit = func() bool { f.harvests.Add(1); return true }
	next := config.Clone(srv.deps.CfgHolder.Load())
	configure(next)
	srv.deps.CfgHolder.Store(next)
	return f
}

// enrichmentOff, deezerOff and harvestOn set the switches the retries read.
func enrichmentOff(c *config.Config) { off := false; c.Enrich.Enabled = &off }
func deezerOff(c *config.Config)     { off := false; c.Enrich.DeezerEnabled = &off }
func harvestOn(c *config.Config)     { c.Atlas.Enabled, c.Atlas.HarvestEnabled = true, true }

// retryRows is what a retry can move: the rows waiting for the enricher, the
// rows holding a fingerprint no-match marker, and the library's watermark,
// the revision a paired device's delta sync starts from.
type retryRows struct {
	Unenriched []string
	NoMatch    []string
	Watermark  int64
}

// rows reads the retry rows as they are.
func (f *retrySwitchFixture) rows(t *testing.T) retryRows {
	t.Helper()
	ctx := context.Background()
	st := f.srv.deps.Manifest
	var out retryRows
	queued, err := st.UnenrichedTracks(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range queued {
		out.Unenriched = append(out.Unenriched, tr.Path)
	}
	sort.Strings(out.Unenriched)
	marks, err := st.FreshAcoustIDNoMatches(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for p := range marks {
		out.NoMatch = append(out.NoMatch, p)
	}
	sort.Strings(out.NoMatch)
	if out.Watermark, _, err = st.LibraryWatermark(ctx); err != nil {
		t.Fatal(err)
	}
	return out
}

// retryAnswer is what either retry route answers: the two responses' fields,
// and the error envelope of a refusal.
type retryAnswer struct {
	ResetTracks        int64  `json:"resetTracks"`
	ArtistImageResets  int64  `json:"artistImageResets"`
	HarvestResubmitted bool   `json:"harvestResubmitted"`
	BookletChecksReset int64  `json:"bookletChecksReset"`
	BookletFetchNudges int    `json:"bookletFetchNudges"`
	EnrichmentOff      bool   `json:"enrichmentOff"`
	DeezerOff          bool   `json:"deezerOff"`
	Error              string `json:"error"`
}

// retryRoutes are the console's two retries: the library-wide one and the
// folder-scoped one, each over the fixture's folder.
var retryRoutes = []struct {
	name, path string
	body       any
}{
	{"library-wide", "/api/enrichment/retry", nil},
	{"scoped", "/api/library/enrichment/retry", map[string]any{"path": retryFolder}},
}

// retry posts one route and decodes its answer.
func (f *retrySwitchFixture) retry(t *testing.T, path string, body any) (int, retryAnswer) {
	t.Helper()
	var got retryAnswer
	code := doJSON(t, f.srv.Handler(), "POST", path, body, &got)
	return code, got
}

// TestRetryWhileOffResetsNothing drives both retry routes with enrich.enabled
// off (backlog B617). Their resets, the artist-image facet and the fingerprint
// clear feed only the enricher, which takes no turn while it is off, so none
// of them runs and no row moves: every row of the fixture is one a retry with
// enrichment on queues again or clears (the "on" case shows it), and the
// library's watermark stays where it was. With no other facet that can run,
// both routes refuse with 409 enrichment-off, the scoped one before it reads
// its path, and neither stamps its rate guard, so a retry once the switch is
// on again runs at once. With the Atlas harvest running, its facet runs and
// the answer says that the enrichment part did nothing.
func TestRetryWhileOffResetsNothing(t *testing.T) {
	t.Run("on, the fixture moves", func(t *testing.T) {
		for _, r := range retryRoutes {
			f := newRetrySwitchFixture(t, func(*config.Config) {})
			before := f.rows(t)
			code, got := f.retry(t, r.path, r.body)
			if code != 200 || got.EnrichmentOff {
				t.Fatalf("%s: status %d, enrichmentOff %v; want 200 and false", r.name, code, got.EnrichmentOff)
			}
			after := f.rows(t)
			if len(after.Unenriched) != 2 || len(after.NoMatch) != 0 {
				t.Fatalf("%s with enrichment on: queued %v, markers %v (before: %+v); the fixture must be "+
					"one a retry moves, or the cases below prove nothing", r.name, after.Unenriched, after.NoMatch, before)
			}
		}
		// And the path the case below sends is one the scoped route refuses
		// as a path while enrichment is on.
		f := newRetrySwitchFixture(t, func(*config.Config) {})
		if code, got := f.retry(t, "/api/library/enrichment/retry", map[string]any{"path": "../outside"}); code != 400 {
			t.Fatalf("scoped, enrichment on, a path above the library: status %d (%s), want 400", code, got.Error)
		}
	})

	t.Run("nothing else can run", func(t *testing.T) {
		for _, r := range retryRoutes {
			f := newRetrySwitchFixture(t, enrichmentOff)
			before := f.rows(t)
			code, got := f.retry(t, r.path, r.body)
			if code != 409 || got.Error != errCodeEnrichmentOff {
				t.Errorf("%s: status %d, code %q; want 409 %s", r.name, code, got.Error, errCodeEnrichmentOff)
			}
			f.requireNothingMoved(t, r.name, before)
			if n := f.harvests.Load(); n != 0 {
				t.Errorf("%s: the harvest was re-submitted %d time(s) on a bridge where it does not run", r.name, n)
			}

			// The refusal stamped no rate guard: on again, the retry runs.
			next := config.Clone(f.srv.deps.CfgHolder.Load())
			next.Enrich.Enabled = nil
			f.srv.deps.CfgHolder.Store(next)
			if code, _ := f.retry(t, r.path, r.body); code != 200 {
				t.Errorf("%s: the first retry once enrichment is on answered %d, want 200: "+
					"the refusal held the rate guard", r.name, code)
			}
		}

		// The scoped route refuses before it reads its path: a path it
		// would answer 400 for is refused as off.
		f := newRetrySwitchFixture(t, enrichmentOff)
		if code, got := f.retry(t, "/api/library/enrichment/retry", map[string]any{"path": "../outside"}); code != 409 ||
			got.Error != errCodeEnrichmentOff {
			t.Errorf("scoped, a path it refuses: status %d, code %q; want 409 %s before the path is read",
				code, got.Error, errCodeEnrichmentOff)
		}
	})

	t.Run("the harvest's facet runs", func(t *testing.T) {
		for _, r := range retryRoutes {
			f := newRetrySwitchFixture(t, func(c *config.Config) { enrichmentOff(c); harvestOn(c) })
			before := f.rows(t)
			code, got := f.retry(t, r.path, r.body)
			if code != 200 {
				t.Fatalf("%s: status %d (%s), want 200", r.name, code, got.Error)
			}
			if !got.EnrichmentOff || !got.HarvestResubmitted || f.harvests.Load() != 1 {
				t.Errorf("%s: enrichmentOff %v, harvestResubmitted %v, harvest calls %d; want true, true, 1",
					r.name, got.EnrichmentOff, got.HarvestResubmitted, f.harvests.Load())
			}
			if got.ResetTracks != 0 || got.ArtistImageResets != 0 || got.DeezerOff {
				t.Errorf("%s: answered resetTracks %d, artistImageResets %d, deezerOff %v; "+
					"with enrichment off no enricher facet runs", r.name, got.ResetTracks, got.ArtistImageResets, got.DeezerOff)
			}
			f.requireNothingMoved(t, r.name, before)
		}
	})
}

// requireNothingMoved fails where a retry moved a row, or forgot the
// session's fingerprint answers, which only the enricher's facets do.
func (f *retrySwitchFixture) requireNothingMoved(t *testing.T, route string, before retryRows) {
	t.Helper()
	if after := f.rows(t); !reflect.DeepEqual(after, before) {
		t.Errorf("%s: the rows moved: before %+v, after %+v", route, before, after)
	}
	if n := f.forgets.Load(); n != 0 {
		t.Errorf("%s: the fingerprint clear ran (%d call(s) to forget the session's answers)", route, n)
	}
}

// TestTheScopedRetryKeepsItsBookletsWhileOff: the folder-scoped retry's
// booklet facet feeds the harvest's booklet loops, never the enricher, so it
// runs with enrich.enabled off (backlog B617): a release Atlas had no booklet
// for is checked again and one it has and the bridge has not fetched is
// nudged, while no enricher facet runs and no row moves. A bridge that wires
// the booklets is one whose harvest client was built at its start, though the
// harvest may be off by now; this one is.
func TestTheScopedRetryKeepsItsBookletsWhileOff(t *testing.T) {
	f := newRetrySwitchFixture(t, enrichmentOff)
	ctx := context.Background()
	st := f.srv.deps.Manifest
	// A second release in the folder, whose booklet Atlas has not had.
	if err := st.UpsertTrack(ctx, &manifest.Track{Path: "Album/03.flac", Size: 1, ModTime: time.Unix(1, 0),
		ArtworkMBID: retryNoBookMBID, ArtistMBID: retryArtistMBID, MusicBrainzAlbumID: retryNoBookMBID}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBookletAvailability(ctx, retryReleaseMBID, true, "etag", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBookletAvailability(ctx, retryNoBookMBID, false, "", 0); err != nil {
		t.Fatal(err)
	}
	f.srv.deps.BookletPath = func(mbid string) string { return "/nonexistent/" + mbid + ".pdf" }
	var nudged []string
	f.srv.deps.BookletNudge = func(mbid string) { nudged = append(nudged, mbid) }
	before := f.rows(t)

	code, got := f.retry(t, "/api/library/enrichment/retry", map[string]any{"path": retryFolder})
	if code != 200 {
		t.Fatalf("status %d (%s), want 200: the booklets can still run", code, got.Error)
	}
	if !got.EnrichmentOff {
		t.Error("the answer does not say the enrichment part did nothing")
	}
	if got.BookletChecksReset != 1 {
		t.Errorf("bookletChecksReset = %d, want 1 (the release Atlas had no booklet for)", got.BookletChecksReset)
	}
	if got.BookletFetchNudges != 1 || len(nudged) != 1 || nudged[0] != retryReleaseMBID {
		t.Errorf("bookletFetchNudges = %d, nudged %v; want the one available, unfetched release %s",
			got.BookletFetchNudges, nudged, retryReleaseMBID)
	}
	if got.ResetTracks != 0 || got.ArtistImageResets != 0 {
		t.Errorf("resetTracks %d, artistImageResets %d; want 0 with enrichment off", got.ResetTracks, got.ArtistImageResets)
	}
	f.requireNothingMoved(t, "scoped", before)
}

// TestRetryWithDeezerOffSkipsTheArtistImageFacet: with enrich.deezerEnabled
// off and enrichment on (backlog B617), both retries run their other enricher
// facets and skip the artist-image one, which would re-run each portrait-less
// row and stamp it again with no portrait, and their answers say so. The
// whole row, which only that facet queues again (the case with Deezer on shows
// it does), stays enriched; the gap row is queued and its marker cleared as
// ever.
func TestRetryWithDeezerOffSkipsTheArtistImageFacet(t *testing.T) {
	for _, r := range retryRoutes {
		t.Run(r.name, func(t *testing.T) {
			on := newRetrySwitchFixture(t, func(*config.Config) {})
			if code, got := on.retry(t, r.path, r.body); code != 200 || got.DeezerOff {
				t.Fatalf("Deezer on: status %d, deezerOff %v", code, got.DeezerOff)
			}
			if got := on.rows(t).Unenriched; len(got) != 2 {
				t.Fatalf("Deezer on: queued %v, want both rows; the whole row must be one the "+
					"artist-image facet queues, or the case below proves nothing", got)
			}

			f := newRetrySwitchFixture(t, deezerOff)
			code, got := f.retry(t, r.path, r.body)
			if code != 200 {
				t.Fatalf("status %d (%s), want 200", code, got.Error)
			}
			if !got.DeezerOff || got.EnrichmentOff {
				t.Errorf("deezerOff %v, enrichmentOff %v; want true, false", got.DeezerOff, got.EnrichmentOff)
			}
			if got.ResetTracks != 1 || got.ArtistImageResets != 0 {
				t.Errorf("resetTracks %d, artistImageResets %d; want the gap row alone, and no portrait reset",
					got.ResetTracks, got.ArtistImageResets)
			}
			rows := f.rows(t)
			if len(rows.Unenriched) != 1 || rows.Unenriched[0] != retryGapPath {
				t.Errorf("queued %v, want only %s: the artist-image facet ran", rows.Unenriched, retryGapPath)
			}
			if len(rows.NoMatch) != 0 {
				t.Errorf("markers %v: with Deezer off the fingerprint clear still runs", rows.NoMatch)
			}
		})
	}
}

// TestTheEnrichmentCardsSayOffWhereTheSwitchIs: the Jobs snapshot's
// enrichment.off and the enrichment payload's off, deezerOff and
// harvestActive (backlog B617) follow the running config on every read, the
// payloads the Jobs card's badge and Pending row and the dashboard's Retry
// missing read.
func TestTheEnrichmentCardsSayOffWhereTheSwitchIs(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	read := func() (jobsSnapshotResponse, enrichmentResponse) {
		t.Helper()
		var jobs jobsSnapshotResponse
		if code := doJSON(t, h, "GET", "/api/jobs", nil, &jobs); code != 200 {
			t.Fatalf("GET /api/jobs: %d", code)
		}
		var card enrichmentResponse
		if code := doJSON(t, h, "GET", "/api/enrichment", nil, &card); code != 200 {
			t.Fatalf("GET /api/enrichment: %d", code)
		}
		return jobs, card
	}
	set := func(configure func(*config.Config)) {
		next := config.Clone(srv.deps.CfgHolder.Load())
		next.Enrich.Enabled, next.Enrich.DeezerEnabled = nil, nil
		next.Atlas.Enabled, next.Atlas.HarvestEnabled = false, false
		configure(next)
		srv.deps.CfgHolder.Store(next)
	}

	jobs, card := read()
	if jobs.Enrichment.Off || card.Off || card.DeezerOff || card.HarvestActive {
		t.Errorf("a config that says nothing: jobs off %v, card %+v; want everything on and no harvest",
			jobs.Enrichment.Off, card)
	}
	set(enrichmentOff)
	if jobs, card = read(); !jobs.Enrichment.Off || !card.Off || card.DeezerOff {
		t.Errorf("enrichment off: jobs off %v, card off %v, deezerOff %v; want true, true, false",
			jobs.Enrichment.Off, card.Off, card.DeezerOff)
	}
	set(func(c *config.Config) { deezerOff(c); harvestOn(c) })
	if jobs, card = read(); jobs.Enrichment.Off || card.Off || !card.DeezerOff || !card.HarvestActive ||
		!jobs.Enrichment.HarvestActive {
		t.Errorf("Deezer off, harvest on: jobs %+v, card off %v, deezerOff %v, harvestActive %v",
			jobs.Enrichment, card.Off, card.DeezerOff, card.HarvestActive)
	}
}

// TestTheSettingsPayloadSendsBothEnrichmentSwitchesAsTheirCheckboxes pins the
// two names in the Save payload's allowlist with the expression a checkbox
// needs (backlog B617): FormData holds "on" for a ticked box and nothing for
// a clear one, so each reads true only for "on" and false otherwise, which a
// cleared box must send for the switch to turn off. The guards beside it see
// that each name is read (TestEverySettingsFieldIsMappedIntoThePatchPayload)
// and sent (TestEverySettingsInputReachesThePayload), not how; and each input
// is a checkbox inside a .field of its own, which hideManagedSettings hides
// and dropUnofferedFields reads.
func TestTheSettingsPayloadSendsBothEnrichmentSwitchesAsTheirCheckboxes(t *testing.T) {
	builder := jsFunctionBody(t, "function buildSettingsPayload(")
	tmpl, err := os.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatal(err)
	}
	page := strings.ReplaceAll(string(tmpl), "\r\n", "\n")
	for _, row := range enrichSwitchRows {
		want := row.field + `: fd.get("` + row.field + `") === "on",`
		if strings.Count(builder, row.field+":") != 1 || !strings.Contains(builder, want) {
			t.Errorf("buildSettingsPayload does not send %s once as %q", row.field, want)
		}
		input := `<input name="` + row.field + `" type="checkbox"`
		i := strings.Index(page, input)
		if i < 0 {
			t.Errorf("settings.html has no checkbox named %s", row.field)
			continue
		}
		open := strings.LastIndex(page[:i], `<div class="field">`)
		if open < 0 || strings.Contains(page[open:i], "</div>") {
			t.Errorf("the %s checkbox is not inside a .field of its own", row.field)
		}
	}
}

// enrichmentRetryRun prints what the shipped enrichmentRetryOffers and
// retryAnswerText make of each card and each answer.
const enrichmentRetryRun = `
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
console.log(JSON.stringify({
  offers: input.cards.map((e) => enrichmentRetryOffers(e)),
  answers: input.answers.map((r) => retryAnswerText(r)),
}));
`

// TestRetryMissingIsOfferedOnlyForWhatCanRunAndSaysWhatRan runs the shipped
// enrichmentRetryOffers and retryAnswerText under node (backlog B617): the
// button counts the enricher's gaps only while enrichment is on, the
// portraits only while Deezer is on too, and bios and descriptions only while
// the harvest runs, and its answer words the facets that ran. Where node is
// not installed it checks only that both functions are there to run.
func TestRetryMissingIsOfferedOnlyForWhatCanRunAndSaysWhatRan(t *testing.T) {
	src := readFile(t, "static/app.js")
	script := withFunctions(t, "", src, "enrichmentRetryOffers", "retryAnswerText") + enrichmentRetryRun
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	type card = map[string]any
	cards := []struct {
		card card
		want bool
	}{
		{card{"missing": 3}, true},
		{card{"missing": 0, "artistImages": card{"missing": 2}}, true},
		{card{"off": true, "missing": 3, "artistImages": card{"missing": 2}}, false},
		{card{"off": true, "missing": 3, "harvestActive": true, "artistBios": card{"missing": 1}}, true},
		{card{"off": true, "harvestActive": true, "albumDescriptions": card{"missing": 1}}, true},
		{card{"deezerOff": true, "missing": 0, "artistImages": card{"missing": 2}}, false},
		{card{"deezerOff": true, "missing": 1, "artistImages": card{"missing": 2}}, true},
		{card{"missing": 0, "artistBios": card{"missing": 4}}, false},
	}
	answers := []struct {
		answer card
		want   string
	}{
		{card{"resetTracks": 3}, "Re-queued 3"},
		{card{"resetTracks": 0}, "Re-checked"},
		{card{"resetTracks": 2, "deezerOff": true}, "Re-queued 2; Deezer is off, no artist photos"},
		{card{"enrichmentOff": true, "harvestResubmitted": true}, "Atlas re-submitted; enrichment is off"},
		{card{"enrichmentOff": true}, "Enrichment is off"},
	}
	in := map[string]any{"cards": []card{}, "answers": []card{}}
	for _, c := range cards {
		in["cards"] = append(in["cards"].([]card), c.card)
	}
	for _, a := range answers {
		in["answers"] = append(in["answers"].([]card), a.answer)
	}
	raw := runScriptUnderNode(t, node, script, in)
	var out struct {
		Offers  []bool   `json:"offers"`
		Answers []string `json:"answers"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("the harness printed %q: %v", raw, err)
	}
	if len(out.Offers) != len(cards) || len(out.Answers) != len(answers) {
		t.Fatalf("the harness answered %d offers and %d answers for %d and %d",
			len(out.Offers), len(out.Answers), len(cards), len(answers))
	}
	for i, c := range cards {
		if out.Offers[i] != c.want {
			t.Errorf("card %v: Retry missing offered %v, want %v", c.card, out.Offers[i], c.want)
		}
	}
	for i, a := range answers {
		if out.Answers[i] != a.want {
			t.Errorf("answer %v: the button says %q, want %q", a.answer, out.Answers[i], a.want)
		}
	}
}
