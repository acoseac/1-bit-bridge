package admin

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// patchTagsExemptFromReporting lists json tags on settingsPatch that
// deliberately do NOT get their own key in the report, with the reason.
// An exemption is a decision; the map is what forces it to be written
// down rather than discovered as a gap.
var patchTagsExemptFromReporting = map[string]string{
	"customEndpointsText": "the textarea form of customEndpoints; the array form wins when " +
		"both are sent, so reporting it separately would name a field that did not decide the outcome",
}

// TestEveryPatchFieldIsReported drives EVERY field settingsPatch accepts
// through the real handler, at its currently-stored value, and requires
// each one back in the report.
//
// This is the test that would notice a new PATCH field being added
// without a report site. That failure is invisible otherwise: the field
// still saves, the handler still answers 200, and the caller simply never
// hears what happened to it — which for a control plane means it cannot
// tell "applied" from "silently needs a restart", the exact ambiguity the
// per-field report exists to remove.
//
// Submitting current values (rather than new ones) is what makes it
// possible to exercise all of them in ONE request: every field is
// reportable as `unchanged` without having to invent a valid new value
// for each, and several fields (listenAddress, adminAddress) would fail
// validation or fight each other if changed together.
func TestEveryPatchFieldIsReported(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cfg := srv.deps.CfgHolder.Load()

	body := currentValuesForEveryPatchField(t, cfg)

	// Sanity: the body must actually cover the struct, or the test
	// passes vacuously while checking almost nothing.
	tags := patchJSONTags(t)
	for _, tag := range tags {
		if _, ok := body[tag]; !ok {
			t.Fatalf("currentValuesForEveryPatchField has no entry for %q — it has fallen "+
				"behind settingsPatch, so this test is no longer checking that field", tag)
		}
	}

	var resp settingsPatchResponse
	if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings", body, &resp); code != 200 {
		t.Fatalf("patch: %d", code)
	}

	for _, tag := range tags {
		if why, exempt := patchTagsExemptFromReporting[tag]; exempt {
			if _, reported := resp.Fields[tag]; reported {
				t.Errorf("field %q is reported but listed as exempt (%s) — "+
					"remove it from patchTagsExemptFromReporting", tag, why)
			}
			continue
		}
		got, ok := resp.Fields[tag]
		if !ok {
			t.Errorf("field %q was supplied but is absent from the report — a caller "+
				"cannot tell whether it applied, needs a restart, or was ignored", tag)
			continue
		}
		if got.Status != applyUnchanged {
			t.Errorf("field %q submitted at its current value: status = %q, want %q",
				tag, got.Status, applyUnchanged)
		}
	}

	// Nothing changed, so nothing is pending.
	if resp.RestartRequired {
		t.Error("a patch that changed nothing must not require a restart")
	}
}

// TestReportNamesAreRealPatchFields catches the other direction: a report
// site naming a field that settingsPatch does not have. encoding/json
// would drop such a field on the way IN, so a typo means the handler
// reports on something no caller can correlate with what it sent.
func TestReportNamesAreRealPatchFields(t *testing.T) {
	valid := map[string]bool{}
	for _, tag := range patchJSONTags(t) {
		valid[tag] = true
	}
	for _, name := range reportedFieldNamesInSource(t) {
		if !valid[name] {
			t.Errorf("handler reports field %q, which is not a json tag on settingsPatch — "+
				"no caller can correlate that with anything it sent", name)
		}
	}
}

