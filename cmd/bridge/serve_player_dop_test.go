package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// dsdFactsOf is the element's dsdPlayback for a ready player on b, as GET
// /v1/outputs gets it from serve's view.
func dsdFactsOf(t *testing.T, b *playback.Backend) api.BridgeOutputFacts {
	t.Helper()
	ready := make(chan struct{})
	close(ready)
	cfg := playerOutputConfig(t.TempDir(), nil)
	seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
	return newBridgeOutput(servePlayerPlan{lists: true}, &bridgePlayer{be: b, ready: ready}, cfg).Facts(context.Background())
}

// rawKeys is a JSON object's members.
func rawKeys(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return m
}

// TestBridgeDSDTupleTransport: on the wire, an approved DoP tuple the
// effective set publishes carries "transport": "dop", and a native one no
// transport member at all (absent is native), from the real reader's
// epoch through serve's facts.
func TestBridgeDSDTupleTransport(t *testing.T) {
	f := dsdFactsOf(t, playbacktest.ApprovedWith(t, "mixed", playbacktest.Options{Mode: "auto", AllowDoP: true}))
	if f.DSDPlayback == nil {
		t.Fatal("no epoch")
	}
	raw, err := json.Marshal(f.DSDPlayback)
	if err != nil {
		t.Fatal(err)
	}
	var tuples []json.RawMessage
	if err := json.Unmarshal(rawKeys(t, raw)["tuples"], &tuples); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tuple := range tuples {
		m := rawKeys(t, tuple)
		format := string(m["format"])
		transport, present := m["transport"]
		seen[format] = true
		switch format {
		case `"S32_LE"`:
			if string(transport) != `"dop"` {
				t.Errorf("the DoP tuple %s carries transport %s, want \"dop\"", tuple, transport)
			}
		case `"DSD_U32_BE"`:
			if present {
				t.Errorf("the native tuple %s carries a transport member: absent is native", tuple)
			}
		}
	}
	if !seen[`"S32_LE"`] || !seen[`"DSD_U32_BE"`] {
		t.Fatalf("published %s, want the DoP tuple at DSD64 and the native one at DSD128", raw)
	}
}

// TestPlaybackFactsTransport: on the wire, a playback observation names its
// transport (dop, pcm, or none for native DSD), the packer's version for
// DSD, when it was observed, and the approved record's revision for DSD.
func TestPlaybackFactsTransport(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		live playback.PlaybackFacts
		want map[string]string // member to raw value; "" is absent
	}{
		{"dop", playback.PlaybackFacts{BootID: "boot", PolicyGeneration: 3, Session: 1, Occurrence: 2, State: "open", Format: "S32_LE", RateHz: 176400, LogicalRate: 2822400, Channels: 2, ValidBits: 1, PolicyID: "dop64", Transport: "dop", PackingVersion: 1, ObservedAt: at, QualificationRevision: "r1"},
			map[string]string{"transport": `"dop"`, "packingVersion": "1", "observedAt": `"2026-10-28T12:00:00Z"`, "qualificationRevision": `"r1"`}},
		{"native", playback.PlaybackFacts{BootID: "boot", PolicyGeneration: 3, Session: 1, Occurrence: 2, State: "open", Format: "DSD_U32_BE", RateHz: 88200, LogicalRate: 2822400, Channels: 2, ValidBits: 1, PolicyID: "n64", PackingVersion: 1, ObservedAt: at, QualificationRevision: "r1"},
			map[string]string{"transport": "", "packingVersion": "1", "observedAt": `"2026-10-28T12:00:00Z"`, "qualificationRevision": `"r1"`}},
		{"pcm", playback.PlaybackFacts{BootID: "boot", PolicyGeneration: 3, Session: 1, Occurrence: 2, State: "open", Format: "S32_LE", RateHz: 48000, Channels: 2, ValidBits: 24, Transport: "pcm", ObservedAt: at},
			map[string]string{"transport": `"pcm"`, "packingVersion": "", "observedAt": `"2026-10-28T12:00:00Z"`, "qualificationRevision": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := tc.live
			_, out := bridgeDSDFacts(playback.PlayerPolicy{BootID: "boot", Generation: 3}, &live)
			raw, err := json.Marshal(out)
			if err != nil {
				t.Fatal(err)
			}
			m := rawKeys(t, raw)
			for key, want := range tc.want {
				got, present := m[key]
				if want == "" && present {
					t.Errorf("%s carries %s %s: want it absent", raw, key, got)
				}
				if want != "" && string(got) != want {
					t.Errorf("%s: %s is %s, want %s", raw, key, got, want)
				}
			}
		})
	}
}

