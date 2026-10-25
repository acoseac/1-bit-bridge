package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// thePlayerUUID is an identity a test seeds in a data directory.
const thePlayerUUID = "7d444840-9dc0-11d1-b245-5ffdce74fad2"

// playerOutputConfig is a config on which this host, standing in for
// Linux, enables the player: the GO bar bound by serial, changed by mut.
func playerOutputConfig(dataDir string, mut func(*config.PlayerConfig)) *config.Config {
	cfg := &config.Config{DataDir: dataDir}
	cfg.Output.Player = config.PlayerConfig{Enabled: true, Device: config.PlayerDevice{Transport: "usb", Binding: config.BindingSerial,
		VendorID: "20b1", ProductID: "3009", Serial: usbid.New("GOBAR-7Q2X-91KZ-4471")}}
	if mut != nil {
		mut(&cfg.Output.Player)
	}
	return cfg
}

// seedPlayerUUID writes id as dataDir's player identity, as bind or an
// earlier start made it.
func seedPlayerUUID(t *testing.T, dataDir, id string) {
	t.Helper()
	if err := config.WritePlayerUUIDFile(filepath.Join(dataDir, config.PlayerUUIDFile), id); err != nil {
		t.Fatal(err)
	}
}

// TestThePlanSaysWhetherGETOutputsListsThePlayer pins the player's gate for
// GET /v1/outputs and /v1/health's bridgeOutput (B470 PR 5b): the boot
// snapshot's player enabled (config.PlayerEnabled: enabled, a block that
// reads, Linux, not public, no OpenHome port clash) and serve's lock held,
// whether or not the plan starts the player; a plan that lists the player
// and does not start it says why in one word. Enabled is that, and reads
// nothing.
func TestThePlanSaysWhetherGETOutputsListsThePlayer(t *testing.T) {
	n := func(v int) *int { return &v }
	noDAC := func(p *config.PlayerConfig) { p.Device = config.PlayerDevice{} }
	for _, tc := range []struct {
		name         string
		goos         string
		lock, public bool
		mut          func(*config.PlayerConfig)
		lists, start bool
		refused      string
	}{
		{name: "enabled, bound, the lock held", goos: "linux", lock: true, lists: true, start: true},
		{name: "the lock not held", goos: "linux"},
		{name: "no DAC bound", goos: "linux", lock: true, mut: noDAC, lists: true, refused: "not_configured"},
		{name: "no DAC bound and no lock", goos: "linux", mut: noDAC},
		{name: "a pause release the engine does not take", goos: "linux", lock: true, mut: func(p *config.PlayerConfig) { p.PauseReleaseSeconds = n(601) },
			lists: true, refused: "config_invalid"},
		{name: "the player off", goos: "linux", lock: true, mut: func(p *config.PlayerConfig) { p.Enabled = false }},
		{name: "off Linux", goos: "darwin", lock: true},
		{name: "a public bridge", goos: "linux", lock: true, public: true},
		{name: "an OpenHome port serve binds", goos: "linux", lock: true, mut: func(p *config.PlayerConfig) { p.OpenHome, p.OpenHomePort = true, n(7788) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := playerOutputConfig(t.TempDir(), tc.mut)
			if tc.public {
				cfg.Deployment.Mode = "public"
			}
			plan := planServePlayer(cfg, tc.goos, tc.lock)
			if plan.lists != tc.lists || plan.start != tc.start || plan.refused != tc.refused {
				t.Fatalf("plan lists %v, starts %v, refused %q; want %v, %v, %q (off %q)", plan.lists, plan.start, plan.refused, tc.lists, tc.start, tc.refused, plan.off)
			}
			if got := newBridgeOutput(plan, nil, cfg).Enabled(); got != tc.lists {
				t.Fatalf("Enabled() = %v, want %v", got, tc.lists)
			}
		})
	}
}

// fakeSnap is an engine snapshot as the element reads one.
type fakeSnap struct {
	state       engine.State
	pending     engine.Intent
	out         engine.OutputState
	maint       bool
	unconfirmed bool
	attempt     engine.Attempt
}