// TestRestartRequiredIsDerivedFromTheFieldReport pins the rollup in BOTH
// directions.
//
// Deriving it is the point: tracking a separate boolean beside the map is
// how the two drift, and the drift is silent in the dangerous direction —
// a field that records `restart` while the boolean stays false reports
// success and changes nothing until a bounce nobody was told to perform.
func TestRestartRequiredIsDerivedFromTheFieldReport(t *testing.T) {
	cases := []struct {
		name string
		r    applyReport
		want bool
	}{
		{"empty", applyReport{}, false},
		{"all live", applyReport{"a": {Status: applyLive}, "b": {Status: applyLive}}, false},
		{"all unchanged", applyReport{"a": {Status: applyUnchanged}}, false},
		{"one restart among many", applyReport{
			"a": {Status: applyLive},
			"b": {Status: applyUnchanged},
			"c": {Status: applyRestart},
		}, true},
		{"only restart", applyReport{"a": {Status: applyRestart}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.r.needsRestart(); got != tc.want {
				t.Errorf("needsRestart() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestMixedPatchReportsPerFieldNotPerRequest is the papercut that
// motivated the whole change: one request carrying a field that applies
// now and a field that does not.
//
// The legacy boolean says `true` and stops there. Before this change that
// was the entire answer, so an operator who renamed their library and
// changed the scan interval in one save could not tell which of the two
// was waiting on the bounce — and a control plane pushing a
// desired-state document got one bit for the whole document.
func TestMixedPatchReportsPerFieldNotPerRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)

	var resp settingsPatchResponse
	code := doJSON(t, srv.Handler(), "PATCH", "/api/settings", map[string]any{
		"libraryName": "Renamed Live", // read per request off the holder
		// libraryWatchEnabled rather than a cadence field: the cadence
		// ones became hot, and this one is class C — the fsnotify watcher
		// is spawned once at boot behind a drain contract, so it is
		// expected to stay restart-bound for the life of this stack.
		"libraryWatchEnabled": true,
		"adminAddress":        "127.0.0.1:7789", // already the stored value
	}, &resp)
	if code != 200 {
		t.Fatalf("patch: %d", code)
	}

	want := map[string]applyStatus{
		"libraryName":         applyLive,
		"libraryWatchEnabled": applyRestart,
		"adminAddress":        applyUnchanged,
	}
	for field, wantStatus := range want {
		got, ok := resp.Fields[field]
		if !ok {
			t.Errorf("%s: missing from report", field)
			continue
		}
		if got.Status != wantStatus {
			t.Errorf("%s: status = %q, want %q", field, got.Status, wantStatus)
		}
	}
	if len(resp.Fields) != len(want) {
		t.Errorf("report has %d fields (%v), want exactly the %d supplied — a field the "+
			"request did not carry must not appear",
			len(resp.Fields), resp.Fields.fields(), len(want))
	}
	if !resp.RestartRequired {
		t.Error("libraryWatchEnabled changed, so the legacy rollup must still be true")
	}
}

// TestReasonIsPresentOnlyWhenTheAnswerWasConditional pins the rule that
// keeps `reason` meaningful: it is populated exactly when the OUTCOME
// depended on THIS bridge's runtime state, not on a static property of
// the field. (See TestFingerprintEnabledLiveWithDegradedReason for the
// other qualifying shape — a `live` that applied but is inert.)
//
// Without the rule the honest thing to do looks like explaining every
// restart, which produces twenty near-identical strings a reader learns
// to skip — at which point the two that actually carry information are
// skipped along with them.
func TestReasonIsPresentOnlyWhenTheAnswerWasConditional(t *testing.T) {
	// autoOptimizeEnabled with NO sweeper wired: the flip cannot take
	// effect, and saying why is the whole point of the honesty rule.
	srv, _, _ := newTestServer(t)
	if srv.deps.TriggerAutoOptimizeSweep != nil {
		t.Fatal("fixture unexpectedly wires a sweeper; this case needs it absent")
	}
	var resp settingsPatchResponse
	if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
		map[string]any{"autoOptimizeEnabled": true}, &resp); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	got := resp.Fields["autoOptimizeEnabled"]
	if got.Status != applyRestart {
		t.Fatalf("no sweeper wired: status = %q, want %q (the flip cannot hot-apply)",
			got.Status, applyRestart)
	}
	if got.Reason == "" {
		t.Error("a conditional restart must say why — otherwise the operator flips the " +
			"switch, sees nothing happen, and has nothing to act on")
	}
	if !strings.Contains(got.Reason, "sweeper") {
		t.Errorf("reason %q does not name the missing sweeper", got.Reason)
	}

	// A restart that is a static property of the field carries no
	// reason: "listeners bind once" is true on every bridge.
	srv2, _, _ := newTestServer(t)
	var resp2 settingsPatchResponse
	if code := doJSON(t, srv2.Handler(), "PATCH", "/api/settings",
		map[string]any{"listenAddress": "127.0.0.1:9999"}, &resp2); code != 200 {
		t.Fatalf("patch listen: %d", code)
	}
	if r := resp2.Fields["listenAddress"].Reason; r != "" {
		t.Errorf("listenAddress carries reason %q — it is restart-bound on every bridge, "+
			"so the reason adds nothing and dilutes the ones that do", r)
	}
}

// TestReportOmitsFieldsTheRequestDidNotSupply — absence in the report
// means "you did not send it", which is a different fact from
// `unchanged` ("you sent it and it was already that").
func TestReportOmitsFieldsTheRequestDidNotSupply(t *testing.T) {
	srv, _, _ := newTestServer(t)
	var resp settingsPatchResponse
	if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
		map[string]any{"libraryName": "Only This"}, &resp); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	if len(resp.Fields) != 1 {
		t.Fatalf("report = %v, want exactly libraryName", resp.Fields.fields())
	}
	if _, ok := resp.Fields["scanIntervalSec"]; ok {
		t.Error("scanIntervalSec was not supplied but appears in the report")
	}
}

// TestFieldApplyWireShape pins the JSON: an OBJECT value, with reason
// omitted when empty.
//
// The object is what makes `reason` additive. A bare string value
// ("restart") would have to widen into an object to carry one, and this
// is a public API on a self-hosted binary whose script consumers cannot
// be surveyed before breaking them.
func TestFieldApplyWireShape(t *testing.T) {
	b, err := json.Marshal(settingsPatchResponse{
		RestartRequired: true,
		Fields: applyReport{
			"libraryName":         {Status: applyLive},
			"autoOptimizeEnabled": {Status: applyRestart, Reason: "no sweeper wired"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var round map[string]any
	if err := json.Unmarshal(b, &round); err != nil {
		t.Fatal(err)
	}
	fields, ok := round["fields"].(map[string]any)
	if !ok {
		t.Fatalf("fields is not an object: %s", b)
	}
	live, ok := fields["libraryName"].(map[string]any)
	if !ok {
		t.Fatalf("field value is not an object: %s", b)
	}
	// Key-absence via a decoded map, never a substring probe on the raw
	// body: `"reason":""` and an absent reason are indistinguishable to
	// strings.Contains.
	if _, present := live["reason"]; present {
		t.Errorf("empty reason must be omitted, got %v", live)
	}
	if live["status"] != string(applyLive) {
		t.Errorf("status = %v, want %q", live["status"], applyLive)
	}
	cond := fields["autoOptimizeEnabled"].(map[string]any)
	if cond["reason"] != "no sweeper wired" {
		t.Errorf("reason did not survive the round trip: %v", cond)
	}
}

// --- helpers ---

// patchJSONTags returns every json field name settingsPatch accepts.
func patchJSONTags(t *testing.T) []string {
	t.Helper()
	tags := jsonTagsOf(reflect.TypeOf(settingsPatch{}))
	out := make([]string, 0, len(tags))
	for tag := range tags {
		out = append(out, tag)
	}
	sort.Strings(out)
	if len(out) < 20 {
		t.Fatalf("only %d tags found on settingsPatch — reflection has stopped working", len(out))
	}
	return out
}

// currentValuesForEveryPatchField builds a PATCH body that supplies every
// field at the value cfg currently holds, so every one reports
// `unchanged`.
//
// Hand-written rather than reflected off the config: the mapping from a
// patch field to the config field it compares against is exactly what can
// be wrong (several compare against a RESOLVED Effective* value, not a
// raw one), so a reflective version would reproduce whatever bug it was
// meant to catch.
func currentValuesForEveryPatchField(t *testing.T, cfg *config.Config) map[string]any {
	t.Helper()
	mode, err := cfg.Tailscale.EffectiveMode()
	if err != nil {
		t.Fatalf("resolve tailscale mode: %v", err)
	}
	dupes, err := cfg.Duplicates.EffectiveFilter()
	if err != nil {
		t.Fatalf("resolve duplicates filter: %v", err)
	}
	return map[string]any{
		"libraryName":     cfg.LibraryName,
		"listenAddress":   cfg.ListenAddress,
		"adminAddress":    cfg.AdminAddress,
		"scanIntervalSec": cfg.ScanIntervalSec,
		// Resolved, not raw: the handler compares against
		// EffectiveIntervalHours / EffectiveKeep, so a raw nil would
		// read as a change.
		"backupIntervalHours":      cfg.Backup.EffectiveIntervalHours(),
		"backupKeep":               cfg.Backup.EffectiveKeep(),
		"updateAutoInstall":        cfg.Update.AutoInstall,
		"updateQuietHours":         cfg.Update.QuietHours,
		"updateCheckIntervalHours": cfg.Update.CheckIntervalHours,
		"customEndpoints":          cfg.CustomEndpoints,
		// Present so the exemption is exercised rather than assumed.
		"customEndpointsText":      strings.Join(cfg.CustomEndpoints, "\n"),
		"upscaleEnabled":           cfg.Upscale.Enabled,
		"analysisEnabled":          cfg.Analysis.Enabled,
		"smartPlaylistsEnabled":    cfg.SmartPlaylists.EffectiveEnabled(),
		"optimizeEnabled":          cfg.Upscale.EffectiveOptimizeEnabled(),
		"autoOptimizeEnabled":      cfg.Upscale.AutoOptimize.Enabled,
		"libraryWatchEnabled":      cfg.LibraryWatch.Enabled,
		"enrichMusicBrainzBaseURL": cfg.Enrich.MusicBrainzBaseURL,
		"enrichCoverArtBaseURL":    cfg.Enrich.CoverArtBaseURL,
		"atlasEnabled":             cfg.Atlas.Enabled,
		"fingerprintEnabled":       cfg.Fingerprint.Enabled,
		// Blank is the documented no-op (the settings form submits this
		// on every save, so a blank MUST keep the stored key).
		"fingerprintApiKey": "",
		"duplicatesFilter":  dupes,
		"tailscaleMode":     string(mode),
		"mdnsEnabled":       cfg.EffectiveMDNSEnabled(),
		"dlnaEnabled":       cfg.DLNA.Enabled,
	}
}

// reportedFieldNamesInSource scrapes the field names the settings
// handler can emit, straight out of its source.
//
// A source scan rather than reflection because the names are string
// literals at the call sites — there is no runtime handle on the set
// short of driving every branch, and the branches include ones the
// fixture cannot reach (a wired mDNS lifecycle, a wired sweeper).
// Comments are stripped first, for the reason the tray-parity test
// strips them: this repo's commentary quotes the identifiers it
// discusses, so a comment naming a field would otherwise register as a
// call site.
func reportedFieldNamesInSource(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile("handlers_api.go")
	if err != nil {
		t.Fatalf("read handlers_api.go: %v", err)
	}
	src := trayCommentRe.ReplaceAllString(string(b), "")
	found := map[string]bool{}
	for _, m := range reportCallRe.FindAllStringSubmatch(src, -1) {
		found[m[1]] = true
	}
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	// Vacuous-pass guard: a regex that has stopped matching reports no
	// problems, which is the one outcome that hides the drift it exists
	// to catch.
	if len(out) < 15 {
		t.Fatalf("only %d report call sites scraped (%v) — the regex has stopped matching, "+
			"so this test is checking nothing", len(out), out)
	}
	return out
}

// reportCallRe matches `report.live("x")` / `.restart("x")` /
// `.unchanged("x")` / `.restartBecause("x", …)`.
var reportCallRe = regexp.MustCompile(`\breport\.(?:live|restart|unchanged|restartBecause|changed)\(\s*"([A-Za-z][A-Za-z0-9]*)"`)

// TestTrayBadgesAgreeWithWhatTheServerReports is the third leg of the
// badge-parity story.
//
// TestFeatureTrayRestartBadgesAgreeWithSettings pins tray badge vs.
// Settings-page badge — two PREDICTIONS agreeing with each other. Both
// can be wrong together, and until the per-field report existed there was
// nothing to check them against. Now there is: the server states, per
// field, whether that change took effect. A badge that disagrees with it
// is telling the operator the wrong thing about their own bridge.
//
// It also makes the badges maintenance-visible. Converting a field from
// restart-bound to hot-applying (PRs 2–4) now fails here until the badge
// is dropped — which is the correct order of events, since a stale
// "restart" badge sends the operator to bounce a bridge that already
// applied the change.
func TestTrayBadgesAgreeWithWhatTheServerReports(t *testing.T) {
	settingsHTML, err := os.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}

	checked := 0
	for field, badge := range trayBadgesByField(t) {
		newValue, ok := differentValueFor(t, field)
		if !ok {
			continue // no mechanical "other value" — covered by the parity tests
		}
		// The Settings page is the other prediction; skip fields it does
		// not render, exactly as the existing parity test does.
		if _, rendered := settingsBadgeFor(string(settingsHTML), field); !rendered {
			continue
		}

		t.Run(field, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			// Wire the sweeper: the badge predicts the TYPICAL bridge,
			// and autoOptimizeEnabled's honest answer on a bridge with no
			// sweeper is `restart` for a reason that is about this host,
			// not about the field. Without this the test would demand a
			// badge that is wrong everywhere the feature actually runs.
			srv.deps.TriggerAutoOptimizeSweep = func() bool { return true }

			var resp settingsPatchResponse
			if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
				map[string]any{field: newValue}, &resp); code != 200 {
				t.Fatalf("patch %s=%v: %d", field, newValue, code)
			}
			got, ok := resp.Fields[field]
			if !ok {
				t.Fatalf("%s absent from report", field)
			}
			if got.Status == applyUnchanged {
				t.Fatalf("%s: differentValueFor produced the stored value (%v), so this "+
					"case proves nothing", field, newValue)
			}
			wantBadge := got.Status == applyRestart
			if badge != wantBadge {
				t.Errorf("%s: tray badge says restart=%v, server reports %q — the operator "+
					"is being told the wrong thing about whether their change took effect",
					field, badge, got.Status)
			}
		})
		checked++
	}
	if checked < 6 {
		t.Fatalf("only %d tray fields exercised — the scrape or the value generator has "+
			"stopped working, so this test is checking almost nothing", checked)
	}
}

// trayBadgesByField scrapes `{field: "x", …, restart: true}` tray rows.
// Shares the row regex's shape with the existing badge-parity test so the
// two cannot disagree about what a row is.
func trayBadgesByField(t *testing.T) map[string]bool {
	t.Helper()
	var sb strings.Builder
	for _, body := range trayScriptBodies(t) {
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	rowRe := regexp.MustCompile(`\{[^{}]*\bfield:\s*"([A-Za-z0-9]+)"[^{}]*\}`)
	out := map[string]bool{}
	for _, m := range rowRe.FindAllStringSubmatch(sb.String(), -1) {
		field, row := m[1], m[0]
		if _, seen := out[field]; seen {
			continue
		}
		out[field] = strings.Contains(row, "restart: true")
	}
	if len(out) < 8 {
		t.Fatalf("only %d tray rows scraped (%v) — the regex has stopped matching", len(out), out)
	}
	return out
}

// differentValueFor returns a value guaranteed to differ from the test
// fixture's stored one, and whether such a value exists mechanically.
//
// Hand-written per field rather than derived: several have validation
// floors (scanIntervalSec, backupKeep, updateCheckIntervalHours), and a
// generated value that trips one would fail the PATCH for a reason
// unrelated to what is being tested.
func differentValueFor(t *testing.T, field string) (any, bool) {
	t.Helper()
	switch field {
	// Every tray toggle: the fixture leaves these at their zero/default,
	// and `true` differs from all of them EXCEPT smartPlaylistsEnabled
	// and optimizeEnabled, which default ON.
	case "libraryWatchEnabled", "atlasEnabled", "analysisEnabled",
		"fingerprintEnabled", "upscaleEnabled", "autoOptimizeEnabled",
		"updateAutoInstall", "dlnaEnabled":
		return true, true
	case "smartPlaylistsEnabled", "optimizeEnabled":
		return false, true
	// Numbers, all clear of their validation floors.
	case "scanIntervalSec":
		return 7200, true
	case "backupIntervalHours":
		return 12, true
	case "backupKeep":
		return 9, true
	case "updateCheckIntervalHours":
		return 3, true
	// Strings. These three are Settings-page-only — no tray carries them —
	// which is exactly why they went unchecked while their badges went
	// stale. Omitting them here would leave the new page-scraping test
	// skipping the fields it was written for.
	case "updateQuietHours":
		return "02:00-04:00", true
	case "fingerprintApiKey":
		// Non-blank: a blank submit is the documented no-op and reports
		// `unchanged`, which the caller treats as "proves nothing".
		return "probe-key-123", true
	case "enrichMusicBrainzBaseURL":
		return "https://mirror.example.test/ws/2", true
	case "enrichCoverArtBaseURL":
		return "https://mirror.example.test", true
	}
	return nil, false
}

// TestCustomEndpointsReportedAfterPruning — the verdict is decided after
// NormalizeAndValidate, which drops entries that are not absolute https
// URLs.
//
// Deciding before it would report `live` for a request whose entries
// validation then pruned: the saved list unchanged, the response claiming
// otherwise. That is the one answer worse than no answer for a control
// plane reconciling desired state — it records a convergence that did not
// happen and stops trying.
func TestCustomEndpointsReportedAfterPruning(t *testing.T) {
	t.Run("only-invalid entries report unchanged", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		var resp settingsPatchResponse
		// http, not https — ValidateCustomEndpoints prunes it.
		if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
			map[string]any{"customEndpoints": []string{"http://not-https.example"}}, &resp); code != 200 {
			t.Fatalf("patch: %d", code)
		}
		if got := resp.Fields["customEndpoints"].Status; got != applyUnchanged {
			t.Errorf("status = %q, want %q — every entry was pruned, so the saved list "+
				"is exactly what it was", got, applyUnchanged)
		}
		if got := srv.deps.CfgHolder.Load().CustomEndpoints; len(got) != 0 {
			t.Errorf("stored list = %v, want empty (the entry was invalid)", got)
		}
	})

	t.Run("a surviving entry reports live", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		var resp settingsPatchResponse
		if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
			map[string]any{"customEndpoints": []string{"https://bridge.example:7788"}}, &resp); code != 200 {
			t.Fatalf("patch: %d", code)
		}
		if got := resp.Fields["customEndpoints"].Status; got != applyLive {
			t.Errorf("status = %q, want %q", got, applyLive)
		}
	})
}