// TestOutputsQualificationFacts: GET /v1/outputs states the owner
// qualification the epoch's record holds, the DAC's model, its descriptor
// firmware code and the qualification date, and never its USB port or its
// serial, from a player that holds its boot binding, the serial included,
// as serve's does.
func TestOutputsQualificationFacts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner approval uses Unix ownership; the player is Linux only")
	}
	b := playbacktest.ApprovedWith(t, "mixed", playbacktest.Options{Mode: "auto", AllowDoP: true, Schema: 2, QualifiedOn: "2026-10-28"})
	ready := make(chan struct{})
	close(ready)
	cfg := playerOutputConfig(t.TempDir(), nil)
	seedPlayerUUID(t, cfg.DataDir, thePlayerUUID)
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
		WithBridgeOutput(newBridgeOutput(servePlayerPlan{lists: true}, &bridgePlayer{be: b, ready: ready, device: cfg.Output.Player.Device}, cfg))
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
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
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/outputs: %d %v", resp.StatusCode, err)
	}
	var top struct {
		Outputs []map[string]json.RawMessage `json:"outputs"`
	}
	if err := json.Unmarshal(body, &top); err != nil || len(top.Outputs) != 1 {
		t.Fatalf("%s: %v", body, err)
	}
	dsd := rawKeys(t, rawKeys(t, top.Outputs[0]["bridge"])["dsdPlayback"])
	for key, want := range map[string]string{"model": `"iFi GO bar"`, "bcdDevice": `"0123"`, "qualifiedOn": `"2026-10-28"`} {
		if string(dsd[key]) != want {
			t.Errorf("dsdPlayback.%s is %s, want %s", key, dsd[key], want)
		}
	}
	trees := goBarTrees(t)
	goBarIdentityFiles(t, trees)
	q, err := (alsa.SysfsReader{Root: trees.sysfs}).Qualification(5)
	if err != nil || q.Port == "" {
		t.Fatalf("the GO bar's port: %q %v", q.Port, err)
	}
	for _, secret := range []string{q.Port, "GOBAR-7Q2X-91KZ-4471"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("GET /v1/outputs names the DAC's port or serial %q: %s", secret, body)
		}
	}
	for _, key := range []string{"port", "serial", "Port", "Serial"} {
		if _, present := dsd[key]; present {
			t.Errorf("dsdPlayback carries %s", key)
		}
	}
}

// dopOpens counts and refuses every open a backend asks, by kind.
type dopOpens struct {
	mu     sync.Mutex
	native []alsapcm.DSDConfig
	pcm    []alsapcm.SinkConfig
}

func (o *dopOpens) openNative(_ context.Context, c alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.native = append(o.native, c)
	return nil, errors.New("the test's DAC refuses every open")
}

func (o *dopOpens) openSink(_ context.Context, c alsapcm.SinkConfig) (*alsapcm.Sink, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pcm = append(o.pcm, c)
	return nil, errors.New("the test's DAC refuses every open")
}

func (o *dopOpens) counts() ([]alsapcm.DSDConfig, []alsapcm.SinkConfig) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]alsapcm.DSDConfig(nil), o.native...), append([]alsapcm.SinkConfig(nil), o.pcm...)
}

// dopContents is a resolver of named contents.
type dopContents map[string][]byte

func (c dopContents) Open(_ context.Context, ref engine.Reference) (playback.Content, error) {
	b, ok := c[ref.String()]
	if !ok {
		return playback.Content{}, errors.New("no such content")
	}
	return playback.Content{Data: bytes.NewReader(b), Size: int64(len(b))}, nil
}