func (s fakeSnap) State() engine.State        { return s.state }
func (s fakeSnap) Pending() engine.Intent     { return s.pending }
func (s fakeSnap) Output() engine.OutputState { return s.out }
func (s fakeSnap) Maintenance() (engine.MaintenanceInfo, bool) {
	return engine.MaintenanceInfo{Reason: "a test's"}, s.maint
}
func (s fakeSnap) ReleaseUnconfirmed() (engine.ReleaseInfo, bool) {
	return engine.ReleaseInfo{Session: 1}, s.unconfirmed
}
func (s fakeSnap) LastAttempt() engine.Attempt { return s.attempt }

// available is an output reported available.
var available = engine.OutputState{Known: true, Available: true, Generation: 1}

// TestThePlayersStateFromOneSnapshot is the element's state and reason
// table over an engine snapshot (B470 PR 5b), in its order: closed, the
// output not known or unavailable by its report's word, maintenance, a
// release not confirmed, then the transport, starting read as playing and a
// release by what follows it; and lastError device_busy only for a last
// attempt that failed busy.
func TestThePlayersStateFromOneSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name          string
		snap          fakeSnap
		closed        bool
		state, reason string
	}{
		{"closed", fakeSnap{out: available}, true, "unavailable", "closed"},
		{"closed outranks an output not known", fakeSnap{}, true, "unavailable", "closed"},
		{"the output not known", fakeSnap{state: engine.Playing}, false, "unavailable", "output_unknown"},
		{"the output missing", fakeSnap{out: engine.OutputState{Known: true, Reason: "device_missing"}}, false, "unavailable", "device_missing"},
		{"the output withheld for a changed binding", fakeSnap{out: engine.OutputState{Known: true, Reason: "binding_changed"}}, false, "unavailable", "binding_changed"},
		{"the output disabled live", fakeSnap{out: engine.OutputState{Known: true, Reason: "disabled"}}, false, "unavailable", "disabled"},
		{"a word no list names", fakeSnap{out: engine.OutputState{Known: true, Reason: "a_word_from_later"}}, false, "unavailable", "output_unavailable"},
		{"no word at all", fakeSnap{out: engine.OutputState{Known: true}}, false, "unavailable", "output_unavailable"},
		{"the output outranks maintenance", fakeSnap{out: engine.OutputState{Known: true, Reason: "device_missing"}, maint: true}, false, "unavailable", "device_missing"},
		{"maintenance", fakeSnap{out: available, maint: true}, false, "unavailable", "maintenance"},
		{"maintenance as a state", fakeSnap{out: available, state: engine.Maintenance}, false, "unavailable", "maintenance"},
		{"maintenance outranks an unconfirmed release", fakeSnap{out: available, maint: true, unconfirmed: true}, false, "unavailable", "maintenance"},
		{"a release not confirmed", fakeSnap{out: available, unconfirmed: true}, false, "unavailable", "release_unconfirmed"},
		{"stopped", fakeSnap{out: available, state: engine.Stopped}, false, "stopped", ""},
		{"playing", fakeSnap{out: available, state: engine.Playing}, false, "playing", ""},
		{"paused", fakeSnap{out: available, state: engine.Paused}, false, "paused", ""},
		{"releasing to stop", fakeSnap{out: available, state: engine.Releasing, pending: engine.IntentStop}, false, "stopped", ""},
		{"releasing to pause", fakeSnap{out: available, state: engine.Releasing, pending: engine.IntentPause}, false, "paused", ""},
		{"releasing to play", fakeSnap{out: available, state: engine.Releasing, pending: engine.IntentPlay}, false, "playing", ""},
		{"releasing with no intent", fakeSnap{out: available, state: engine.Releasing, pending: engine.Intent(9)}, false, "stopped", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, reason := playerState(tc.snap, tc.closed)
			if state != tc.state || reason != tc.reason {
				t.Fatalf("%s/%q, want %s/%q", state, reason, tc.state, tc.reason)
			}
		})
	}
	for _, tc := range []struct {
		attempt engine.Attempt
		want    string
	}{
		{engine.Attempt{}, ""},
		{engine.Attempt{Seq: 3}, ""},
		{engine.Attempt{Seq: 3, Failed: true}, ""},
		{engine.Attempt{Seq: 3, Failed: true, Busy: true}, "device_busy"},
	} {
		if got := playerLastError(fakeSnap{attempt: tc.attempt}); got != tc.want {
			t.Errorf("the last attempt %+v: lastError %q, want %q", tc.attempt, got, tc.want)
		}
	}
}