// TestCadenceChangeFiresTheRearm pins the second half of "live" for the
// cadence fields.
//
// Reporting `live` for a schedule that will not be re-read until the OLD
// interval elapses is technically true and practically a lie: on the 6 h
// scan default the operator who shortens the cadence waits out the long
// one first, which is indistinguishable from being ignored. The rearm is
// what closes that gap, so the report and the rearm have to move
// together.
func TestCadenceChangeFiresTheRearm(t *testing.T) {
	cases := []struct {
		field string
		value any
		rearm bool
	}{
		// Schedules: a loop is parked on the old interval and has to be
		// woken to re-read it.
		{"scanIntervalSec", 7200, true},
		{"backupIntervalHours", 12, true},
		{"updateCheckIntervalHours", 3, true},
		// Read at decision time, so there is no parked wait to disturb.
		// Firing the rearm for these would be harmless but dishonest
		// about what changed.
		{"backupKeep", 9, false},
		{"updateAutoInstall", true, false},
		{"updateQuietHours", "02:00-04:00", false},
		// Not a cadence at all.
		{"libraryName", "Rearm Probe", false},
	}
	for _, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			var fired int
			srv.deps.TriggerCadenceRearm = func() { fired++ }

			var resp settingsPatchResponse
			if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
				map[string]any{tc.field: tc.value}, &resp); code != 200 {
				t.Fatalf("patch: %d", code)
			}
			if got := resp.Fields[tc.field].Status; got != applyLive {
				t.Fatalf("%s: status = %q, want %q — this case assumes the field is hot",
					tc.field, got, applyLive)
			}
			if want := 0; !tc.rearm && fired != want {
				t.Errorf("%s: rearm fired %d times, want %d", tc.field, fired, want)
			}
			if tc.rearm && fired != 1 {
				t.Errorf("%s: rearm fired %d times, want 1 — without it the new schedule "+
					"is not read until the old interval elapses", tc.field, fired)
			}
		})
	}
}

