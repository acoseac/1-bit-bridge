package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// manageControls swaps the server's live config for one whose control
// plane owns `names`. Clone → mutate → Store, which is the same
// copy-on-write path the settings PATCH uses; mutating Load()'s result in
// place is the thing config.RuntimeConfig exists to prevent.
// loopbackReq builds a request the admin listener will accept.
// httptest.NewRequest sets RemoteAddr to 192.0.2.1, which loopbackOnly
// refuses — with a 403, the same status a managed control returns. A test
// asserting only the STATUS would pass here without ever reaching the
// gate, which is why every assertion below reads the envelope too.
func loopbackReq(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.RemoteAddr = "127.0.0.1:54321"
	return r
}

func manageControls(t *testing.T, srv *Server, names ...string) {
	t.Helper()
	cfg := srv.deps.CfgHolder.Clone()
	cfg.Deployment.ManagedControls = names
	srv.deps.CfgHolder.Store(cfg)
}

// managedControlRoutes is the mapping under test: which request the
// console would send, and which control owns it.
var managedControlRoutes = []struct {
	control string
	method  string
	path    string
}{
	{config.ManagedControlRestart, "POST", "/api/restart"},
	{config.ManagedControlUpdates, "POST", "/api/updates/check"},
	{config.ManagedControlUpdates, "POST", "/api/updates/install"},
	{config.ManagedControlUpdates, "POST", "/api/updates/rollback"},
	{config.ManagedControlRoots, "POST", "/api/roots"},
	{config.ManagedControlRoots, "DELETE", "/api/roots"},
	{config.ManagedControlVariantsDir, "POST", "/api/upscale/variants-dir"},
	{config.ManagedControlBackups, "POST", "/api/backups"},
}

// TestManagedControlsRefuseTheRequest drives the real handler stack. The
// console hiding these is a courtesy; this is the boundary, because every
// one of them is a plain authenticated request a session holder can send
// by hand.
func TestManagedControlsRefuseTheRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	all := config.KnownManagedControls()
	manageControls(t, srv, all...)

	for _, rt := range managedControlRoutes {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, loopbackReq(rt.method, rt.path, "{}"))

		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", rt.method, rt.path, w.Code)
			continue
		}
		var env struct{ Error, Message string }
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Errorf("%s %s: body is not an error envelope: %v", rt.method, rt.path, err)
			continue
		}
		if env.Error != "managed_by_host" {
			t.Errorf("%s %s: error = %q, want managed_by_host", rt.method, rt.path, env.Error)
		}
		// The message is the whole point of refusing rather than hiding:
		// it has to say who does this instead.
		if env.Message == "" {
			t.Errorf("%s %s: refused with an empty message", rt.method, rt.path)
		}
	}

	// NEGATIVE CONTROL, and the reason this test is not vacuous: with
	// nothing managed the SAME requests must get past the gate. They
	// fail for their own reasons (empty root path, no updater wired) —
	// what matters is that none of them is refused as managed.
	manageControls(t, srv)
	for _, rt := range managedControlRoutes {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, loopbackReq(rt.method, rt.path, "{}"))

		var env struct{ Error string }
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Error == "managed_by_host" {
			t.Errorf("%s %s: refused as managed with an empty managedControls list", rt.method, rt.path)
		}
	}
}

// TestOneManagedControlDoesNotGateAnother pins that the names are
// individually load-bearing. A gate keyed on "is anything managed" would
// pass the test above and take the whole console away from an operator
// who pinned one control.
func TestOneManagedControlDoesNotGateAnother(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()
	manageControls(t, srv, config.ManagedControlRestart)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, loopbackReq("POST", "/api/restart", "{}"))
	if w.Code != http.StatusForbidden {
		t.Errorf("POST /api/restart with restart managed: status = %d, want 403", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, loopbackReq("POST", "/api/roots", `{"path":"/nope"}`))
	var env struct{ Error string }
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Error == "managed_by_host" {
		t.Errorf("POST /api/roots refused as managed when only `restart` is managed")
	}
}