// TestEveryOutputReasonIsPassedOn enumerates every reason an output report
// can carry from the binding's own list (binding.States, never a retyped
// list) and serve's binding_changed: each reaches the element as its own
// word, none as the fallback, and each is one the element's list (and so
// PROTOCOL.md, TestEveryBridgeWordIsInTheProtocol) names. A state added to
// the binding cannot ship unmapped.
func TestEveryOutputReasonIsPassedOn(t *testing.T) {
	var want []string
	for _, s := range binding.States() {
		if s != binding.StateAvailable {
			want = append(want, string(s))
		}
	}
	want = append(want, reasonBindingChanged)
	if len(want) < 10 {
		t.Fatalf("%d output reasons: the binding's list is short", len(want))
	}
	for _, r := range want {
		state, reason := playerState(fakeSnap{out: engine.OutputState{Known: true, Reason: r}}, false)
		if state != "unavailable" || reason != r {
			t.Errorf("an output unavailable for %s: %s/%q, want unavailable/%s", r, state, reason, r)
		}
		if !slices.Contains(bridgeOutputReasons(), r) {
			t.Errorf("the element's reasons do not name %s", r)
		}
	}
	if slices.Contains(outputReasons(), string(binding.StateAvailable)) {
		t.Error("available is no reason an unavailable output gives")
	}
}

// bridgeObjectSection is PROTOCOL.md's section on the bridge object, from
// its heading to the next heading.
func bridgeObjectSection(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	spec := strings.ReplaceAll(string(raw), "\r\n", "\n")
	const heading = "#### The `bridge` object (kind `bridge`)"
	start := strings.Index(spec, heading)
	if start < 0 {
		t.Fatalf("PROTOCOL.md has no %q section", heading)
	}
	section := spec[start+len(heading):]
	if end := strings.Index(section, "\n#"); end >= 0 {
		section = section[:end]
	}
	return section
}

// TestEveryBridgeWordIsInTheProtocol: every state, reason and lastError
// word the element can carry, its libraryScope and its codecs, is named, in
// backticks, inside PROTOCOL.md's section on the bridge object, never just
// anywhere in the file (stopped, playing and closed appear elsewhere), so a
// client maps each to its own copy (B470 PR 5b).
func TestEveryBridgeWordIsInTheProtocol(t *testing.T) {
	section := bridgeObjectSection(t)
	words := append(append(bridgeOutputStates(), bridgeOutputReasons()...), bridgeOutputLastErrors()...)
	words = append(words, api.LibraryScopeSelf, api.OutputKindBridge)
	words = append(words, playedCodecs()...)
	for _, w := range words {
		if !strings.Contains(section, "`"+w+"`") {
			t.Errorf("PROTOCOL.md's bridge object section does not name `%s`", w)
		}
	}
}

// emptyLibrary is a library index with no tracks, and a resolver over no
// roots, for a player no test plays from.
type emptyLibrary struct{}

func (emptyLibrary) TrackPaths(context.Context) ([]string, error) { return nil, nil }
func (emptyLibrary) GetTrack(context.Context, string) (*manifest.Track, error) {
	return nil, nil
}
func (emptyLibrary) GetUPnPRouting(context.Context, string) (*manifest.UPnPRouting, error) {
	return nil, nil
}
func (emptyLibrary) Resolve(string) (string, error) { return "", fs.ErrNotExist }
func (emptyLibrary) Roots() []string                { return nil }

// startPlayerDirect starts serve's player as runServe does, on plan over a
// stand-in backend, with a hotplug feed that publishes nothing (the output
// stays unknown), and returns it; the test's cleanup stops it.
func startPlayerDirect(t *testing.T, plan servePlayerPlan, cfg *config.Config, seams *servePlayerSeams, boot func(*bridgePlayer)) *bridgePlayer {
	t.Helper()
	standIn := newPlayerStandIn()
	p := startServePlayer(plan, servePlayerWiring{Cfg: cfg, Hold: newDataDirHold(func() {}), Store: emptyLibrary{}, Paths: emptyLibrary{},
		Feed: hotplug.NewFeed(), Backend: standIn.build, Seams: seams, Boot: boot})
	t.Cleanup(p.stop)
	return p
}

// awaitReady waits for the player to run or fail.
func awaitReady(t *testing.T, p *bridgePlayer) {
	t.Helper()
	select {
	case <-p.ready:
	case <-serveGiveUp(t):
		t.Fatal("the player neither ran nor failed")
	}
}