// TestUnchangedCadenceDoesNotFireTheRearm — the rearm RESTARTS a wait, so
// firing it on a same-value save would push the next run out by a full
// interval every time the operator pressed Save on an unrelated field.
func TestUnchangedCadenceDoesNotFireTheRearm(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cfg := srv.deps.CfgHolder.Load()
	var fired int
	srv.deps.TriggerCadenceRearm = func() { fired++ }

	var resp settingsPatchResponse
	if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
		map[string]any{"scanIntervalSec": cfg.ScanIntervalSec}, &resp); code != 200 {
		t.Fatalf("patch: %d", code)
	}
	if got := resp.Fields["scanIntervalSec"].Status; got != applyUnchanged {
		t.Fatalf("status = %q, want %q", got, applyUnchanged)
	}
	if fired != 0 {
		t.Errorf("rearm fired %d times on an unchanged value — every save would push the "+
			"next scheduled run out by a full interval", fired)
	}
}

// TestFingerprintEnabledLiveWithDegradedReason pins the shape of an
// honest `live` that still needs to warn.
//
// Switching fingerprinting on APPLIES — the sweeper, the enricher gate
// and the AcoustID client all read it live — but whether anything RUNS
// depends on fpcalc being installed. A restart would not change that, so
// `restart` would be a lie; silence would have the operator move the
// switch, see "Saved.", and never learn that nothing will happen.
//
// This is the honesty rule applied where the obstacle is the toolchain
// rather than the wiring: same obligation, different status.
func TestFingerprintEnabledLiveWithDegradedReason(t *testing.T) {
	t.Run("toolchain missing", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.deps.FingerprintDegraded = func() string { return "fpcalc_missing" }

		var resp settingsPatchResponse
		if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
			map[string]any{"fingerprintEnabled": true}, &resp); code != 200 {
			t.Fatalf("patch: %d", code)
		}
		got := resp.Fields["fingerprintEnabled"]
		if got.Status != applyLive {
			t.Errorf("status = %q, want %q — a restart cannot install fpcalc", got.Status, applyLive)
		}
		if got.Reason == "" {
			t.Fatal("no reason: the operator gets 'Saved.' for a switch that will do nothing")
		}
		if !strings.Contains(got.Reason, "fpcalc") {
			t.Errorf("reason %q does not name what is missing", got.Reason)
		}
		if resp.RestartRequired {
			t.Error("a live-with-a-warning field must not raise the restart rollup")
		}
	})

	t.Run("toolchain present", func(t *testing.T) {
		srv, _, _ := newTestServer(t)
		srv.deps.FingerprintDegraded = func() string { return "" }

		var resp settingsPatchResponse
		if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
			map[string]any{"fingerprintEnabled": true}, &resp); code != 200 {
			t.Fatalf("patch: %d", code)
		}
		got := resp.Fields["fingerprintEnabled"]
		if got.Status != applyLive || got.Reason != "" {
			t.Errorf("got %+v, want a bare live — nothing to warn about", got)
		}
	})

	t.Run("switching OFF never warns", func(t *testing.T) {
		// The toolchain is irrelevant to turning the feature off, and a
		// warning there would be noise attached to a change that fully
		// took effect.
		srv, _, _ := newTestServer(t)
		srv.deps.FingerprintDegraded = func() string { return "fpcalc_missing" }
		if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
			map[string]any{"fingerprintEnabled": true}, nil); code != 200 {
			t.Fatalf("arrange: %d", code)
		}
		var resp settingsPatchResponse
		if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
			map[string]any{"fingerprintEnabled": false}, &resp); code != 200 {
			t.Fatalf("patch off: %d", code)
		}
		if r := resp.Fields["fingerprintEnabled"].Reason; r != "" {
			t.Errorf("switching off carried reason %q", r)
		}
	})
}

