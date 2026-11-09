package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The update check's switch on the console (backlog B761): the settings GET
// and PATCH carry updateCheckEnabled, a change tells the updater once it is
// saved and published (Deps.UpdateChecksSwitched) and a save that fails tells
// it nothing; Check now and Install answer the switch's refusal as 409
// update-checks-off, a rollback never; the Settings page's Updates panel and
// the Jobs card say "update checks off" and offer neither Check now nor
// Install.

// fileUpdateCheckEnabled is update.checkEnabled as the config file at path
// writes it, and whether it writes it at all.
func fileUpdateCheckEnabled(t *testing.T, path string) (any, bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Update map[string]any `yaml:"update"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("bridge.yaml does not parse: %v", err)
	}
	v, ok := doc.Update["checkEnabled"]
	return v, ok
}

// TestTheSettingsCarryTheUpdateCheckSwitch drives the real settings handler:
// GET shows the switch resolved (on where the file says nothing); a PATCH
// that turns it off is live, writes false and tells the updater once, after
// the commit, with the live config already off; one that sends the value it
// holds is unchanged and tells nothing; one that turns it on is live, writes
// no key and tells it; and a PATCH that fails, at its validation or at its
// save, changes nothing and tells nothing.
func TestTheSettingsCarryTheUpdateCheckSwitch(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	var told []bool
	srv.deps.UpdateChecksSwitched = func() {
		told = append(told, srv.deps.CfgHolder.Load().Update.EffectiveCheckEnabled())
	}
	shown := func() any {
		t.Helper()
		var got map[string]any
		if code := doJSON(t, srv.Handler(), http.MethodGet, "/api/settings", nil, &got); code != http.StatusOK {
			t.Fatalf("GET /api/settings = %d", code)
		}
		return got["updateCheckEnabled"]
	}
	patch := func(body map[string]any) (int, settingsPatchResponse) {
		t.Helper()
		var resp settingsPatchResponse
		code := doJSON(t, srv.Handler(), http.MethodPatch, "/api/settings", body, &resp)
		return code, resp
	}

	if v := shown(); v != true {
		t.Fatalf("GET /api/settings updateCheckEnabled = %v, want true where the file says nothing", v)
	}

	code, resp := patch(map[string]any{"updateCheckEnabled": false})
	if code != http.StatusOK || resp.Fields["updateCheckEnabled"].Status != applyLive {
		t.Fatalf("PATCH off = %d %+v, want 200 and live", code, resp.Fields["updateCheckEnabled"])
	}
	if len(told) != 1 || told[0] {
		t.Errorf("the updater was told %v, want once, with the live config already off", told)
	}
	if v, there := fileUpdateCheckEnabled(t, cfgPath); !there || v != false {
		t.Errorf("the file holds update.checkEnabled = %v (there %v), want false", v, there)
	}
	if v := shown(); v != false {
		t.Errorf("GET /api/settings updateCheckEnabled = %v after the PATCH, want false", v)
	}

	code, resp = patch(map[string]any{"updateCheckEnabled": false})
	if code != http.StatusOK || resp.Fields["updateCheckEnabled"].Status != applyUnchanged || len(told) != 1 {
		t.Errorf("PATCH off again = %d %+v, told %v; want unchanged, and the updater told nothing", code, resp.Fields["updateCheckEnabled"], told)
	}

	// A PATCH that fails changes nothing and tells nothing: at its
	// validation (a negative cadence beside the switch)...
	if code, _ := patch(map[string]any{"updateCheckEnabled": true, "updateCheckIntervalHours": -1}); code != http.StatusBadRequest {
		t.Errorf("PATCH with an invalid cadence = %d, want 400", code)
	}
	// ...and at its save (the file changed outside the holder).
	if err := os.WriteFile(cfgPath, append(mustRead(t, cfgPath), []byte("# edited outside\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := patch(map[string]any{"updateCheckEnabled": true}); code == http.StatusOK {
		t.Errorf("PATCH over a file changed outside = %d, want it refused", code)
	}
	if len(told) != 1 || srv.deps.CfgHolder.Load().Update.EffectiveCheckEnabled() {
		t.Errorf("failed PATCHes told the updater %v and left checks on %v; want neither", told, srv.deps.CfgHolder.Load().Update.EffectiveCheckEnabled())
	}
}

// TestTurningUpdateChecksOnWritesNoKeyAndTellsTheUpdater: on, the save writes
// no key, the file's default, and the updater is told once.
func TestTurningUpdateChecksOnWritesNoKeyAndTellsTheUpdater(t *testing.T) {
	srv, _, cfgPath := newTestServer(t)
	off := false
	cfg := srv.deps.CfgHolder.Clone()
	cfg.Update.CheckEnabled = &off
	srv.deps.CfgHolder.Store(cfg)
	var told []bool
	srv.deps.UpdateChecksSwitched = func() {
		told = append(told, srv.deps.CfgHolder.Load().Update.EffectiveCheckEnabled())
	}
	var resp settingsPatchResponse
	if code := doJSON(t, srv.Handler(), http.MethodPatch, "/api/settings", map[string]any{"updateCheckEnabled": true}, &resp); code != http.StatusOK ||
		resp.Fields["updateCheckEnabled"].Status != applyLive {
		t.Fatalf("PATCH on = %d %+v, want 200 and live", code, resp.Fields["updateCheckEnabled"])
	}
	if len(told) != 1 || !told[0] {
		t.Errorf("the updater was told %v, want once, with the live config on", told)
	}
	if v, there := fileUpdateCheckEnabled(t, cfgPath); there {
		t.Errorf("the file holds update.checkEnabled = %v, want no key: on is the default", v)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// switchRefusing is an update provider whose Check now and Install answer the
// switch's refusal, as the cmd/bridge adapter maps the updater's, and whose
// rollback runs.
type switchRefusing struct {
	stubUpdater
	rolledBack bool
}

func (s *switchRefusing) CheckNow(context.Context) (UpdateStatus, error) {
	return s.st, fmt.Errorf("%w: nothing was asked of GitHub", ErrUpdateChecksOff)
}

func (s *switchRefusing) Install(context.Context, bool) (UpdateStatus, error) {
	return s.st, fmt.Errorf("%w: no release is installed while they are off", ErrUpdateChecksOff)
}

func (s *switchRefusing) Rollback(context.Context, bool) error {
	s.rolledBack = true
	return nil
}

// TestTheUpdateEndpointsAnswerTheSwitchsRefusalWithItsCode: Check now and
// Install refused for the switch answer 409 update-checks-off with the
// updater's words; the rollback is not the switch's, and runs.
func TestTheUpdateEndpointsAnswerTheSwitchsRefusalWithItsCode(t *testing.T) {
	srv, _, _ := newTestServer(t)
	up := &switchRefusing{stubUpdater: stubUpdater{st: UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable"}}}
	srv.deps.Updater = up
	for _, path := range []string{"/api/updates/check", "/api/updates/install"} {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, loopbackReq(http.MethodPost, path, ""))
		var env struct{ Error, Message string }
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if w.Code != http.StatusConflict || env.Error != "update-checks-off" || !strings.Contains(env.Message, "update checks are off") {
			t.Errorf("POST %s with update checks off = %d %q %q, want 409 update-checks-off in the updater's words", path, w.Code, env.Error, env.Message)
		}
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, loopbackReq(http.MethodPost, "/api/updates/rollback", ""))
	if w.Code != http.StatusNoContent || !up.rolledBack {
		t.Errorf("POST /api/updates/rollback with update checks off = %d (rolled back %v), want 204 and the rollback run", w.Code, up.rolledBack)
	}
}

// TestTheSettingsPageSaysUpdateChecksAreOff: the Updates panel's first paint,
// for a status whose checks are off, says "update checks off", keeps Check
// now on the page but hidden (the frame that turns checks on reveals it), and
// offers no Install whatever the status says; with checks on it offers both,
// as before. The switch's own input is on the page, checked as the config
// says.
func TestTheSettingsPageSaysUpdateChecksAreOff(t *testing.T) {
	statusRe := regexp.MustCompile(`(?s)<dd id="update-status">(.*?)</dd>`)
	checkRe := regexp.MustCompile(`<button [^>]*id="update-check"[^>]*>`)
	inputRe := regexp.MustCompile(`<input name="updateCheckEnabled"[^>]*>`)
	for _, tc := range []struct {
		name         string
		st           UpdateStatus
		status       string
		checkHidden  bool
		installShown bool
	}{
		{"checks off, a candidate in hand",
			UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", CanInstall: true, UpdateAvailable: true, LatestVersion: "0.2.5"},
			"update checks off", true, false},
		{"checks on, a candidate in hand",
			UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", CanInstall: true, UpdateAvailable: true, LatestVersion: "0.2.5", CheckEnabled: true},
			"update available · 0.2.5", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := renderSettingsWith(t, tc.st)
			m := statusRe.FindStringSubmatch(page)
			if m == nil || pageText(m[1]) != tc.status {
				t.Errorf("the status row reads %q, want %q", pageText(m[1]), tc.status)
			}
			button := checkRe.FindString(page)
			if button == "" {
				t.Fatal("the Updates panel has no Check now button: it must stay on the page to be revealed")
			}
			if hidden := strings.Contains(button, " hidden"); hidden != tc.checkHidden {
				t.Errorf("Check now hidden = %v, want %v (%s)", hidden, tc.checkHidden, button)
			}
			if shown := strings.Contains(page, `id="update-install"`); shown != tc.installShown {
				t.Errorf("Install offered = %v, want %v", shown, tc.installShown)
			}
			if in := inputRe.FindString(page); in == "" || !strings.Contains(in, "checked") {
				t.Errorf("the switch's input = %q, want it on the page, checked as the config says", in)
			}
		})
	}
}

// TestTheJobsCardCarriesTheUpdateCheckSwitch: /api/jobs says whether update
// checks are on, resolved from the live config, for the Jobs card.
func TestTheJobsCardCarriesTheUpdateCheckSwitch(t *testing.T) {
	srv, _, _ := newTestServer(t)
	read := func() any {
		t.Helper()
		var got struct {
			Updates map[string]any `json:"updates"`
		}
		if code := doJSON(t, srv.Handler(), http.MethodGet, "/api/jobs", nil, &got); code != http.StatusOK {
			t.Fatalf("GET /api/jobs = %d", code)
		}
		return got.Updates["checkEnabled"]
	}
	if v := read(); v != true {
		t.Errorf("updates.checkEnabled = %v, want true where the config says nothing", v)
	}
	off := false
	cfg := srv.deps.CfgHolder.Clone()
	cfg.Update.CheckEnabled = &off
	srv.deps.CfgHolder.Store(cfg)
	if v := read(); v != false {
		t.Errorf("updates.checkEnabled = %v with checks off, want false", v)
	}
}

// TestTheUpdatesPanelScriptSaysUpdateChecksAreOff pins, without node, what
// the panel's script does with a status whose checks are off: renderUpdateTile
// takes only an explicit false for off, says so in the status row, hides Check
// now and offers no Install; updateStatusLine says it on the Jobs card, and
// renderJobCards hides the card's Check now. TestTheUpdatesPanelShowsUpdateChecksOff
// runs the panel under node where node is installed.
func TestTheUpdatesPanelScriptSaysUpdateChecksAreOff(t *testing.T) {
	src := readConsoleJS(t, "static/app.js")
	tile := stripJSComments(extractJSFunction(t, src, "renderUpdateTile"))
	for _, want := range []string{
		`const checksOff = u?.checkEnabled === false;`,
		`if (checkBtn) checkBtn.hidden = checksOff;`,
		`const should = u?.updateAvailable && u?.canInstall && !checksOff;`,
		"status.innerHTML = `<span class=\"badge idle\">update checks off</span>`;",
	} {
		if !strings.Contains(tile, want) {
			t.Errorf("renderUpdateTile has no %s", want)
		}
	}
	if line := stripJSComments(extractJSFunction(t, src, "updateStatusLine")); !strings.Contains(line, `if (r.checkEnabled === false) return "update checks off";`) {
		t.Error("updateStatusLine does not say update checks are off")
	}
	if cards := stripJSComments(extractJSFunction(t, src, "renderJobCards")); !strings.Contains(cards, `if (updCheck) updCheck.hidden = updChecksOff;`) {
		t.Error("renderJobCards does not hide the Jobs card's Check now while update checks are off")
	}
}