// wantFacts requires facts to list the player with identity id in state
// and reason, or, with id "", to list nothing.
func wantFacts(t *testing.T, what string, f api.BridgeOutputFacts, id, state, reason string) {
	t.Helper()
	if id == "" {
		if f.Listed {
			t.Fatalf("%s: the player is listed (%+v), want no element", what, f)
		}
		return
	}
	if !f.Listed || f.UUID != id || f.State != state || f.Reason != reason {
		t.Fatalf("%s: listed %v as %q, %s/%q; want %q, %s/%q", what, f.Listed, f.UUID, f.State, f.Reason, id, state, reason)
	}
}

// TestThePlayersElementThroughItsPlan: a plan that lists the player and
// does not start it is unavailable with its word while the player has an
// identity (bind's), and lists nothing without one (B470 PR 5b); no engine
// is started, and the route's view makes no file.
func TestThePlayersElementThroughItsPlan(t *testing.T) {
	needsDirectorySync(t)
	for _, tc := range []struct {
		name, refused string
		mut           func(*config.PlayerConfig)
	}{
		{"no DAC bound", "not_configured", func(p *config.PlayerConfig) { p.Device = config.PlayerDevice{} }},
		{"a pause release the engine does not take", "config_invalid", func(p *config.PlayerConfig) { v := 601; p.PauseReleaseSeconds = &v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := playerOutputConfig(t.TempDir(), tc.mut)
			plan := planServePlayer(cfg, "linux", true)
			p := startPlayerDirect(t, plan, cfg, nil, nil)
			out := newBridgeOutput(plan, p, cfg)
			wantFacts(t, "with no identity", out.Facts(context.Background()), "", "", "")
			if files := playerFiles(t, cfg.DataDir); len(files) > 0 {
				t.Fatalf("the route's view made %v", files)
			}
			seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
			f := out.Facts(context.Background())
			wantFacts(t, "with bind's identity", f, thePlayerUUID, "unavailable", tc.refused)
			if f.LastError != "" || f.OpenHome != nil || !f.FixedVolume || f.DSDMode != "off" || f.AllowDoP || !slices.Equal(f.Codecs, []string{"flac", "wav", "aiff", "alac"}) {
				t.Fatalf("the facts %+v", f)
			}
			if p.eng != nil {
				t.Fatal("a plan that refuses started an engine")
			}
		})
	}
}

