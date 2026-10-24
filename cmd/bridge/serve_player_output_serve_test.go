package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/player/openhome"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// busyOnStart is the stand-in backend with a Start that, while busy is
// set, fails as the real backend does for a DAC another process holds:
// with engine.ErrDeviceBusy in its chain.
type busyOnStart struct {
	*playerStandIn
	busy atomic.Bool
}

func (b *busyOnStart) Start(ctx context.Context, req engine.StartRequest) error {
	if b.busy.Load() {
		return fmt.Errorf("the stand-in could not open its DAC: %w", errors.Join(errors.New("another process holds it"), engine.ErrDeviceBusy))
	}
	return b.playerStandIn.Start(ctx, req)
}

// mintBearer mints a device token in dataDir's token file, which serve
// loads when it starts.
func mintBearer(t *testing.T, dataDir string) string {
	t.Helper()
	tokens, err := auth.OpenStore(filepath.Join(dataDir, tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	bearer, _, err := tokens.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	return bearer
}

// serveJSON is GET path on serve's API, with bearer when not "", decoded
// into out; it returns the status.
func serveJSON(t *testing.T, addr, bearer, path string, out any) int {
	t.Helper()
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true}}
	req, err := http.NewRequest(http.MethodGet, "https://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
		}
	}
	return resp.StatusCode
}

// bridgeElementOf is the one element of kind bridge serve lists, failing the
// test when there is not exactly one.
func bridgeElementOf(t *testing.T, ps *playerServe, bearer string) api.OutputInfo {
	t.Helper()
	var body api.OutputsResponse
	if code := serveJSON(t, ps.addr, bearer, "/v1/outputs", &body); code != http.StatusOK {
		t.Fatalf("GET /v1/outputs: %d", code)
	}
	var out []api.OutputInfo
	for _, el := range body.Outputs {
		if el.Kind == "bridge" {
			out = append(out, el)
		}
	}
	if len(out) != 1 || out[0].Bridge == nil {
		t.Fatalf("serve listed %+v, want one element of kind bridge", body.Outputs)
	}
	return out[0]
}

// wantElement requires el's state, reason and lastError.
func wantElement(t *testing.T, what string, el api.OutputInfo, state, reason, lastError string) {
	t.Helper()
	b := el.Bridge
	gotReason, gotLast := "", ""
	if b.Reason != nil {
		gotReason = *b.Reason
	}
	if b.LastError != nil {
		gotLast = *b.LastError
	}
	if b.State != state || gotReason != reason || gotLast != lastError {
		t.Fatalf("%s: %s/%q, lastError %q; want %s/%q, lastError %q", what, b.State, gotReason, gotLast, state, reason, lastError)
	}
}

