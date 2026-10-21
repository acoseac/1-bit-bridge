package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
	"github.com/acoseac/1-bit-bridge/internal/outputcheck"
)

// TestChecklistInputReadsTheOutputsBody: the checklist reads the body
// GET /v1/outputs serves: the player, MPD and path rules every element
// shares, each element's card, and the loopback route's base URL.
func TestChecklistInputReadsTheOutputsBody(t *testing.T) {
	yes := true
	mpdObj := &api.OutputMPD{Port: 6600, Addresses: []string{"192.168.1.42"}, Version: "0.24.0", State: "play",
		ReachableFromLAN: true, LoopbackBaseURL: "http://127.0.0.1:7791"}
	rules := []api.OutputPathRule{{Root: "INTERNAL", State: "mismatch", Checked: 20, Found: 3}}
	resp := api.OutputsResponse{Outputs: []api.OutputInfo{
		{Kind: "mpd", Player: api.OutputPlayer{Software: "volumio", Version: "4.119"}, MPD: mpdObj, PathRules: rules,
			DAC: &api.OutputDAC{ID: "Audio", Name: "iFi", USBID: "20b1:3009", IsMPDsOutput: &yes,
				Capabilities: api.OutputCapabilities{MaxPCMRate: 384000, NativeDSDRates: []int{11289600}}}},
		{Kind: "mpd", Player: api.OutputPlayer{Software: "volumio", Version: "4.119"}, MPD: mpdObj, PathRules: rules,
			DAC: &api.OutputDAC{ID: "Other", Name: "Other card"}},
		{Kind: "bridge", PathRules: []api.OutputPathRule{}},
	}}
	in := checklistInput(resp)
	if in.Player.Software != "volumio" || in.Player.Version != "4.119" || in.Player.MPDVersion != "0.24.0" ||
		in.Player.Port != 6600 || len(in.Player.Addresses) != 1 || in.Player.Addresses[0] != "192.168.1.42" {
		t.Fatalf("player %+v", in.Player)
	}
	if in.Loopback.BaseURL != "http://127.0.0.1:7791" {
		t.Fatalf("loopback %+v", in.Loopback)
	}
	if len(in.Roots) != 1 || in.Roots[0] != (outputcheck.RootRule{Root: "INTERNAL", State: "mismatch", Checked: 20, Found: 3}) {
		t.Fatalf("roots %+v", in.Roots)
	}
	if len(in.DACs) != 2 || !in.DACs[0].IsMPDsOutput || in.DACs[1].IsMPDsOutput ||
		in.DACs[0].MaxPCMRate != 384000 || in.DACs[0].USBID != "20b1:3009" || in.DACs[1].ID != "Other" {
		t.Fatalf("dacs %+v", in.DACs)
	}
}

// TestTheConsoleAndTheDoctorShowOneChecklist: the dashboard's panel and the
// doctor's report (the console's Diagnostics panel, and `bridge doctor`)
// carry the same four lines, from one source: same keys, verdicts,
// summaries and fixes, in the same order. The console's doctor reads the
// serving process's source, not a one-shot builder of its own.
func TestTheConsoleAndTheDoctorShowOneChecklist(t *testing.T) {
	yes := true
	src := outputChecklistSource{
		Active: func(context.Context) bool { return true },
		Outputs: func(context.Context) (api.OutputsResponse, error) {
			return api.OutputsResponse{Outputs: []api.OutputInfo{{
				Kind:   "mpd",
				Player: api.OutputPlayer{Software: "volumio", Version: "4.119"},
				MPD:    &api.OutputMPD{Port: 6600, Version: "0.24.0", Addresses: []string{}, LoopbackBaseURL: "http://127.0.0.1:7791"},
				DAC: &api.OutputDAC{ID: "Audio", Name: "iFi (by AMR) HD USB Audio", USBID: "20b1:3009", IsMPDsOutput: &yes,
					Capabilities: api.OutputCapabilities{MaxPCMRate: 384000, NativeDSDRates: []int{11289600}}},
				PathRules: []api.OutputPathRule{{Root: "INTERNAL", State: "unproven"}},
			}}}, nil
		},
		LoopbackErr: func() error { return nil },
		Tokens:      func() []auth.Token { return nil },
	}
	ctx := context.Background()
	console, ok := consoleOutputChecklist(src)(ctx)
	if !ok || len(console) != 4 {
		t.Fatalf("console: ok %v, %d lines", ok, len(console))
	}

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + dir + "\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\ndataDir: " + filepath.Join(dir, "data") + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	report := adminDoctorRunner(cfgPath, nil, checklistDoctorChecks(src))(ctx)
	byName := map[string]admin.DoctorCheck{}
	var order []string
	for _, c := range report.Checks {
		switch c.Name {
		case "player", "dac", "music", "phone":
			byName[c.Name] = c
			order = append(order, c.Name)
		}
	}
	if strings.Join(order, ",") != "player,dac,music,phone" {
		t.Fatalf("the console's doctor carries %v", order)
	}
	for _, l := range console {
		c := byName[l.Key]
		if c.Status != l.Status || c.Summary != l.Summary || c.Hint != l.Fix {
			t.Errorf("%s: console %q %q %q, doctor %q %q %q", l.Key, l.Status, l.Summary, l.Fix, c.Status, c.Summary, c.Hint)
		}
	}
	cli := doctorChecks(mustChecklist(t, src))
	for i, c := range cli {
		if c.Name != console[i].Key || string(c.Status) != console[i].Status || c.Summary != console[i].Summary || c.Hint != console[i].Fix {
			t.Errorf("bridge doctor line %d %+v, console %+v", i, c, console[i])
		}
	}
}