// TestFingerprintDegradedMessagesAreBounded — the keys come from the
// toolchain probe, never from an error string, so the message set cannot
// grow unbounded (the rule markSkipped's reason keys follow).
func TestFingerprintDegradedMessagesAreBounded(t *testing.T) {
	for _, key := range []string{"fpcalc_missing", "no_api_key", "something_new"} {
		if msg := fingerprintDegradedMessage(key); msg == "" {
			t.Errorf("%s: empty message", key)
		}
	}
	if fingerprintDegradedMessage("fpcalc_missing") == fingerprintDegradedMessage("no_api_key") {
		t.Error("the two known reasons must read differently — they need different fixes")
	}
}

// uiOnlyControls maps a Settings-page control that is NOT a settingsPatch
// field onto the field(s) it actually drives. Its badge has to agree with
// theirs, since that is what the operator's click will change.
var uiOnlyControls = map[string][]string{
	// The enrichment-source picker is derived, not stored — it writes the
	// two base URLs (see mapEnrichSourceToBases in app.js). There is
	// deliberately no `enrich.source` config field, so nothing else
	// connects this control to an apply semantic.
	"enrichSource": {"enrichMusicBrainzBaseURL", "enrichCoverArtBaseURL"},
}

// TestSettingsPageBadgesAgreeWithWhatTheServerReports walks the SETTINGS
// PAGE's own badges and checks each against what the handler reports.
//
// This is the direction the other two badge tests do not cover, and the
// gap was real: both of them iterate TRAY rows and consult the Settings
// page only for fields a tray happens to contain. A field that lives on
// the Settings page alone — updateQuietHours, fingerprintApiKey, the
// enrichment-source picker — was never visited by either, so all three
// kept a stale `restart` badge through the conversion PRs and shipped to
// production saying a change needed a bounce that it did not.
//
// Scraping the page rather than a hand-listed set is the point: a badge
// added to a new field is checked automatically, which a list would not
// be.
func TestSettingsPageBadgesAgreeWithWhatTheServerReports(t *testing.T) {
	html, err := os.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	page := string(html)

	checked := 0
	for _, field := range settingsPageFields(t, page) {
		// A UI-only control inherits the semantics of what it writes.
		probes, ok := uiOnlyControls[field]
		if !ok {
			probes = []string{field}
		}
		badge, rendered := settingsBadgeFor(page, field)
		if !rendered {
			continue
		}

		var values []any
		for _, p := range probes {
			v, ok := differentValueFor(t, p)
			if !ok {
				values = nil
				break
			}
			values = append(values, v)
		}
		if values == nil {
			continue // no mechanical "other value" for this one
		}

		t.Run(field, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			// The badge predicts the TYPICAL bridge, i.e. one where the
			// feature's wiring is present.
			srv.deps.TriggerAutoOptimizeSweep = func() bool { return true }
			srv.deps.TriggerCadenceRearm = func() {}

			body := map[string]any{}
			for i, p := range probes {
				body[p] = values[i]
			}
			var resp settingsPatchResponse
			if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings", body, &resp); code != 200 {
				t.Fatalf("patch %v: %d", body, code)
			}
			// A control that drives several fields needs a restart iff ANY
			// of them does.
			wantBadge := false
			for _, p := range probes {
				got, ok := resp.Fields[p]
				if !ok {
					t.Fatalf("%s absent from report", p)
				}
				if got.Status == applyUnchanged {
					t.Fatalf("%s: differentValueFor produced the stored value, so this "+
						"row proves nothing", p)
				}
				if got.Status == applyRestart {
					wantBadge = true
				}
			}
			if badge != wantBadge {
				t.Errorf("Settings page badge for %q says restart=%v, server reports %v — "+
					"the operator is being told the wrong thing about whether their change "+
					"took effect", field, badge, wantBadge)
			}
		})
		checked++
	}
	if checked < 8 {
		t.Fatalf("only %d Settings-page fields exercised — the scrape has stopped working, "+
			"so this test is checking almost nothing", checked)
	}
}