// TestServeListsItsPlayerOnAGateOfItsOwn boots serve on a box with the
// player and no MPD (output.mpd off) and walks the element through the
// real engine (B470 PR 5b): /v1/health advertises localOutputs and
// bridgeOutput and no outputReading; GET /v1/outputs lists the player alone,
// its identity the started player's, stopped, then a busy start's
// lastError, playing, paused, maintenance, and a live disable unavailable as
// disabled; the loopback track route never starts and the console shows no
// checklist, both on the MPD gate alone; once serve has stopped, closed.
func TestServeListsItsPlayerOnAGateOfItsOwn(t *testing.T) {
	needsDirectorySync(t)
	standIn := newPlayerStandIn()
	backend := &busyOnStart{playerStandIn: standIn}
	var bearer, adminAddr string
	loopback := make(chan string, 1)
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn,
		before: func(dataDir string) { bearer = mintBearer(t, dataDir) },
		with: func(o *serveOpts) {
			o.playerBackend = func(cfg playback.Config) (playBackend, error) {
				if _, err := standIn.build(cfg); err != nil {
					return nil, err
				}
				return backend, nil
			}
			o.outputLoopbackBoot = func(base string) { loopback <- base }
			adminAddr = o.adminListener.Addr().String()
		}})
	select {
	case base := <-loopback:
		if base != "" {
			t.Fatalf("the player alone started the loopback track route at %s", base)
		}
	case <-serveGiveUp(t):
		t.Fatal("serve never decided about its loopback route")
	}
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	uuid, err := config.ReadPlayerUUID(ps.dataDir)
	if err != nil {
		t.Fatal(err)
	}

	var health api.HealthResponse
	serveJSON(t, ps.addr, "", "/v1/health", &health)
	for key, want := range map[string]bool{"localOutputs": true, "bridgeOutput": true, "outputReading": false} {
		if slices.Contains(health.Features, key) != want {
			t.Errorf("%s advertised %v: %v", key, !want, health.Features)
		}
	}
	el := bridgeElementOf(t, ps, bearer)
	if el.ID != "bridge:"+uuid || el.Player.Software != "bridge" || el.Player.Version != version.ServerVersion || el.MPD != nil || el.DAC != nil || len(el.PathRules) != 0 {
		t.Fatalf("the element %+v", el)
	}
	if b := el.Bridge; b.OpenHome != nil || b.LibraryScope != "self" || !b.FixedVolume || b.DSDMode != "off" || b.AllowDoP ||
		!slices.Equal(b.Codecs, []string{"flac", "wav", "aiff", "alac"}) {
		t.Fatalf("the bridge object %+v", *b)
	}
	wantElement(t, "the output available", el, "stopped", "", "")

	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	backend.busy.Store(true)
	if err := eng.Play(context.Background()); !errors.Is(err, engine.ErrDeviceBusy) {
		t.Fatalf("a play on a busy DAC answered %v", err)
	}
	awaitEngine(t, eng, "the busy start stopped", func(s *engine.Snapshot) bool { return s.State() == engine.Stopped && s.LastAttempt().Busy })
	wantElement(t, "after a busy start", bridgeElementOf(t, ps, bearer), "stopped", "", "device_busy")
	backend.busy.Store(false)
	if err := eng.Play(context.Background()); err != nil {
		t.Fatalf("play: %v", err)
	}
	wantElement(t, "playing", bridgeElementOf(t, ps, bearer), "playing", "", "")
	if err := eng.Pause(context.Background()); err != nil {
		t.Fatalf("pause: %v", err)
	}
	wantElement(t, "paused", bridgeElementOf(t, ps, bearer), "paused", "", "")
	hold, err := eng.EnterMaintenance(context.Background(), eng.Snapshot().Generation(), "a test's")
	if err != nil {
		t.Fatal(err)
	}
	wantElement(t, "maintenance", bridgeElementOf(t, ps, bearer), "unavailable", "maintenance", "")
	if err := eng.ExitMaintenance(context.Background(), hold.Lease); err != nil {
		t.Fatal(err)
	}
	wantElement(t, "out of maintenance", bridgeElementOf(t, ps, bearer), "stopped", "", "")
	ps.livePlayer(t, func(p *config.PlayerConfig) { p.Enabled = false })
	awaitEngine(t, eng, "the output withheld", func(s *engine.Snapshot) bool { return !s.Output().Available && s.Output().Reason == "disabled" })
	wantElement(t, "the player turned off live", bridgeElementOf(t, ps, bearer), "unavailable", "disabled", "")
	serveJSON(t, ps.addr, "", "/v1/health", &health)
	if !slices.Contains(health.Features, "bridgeOutput") {
		t.Errorf("a live disable took bridgeOutput away, a boot decision: %v", health.Features)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get("http://" + adminAddr + "/api/output/checklist")
	if err != nil {
		t.Fatal(err)
	}
	var checklist struct {
		Active bool              `json:"active"`
		Lines  []json.RawMessage `json:"lines"`
	}
	err = json.NewDecoder(resp.Body).Decode(&checklist)
	resp.Body.Close()
	if err != nil || checklist.Active || len(checklist.Lines) != 0 {
		t.Fatalf("the console's checklist on a box with the player alone: %+v (%v), want none", checklist, err)
	}

	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
	f := newBridgeOutput(ps.player.plan, ps.player, &config.Config{DataDir: ps.dataDir}).Facts(context.Background())
	if !f.Listed || f.UUID != uuid || f.State != "unavailable" || f.Reason != "closed" {
		t.Fatalf("once serve stopped: %+v", f)
	}
}

