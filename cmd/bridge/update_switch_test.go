package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/supervision"
	"github.com/acoseac/1-bit-bridge/internal/updater"
)

// The update check's switch through serve and the CLI (backlog B761): serve's
// updater reads update.checkEnabled off the live config, the console's save
// tells it of a change once it is published, and `bridge update` reads the
// config the service loads, file and environment alike. Every request is
// counted by a releases API of the test's own (serveOpts.updateReleases,
// updateReleases).

const updateCheckVar = "BRIDGE_UPDATE_CHECK_ENABLED"

// releasesStandIn is a GitHub releases API of the test's own whose latest
// release is v99.0.0, counting what it is asked; while holding is set it
// holds each request for the latest release until the test releases it or
// the request's client gives up, handing the test each one.
type releasesStandIn struct {
	srv     *httptest.Server
	asked   atomic.Int32
	holding atomic.Bool
	held    chan releasesHeld

	mu       sync.Mutex
	releases []chan struct{}
}

// releasesHeld is one request the stand-in holds.
type releasesHeld struct {
	ended   <-chan struct{}
	release chan struct{}
}

func newReleasesStandIn(t *testing.T) *releasesStandIn {
	t.Helper()
	s := &releasesStandIn{held: make(chan releasesHeld, 8)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.asked.Add(1)
		if r.URL.Path != "/repos/"+updater.DefaultRepo+"/releases/latest" {
			http.NotFound(w, r)
			return
		}
		if s.holding.Load() {
			ended, release := make(chan struct{}), make(chan struct{})
			s.mu.Lock()
			s.releases = append(s.releases, release)
			s.mu.Unlock()
			s.held <- releasesHeld{ended: ended, release: release}
			select {
			case <-r.Context().Done():
				close(ended)
				// The client gave up: answer nothing.
				panic(http.ErrAbortHandler)
			case <-release:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"tag_name":"v99.0.0","name":"v99.0.0",`+
			`"html_url":"https://github.com/`+updater.DefaultRepo+`/releases/tag/v99.0.0","assets":[]}`)
	}))
	t.Cleanup(s.srv.Close)
	// Before the server's Close, which waits for every request.
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, r := range s.releases {
			select {
			case <-r:
			default:
				close(r)
			}
		}
	})
	return s
}

// client is a releases client that asks s, on a transport of its own.
func (s *releasesStandIn) client(t *testing.T) *updater.Client {
	t.Helper()
	return updater.NewClient(updater.DefaultRepo, 30*time.Second).WithBaseURL(s.srv.URL)
}

// asCLIs points `bridge update`'s client at s for the test's length.
func (s *releasesStandIn) asCLIs(t *testing.T) {
	t.Helper()
	prev := updateReleases
	updateReleases = s.client(t)
	t.Cleanup(func() { updateReleases = prev })
}

func (s *releasesStandIn) waitHeld(t *testing.T) releasesHeld {
	t.Helper()
	select {
	case h := <-s.held:
		return h
	case <-time.After(time.Minute):
		t.Fatal("no request reached the releases API")
		return releasesHeld{}
	}
}

func (h releasesHeld) waitEnded(t *testing.T) {
	t.Helper()
	select {
	case <-h.ended:
	case <-time.After(time.Minute):
		t.Fatal("the request held was never stopped: it stayed on the wire")
	}
}

// updateCLIConfig writes a config `bridge update` loads, with tail appended,
// and returns its path.
func updateCLIConfig(t *testing.T, tail string) string {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bridge.yaml")
	text := "libraryRoots:\n  - " + lib + "\ndataDir: " + filepath.Join(dir, "data") + "\n" + tail
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestBridgeUpdateRefusesItsCheckAndItsInstallWhileUpdateChecksAreOff:
// `bridge update` reads the switch through the loader the service reads its
// config with, the file and BRIDGE_UPDATE_CHECK_ENABLED alike, and while it
// says off refuses both things it does, the check (--check) and the install
// (--yes), exit 1, naming what turned the checks off, before anything is
// asked of GitHub or written in the data dir: with the file saying on and the
// variable off, which a unit or a compose file sets for the service and the
// operator's shell must carry too, and with the file saying off. With the
// checks on, --check asks once (the positive control).
func TestBridgeUpdateRefusesItsCheckAndItsInstallWhileUpdateChecksAreOff(t *testing.T) {
	for _, c := range []struct {
		name, tail, env, says string
	}{
		{name: "the file on, the variable off", tail: "update:\n  checkEnabled: true\n", env: "false", says: updateCheckVar},
		{name: "the file off", tail: "update:\n  checkEnabled: false\n", says: "update.checkEnabled is false in"},
	} {
		for _, args := range [][]string{{"--check"}, {"--yes"}} {
			t.Run(c.name+": "+strings.Join(args, " "), func(t *testing.T) {
				t.Setenv(updateCheckVar, c.env)
				if c.env == "" {
					os.Unsetenv(updateCheckVar)
				}
				gh := newReleasesStandIn(t)
				gh.asCLIs(t)
				cfg := updateCLIConfig(t, c.tail)
				code, out := runCLI(t, append([]string{"update", "--config", cfg}, args...)...)
				if code != 1 {
					t.Errorf("bridge update %s with update checks off exited %d, want 1:\n%s", strings.Join(args, " "), code, out)
				}
				if !strings.Contains(out, "update checks are off") || !strings.Contains(out, c.says) ||
					!strings.Contains(out, "neither asks GitHub for a release nor installs one") {
					t.Errorf("the refusal does not say what is off and why (%q):\n%s", c.says, out)
				}
				if n := gh.asked.Load(); n != 0 {
					t.Errorf("bridge update asked the releases API %d time(s) with update checks off, want 0", n)
				}
				if entries, err := os.ReadDir(filepath.Join(filepath.Dir(cfg), "data")); err == nil && len(entries) != 0 {
					t.Errorf("the refused command wrote in the data dir: %v", entries)
				}
			})
		}
	}

	t.Run("checks on: the check asks once", func(t *testing.T) {
		t.Setenv(updateCheckVar, "")
		os.Unsetenv(updateCheckVar)
		gh := newReleasesStandIn(t)
		gh.asCLIs(t)
		code, out := runCLI(t, "update", "--check", "--config", updateCLIConfig(t, "update:\n  checkEnabled: true\n"))
		if code != 0 || gh.asked.Load() != 1 || !strings.Contains(out, "Latest:  99.0.0") {
			t.Errorf("bridge update --check with update checks on: exit %d after %d request(s), want 0 after 1, 99.0.0 found:\n%s",
				code, gh.asked.Load(), out)
		}
	})
}

// updatesPanelState is what GET /api/updates says of the update check and
// of what a check found.
type updatesPanelState struct {
	CheckEnabled    bool   `json:"checkEnabled"`
	UpdateAvailable bool   `json:"updateAvailable"`
	LatestVersion   string `json:"latestVersion"`
	LastError       string `json:"lastError"`
	CanRollback     bool   `json:"canRollback"`
}

func readUpdatesPanel(t *testing.T, b *consoleBridge) updatesPanelState {
	t.Helper()
	var p updatesPanelState
	getJSON(t, b.console, b.adminBase+"/api/updates", "", &p)
	return p
}

// postUpdates sends what the console's Check now or Install sends, and returns
// the answer's status and its error code.
func postUpdates(t *testing.T, b *consoleBridge, path string) (int, string) {
	t.Helper()
	resp, err := b.console.Post(b.adminBase+path, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var env struct{ Error string }
	_ = json.Unmarshal(raw, &env)
	return resp.StatusCode, env.Error
}

// TestServeTurnsUpdateChecksOffLiveAndShowsNoUpdate boots serve with update
// checks on, over a binary with its previous version's backup beside it,
// asking a releases API of the test's own, which has 99.0.0; the start's
// check finds it. The console's save then turns the checks off: the Updates
// panel says so and shows no update (GET /api/updates, and the Settings
// page's first paint, with no Install and Check now hidden), /v1/health hints
// none to a paired device, Check now and Install answer 409
// update-checks-off, and nothing more is asked of the releases API; Roll back
// is not the switch's, and puts the backup back. Turned on again, the panel
// shows the last check's answer again and nothing is asked: the next check
// comes a full interval later.
func TestServeTurnsUpdateChecksOffLiveAndShowsNoUpdate(t *testing.T) {
	persistentMedium(t)
	t.Setenv(updateCheckVar, "")
	os.Unsetenv(updateCheckVar)
	gh := newReleasesStandIn(t)
	var bin string
	b := startConsoleBridge(t, "", nil, func(o *serveOpts) {
		o.updateReleases = gh.client(t)
		bin, _ = layOutRollback(t, filepath.Dir(o.configPath), true)
		o.binaryPath = bin
	})
	eventually(t, func() bool { return readUpdatesPanel(t, b).UpdateAvailable }, "the start's check to find 99.0.0")
	if p := readUpdatesPanel(t, b); !p.CheckEnabled || p.LatestVersion != "99.0.0" || !p.CanRollback {
		t.Fatalf("the Updates panel with checks on = %+v, want them on, 99.0.0 found and Roll back offered", p)
	}
	asked := gh.asked.Load()

	if code, resp := patchSettings(t, b, `{"updateCheckEnabled":false}`); code != http.StatusOK {
		t.Fatalf("PATCH updateCheckEnabled false = %d %s", code, resp)
	}
	if p := readUpdatesPanel(t, b); p.CheckEnabled || p.UpdateAvailable || p.LatestVersion != "" || p.LastError != "" || !p.CanRollback {
		t.Errorf("the Updates panel with checks off = %+v, want them off, no update shown, Roll back still offered", p)
	}
	page := consolePage(t, b, "/settings")
	if status := regexp.MustCompile(`(?s)<dd id="update-status">(.*?)</dd>`).FindStringSubmatch(page); status == nil ||
		!strings.Contains(status[1], "update checks off") {
		t.Errorf("the Settings page's Updates panel does not say update checks are off")
	}
	if strings.Contains(page, `id="update-install"`) {
		t.Error("the Settings page offers Install with update checks off")
	}
	if check := regexp.MustCompile(`<button [^>]*id="update-check"[^>]*>`).FindString(page); !strings.Contains(check, " hidden") {
		t.Errorf("the Settings page's Check now is not hidden with update checks off: %s", check)
	}
	mint := pairViaAdmin(t, t.Context(), b.console, b.adminBase+"/api/tokens", `{"name":"update switch"}`, http.StatusCreated, b.stderr)
	token := linkQueryItems(t, mint.PairURL)["token"]
	status, raw := getBytesWithToken(t, b, "/v1/health", token)
	var health map[string]any
	if err := json.Unmarshal(raw, &health); status != http.StatusOK || err != nil {
		t.Fatalf("GET /v1/health = %d (%v): %s", status, err, raw)
	}
	if v, ok := health["updateAvailable"]; !ok || v != false {
		t.Errorf("/v1/health's updateAvailable = %v (present %v) with update checks off, want false", v, ok)
	}
	if v, ok := health["latestServerVersion"]; ok {
		t.Errorf("/v1/health advertises latestServerVersion %v with update checks off", v)
	}
	for _, path := range []string{"/api/updates/check", "/api/updates/install"} {
		if code, short := postUpdates(t, b, path); code != http.StatusConflict || short != "update-checks-off" {
			t.Errorf("POST %s with update checks off = %d %s, want 409 update-checks-off", path, code, short)
		}
	}
	if n := gh.asked.Load(); n != asked {
		t.Errorf("the releases API was asked %d more time(s) after update checks went off, want 0", n-asked)
	}
	if code, short, msg := postRollback(t, b); code != http.StatusNoContent {
		t.Errorf("POST /api/updates/rollback with update checks off = %d %s %q, want 204", code, short, msg)
	} else if got, err := os.ReadFile(bin); err != nil || string(got) != "the previous bridge binary" {
		t.Errorf("the binary after the rollback = %q (%v), want the backup put back", got, err)
	}

	if code, resp := patchSettings(t, b, `{"updateCheckEnabled":true}`); code != http.StatusOK {
		t.Fatalf("PATCH updateCheckEnabled true = %d %s", code, resp)
	}
	if p := readUpdatesPanel(t, b); !p.CheckEnabled || !p.UpdateAvailable || p.LatestVersion != "99.0.0" {
		t.Errorf("the Updates panel with checks on again = %+v, want the last check's 99.0.0 shown", p)
	}
	if n := gh.asked.Load(); n != asked {
		t.Errorf("turning update checks on asked the releases API %d time(s), want 0", n-asked)
	}
}

// TestServeStopsAnUpdateCheckUnderWayWhenChecksGoOff boots serve with update
// checks on and its releases API holding every request. A console save that
// fails, its cadence invalid beside the switch turned off, changes nothing:
// the start's check, held, is let go and answers, and the panel shows what it
// found. A Check now is then held on the wire when a save turns the checks
// off: it is stopped where it is, answers 409 update-checks-off, and the
// panel shows the checks off, the stop no failure.
func TestServeStopsAnUpdateCheckUnderWayWhenChecksGoOff(t *testing.T) {
	persistentMedium(t)
	t.Setenv(updateCheckVar, "")
	os.Unsetenv(updateCheckVar)
	gh := newReleasesStandIn(t)
	gh.holding.Store(true)
	b := startConsoleBridge(t, "", nil, func(o *serveOpts) { o.updateReleases = gh.client(t) })
	start := gh.waitHeld(t)

	if code, _ := patchSettings(t, b, `{"updateCheckEnabled":false,"updateCheckIntervalHours":-1}`); code != http.StatusBadRequest {
		t.Fatalf("a PATCH with an invalid cadence = %d, want 400", code)
	}
	close(start.release)
	eventually(t, func() bool { return readUpdatesPanel(t, b).UpdateAvailable }, "the start's check, which the failed save left running, to answer")

	// The Check now on a goroutine of its own, which hands its answer back
	// rather than fail the test from there.
	type answer struct {
		code  int
		short string
		err   error
	}
	answered := make(chan answer, 1)
	go func() {
		resp, err := b.console.Post(b.adminBase+"/api/updates/check", "application/json", nil)
		if err != nil {
			answered <- answer{err: err}
			return
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var env struct{ Error string }
		_ = json.Unmarshal(raw, &env)
		answered <- answer{code: resp.StatusCode, short: env.Error}
	}()
	held := gh.waitHeld(t)
	if code, resp := patchSettings(t, b, `{"updateCheckEnabled":false}`); code != http.StatusOK {
		t.Fatalf("PATCH updateCheckEnabled false = %d %s", code, resp)
	}
	held.waitEnded(t)
	select {
	case got := <-answered:
		if got.err != nil || got.code != http.StatusConflict || got.short != "update-checks-off" {
			t.Errorf("the Check now the switch stopped answered %d %s (%v), want 409 update-checks-off", got.code, got.short, got.err)
		}
	case <-time.After(time.Minute):
		t.Fatal("the Check now the switch stopped never answered")
	}
	if p := readUpdatesPanel(t, b); p.CheckEnabled || p.UpdateAvailable || p.LastError != "" {
		t.Errorf("the Updates panel after the stop = %+v, want checks off, no update and no error", p)
	}
	if n := gh.asked.Load(); n != 2 {
		t.Errorf("the releases API was asked %d time(s), want 2: the start's and the stopped Check now", n)
	}
}

// TestAConsoleSaveKeepsTheFilesOwnUpdateCheckSwitchThroughServe boots serve
// with the switch as the file and BRIDGE_UPDATE_CHECK_ENABLED set it and
// drives the console's real settings save of another field, the switch sent
// as the console shows it: the file keeps its own value whatever the variable
// made the running switch, and the running switch stays the variable's. The
// file's false is never lost to an unrelated save, nor written from the
// variable into a file that said nothing.
func TestAConsoleSaveKeepsTheFilesOwnUpdateCheckSwitchThroughServe(t *testing.T) {
	persistentMedium(t)
	for _, c := range []struct {
		name, tail, env string
		live            bool
		filed           any
		there           bool
	}{
		{name: "the file off, no variable", tail: "update:\n  checkEnabled: false\n", live: false, filed: false, there: true},
		{name: "the variable off over a file that says nothing", env: "false", live: false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(updateCheckVar, c.env)
			if c.env == "" {
				os.Unsetenv(updateCheckVar)
			}
			gh := newReleasesStandIn(t)
			b := startConsoleBridge(t, c.tail, nil, func(o *serveOpts) { o.updateReleases = gh.client(t) })
			cfgPath := filepath.Join(filepath.Dir(b.lib), "bridge.yaml")
			for i := 1; i <= 2; i++ {
				body := fmt.Sprintf(`{"libraryName":"Saved %d","updateCheckEnabled":%v}`, i, shownField(t, b, "updateCheckEnabled"))
				if code, resp := patchSettings(t, b, body); code != http.StatusOK {
					t.Fatalf("PATCH %s = %d %s", body, code, resp)
				}
				v, there := fileUpdateCheck(t, cfgPath)
				if there != c.there || (there && v != c.filed) {
					t.Errorf("save %d: the file holds update.checkEnabled = %v (there %v), want %v (there %v)", i, v, there, c.filed, c.there)
				}
				if got := readUpdatesPanel(t, b).CheckEnabled; got != c.live {
					t.Errorf("save %d: the running switch is %v, want %v", i, got, c.live)
				}
			}
			if n := gh.asked.Load(); n != 0 {
				t.Errorf("the releases API was asked %d time(s) with update checks off, want 0", n)
			}
		})
	}
}

// fileUpdateCheck reads update.checkEnabled from the bridge.yaml at path as
// the file holds it.
func fileUpdateCheck(t *testing.T, path string) (any, bool) {
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

// consolePage is the console's page at path, as a browser reads it.
func consolePage(t *testing.T, b *consoleBridge, path string) string {
	t.Helper()
	resp, err := b.console.Get(b.adminBase + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", path, resp.StatusCode)
	}
	return string(raw)
}

// TestAStartsCrashRollbackRunsWithUpdateChecksOff: the rollbacks a start
// makes are not the update check's switch's (backlog B761), any more than the
// console's is: `bridge update` has no rollback of its own, and an install
// whose starts crashed is rolled back by the start after them, with update
// checks off in the settings as with them on: the backup is put back and the
// start exits 75 for its service manager.
func TestAStartsCrashRollbackRunsWithUpdateChecksOff(t *testing.T) {
	r := newRefusedInstall(t, "update:\n  checkEnabled: false\n")
	for start := 1; start <= updater.ProbationFailedStarts; start++ {
		r.startThatPanics(t)
	}
	code, stderr, served := serveToItsEnd(t, serveOpts{configPath: r.cfg, binaryPath: r.bin})
	if served || code != supervision.RestartExitCode {
		t.Fatalf("the start after the crashes = %d (served %v), want %d before it served; stderr=%s",
			code, served, supervision.RestartExitCode, stderr)
	}
	if got := reportedVersion(t, r.bin); !strings.Contains(got, previousVersion) {
		t.Errorf("the binary reports %q after the rollback, want %s", got, previousVersion)
	}
}