// TestTheChecklistJudgesTheConfiguredRootsWhenTheBoxCannotBeRead: when the
// outputs body cannot be read, the checklist carries the error, judges the
// configured roots, by the name a path rule gives a root, as unchecked,
// and reads the route's base URL and start error from the route itself,
// so the Music line still says whether those tracks play over it.
func TestTheChecklistJudgesTheConfiguredRootsWhenTheBoxCannotBeRead(t *testing.T) {
	unread := outputChecklistSource{
		Active: func(context.Context) bool { return true },
		Outputs: func(context.Context) (api.OutputsResponse, error) {
			return api.OutputsResponse{}, context.DeadlineExceeded
		},
		Roots:       func() []string { return []string{filepath.Join("srv", "Music")} },
		LoopbackErr: func() error { return nil },
		Tokens:      func() []auth.Token { return []auth.Token{{LastUsedAt: time.Now()}} },
	}
	for _, tc := range []struct {
		name          string
		edit          func(*outputChecklistSource)
		status        string
		summary, hint string
	}{
		{"port taken", func(s *outputChecklistSource) {
			s.LoopbackErr = func() error { return errors.New("listen tcp4 127.0.0.1:7791: bind: address already in use") }
		}, "fail", "MPD was not asked about Music", "output.loopbackPort"},
		{"route serving", func(s *outputChecklistSource) {
			s.LoopbackBaseURL = func() string { return "http://127.0.0.1:7791" }
		}, "warn", "plays through the bridge's loopback HTTP route", "Player line"},
		{"no roots", func(s *outputChecklistSource) {
			s.Roots = func() []string { return nil }
		}, "ok", "Ready: can play music from another bridge or NAS", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := unread
			tc.edit(&src)
			lines := mustChecklist(t, src)
			if p := lines[0]; p.Status != outputcheck.Fail || !strings.Contains(p.Summary, context.DeadlineExceeded.Error()) {
				t.Errorf("player %+v", p)
			}
			if d := lines[1]; !strings.Contains(d.Summary, "Not checked") {
				t.Errorf("dac %+v", d)
			}
			m := lines[2]
			if string(m.Status) != tc.status || !strings.Contains(m.Summary, tc.summary) || !strings.Contains(m.Fix, tc.hint) {
				t.Errorf("music %+v, want %s with %q and %q", m, tc.status, tc.summary, tc.hint)
			}
		})
	}
}

// TestServeChecklistReadsTheLiveRootsAndTheRoute: serve's checklist source
// reads the resolver's live roots and the route's own state, so a box
// whose outputs body cannot be read is still judged on the roots the
// bridge serves now and on the route that runs.
func TestServeChecklistReadsTheLiveRootsAndTheRoute(t *testing.T) {
	got := serveChecklistSourceFields(t)
	for field, want := range map[string]string{
		"Active":          "apiSrv.LocalOutputsActive",
		"Outputs":         "apiSrv.LocalOutputsResponse",
		"Roots":           "apiSrv.Resolver().Roots",
		"LoopbackBaseURL": "outLoop.BaseURL",
		"LoopbackErr":     "outLoop.StartErr",
		"Tokens":          "store.List",
	} {
		if got[field] != want {
			t.Errorf("serve's checklist source sets %s to %q, want %q", field, got[field], want)
		}
	}
}