// TestTheElementSaysAReleaseIsUnconfirmedUntilItsLateAcknowledgement: a
// release the DAC does not confirm by its deadline (the stand-in holds
// Release, as a kernel call that does not return) leaves the player
// unavailable as release_unconfirmed, through the real engine and serve's
// deadline, and its late acknowledgement makes it stopped again (B470 PR
// 5b).
func TestTheElementSaysAReleaseIsUnconfirmedUntilItsLateAcknowledgement(t *testing.T) {
	needsDirectorySync(t)
	standIn := newPlayerStandIn()
	standIn.holdRelease = make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(standIn.holdRelease) }) }
	var bearer string
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: standIn,
		before: func(dataDir string) { bearer = mintBearer(t, dataDir) }})
	t.Cleanup(letGo)
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	queue(t, eng, 0, someReference(t, "0123456789abcdef"))
	if err := eng.Play(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantElement(t, "playing", bridgeElementOf(t, ps, bearer), "playing", "", "")
	began := time.Now()
	_ = eng.Stop(context.Background())
	awaitEngine(t, eng, "the release unconfirmed", func(s *engine.Snapshot) bool { _, ok := s.ReleaseUnconfirmed(); return ok })
	if waited := time.Since(began); waited < engine.ReleaseTimeout {
		t.Fatalf("the release was unconfirmed after %v, before its deadline of %v", waited, engine.ReleaseTimeout)
	}
	wantElement(t, "past the release's deadline", bridgeElementOf(t, ps, bearer), "unavailable", "release_unconfirmed", "")
	letGo()
	awaitEngine(t, eng, "the late acknowledgement", func(s *engine.Snapshot) bool { _, ok := s.ReleaseUnconfirmed(); return !ok })
	wantElement(t, "after the late acknowledgement", bridgeElementOf(t, ps, bearer), "stopped", "", "")
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// TestServeNamesItsOpenHomeSourceWhileItListens: the element's openHome is
// the Source's UDN and the authority of each LOCATION it advertises while
// the boot snapshot's opt-in started the surface and it listens on a LAN
// address, and null otherwise (B470 PR 5b).
func TestServeNamesItsOpenHomeSourceWhileItListens(t *testing.T) {
	needsDirectorySync(t)
	boot := func(t *testing.T, optIn bool, box *openhomeBox) (*playerServe, string) {
		t.Helper()
		var bearer string
		block := goBarPlayer
		if optIn {
			block += "    openhome: true\n"
		}
		ps := bootPlayerServe(t, playerServeCase{player: block, standIn: newPlayerStandIn(),
			before: func(dataDir string) { bearer = mintBearer(t, dataDir) },
			with:   func(o *serveOpts) { o.playerOpenHome = box.wiring() }})
		ps.runningEngine(t)
		return ps, bearer
	}
	t.Run("listening", func(t *testing.T) {
		box := newOpenHomeBox(t)
		ps, bearer := boot(t, true, box)
		s, at := box.surface(t)
		uuid, err := config.ReadPlayerUUID(ps.dataDir)
		if err != nil {
			t.Fatal(err)
		}
		oh := bridgeElementOf(t, ps, bearer).Bridge.OpenHome
		if oh == nil || oh.UDN != "uuid:"+uuid || !slices.Equal(oh.Endpoints, []string{openhome.Authority(at)}) ||
			!slices.Equal(oh.Endpoints, s.Status().Endpoints()) {
			t.Fatalf("openHome %+v, want uuid:%s at %s", oh, uuid, at)
		}
	})
	t.Run("the opt-in off", func(t *testing.T) {
		box := newOpenHomeBox(t)
		ps, bearer := boot(t, false, box)
		if oh := bridgeElementOf(t, ps, bearer).Bridge.OpenHome; oh != nil {
			t.Fatalf("with the opt-in off, openHome %+v", oh)
		}
	})
	t.Run("no address to listen on", func(t *testing.T) {
		box := newOpenHomeBox(t)
		box.Set([]lan.Address{})
		ps, bearer := boot(t, true, box)
		var s *openhome.Surface
		select {
		case s = <-box.started:
		case <-serveGiveUp(t):
			t.Fatal("the player never started its surface")
		}
		for s.Status().Reconciles == 0 {
			select {
			case <-s.Changed():
			case <-serveGiveUp(t):
				t.Fatal("the surface never listed the box")
			}
		}
		if oh := bridgeElementOf(t, ps, bearer).Bridge.OpenHome; oh != nil {
			t.Fatalf("a surface listening on no address: openHome %+v", oh)
		}
	})
}

// manifestCodecWord is how a client matches a /v1/manifest track against
// the element's codecs (PROTOCOL.md, the bridge object): the track's codec,
// as the scanner writes it, to the container word, for the codecs the
// player plays. Any other codec (AAC, MP3, DSF, DFF, a compressed AIFF-C,
// none at all) names no container the player plays.
var manifestCodecWord = map[string]string{"FLAC": "flac", "WAV": "wav", "AIFF": "aiff", "ALAC": "alac"}

// TestTheCodecsAreTheManifestsCodecsThePlayerPlays pins the mapping a client
// matches tracks by (B470 PR 5b): every word the element lists comes from
// one manifest codec, and on real files the scanner's codec and the
// decoder's container agree: a mapped codec's file decodes to that
// container, which the player plays, and an unmapped one's (an AAC .m4a, a
// DSF, a DFF) is refused, or decodes to a container the player does not
// play; ALAC and AAC in .m4a are told apart by the codec. PROTOCOL.md's
// bridge section names each pair.
func TestTheCodecsAreTheManifestsCodecsThePlayerPlays(t *testing.T) {
	for _, w := range playedCodecs() {
		n := 0
		for _, word := range manifestCodecWord {
			if word == w {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d manifest codecs map to %s, want one", n, w)
		}
	}
	if len(manifestCodecWord) != len(playedCodecs()) {
		t.Errorf("the mapping names %d codecs for %d words", len(manifestCodecWord), len(playedCodecs()))
	}
	golden := filepath.Join("..", "..", "internal", "player", "decode", "testdata")
	for _, rel := range []string{
		filepath.Join("goldens", "decorrelated_16bit_44100.flac"),
		filepath.Join("goldens", "decorrelated_24bit_44100.wav"),
		filepath.Join("goldens", "decorrelated_16bit_44100.aiff"),
		filepath.Join("goldens", "decorrelated_16bit_44100_sowt.aifc"),
		filepath.Join("alac-goldens", "afconvert_16bit_44100.m4a"),
		filepath.Join("alac-goldens", "afconvert_aac_44100.m4a"),
		filepath.Join("dsd-goldens", "lsb_dsd64_whole_blocks.dsf"),
		filepath.Join("dsd-goldens", "dsd64.dff"),
	} {
		t.Run(filepath.Base(rel), func(t *testing.T) {
			path := filepath.Join(golden, rel)
			tr := manifest.Track{Path: filepath.Base(rel)}
			if err := manifest.Extract(path, &tr); err != nil {
				t.Fatalf("the scanner's extract: %v", err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var container format.Container
			opened := false
			if src, err := decode.Open(f); err == nil {
				opened = true
				switch d := src.(type) {
				case decode.Decoder:
					container = d.Format().Container
				case decode.DSDDecoder:
					container = d.Format().Container
				}
				_ = src.Close()
			}
			word, mapped := manifestCodecWord[tr.Codec]
			switch {
			case mapped && (!opened || string(container) != word || !playback.Plays(container)):
				t.Fatalf("codec %q maps to %s, but the file decodes to %q (opened %v), which the player plays: %v", tr.Codec, word, container, opened, playback.Plays(container))
			case !mapped && opened && playback.Plays(container):
				t.Fatalf("codec %q maps to no word, but the player plays its file's %s", tr.Codec, container)
			}
		})
	}
	section := bridgeObjectSection(t)
	for codec, word := range manifestCodecWord {
		found := false
		for _, line := range strings.Split(section, "\n") {
			if strings.Contains(line, "`"+codec+"`") && strings.Contains(line, "`"+word+"`") {
				found = true
			}
		}
		if !found {
			t.Errorf("PROTOCOL.md's bridge section has no line pairing codec `%s` with `%s`", codec, word)
		}
	}
}

// TestBridgeDoctorStaysMPDs: `bridge doctor`'s checklist is MPD's alone,
// on the MPD gate (B470 PR 5b): a box with the player enabled and
// output.mpd off prints no checklist and touches no player file, and with
// output.mpd on it prints MPD's four lines as before.
func TestBridgeDoctorStaysMPDs(t *testing.T) {
	for _, mode := range []string{"off", "on"} {
		t.Run("output.mpd "+mode, func(t *testing.T) {
			dir := t.TempDir()
			music, data := filepath.Join(dir, "Music"), filepath.Join(dir, "data")
			for _, d := range []string{music, data} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			pairPhone(t, data)
			path := filepath.Join(dir, "bridge.yaml")
			body := "libraryRoots:\n  - " + music + "\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\ndataDir: " + data +
				"\noutput:\n  mpd: " + mode + "\n  loopbackPort: 0\n  player:\n" + goBarPlayer
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			checks := doctorOutputChecklist(mustLoadConfig(t, path), linuxBoxSeam(music))(context.Background())
			if mode == "off" && len(checks) != 0 {
				t.Fatalf("the player alone printed %d checklist lines", len(checks))
			}
			if mode == "on" {
				var names []string
				for _, c := range checks {
					names = append(names, c.Name)
				}
				if !slices.Equal(names, []string{"player", "dac", "music", "phone"}) {
					t.Fatalf("the checklist's lines %q", names)
				}
			}
			if files := playerFiles(t, data); len(files) > 0 {
				t.Fatalf("the doctor made %v", files)
			}
		})
	}
}