// TestThePlayersElementAcrossItsStart walks the element through each way a
// start ends (B470 PR 5b), the identity read afresh at every ask and never
// cached: missing before the start makes it; starting while the start runs;
// once the engine runs, the started player's identity with the output not
// known yet (no report reached it); a recorded failure, a panic the
// supervisor recovered, and serve's stop during the start each start_failed,
// never starting forever; a refused engine removes the identity it made,
// and the element with it; and once the player stops, closed.
func TestThePlayersElementAcrossItsStart(t *testing.T) {
	needsDirectorySync(t)
	ctx := context.Background()
	t.Run("missing, then made, then ready, then closed", func(t *testing.T) {
		cfg := playerOutputConfig(t.TempDir(), nil)
		plan := planServePlayer(cfg, "linux", true)
		reached, release := make(chan struct{}), make(chan struct{})
		var out *bridgeOutput
		p := startPlayerDirect(t, plan, cfg, &servePlayerSeams{engineConfig: func(*engine.Config) {
			close(reached)
			<-release
		}}, func(p *bridgePlayer) {
			out = newBridgeOutput(plan, p, cfg)
			wantFacts(t, "before the start", out.Facts(ctx), "", "", "")
		})
		<-reached
		made, err := config.ReadPlayerUUID(cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		wantFacts(t, "while the start runs", out.Facts(ctx), made, "unavailable", "starting")
		close(release)
		awaitReady(t, p)
		if p.eng == nil {
			t.Fatalf("the player did not start: %q", p.failed)
		}
		wantFacts(t, "once the engine runs", out.Facts(ctx), made, "unavailable", "output_unknown")
		if p.id != made {
			t.Fatalf("the started player's identity %q, the file's %q", p.id, made)
		}
		p.stop()
		wantFacts(t, "once the player stopped", out.Facts(ctx), made, "unavailable", "closed")
	})
	t.Run("made, then refused, then removed", func(t *testing.T) {
		cfg := playerOutputConfig(t.TempDir(), nil)
		plan := planServePlayer(cfg, "linux", true)
		if err := os.WriteFile(filepath.Join(cfg.DataDir, "not-a-directory"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		var out *bridgeOutput
		var during api.BridgeOutputFacts
		p := startPlayerDirect(t, plan, cfg, &servePlayerSeams{engineConfig: func(c *engine.Config) {
			during = out.Facts(ctx)
			c.ReservePath = filepath.Join(c.DataDir, "not-a-directory", playerReservationFile)
		}}, func(p *bridgePlayer) { out = newBridgeOutput(plan, p, cfg) })
		awaitReady(t, p)
		if !during.Listed || during.State != "unavailable" || during.Reason != "starting" {
			t.Fatalf("while the start ran with the identity it made: %+v", during)
		}
		if p.eng != nil || p.failed == "" {
			t.Fatalf("the engine took storage it refuses: %v %q", p.eng != nil, p.failed)
		}
		for i := 0; i < 2; i++ {
			wantFacts(t, "once the refused start removed its identity", out.Facts(ctx), "", "", "")
		}
	})
	t.Run("a recorded failure, the identity bind's", func(t *testing.T) {
		cfg := playerOutputConfig(t.TempDir(), nil)
		seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
		plan := planServePlayer(cfg, "linux", true)
		if err := os.WriteFile(filepath.Join(cfg.DataDir, "not-a-directory"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		p := startPlayerDirect(t, plan, cfg, &servePlayerSeams{engineConfig: func(c *engine.Config) {
			c.ReservePath = filepath.Join(c.DataDir, "not-a-directory", playerReservationFile)
		}}, nil)
		awaitReady(t, p)
		if p.failed == "" {
			t.Fatal("no failure recorded")
		}
		wantFacts(t, "a start that failed", newBridgeOutput(plan, p, cfg).Facts(ctx), thePlayerUUID, "unavailable", "start_failed")
	})
	t.Run("a panic the supervisor recovered", func(t *testing.T) {
		cfg := playerOutputConfig(t.TempDir(), nil)
		plan := planServePlayer(cfg, "linux", true)
		p := startPlayerDirect(t, plan, cfg, &servePlayerSeams{engineConfig: func(*engine.Config) { panic("in the start") }}, nil)
		awaitReady(t, p)
		<-p.done
		if p.failed != "" || p.eng != nil {
			t.Fatalf("after the panic: failed %q, engine %v", p.failed, p.eng != nil)
		}
		made, err := config.ReadPlayerUUID(cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		wantFacts(t, "after a panic in the start", newBridgeOutput(plan, p, cfg).Facts(ctx), made, "unavailable", "start_failed")
	})
	t.Run("serve's stop during the start", func(t *testing.T) {
		cfg := playerOutputConfig(t.TempDir(), nil)
		plan := planServePlayer(cfg, "linux", true)
		var started *bridgePlayer
		p := startPlayerDirect(t, plan, cfg, &servePlayerSeams{engineConfig: func(*engine.Config) { started.cancel() }},
			func(p *bridgePlayer) { started = p })
		awaitReady(t, p)
		<-p.done
		if p.eng != nil || p.failed != "" {
			t.Fatalf("a start serve stopped: engine %v, failed %q", p.eng != nil, p.failed)
		}
		made, err := config.ReadPlayerUUID(cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		wantFacts(t, "a start serve stopped", newBridgeOutput(plan, p, cfg).Facts(ctx), made, "unavailable", "start_failed")
	})
	t.Run("without serve's lock, nothing", func(t *testing.T) {
		cfg := playerOutputConfig(t.TempDir(), nil)
		seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
		plan := planServePlayer(cfg, "linux", false)
		p := startPlayerDirect(t, plan, cfg, nil, nil)
		wantFacts(t, "another serve's identity", newBridgeOutput(plan, p, cfg).Facts(ctx), "", "", "")
	})
}

// dirListing is every entry of dir with its size, mode and time, for a
// test that requires nothing to have changed there.
func dirListing(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(e.Name() + " " + info.Mode().String() + " " + info.ModTime().UTC().Format(time.RFC3339Nano) + " " + strconv.FormatInt(info.Size(), 10) + "\n")
	}
	return b.String()
}

// TestTheOutputsRouteMakesNoPlayerFile: GET /v1/outputs reads the player's
// identity and never makes or writes it (player.md: the identity is made
// only under every gate and serve's lock), so a data directory with none is
// left with none, and one with an identity is left as it was, entry by
// entry (B470 PR 5b).
func TestTheOutputsRouteMakesNoPlayerFile(t *testing.T) {
	needsDirectorySync(t)
	cfg := playerOutputConfig(t.TempDir(), func(p *config.PlayerConfig) { p.Device = config.PlayerDevice{} })
	plan := planServePlayer(cfg, "linux", true)
	p := startPlayerDirect(t, plan, cfg, nil, nil)
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	bearer, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(&config.Config{ListenAddress: ":7788", LibraryName: "T", LibraryRoots: []string{t.TempDir()}}, store, nil, "fp").
		WithLocalOutputs(api.LocalOutputsConfig{GOOS: "linux", Output: func() config.OutputConfig { return config.OutputConfig{MPD: "off"} }}).
		WithBridgeOutput(newBridgeOutput(plan, p, cfg))
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	get := func() []api.OutputInfo {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, hs.URL+"/v1/outputs", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := hs.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body api.OutputsResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/outputs: %d %v", resp.StatusCode, err)
		}
		return body.Outputs
	}
	before := dirListing(t, cfg.DataDir)
	for i := 0; i < 3; i++ {
		if got := get(); len(got) != 0 {
			t.Fatalf("a player with no identity is listed: %+v", got)
		}
	}
	if after := dirListing(t, cfg.DataDir); after != before {
		t.Fatalf("the route changed the data directory:\nbefore\n%safter\n%s", before, after)
	}
	if _, err := os.Lstat(filepath.Join(cfg.DataDir, config.PlayerUUIDFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the route made the identity: %v", err)
	}
	seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
	before = dirListing(t, cfg.DataDir)
	for i := 0; i < 3; i++ {
		if got := get(); len(got) != 1 || got[0].ID != "bridge:"+thePlayerUUID {
			t.Fatalf("the player with bind's identity: %+v", got)
		}
	}
	if after := dirListing(t, cfg.DataDir); after != before {
		t.Fatalf("the route changed the data directory:\nbefore\n%safter\n%s", before, after)
	}
}

// TestTheChecklistReadsNoBridgeElement: every consumer of the outputs body
// that is MPD's reads the kind mpd elements alone (B470 PR 5b): the
// checklist's input from a body that holds the bridge's own element after
// MPD's, or that element alone, is the MPD element's, and nothing of the
// bridge's.
func TestTheChecklistReadsNoBridgeElement(t *testing.T) {
	yes := true
	mpdEl := api.OutputInfo{ID: "a1", Kind: "mpd", Player: api.OutputPlayer{Software: "volumio", Version: "4.119"},
		MPD: &api.OutputMPD{Port: 6600, Addresses: []string{"192.168.1.42"}, Version: "0.24.0", State: "play", LoopbackBaseURL: "http://127.0.0.1:7791"},
		DAC: &api.OutputDAC{ID: "Audio", Name: "iFi", USBID: "20b1:3009", IsMPDsOutput: &yes}, PathRules: []api.OutputPathRule{{Root: "Music", State: "proven"}}}
	// A later change reports the player's DAC (dac, null today): even then
	// the checklist reads none of it.
	bridgeEl := api.OutputInfo{ID: "bridge:" + thePlayerUUID, Kind: "bridge", Player: api.OutputPlayer{Software: "bridge", Version: "1"},
		DAC: &api.OutputDAC{ID: "Bridge", Name: "the player's DAC", USBID: "20b1:3009"}, PathRules: []api.OutputPathRule{},
		Bridge: &api.OutputBridge{State: "stopped", LibraryScope: "self", Codecs: []string{"flac"}}}
	want := checklistInput(api.OutputsResponse{Outputs: []api.OutputInfo{mpdEl}})
	if got := checklistInput(api.OutputsResponse{Outputs: []api.OutputInfo{mpdEl, bridgeEl}}); !reflect.DeepEqual(got, want) {
		t.Fatalf("with the bridge's element: %+v\nwant MPD's alone: %+v", got, want)
	}
	if got, none := checklistInput(api.OutputsResponse{Outputs: []api.OutputInfo{bridgeEl}}), checklistInput(api.OutputsResponse{}); !reflect.DeepEqual(got, none) {
		t.Fatalf("the bridge's element alone gave the checklist %+v", got)
	}
}