// settingsPageFields returns every named form control on the Settings
// page, in document order.
func settingsPageFields(t *testing.T, page string) []string {
	t.Helper()
	var out []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`name="([A-Za-z][A-Za-z0-9]*)"`).FindAllStringSubmatch(page, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	if len(out) < 15 {
		t.Fatalf("only %d named controls scraped — the regex has stopped matching", len(out))
	}
	return out
}

// restartProseRe finds a description sentence claiming a change needs a
// restart. Deliberately narrow — it matches the one phrasing the page
// uses for that claim, not every sentence containing the word (the
// updater's "atomic swap → restart" describes what the FEATURE does, and
// the mDNS hint says the opposite).
var restartProseRe = regexp.MustCompile(`takes effect after a restart|requires? a restart before`)

// TestSettingsProseDoesNotContradictTheBadge closes the third surface.
//
// A setting's apply semantics are stated in THREE places on this page: the
// badge, the description prose, and (indirectly) the server's own report.
// The badge tests cover the first against the third. The prose was
// unchecked — and it drifted: `optimizeEnabled` lost its badge when the
// field went live but kept a sentence saying "wired at startup, so a
// change takes effect after a restart", which is the same wrong answer the
// badge had been giving, in a place nobody was looking.
//
// A reader who trusts prose over a chip gets the stale answer either way,
// so the two have to agree.
func TestSettingsProseDoesNotContradictTheBadge(t *testing.T) {
	html, err := os.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	page := string(html)

	checked := 0
	for _, field := range settingsPageFields(t, page) {
		badge, rendered := settingsBadgeFor(page, field)
		if !rendered {
			continue
		}
		hint := hintTextFor(page, field)
		if hint == "" {
			continue
		}
		// Collapse whitespace FIRST: the template wraps its hints across
		// lines, so a claim can straddle a newline and slip past a naive
		// match — which is exactly how the first version of this test
		// passed against the stale prose it was written to catch.
		claimsRestart := restartProseRe.MatchString(collapseWS(hint))
		if claimsRestart && !badge {
			t.Errorf("field %q: the description says a change takes effect after a restart, "+
				"but it carries no restart badge — one of the two is stale, and a reader who "+
				"trusts the prose gets the wrong answer", field)
		}
		checked++
	}
	if checked < 8 {
		t.Fatalf("only %d fields with hints scraped — the scrape has stopped working", checked)
	}
}

