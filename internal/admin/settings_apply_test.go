package admin

import (
	"encoding/json"
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
		"libraryName":     "Renamed Live",   // read per request off the holder
		"scanIntervalSec": 7200,             // captured by a boot-time ticker
		"adminAddress":    "127.0.0.1:7789", // already the stored value
	}, &resp)
	if code != 200 {
		t.Fatalf("patch: %d", code)
	}

	want := map[string]applyStatus{
		"libraryName":     applyLive,
		"scanIntervalSec": applyRestart,
		"adminAddress":    applyUnchanged,
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
		t.Error("scanIntervalSec changed, so the legacy rollup must still be true")
	}
}

// TestReasonIsPresentOnlyWhenTheAnswerWasConditional pins the rule that
// keeps `reason` meaningful: it is populated exactly when the status
// depended on THIS bridge's runtime wiring, not on a static property of
// the field.
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
	}
	return nil, false
}
