package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// TestSettingsPageRendersTheRetentionControls is the assertion whose
// ABSENCE was the finding.
//
// The Diagnostics panel told operators to "set a window in Settings" from
// the day the retention work shipped, and there was no such control — not
// in settingsPatch, not in settings_apply.go, not in the template, not in
// ops/settings-apply-semantics.md. The knob was reachable only by
// hand-editing bridge.yaml or by a derived env var whose name is printed
// nowhere.
//
// The `name` attributes are the load-bearing half: app.js builds its Save
// payload from an explicit allowlist keyed on them, so a control that
// renders without one saves nothing while the page still says "Saved."
func TestSettingsPageRendersTheRetentionControls(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	page := string(body)

	for _, want := range []string{
		`id="retentionPlaybackHistoryDays"`,
		`name="retentionPlaybackHistoryDays"`,
		`id="retentionDeviceRegistrationDays"`,
		`name="retentionDeviceRegistrationDays"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("/settings is missing %s", want)
		}
	}

	// THE HALF THE TEMPLATE CHECK CANNOT SEE. app.js builds its Save
	// payload from an explicit allowlist, not a FormData dump, so a
	// control that renders and is applied by the handler still saves
	// NOTHING if nobody put it in that object — while the page reports
	// "Saved." CLAUDE.md names this exact shape, and a negative control
	// proved my template + handler tests blind to it: deleting the field
	// from the allowlist left both of them green.
	assertInSettingsPayload(t,
		"retentionPlaybackHistoryDays", "retentionDeviceRegistrationDays")

	// And the Diagnostics sentence that names Settings must point at a
	// tab that exists, rather than at nothing.
	dresp, err := http.Get(ts.URL + "/diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	defer dresp.Body.Close()
	dbody, _ := io.ReadAll(dresp.Body)
	if !strings.Contains(string(dbody), "/settings?tab=backups") {
		t.Error("the Diagnostics retention panel does not link to the control it tells the " +
			"operator to use")
	}
}

// TestRetentionPatchAppliesLiveAndValidates drives the REAL handler.
//
// `live` is the claim in ops/settings-apply-semantics.md, and it is only
// true because the daily sweeper reads cfg through the holder at the top
// of every pass. Before this control existed nothing could move the
// holder for these fields — there is no config-file watcher — so the
// sweeper documented a capability nothing could reach.
func TestRetentionPatchAppliesLiveAndValidates(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	var resp settingsPatchResponse
	code := doJSON(t, h, "PATCH", "/api/settings", map[string]any{
		"retentionPlaybackHistoryDays":    365,
		"retentionDeviceRegistrationDays": 180,
	}, &resp)
	if code != 200 {
		t.Fatalf("patch: %d", code)
	}
	for _, f := range []string{"retentionPlaybackHistoryDays", "retentionDeviceRegistrationDays"} {
		got, ok := resp.Fields[f]
		if !ok {
			t.Fatalf("field %q absent from the report", f)
		}
		if got.Status != applyLive {
			t.Errorf("field %q status = %q, want %q — the sweeper reads cfg per pass",
				f, got.Status, applyLive)
		}
	}
	if resp.RestartRequired {
		t.Error("a retention change must not require a restart")
	}

	// It reached the live holder, which is the whole point.
	cfg := srv.deps.CfgHolder.Load()
	if cfg.Retention.PlaybackHistoryDays != 365 || cfg.Retention.DeviceRegistrationDays != 180 {
		t.Errorf("holder has %d/%d, want 365/180",
			cfg.Retention.PlaybackHistoryDays, cfg.Retention.DeviceRegistrationDays)
	}

	// And the range rules still bite through this route. 30 is below the
	// 90-day floor; 999999 is the measured overflow that used to delete
	// the entire table.
	for _, bad := range []int{30, 999999} {
		var r settingsPatchResponse
		if code := doJSON(t, h, "PATCH", "/api/settings",
			map[string]any{"retentionPlaybackHistoryDays": bad}, &r); code == 200 {
			t.Errorf("playbackHistoryDays=%d was accepted through the settings PATCH", bad)
		}
	}
	if cfg := srv.deps.CfgHolder.Load(); cfg.Retention.PlaybackHistoryDays != 365 {
		t.Errorf("a refused patch changed the live value to %d", cfg.Retention.PlaybackHistoryDays)
	}
}

// TestDatabaseStatsAreCachedAndInvalidatedByCompaction pins the TTL that
// keeps GET /api/diagnostics honest on a 5 s poll.
//
// Behavioural rather than a call-counting fake: Deps.Manifest is a
// concrete *manifest.Store, so the only way to observe "did it re-query"
// is to change the answer and see whether the change shows through.
func TestDatabaseStatsAreCachedAndInvalidatedByCompaction(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	first := srv.databaseStats(ctx)
	if !first.countsOK {
		t.Fatal("the fixture's store could not be read; the assertions below would be vacuous")
	}

	if err := srv.deps.Manifest.InsertHistoryBatch(ctx, []manifest.PlaybackHistoryRow{
		{DeviceToken: "dev", Path: "a.flac", StartedAt: time.Now().UnixNano(), DurationUsed: 60},
	}); err != nil {
		t.Fatal(err)
	}

	if second := srv.databaseStats(ctx); second.historyRows != first.historyRows {
		t.Errorf("history rows moved from %d to %d inside the TTL — the block is not cached, "+
			"so every 5 s poll runs the full table scan",
			first.historyRows, second.historyRows)
	}

	// Drive the REAL endpoint, not invalidateDatabaseStats directly. The
	// first version of this test called the helper itself and went green
	// against a compaction handler that had stopped calling it — the name
	// said "InvalidatedByCompaction" and the wiring was never touched.
	req := httptest.NewRequest("POST", "/api/database/compact", nil)
	req.Header.Set("content-type", "application/json")
	req.RemoteAddr = "127.0.0.1:54321"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("compact: status %d, body %s", rr.Code, rr.Body.String())
	}

	third := srv.databaseStats(ctx)
	if third.historyRows != first.historyRows+1 {
		t.Errorf("after a compaction history rows = %d, want %d — the compaction must force a "+
			"fresh read, or the operator presses the button and watches a stale number",
			third.historyRows, first.historyRows+1)
	}
}

// assertInSettingsPayload checks that each field name appears in the
// object initSettings() sends to PATCH /api/settings.
//
// Comments are stripped first, for the reason this package's other
// source scans strip them: the code beside these names explains the
// defect BY NAMING IT, so an unstripped scan finds the commentary and
// passes while the allowlist entry is gone. CRLF is normalised because
// nothing pins eol in .gitattributes.
func assertInSettingsPayload(t *testing.T, fields ...string) {
	t.Helper()
	b, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(js, "function initSettings(")
	if i < 0 {
		t.Fatal("initSettings not found in app.js — the scan is broken")
	}
	body := js[i:]
	if j := strings.Index(body[1:], "\nfunction "); j > 0 {
		body = body[:j+1]
	}
	body = jsBlockCommentRe.ReplaceAllString(body, " ")
	body = jsLineCommentRe.ReplaceAllString(body, " ")
	// Vacuity guard: a window that no longer contains the payload builder
	// would pass every assertion below while checking nothing.
	if !strings.Contains(body, "backupKeep") {
		t.Fatal("the initSettings window does not contain the settings payload — the scan is " +
			"reading the wrong span")
	}
	for _, f := range fields {
		if !strings.Contains(body, f+":") {
			t.Errorf("%q renders as a control and is applied by the handler, but is not in "+
				"app.js's Save payload — the operator would set it, be told \"Saved.\", and "+
				"nothing would change", f)
		}
	}
}

// TestLogBundlePrintsWhatItPaysToCompute — the bundle has run three
// PRAGMAs, two COUNTs and a MIN since the compaction work landed, and
// printed none of them. That contradicts the split's stated purpose ("so
// the bug-report bundle embeds the SAME numbers the page shows"), and
// those are among the most useful things to know when triaging a
// stranger's bridge from a pasted text file.
func TestLogBundlePrintsWhatItPaysToCompute(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var buf strings.Builder
	srv.writeBundleDiagnostics(context.Background(), &nopResponseWriter{w: &buf})
	out := buf.String()

	for _, want := range []string{"database file:", "retention:"} {
		if !strings.Contains(out, want) {
			t.Errorf("the bundle's diagnostics section has no %q line:\n%s", want, out)
		}
	}
	if strings.Contains(out, "unavailable") {
		t.Errorf("the fixture's store is healthy but the bundle reported unavailable:\n%s", out)
	}
}

// nopResponseWriter adapts a strings.Builder to http.ResponseWriter so the
// bundle writer can be driven without a server. writeBundleDiagnostics
// only ever calls Write.
type nopResponseWriter struct{ w *strings.Builder }

func (n *nopResponseWriter) Header() http.Header         { return http.Header{} }
func (n *nopResponseWriter) Write(b []byte) (int, error) { return n.w.Write(b) }
func (n *nopResponseWriter) WriteHeader(int)             {}

// TestSettingsResponseCarriesRetention — the control needs somewhere to
// load its current state from, and a settingsResponse without the field
// would paint the zero value on every page load whatever is configured.
func TestSettingsResponseCarriesRetention(t *testing.T) {
	cfg := &config.Config{Retention: config.RetentionConfig{
		PlaybackHistoryDays: 365, DeviceRegistrationDays: 180,
	}}
	got := settingsResponseFromConfig(cfg, false)
	if got.RetentionPlaybackHistoryDays != 365 || got.RetentionDeviceRegistrationDays != 180 {
		t.Errorf("settingsResponse carries %d/%d, want 365/180",
			got.RetentionPlaybackHistoryDays, got.RetentionDeviceRegistrationDays)
	}
	// No omitempty: a zero must survive to the wire, or "keep everything"
	// is indistinguishable from a server that does not support the field.
	blob, err := json.Marshal(settingsResponseFromConfig(&config.Config{}, false))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"retentionPlaybackHistoryDays", "retentionDeviceRegistrationDays"} {
		if _, ok := m[k]; !ok {
			t.Errorf("%q is dropped from the payload at its default 0 — the control would come "+
				"up blank and a caller cannot tell the field is supported", k)
		}
	}
}
