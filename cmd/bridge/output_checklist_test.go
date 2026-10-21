package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
		Phones:      func() int { return 0 },
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

func mustChecklist(t *testing.T, src outputChecklistSource) []outputcheck.Line {
	t.Helper()
	lines, ok := outputChecklist(context.Background(), src)
	if !ok {
		t.Fatal("no checklist")
	}
	return lines
}

// TestProbeLoopbackRouteKnowsItsOwnRoute: the doctor asks the configured
// port. This bridge's route answers there with its typed refusal; nothing
// listening is a route that is not serving; anything else on the port is
// named as holding it.
func TestProbeLoopbackRouteKnowsItsOwnRoute(t *testing.T) {
	srv := dlna.NewLoopbackServer(dlna.LoopbackConfig{Library: emptyTrackLookup{}, Roots: func() []string { return nil }})
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	if base, err := probeLoopbackRoute(context.Background(), portOf(t, srv.BaseURL())); err != nil || base != srv.BaseURL() {
		t.Fatalf("own route: base %q err %v, want %q", base, err, srv.BaseURL())
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
	dir := t.TempDir()
	music := filepath.Join(dir, "Music")
	data := filepath.Join(dir, "data")
	for _, d := range []string{music, data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tokens, err := auth.OpenStore(filepath.Join(data, tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tokens.Mint("iPhone"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + music + "\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\ndataDir: " + data +
		"\noutput:\n  mpd: on\n  loopbackPort: 0\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	d := buildDoctorDeps(path)
	if d.ConfigFile == nil || d.ConfigFile.LoadErr != nil {
		t.Fatalf("config did not load: %+v", d.ConfigFile)
	}
	if d.OutputChecklist == nil {
		t.Fatal("bridge doctor does not ask for the player checklist")
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	d.OutputChecklist = doctorOutputChecklist(cfg, func(c *api.LocalOutputsConfig) {
		c.GOOS = "linux"
		c.Inspect = func(context.Context, mpd.Options) mpd.Report {
			return mpd.Report{Software: mpd.Software{Name: "Volumio", Version: "4.119"}, MusicDirectory: music}
		}
		c.Cards = func() (alsa.Snapshot, error) { return alsa.Snapshot{}, nil }
		c.Greet = func(context.Context, string) bool { return false }
		c.Getenv = func(string) string { return "" }
	})
	report := doctor.Run(context.Background(), d)
	got := map[string]doctor.Check{}
	for _, c := range report.Checks {
		got[c.Name] = c
	}
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
		c, ok := got[want.name]
		if !ok {
			t.Fatalf("no %s line in %+v", want.name, report.Checks)
		}
		if c.Status != want.status || !strings.Contains(c.Summary, want.summary) {
			t.Errorf("%s: %s %q, want %s containing %q", want.name, c.Status, c.Summary, want.status, want.summary)
		}
		if c.Status != doctor.OK && c.Hint == "" {
			t.Errorf("%s is %s and gives no fix", want.name, c.Status)
		}
	}

	// A bridge that is no player prints no checklist.
	off := doctorOutputChecklist(cfg, func(c *api.LocalOutputsConfig) { c.GOOS = "darwin" })
	if lines := off(context.Background()); len(lines) != 0 {
		t.Fatalf("a bridge off Linux printed %d checklist lines", len(lines))
	}

	// A player box with no library (the music lives on another bridge or a
	// NAS) is ready, not failing.
	noRoots := *cfg
	noRoots.LibraryRoots = nil
	ready := doctorOutputChecklist(&noRoots, func(c *api.LocalOutputsConfig) {
		c.GOOS = "linux"
		c.Inspect = func(context.Context, mpd.Options) mpd.Report {
			return mpd.Report{Software: mpd.Software{Name: "Volumio"}}
		}
		c.Cards = func() (alsa.Snapshot, error) { return alsa.Snapshot{}, nil }
		c.Greet = func(context.Context, string) bool { return false }
		c.Getenv = func(string) string { return "" }
	})(context.Background())
	var musicLine doctor.Check
	for _, c := range ready {
		if c.Name == "music" {
			musicLine = c
		}
	}
	if musicLine.Status != doctor.OK || musicLine.Summary != "Ready: can play music from another bridge or NAS" || musicLine.Hint != "" {
		t.Fatalf("music line of a player with no library: %+v", musicLine)
	}
}