// hintTextFor returns the `<small class="hint">` that follows a field's
// input, which is where the page puts its per-setting explanation.
func hintTextFor(page, field string) string {
	i := strings.Index(page, `name="`+field+`"`)
	if i < 0 {
		return ""
	}
	start := strings.Index(page[i:], `<small class="hint"`)
	if start < 0 {
		return ""
	}
	start += i
	end := strings.Index(page[start:], "</small>")
	if end < 0 {
		return ""
	}
	// Stop at the NEXT field's input, so a field with no hint of its own
	// cannot borrow its neighbour's.
	if next := strings.Index(page[i+1:], `name="`); next >= 0 && i+1+next < start {
		return ""
	}
	return page[start : start+end]
}

// collapseWS flattens runs of whitespace (including newlines) to single
// spaces, so a phrase wrapped across template lines still matches.
func collapseWS(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestUpscaleDegradedReason — the sox-backed twin of the fingerprint
// case: the toggle APPLIES (the pool is always there, the gate is live),
// but whether anything runs depends on a toolchain this bridge may not
// have. A restart would not install sox, so `live` is the honest status —
// with a reason, or the operator watches a switch they just moved do
// nothing.
func TestUpscaleDegradedReason(t *testing.T) {
	cases := []struct {
		name       string
		probeErr   error
		hasFLAC    bool
		known      bool
		wantReason string // substring; "" means no reason
	}{
		{"sox missing", errors.New("exec: sox not found"), false, false, "sox is not installed"},
		{"sox without FLAC", nil, false, true, "no FLAC support"},
		{"sox fine", nil, true, true, ""},
		// Conservative gate: an unparseable `sox --help` is treated as
		// FLAC-present, so a help-output reword can never make a working
		// install look broken.
		{"formats unknown", nil, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			srv.deps.UpscalePrecheck = func() error { return tc.probeErr }
			srv.deps.UpscaleSoxFLAC = func() (bool, bool) { return tc.hasFLAC, tc.known }

			var resp settingsPatchResponse
			if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
				map[string]any{"upscaleEnabled": true, "analysisEnabled": true}, &resp); code != 200 {
				t.Fatalf("patch: %d", code)
			}
			for _, f := range []string{"upscaleEnabled", "analysisEnabled"} {
				got := resp.Fields[f]
				if got.Status != applyLive {
					t.Errorf("%s: status = %q, want %q — a restart cannot install sox",
						f, got.Status, applyLive)
				}
				if tc.wantReason == "" {
					if got.Reason != "" {
						t.Errorf("%s: unexpected reason %q", f, got.Reason)
					}
					continue
				}
				if !strings.Contains(got.Reason, tc.wantReason) {
					t.Errorf("%s: reason = %q, want it to mention %q", f, got.Reason, tc.wantReason)
				}
			}
			if resp.RestartRequired {
				t.Error("a live-with-a-warning field must not raise the restart rollup")
			}
		})
	}
}