// serveChecklistSourceFields is each keyed field of the one
// outputChecklistSource literal in runServe, as source text.
func serveChecklistSourceFields(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	serve := topLevelFuncNamed(f, "runServe")
	if serve == nil {
		t.Fatal("main.go has no runServe")
	}
	fields := map[string]string{}
	literals := 0
	ast.Inspect(serve.Body, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && isIdent(lit.Type, "outputChecklistSource") {
			literals++
			forEachKeyedElement(lit, func(key *ast.Ident, value ast.Expr) { fields[key.Name] = types.ExprString(value) })
		}
		return true
	})
	if literals != 1 {
		t.Fatalf("runServe builds %d checklist sources, want one", literals)
	}
	return fields
}

// TestTheDoctorChecklistReadsTheConfiguredRoots: `bridge doctor` gathers
// the configured roots and the route as its port answers, so a box whose
// outputs body cannot be read is judged on them, as serve's is.
func TestTheDoctorChecklistReadsTheConfiguredRoots(t *testing.T) {
	roots := []string{filepath.Join(t.TempDir(), "Music"), filepath.Join(t.TempDir(), "More")}
	cfg := &config.Config{LibraryRoots: roots, DataDir: t.TempDir()}
	lo := api.NewLocalOutputs(api.LocalOutputsConfig{})
	t.Cleanup(lo.Close)
	route := &loopbackAnswer{ask: func() (string, error) { return "http://127.0.0.1:7791", nil }}
	src := doctorChecklistSource(cfg, lo, route)
	if src.Roots == nil || strings.Join(src.Roots(), "|") != strings.Join(roots, "|") {
		t.Fatalf("the doctor's roots are not the configured ones")
	}
	if src.LoopbackBaseURL == nil || src.LoopbackBaseURL() != "http://127.0.0.1:7791" {
		t.Fatal("the doctor does not read the route's base URL from its port")
	}
}