// TestEveryManagedControlNameGatesARoute catches the failure this whole
// mechanism is prone to: a name that exists, is documented, is written
// into a tenant's bridge.yaml — and gates nothing, so the control it was
// meant to remove is still live and nothing says so.
//
// Source scan rather than behaviour, because the question is about the
// route TABLE (does anything wire this name?) and a behavioural test can
// only ask it one route at a time, which is the same list again.
func TestEveryManagedControlNameGatesARoute(t *testing.T) {
	src, err := os.ReadFile("admin.go")
	if err != nil {
		t.Fatal(err)
	}
	// CRLF-normalised: no .gitattributes pins eol, so a Windows checkout
	// would otherwise fail every `\n`-literal scan in this repo.
	routes := strings.ReplaceAll(string(src), "\r\n", "\n")

	for _, name := range config.KnownManagedControls() {
		constName := map[string]string{
			config.ManagedControlRestart:     "ManagedControlRestart",
			config.ManagedControlUpdates:     "ManagedControlUpdates",
			config.ManagedControlRoots:       "ManagedControlRoots",
			config.ManagedControlVariantsDir: "ManagedControlVariantsDir",
			config.ManagedControlBackups:     "ManagedControlBackups",
		}[name]
		if constName == "" {
			t.Errorf("control %q has no constant in this test's table — add it here and to the route table", name)
			continue
		}
		if !strings.Contains(routes, "s.managed(config."+constName) {
			t.Errorf("control %q (config.%s) gates no route in admin.go", name, constName)
		}
	}
}

// TestManagedBridgeDoesNotOfferLogFilesFromTheHost — every wording
// resolveLogFile can produce tells the reader where the log is: a
// terminal, the journal, `docker logs`. All three assume a shell on the
// host, which is the one thing a hosted tenant does not have.
func TestManagedBridgeDoesNotOfferLogFilesFromTheHost(t *testing.T) {
	srv, _, _ := newTestServer(t)
	h := srv.Handler()

	manageControls(t, srv, config.ManagedControlRestart)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, loopbackReq("GET", "/api/logs/status", ""))

	var st logStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v (body %s)", err, w.Body.String())
	}
	if st.Available {
		t.Error("log export offered on a managed bridge")
	}
	for _, forbidden := range []string{"journalctl", "docker logs", "terminal"} {
		if strings.Contains(st.Reason, forbidden) {
			t.Errorf("reason %q tells a hosted tenant to run %q", st.Reason, forbidden)
		}
	}
	if !strings.Contains(st.Reason, "host") {
		t.Errorf("reason %q does not say who has the logs", st.Reason)
	}

	// NEGATIVE CONTROL: unmanaged, the same bridge falls through to the
	// pre-existing wording, which names the file it could not find.
	manageControls(t, srv)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, loopbackReq("GET", "/api/logs/status", ""))
	var st2 logStatusResponse
	if err := json.Unmarshal(w.Body.Bytes(), &st2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st2.Reason == st.Reason {
		t.Errorf("managed and unmanaged give the same reason %q — the branch is not reached", st2.Reason)
	}
}