// TestTheUpdatesPanelShowsUpdateChecksOff runs the shipped panel under node,
// where node is installed, over updates frames: checks on with a candidate
// (Install and Check now offered), checks off (the status says so, Check now
// hidden, no Install, Roll back as canRollback says), and on again.
func TestTheUpdatesPanelShowsUpdateChecksOff(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped console source")
	}
	checked := time.Date(2026, 11, 4, 11, 0, 0, 0, time.UTC)
	on := UpdateStatus{CurrentVersion: "0.2.4", LatestVersion: "0.2.5", UpdateAvailable: true, Channel: "stable",
		LastCheck: &checked, CanInstall: true, CanRollback: true, CheckEnabled: true}
	off := UpdateStatus{CurrentVersion: "0.2.4", Channel: "stable", LastCheck: &checked, CanInstall: true, CanRollback: true}
	got := runUpdatesPanel(t, node, []updatesPanelStep{
		{Frame: updatesFrame(t, on)},
		{Frame: updatesFrame(t, off)},
		{Frame: updatesFrame(t, on)},
	})
	for i, want := range []struct {
		status      []string
		install     bool
		checkHidden bool
	}{
		{[]string{"update available", "0.2.5"}, true, false},
		{[]string{"update checks off"}, false, true},
		{[]string{"update available", "0.2.5"}, true, false},
	} {
		g := got[i]
		for _, s := range want.status {
			if !strings.Contains(g.Status, s) {
				t.Errorf("frame %d: the status reads %q, want it to say %q", i, g.Status, s)
			}
		}
		if g.Install != want.install || g.CheckHidden != want.checkHidden {
			t.Errorf("frame %d: Install %v, Check now hidden %v; want %v, %v",
				i, g.Install, g.CheckHidden, want.install, want.checkHidden)
		}
		if g.RollbackHidden {
			t.Errorf("frame %d: Roll back hidden; the rollback is not the switch's", i)
		}
	}
}