// TestUsablePhonesCountsOnlyTokensAPhoneCanUse: a token counts as a phone
// only when it has not expired and a phone has used it since it was issued
// or last rotated, so the Phone line is never green over a token no phone
// can use. Serve and the doctor count with this one function.
func TestUsablePhonesCountsOnlyTokensAPhoneCanUse(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	twoDaysAgo, yesterday, hourAgo, tomorrow := now.Add(-48*time.Hour), now.Add(-24*time.Hour), now.Add(-time.Hour), now.Add(24*time.Hour)
	for _, tc := range []struct {
		name string
		tok  auth.Token
		want int
	}{
		{"expired", auth.Token{LastUsedAt: twoDaysAgo, ExpiresAt: &yesterday}, 0},
		{"never used", auth.Token{}, 0},
		{"rotated and not used since", auth.Token{LastUsedAt: yesterday, RotatedAt: hourAgo}, 0},
		{"used", auth.Token{LastUsedAt: hourAgo}, 1},
		{"used since its rotation, expiring later", auth.Token{LastUsedAt: hourAgo, RotatedAt: yesterday, ExpiresAt: &tomorrow}, 1},
	} {
		if got := usablePhones([]auth.Token{tc.tok}, now); got != tc.want {
			t.Errorf("%s: counts %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestTheDemoTokenIsNoPhone: the demo's static token lives in memory, never
// in the token file, and List does not return it, so however often it is
// used a demo bridge counts no phone for it.
func TestTheDemoTokenIsNoPhone(t *testing.T) {
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("the-demo-token"))
	if err := store.SetStaticToken(hex.EncodeToString(sum[:]), "demo"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Validate("the-demo-token"); !ok {
		t.Fatal("the demo token does not validate")
	}
	if n := usablePhones(store.List(), time.Now()); n != 0 {
		t.Fatalf("the demo token counts as %d phones", n)
	}
}

// TestBridgeDoctorCountsOnlyAPhoneThatUsedItsToken: `bridge doctor` reads
// tokens.json, where a token minted and never used is no phone; once a
// phone has used it and the serving bridge has written that down, it is.
func TestBridgeDoctorCountsOnlyAPhoneThatUsedItsToken(t *testing.T) {
	data := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.OpenStore(filepath.Join(data, tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.Mint("iPhone")
	if err != nil {
		t.Fatal(err)
	}
	none := 0
	cfg := &config.Config{DataDir: data, ListenAddress: "127.0.0.1:0", Output: config.OutputConfig{MPD: "on", LoopbackPort: &none}}
	phone := func() doctor.Check {
		t.Helper()
		checks := doctorOutputChecklist(cfg, linuxBoxSeam(data))(context.Background())
		return findCheck(t, doctor.Report{Checks: checks}, "phone")
	}
	if p := phone(); p.Status != doctor.Warn || !strings.Contains(p.Summary, "1 device token is issued, but no phone uses it") {
		t.Fatalf("a token never used: %+v", p)
	}
	if _, ok := tokens.Validate(raw); !ok {
		t.Fatal("the token does not validate")
	}
	if err := tokens.FlushLastUsed(); err != nil {
		t.Fatal(err)
	}
	if p := phone(); p.Status != doctor.OK || !strings.Contains(p.Summary, "1 device paired") {
		t.Fatalf("a token a phone used: %+v", p)
	}
}

func mustChecklist(t *testing.T, src outputChecklistSource) []outputcheck.Line {
	t.Helper()
	lines, ok := outputChecklist(context.Background(), src)
	if !ok {
		t.Fatal("no checklist")
	}
	return lines
}

// TestProbeLoopbackRouteKnowsItsOwnRoute: the doctor asks the configured
// port. This bridge's route answers there with its typed refusal, not_found
// while it serves and not_serving while output.mpd does not resolve on in
// the bridge that runs it, and the doctor knows it by either; nothing
// listening is a route that is not serving; anything else on the port is
// named as holding it.
func TestProbeLoopbackRouteKnowsItsOwnRoute(t *testing.T) {
	srv := startProbedRoute(t, true)
	if base, err := probeLoopbackRoute(context.Background(), portOf(t, srv.BaseURL())); err != nil || base != srv.BaseURL() {
		t.Fatalf("own route: base %q err %v, want %q", base, err, srv.BaseURL())
	}
	off := startProbedRoute(t, false)
	if base, err := probeLoopbackRoute(context.Background(), portOf(t, off.BaseURL())); base != "" || !errors.Is(err, errLoopbackNotServing) {
		t.Fatalf("own route, output off: base %q err %v, want no base and errLoopbackNotServing", base, err)
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello"))
	}))
	t.Cleanup(other.Close)
	if base, err := probeLoopbackRoute(context.Background(), portOf(t, other.URL)); err == nil || base != "" {
		t.Fatalf("another server: base %q err %v, want an error naming the port", base, err)
	}

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	if base, err := probeLoopbackRoute(context.Background(), closed); err != nil || base != "" {
		t.Fatalf("nothing listening: base %q err %v, want no route and no error", base, err)
	}
}

type emptyTrackLookup struct{}

func (emptyTrackLookup) GetTrackInfo(string) (dlna.TrackInfo, bool) { return dlna.TrackInfo{}, false }

// startProbedRoute starts a loopback route on a port the kernel picks,
// serving or not as on says, and stops it when the test ends.
func startProbedRoute(t *testing.T, on bool) *dlna.LoopbackServer {
	t.Helper()
	srv := dlna.NewLoopbackServer(dlna.LoopbackConfig{
		Library: emptyTrackLookup{},
		Roots:   func() []string { return nil },
		Enabled: func() bool { return on },
	})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return srv
}

// TestBridgeDoctorSaysTheLoopbackRouteIsNotServing: when the running
// bridge's route answers on its port with not_serving, the doctor knows the
// route as this bridge's, never as something else holding the port, and
// the Music line says the route is up and refusing, with the fix for that.
func TestBridgeDoctorSaysTheLoopbackRouteIsNotServing(t *testing.T) {
	route := startProbedRoute(t, false)
	port := portOf(t, route.BaseURL())
	dir := t.TempDir()
	music := filepath.Join(dir, "Music")
	if err := os.MkdirAll(music, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		LibraryRoots:  []string{music},
		DataDir:       filepath.Join(dir, "data"),
		ListenAddress: "127.0.0.1:0",
		Output:        config.OutputConfig{MPD: "on", LoopbackPort: &port},
	}
	checks := doctorOutputChecklist(cfg, linuxBoxSeam(music))(context.Background())
	line := findCheck(t, doctor.Report{Checks: checks}, "music")
	if line.Status != doctor.Fail || !strings.Contains(line.Summary, "answers but is not serving") ||
		!strings.Contains(line.Hint, "Restart the bridge") {
		t.Fatalf("music line %+v, want a fail saying the route answers but is not serving", line)
	}
}

// linuxBoxSeam stands the doctor's outputs builder on a Linux box with
// Volumio, whose MPD does not answer, whose music directory is music, and
// which has no cards and no LAN greeting.
func linuxBoxSeam(music string) func(*api.LocalOutputsConfig) {
	return func(c *api.LocalOutputsConfig) {
		c.GOOS = "linux"
		c.Inspect = func(context.Context, mpd.Options) mpd.Report {
			return mpd.Report{Software: mpd.Software{Name: "Volumio", Version: "4.119"}, MusicDirectory: music}
		}
		c.Cards = func() (alsa.Snapshot, error) { return alsa.Snapshot{}, nil }
		c.Greet = func(context.Context, string) bool { return false }
		c.Getenv = func(string) string { return "" }
	}
}

func portOf(t *testing.T, base string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatalf("%q: %v", base, err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

// TestBridgeDoctorPrintsThePlayerChecklist: `bridge doctor` over a config
// with output.mpd on reads the box through the one-shot outputs builder,
// the token file and the loopback port, and prints Player, DAC, Music and
// Phone in the checklist's words. Linux, the MPD report and the cards are
// stood in for; the rest is the doctor's own reading.
func TestBridgeDoctorPrintsThePlayerChecklist(t *testing.T) {
	path, music := writePlayerConfig(t)
	d := buildDoctorDeps(path)
	if d.ConfigFile == nil || d.ConfigFile.LoadErr != nil {
		t.Fatalf("config did not load: %+v", d.ConfigFile)
	}
	if d.OutputChecklist == nil {
		t.Fatal("bridge doctor does not ask for the player checklist")
	}
	d.OutputChecklist = doctorOutputChecklist(mustLoadConfig(t, path), linuxBoxSeam(music))
	report := doctor.Run(context.Background(), d)
	for _, want := range []struct {
		name    string
		status  doctor.Status
		summary string
	}{
		{"player", doctor.Fail, "Volumio is installed, but MPD is not answering on port 6600"},
		{"dac", doctor.Fail, "No DAC found on this box"},
		{"music", doctor.Fail, "MPD was not asked about Music"},
		{"phone", doctor.OK, "1 device paired"},
	} {
		requireDoctorLine(t, report, want.name, want.status, want.summary)
	}
}

// TestBridgeDoctorPrintsNoChecklistOffAPlayer: a bridge that is no player,
// here one off Linux, prints no checklist.
func TestBridgeDoctorPrintsNoChecklistOffAPlayer(t *testing.T) {
	path, _ := writePlayerConfig(t)
	off := doctorOutputChecklist(mustLoadConfig(t, path), func(c *api.LocalOutputsConfig) { c.GOOS = "darwin" })
	if lines := off(context.Background()); len(lines) != 0 {
		t.Fatalf("a bridge off Linux printed %d checklist lines", len(lines))
	}
}

// TestBridgeDoctorCallsAPlayerWithNoLibraryReady: a player box with no
// library (the music lives on another bridge or a NAS) is ready, not
// failing.
func TestBridgeDoctorCallsAPlayerWithNoLibraryReady(t *testing.T) {
	path, _ := writePlayerConfig(t)
	cfg := mustLoadConfig(t, path)
	cfg.LibraryRoots = nil
	checks := doctorOutputChecklist(cfg, linuxBoxSeam(""))(context.Background())
	music := findCheck(t, doctor.Report{Checks: checks}, "music")
	if music.Status != doctor.OK || music.Summary != "Ready: can play music from another bridge or NAS" || music.Hint != "" {
		t.Fatalf("music line of a player with no library: %+v", music)
	}
}

// writePlayerConfig writes a player's config, output.mpd on with one
// library root and no loopback probe, beside a token file whose one token
// a phone has used, and returns the config's path and the root.
func writePlayerConfig(t *testing.T) (path, music string) {
	t.Helper()
	dir := t.TempDir()
	music = filepath.Join(dir, "Music")
	data := filepath.Join(dir, "data")
	for _, d := range []string{music, data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pairPhone(t, data)
	path = filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + music + "\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\ndataDir: " + data +
		"\noutput:\n  mpd: on\n  loopbackPort: 0\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, music
}

// pairPhone mints a device token in data's token file, and has a phone use
// it with the serving bridge writing that down, as a paired phone does.
func pairPhone(t *testing.T, data string) {
	t.Helper()
	tokens, err := auth.OpenStore(filepath.Join(data, tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.Mint("iPhone")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tokens.Validate(raw); !ok {
		t.Fatal("the token does not validate")
	}
	if err := tokens.FlushLastUsed(); err != nil {
		t.Fatal(err)
	}
}

func mustLoadConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// requireDoctorLine requires the report's check called name to have
// status, a summary containing summary, and a fix unless it is green.
func requireDoctorLine(t *testing.T, report doctor.Report, name string, status doctor.Status, summary string) {
	t.Helper()
	c := findCheck(t, report, name)
	if c.Status != status || !strings.Contains(c.Summary, summary) {
		t.Errorf("%s: %s %q, want %s containing %q", name, c.Status, c.Summary, status, summary)
	}
	if c.Status != doctor.OK && c.Hint == "" {
		t.Errorf("%s is %s and gives no fix", name, c.Status)
	}
}