// dopEvents takes a session's events and keeps none.
type dopEvents struct{}

func (dopEvents) Prepared(engine.SessionID, engine.OccurrenceID, engine.Join)      {}
func (dopEvents) Refused(engine.SessionID, engine.OccurrenceID, engine.Unplayable) {}
func (dopEvents) Submitted(engine.SessionID, engine.OccurrenceID)                  {}
func (dopEvents) Audible(engine.SessionID, engine.OccurrenceID)                    {}
func (dopEvents) Position(engine.SessionID, engine.Position)                       {}
func (dopEvents) Fault(engine.SessionID, engine.Fault)                             {}
func (dopEvents) Finished(engine.SessionID)                                        {}
func (dopEvents) DeviceReleased(engine.SessionID)                                  {}
func (dopEvents) Released(engine.SessionID)                                        {}

// startOn starts session s of b on the content ref names, and answers
// Start's error once the session is released and the backend joined.
func startOn(t *testing.T, b *playback.Backend, s engine.SessionID, ref string) error {
	t.Helper()
	r, _ := engine.NewReference(ref)
	err := b.Start(context.Background(), engine.StartRequest{Session: s, First: engine.Occurrence{ID: engine.OccurrenceID(s), Ref: r}, Events: dopEvents{}})
	if e := b.Release(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	if e := b.Wait(context.Background()); e != nil {
		t.Fatal(e)
	}
	return err
}

// goBarIdentityFiles lays out the GO bar's USB strings and descriptors in
// trees, which the qualification identity reads.
func goBarIdentityFiles(t *testing.T, trees outputTrees) {
	t.Helper()
	for k, v := range map[string][]byte{"manufacturer": []byte("iFi"), "product": []byte("GO bar"), "bcdDevice": []byte("0123"), "descriptors": {9, 4, 0, 0, 0, 1, 1, 0, 0}} {
		if e := os.WriteFile(filepath.Join(trees.sysfs, qualifyGoBar, k), v, 0600); e != nil {
			t.Fatal(e)
		}
	}
}

// dopRecordIn lays out the GO bar's USB strings and descriptors in trees
// and writes in dir an owner-approved record of one DoP policy at DSD64
// (dop64) for it, with its synthetic reports.
func dopRecordIn(t *testing.T, dir string, trees outputTrees) {
	t.Helper()
	goBarIdentityFiles(t, trees)
	qi, e := (alsa.SysfsReader{Root: trees.sysfs}).Qualification(5)
	if e != nil {
		t.Fatal(e)
	}
	q := &qualifyBox{t: t, dir: dir, manifestHash: qualification.Digest("fixtures"),
		identity: qualification.Identity{Vendor: "20b1", ProductID: "3009", Manufacturer: qi.Manufacturer, Product: qi.Product, BCDDevice: qi.BCDDevice, Port: qi.Port, Interface: qi.Interface}}
	q.reportsOn("fake-kernel")
	dop := q.policy("dop64", q.dopTuple(), nil)
	q.record("approved.json", qualification.ApprovalPhrase, 1, []qualification.ApprovedPolicy{dop}, nil, nil)
	if _, e := qualification.ReadApproval(dir, "approved.json", q.identity); e != nil {
		t.Fatal("the DoP record does not read:", e)
	}
}

// TestServeOutputPlayAllowDoP: serve's and output play's wiring pass dsdMode
// and allowDoP from the config into the authorization (B470 R10), and the
// real backend built on what they wired publishes and plays the approved
// DoP policy only with allowDoP: under dop or auto with allowDoP false the
// config loads, the epoch is PCM-only with its reason, DSD is refused as
// dop_not_allowed before the DAC is opened, and PCM still reaches the DAC.
func TestServeOutputPlayAllowDoP(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	dsd, err := os.ReadFile(filepath.Join("..", "..", "internal", "player", "decode", "testdata", "dsd-goldens", "dsd64.dff"))
	if err != nil {
		t.Fatal(err)
	}
	content := dopContents{"dsd": dsd, "pcm": qualifyWAV(44100)}
	for _, path := range []string{"serve", "output_play"} {
		for _, tc := range []struct {
			mode  string
			allow bool
		}{{"dop", false}, {"auto", false}, {"dop", true}, {"auto", true}} {
			t.Run(fmt.Sprintf("%s/%s_allowDoP_%t", path, tc.mode, tc.allow), func(t *testing.T) {
				var wired playback.Config
				switch path {
				case "serve":
					wired = serveWiredPlayback(t, tc.mode, tc.allow)
				default:
					var quit func()
					wired, quit = outputPlayWiredPlayback(t, tc.mode, tc.allow)
					defer quit()
				}
				a := wired.Authorization
				if a == nil || a.Mode != tc.mode || a.AllowDoP != tc.allow || a.Record != "approved.json" {
					t.Fatalf("the wiring's authorization %+v, want dsdMode %s and allowDoP %t from the config", a, tc.mode, tc.allow)
				}
				// The wired authorization as it is, but for its control reader,
				// which reads the host's control nodes: the box has none.
				auth := *a
				auth.Controls = alsactl.Reader{Open: func(int) (alsactl.Device, error) { return qualifyNoControls{}, nil }}
				opens := &dopOpens{}
				b, err := playback.New(playback.Config{Authorization: &auth, Locator: wired.Locator, Resolver: content, OpenSink: opens.openSink, OpenNative: opens.openNative})
				if err != nil {
					t.Fatal(err)
				}
				b.RefreshAuthorization(context.Background())
				pol := b.Policy()
				err = startOn(t, b, 1, "dsd")
				native, _ := opens.counts()
				if !tc.allow {
					if len(pol.Tuples) != 0 || pol.Reason != "dop_not_allowed" {
						t.Fatalf("allowDoP false published %+v (reason %q), want a PCM-only epoch for dop_not_allowed", pol.Tuples, pol.Reason)
					}
					var u *engine.Unplayable
					if !errors.As(err, &u) || u.Reason != "dop_not_allowed" || len(native) != 0 {
						t.Fatalf("DSD with allowDoP false: %v, %d native opens; want dop_not_allowed and none", err, len(native))
					}
					_ = startOn(t, b, 2, "pcm")
					if _, pcm := opens.counts(); len(pcm) != 1 || pcm[0].Transport.Kind != format.KindPCM {
						t.Fatalf("PCM under the PCM-only epoch asked %d PCM opens, want PCM to reach the DAC", len(pcm))
					}
					return
				}
				if len(pol.Tuples) != 1 || pol.Tuples[0].Tuple.Transport != "dop" || pol.Tuples[0].PolicyID != "dop64" {
					t.Fatalf("allowDoP true published %+v (reason %q), want the approved DoP policy", pol.Tuples, pol.Reason)
				}
				if len(native) != 1 || native[0].Kind != format.KindDoP || native[0].Format != format.S32LE {
					t.Fatalf("DSD with allowDoP true opened %+v (%v), want the approved DoP sink", native, err)
				}
			})
		}
	}
}

// serveWiredPlayback is the backend config serve's player wires on a config
// of mode and allowDoP whose approved record is a DoP policy's, its watcher
// fed the GO bar's box.
func serveWiredPlayback(t *testing.T, mode string, allow bool) playback.Config {
	t.Helper()
	trees := goBarTrees(t)
	dataDir := t.TempDir()
	dopRecordIn(t, dataDir, trees)
	cfg := playerOutputConfig(dataDir, func(p *config.PlayerConfig) {
		p.DSDMode, p.DSDQualification = mode, "approved.json"
		p.AllowDoP = &allow
	})
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	holdDataDirLock(t, dataDir)
	hold := newDataDirHold(func() {})
	t.Cleanup(hold.done)
	feed, _ := dacFeed(t, trees, cfg.Output.Player.Device)
	standIn := newPlayerStandIn()
	p := startServePlayer(planServePlayer(cfg, "linux", true), servePlayerWiring{Cfg: cfg, Hold: hold, Feed: feed, Store: store, Paths: bridgefs.New([]string{dataDir}), Trees: trees, Backend: standIn.build})
	t.Cleanup(p.stop)
	select {
	case <-p.ready:
	case <-serveGiveUp(t):
		t.Fatal("serve's player did not decide its start")
	}
	standIn.mu.Lock()
	defer standIn.mu.Unlock()
	if !standIn.built {
		t.Fatalf("serve's player built no backend: %s", p.failed)
	}
	return standIn.cfg
}

// outputPlayWiredPlayback is the backend config output play wires on a
// config of mode and allowDoP whose approved record is a DoP policy's,
// read while the run plays; quit ends the run.
func outputPlayWiredPlayback(t *testing.T, mode string, allow bool) (playback.Config, func()) {
	t.Helper()
	c := newPlayCase(t, goBarPlayer+fmt.Sprintf("    dsdMode: %s\n    allowDoP: %t\n    dsdQualification: approved.json\n", mode, allow))
	dopRecordIn(t, c.dataDir, c.trees)
	c.standIn.hold = true
	pr, pw := io.Pipe()
	c.stdin = pr
	env := c.env("linux")
	var mu sync.Mutex
	var wired playback.Config
	env.backend = func(cfg playback.Config) (playBackend, error) {
		mu.Lock()
		wired = cfg
		mu.Unlock()
		c.standIn.notify = cfg.Notify
		return c.standIn, nil
	}
	stdout, stderr := newPlayBuf(), newPlayBuf()
	done := make(chan int, 1)
	go func() {
		done <- runOutputPlay(context.Background(), append([]string{"--config", c.configPath, "--keys"}, c.files...), env, stdout, stderr)
	}()
	stdout.waitFor(t, "a.flac) is audible")
	mu.Lock()
	defer mu.Unlock()
	return wired, func() {
		_, _ = pw.Write([]byte("q"))
		select {
		case <-done:
		case <-time.After(playWatch):
			t.Errorf("output play did not end after q:\n%s\n%s", stdout.String(), stderr.String())
		}
		_ = pw.Close()
	}
}

// TestDoPCandidateRefusedByServeAndOutputPlay: a CLI candidate in the
// approved record's place is no permission to serve or to output play,
// whatever dsdMode and allowDoP say: DSD is refused as the approval being
// unavailable, before the DAC is opened, and nothing is published.
func TestDoPCandidateRefusedByServeAndOutputPlay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the engine keeps its IDs durable with a directory fsync, which Windows has not: the player runs on Linux")
	}
	persistentMedium(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "internal", "player", "decode", "testdata", "dsd-goldens", "dsd64.dff"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"serve", "output_play"} {
		t.Run(mode, func(t *testing.T) {
			build := newServePlayerBackend
			if mode == "output_play" {
				build = newOutputPlayBackend
			}
			opens := &dopOpens{}
			b := playbacktest.ApprovedWith(t, "candidate", playbacktest.Options{Mode: "dop", AllowDoP: true}, func(cfg playback.Config) (*playback.Backend, error) {
				cfg.Resolver = refusedNativeResolver(data)
				cfg.OpenSink, cfg.OpenNative = opens.openSink, opens.openNative
				x, e := build(cfg)
				if e != nil {
					return nil, e
				}
				return x.(*playback.Backend), nil
			})
			eng, err := playOneOnEngine(t, b, "dsd", "candidate refusal engine")
			if err == nil {
				t.Fatal("a candidate record played DSD")
			}
			for _, skip := range eng.Snapshot().Skips() {
				if skip.Reason != "dsd_approval_unavailable" {
					t.Fatalf("a candidate record refused DSD as %+v, want dsd_approval_unavailable", skip)
				}
			}
			native, pcm := opens.counts()
			if len(native) != 0 || len(pcm) != 0 || len(b.Policy().Tuples) != 0 || len(eng.Snapshot().Skips()) != 1 {
				t.Fatalf("a candidate record: %d native and %d PCM opens, %d tuples, skips %+v", len(native), len(pcm), len(b.Policy().Tuples), eng.Snapshot().Skips())
			}
		})
	}
}