// TestSettingsPageDropsHostOwnedControls — the console half. The
// endpoints refuse these anyway; a button that renders and then 403s
// reads as a broken console rather than as somebody else's job.
//
// Server-rendered rather than hidden by JS: a control that appears for a
// moment and then vanishes is one a person can click, and the settings
// form is exactly where somebody clicks fast.
func TestSettingsPageDropsHostOwnedControls(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func() string {
		t.Helper()
		resp, err := http.Get(ts.URL + "/settings")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// NEGATIVE CONTROL FIRST, so a page that never renders these at all
	// cannot pass the real assertion by being empty.
	unmanaged := get()
	for _, want := range []string{
		`id="restart-btn"`,
		`id="settings-panel-updates"`,
		// update-check, not update-install: install renders only when an
		// update is actually available, so it would be absent on this
		// fixture and the control would be the vacuous one.
		`id="update-check"`,
		`href="#settings-panel-updates"`,
	} {
		if !strings.Contains(unmanaged, want) {
			t.Fatalf("self-hosted /settings is missing %s — the assertion below would pass vacuously", want)
		}
	}

	manageControls(t, srv, config.ManagedControlRestart, config.ManagedControlUpdates)
	managed := get()
	for _, gone := range []string{
		`id="restart-btn"`,
		`id="settings-panel-updates"`,
		`id="update-check"`,
		`id="update-rollback"`,
		`href="#settings-panel-updates"`,
	} {
		if strings.Contains(managed, gone) {
			t.Errorf("managed /settings still renders %s", gone)
		}
	}
	// The information survives even though the button does not: a
	// restart-bound field still has to tell the reader when it lands.
	// Substring, not the whole sentence: the template wraps it across two
	// source lines, so a longer literal matches nothing.
	if !strings.Contains(managed, "the host restarts this bridge") {
		t.Error("managed /settings drops the restart button without saying when a restart-bound change applies")
	}
	// Managing the lifecycle must not take the whole page away.
	if !strings.Contains(managed, `name="libraryName"`) {
		t.Error("managed /settings lost unrelated controls")
	}
}

// TestDiagnosticsDropsTheMetricsPointerWhenManaged — /metrics is gated to
// loopback, so on a hosted bridge the paragraph offering it points at a
// URL that answers 403 to the very reader being told to scrape it.
func TestDiagnosticsDropsTheMetricsPointerWhenManaged(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func() string {
		t.Helper()
		resp, err := http.Get(ts.URL + "/diagnostics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if !strings.Contains(get(), `href="/metrics"`) {
		t.Fatal("self-hosted /diagnostics has no /metrics pointer — the assertion below would pass vacuously")
	}
	manageControls(t, srv, config.ManagedControlRestart)
	if strings.Contains(get(), `href="/metrics"`) {
		t.Error("managed /diagnostics still points at the loopback-gated /metrics")
	}
}

// TestLibraryPageDropsRootControlsWhenManaged — `POST /api/roots` refuses
// on a managed bridge, so offering the form is offering a 403. This is
// the page a tenant lands on to add music, which makes it the most likely
// place to press something that cannot work.
func TestLibraryPageDropsRootControlsWhenManaged(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(path string) string {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// NEGATIVE CONTROL FIRST — a page that never renders these cannot
	// prove anything by not rendering them.
	unmanaged := get("/library")
	for _, want := range []string{`id="add-root-form"`, `id="variants-change"`} {
		if !strings.Contains(unmanaged, want) {
			t.Fatalf("self-hosted /library is missing %s — the assertion below would pass vacuously", want)
		}
	}

	manageControls(t, srv, config.ManagedControlRoots, config.ManagedControlVariantsDir)
	managed := get("/library")
	for _, gone := range []string{`id="add-root-form"`, `class="btn danger remove-root"`, `id="variants-change"`} {
		if strings.Contains(managed, gone) {
			t.Errorf("managed /library still renders %s", gone)
		}
	}
	// The roots TABLE stays — a tenant should still see where their music
	// is read from, they just cannot change it.
	if !strings.Contains(managed, `id="roots-body"`) {
		t.Error("managed /library dropped the roots table along with the controls")
	}
}

// TestSettingsDropsSnapshotWhenBackupsManaged — the button is hidden by
// the section collapse on a tenant anyway, since its cadence fields are
// managed. Hiding it on its own control means the console and the API
// agree for the reason each of them decides, not by coincidence.
func TestSettingsDropsSnapshotWhenBackupsManaged(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func() string {
		t.Helper()
		resp, err := http.Get(ts.URL + "/settings")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if !strings.Contains(get(), `id="backup-now"`) {
		t.Fatal("self-hosted /settings has no Snapshot button — the assertion below would pass vacuously")
	}
	manageControls(t, srv, config.ManagedControlBackups)
	if strings.Contains(get(), `id="backup-now"`) {
		t.Error("managed /settings still offers Snapshot now")
	}
}

// TestSettingsSaveSendsOnlyWhatThePageOffered guards the console half of
// a defect that made a managed bridge's Settings page unusable.
//
// The Save payload is an explicit allowlist naming every field, so a
// control the page did not offer was still sent — as false / "" / 0,
// which is what fd.get on a missing or hidden input coerces to. A PATCH
// supplying a managed field is refused WHOLE, so renaming the library
// failed with a wall of nineteen unrelated field names. Driven in a
// browser against a managed fixture before and after.
//
// A source scan, because the payload is built in JS and this suite has no
// engine to run it in. It pins the call site, which is the part a later
// refactor of the submit handler would drop.
func TestSettingsSaveSendsOnlyWhatThePageOffered(t *testing.T) {
	raw, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")

	if !strings.Contains(src, "dropUnofferedFields(form, body);") {
		t.Error("the settings submit handler no longer filters its payload — a managed bridge's Save will fail on fields the page never showed")
	}
	if !strings.Contains(src, "function dropUnofferedFields(") {
		t.Error("dropUnofferedFields is gone")
	}
	// The rule is "absent OR hidden", and the hidden half is the one that
	// matters: hideManagedSettings hides the enclosing .field rather than
	// removing it, so a check for presence alone would still send it.
	if !strings.Contains(src, `el.closest("[hidden]")`) {
		t.Error("the filter no longer treats a hidden ancestor as not-offered — managed fields are hidden, not removed")
	}
}

// TestManagedFieldHidesTheControlsThatResolveIntoIt — the enrichment
// source picker has no server field of its own; it resolves into
// enrichMusicBrainzBaseURL and enrichCoverArtBaseURL. Left visible when
// the control plane owns those, it is the worst shape a settings page
// has: it changes, it saves, the page says "Saved.", nothing happened.
//
// The template declares the relationship (`data-managed-with`) and the
// JS acts on it. This asserts the declaration, which is the half that
// would be lost by an unrelated edit to the enrichment markup; the
// behaviour was driven in a browser.
func TestManagedFieldHidesTheControlsThatResolveIntoIt(t *testing.T) {
	raw, err := os.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.Contains(tmpl, `data-managed-with="enrichMusicBrainzBaseURL"`) {
		t.Error("the enrichment source picker no longer declares which managed field it resolves into")
	}

	raw, err = os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.Contains(js, "data-managed-with=") {
		t.Error("hideManagedSettings no longer acts on data-managed-with")
	}
}

// TestManagedControlsAreEnforcedAtTheRouteTable is the guard
// managed_controls.go's docblock has named all along without it existing.
//
// What the two tests above actually check is narrower than that docblock
// claimed: TestManagedControlsRefuseTheRequest drives a hand-maintained literal
// (`managedControlRoutes`), and TestEveryManagedControlNameGatesARoute walks the
// reverse direction — every NAME gates something. Neither reads the route table,
// so a new `s.managed(...)` route silently gets no behavioural coverage, and a
// removed one leaves an entry asserting against a route that no longer exists.
// The docblock said a new one "cannot be added without a decision"; it could.
//
// This closes the loop the honest way: parse admin.go's registrations, extract
// every (control, method, path) actually wrapped in `s.managed`, and require
// that set to EQUAL the table the behavioural test drives. Adding a managed
// route without a test row fails here; deleting a route without pruning the row
// fails here.
//
// It deliberately does NOT claim to catch "a route that SHOULD be managed and
// is not" — nothing can decide that from source, and pretending otherwise is
// how the original claim came to be wrong. What it guarantees is that the
// declared set and the exercised set are the same set.
func TestManagedControlsAreEnforcedAtTheRouteTable(t *testing.T) {
	raw, err := os.ReadFile("admin.go")
	if err != nil {
		t.Fatal(err)
	}
	// CRLF-normalised at the read: nothing pins eol, so a Windows checkout
	// would otherwise make this scan find nothing and pass vacuously.
	src := strings.ReplaceAll(string(raw), "\r\n", "\n")

	// mux.HandleFunc("<METHOD> <PATH>", s.managed(config.<Const>, ...
	re := regexp.MustCompile(
		`mux\.HandleFunc\("([A-Z]+) ([^"]+)",\s*s\.managed\(config\.(ManagedControl\w+)`)
	constToName := map[string]string{
		"ManagedControlRestart":     config.ManagedControlRestart,
		"ManagedControlUpdates":     config.ManagedControlUpdates,
		"ManagedControlRoots":       config.ManagedControlRoots,
		"ManagedControlVariantsDir": config.ManagedControlVariantsDir,
		"ManagedControlBackups":     config.ManagedControlBackups,
	}

	inSource := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		method, path, constName := m[1], m[2], m[3]
		name, ok := constToName[constName]
		if !ok {
			t.Errorf("admin.go gates a route with config.%s, which this test does not "+
				"know — add it here and to managedControlRoutes", constName)
			continue
		}
		inSource[name+" "+method+" "+path] = true
	}
	if len(inSource) < 5 {
		t.Fatalf("only %d managed routes scraped from admin.go — the registration shape "+
			"changed and this test is no longer reading it", len(inSource))
	}

	inTable := map[string]bool{}
	for _, rt := range managedControlRoutes {
		inTable[rt.control+" "+rt.method+" "+rt.path] = true
	}

	for k := range inSource {
		if !inTable[k] {
			t.Errorf("admin.go gates %q but managedControlRoutes does not exercise it — "+
				"the refusal is the security boundary and nothing drives it", k)
		}
	}
	for k := range inTable {
		if !inSource[k] {
			t.Errorf("managedControlRoutes exercises %q, which admin.go does not gate — "+
				"the row passes for some other reason, or the route moved", k)
		}
	}
}
