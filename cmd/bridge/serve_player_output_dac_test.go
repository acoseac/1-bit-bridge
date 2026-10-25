package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

const outputDACSerial = "B543-PRIVATE-SERIAL-918273"
const dacUSBRoot = "sys/devices/pci0000:00/0000:01:00.0/usb1"
const dacPort = "pci-0000:01:00.0/usb2-1.2:1.0"
const dacLongPort = "usb-0000:01:00.0-1.2"

// dacUnit is a synthetic USB device with two distinctly capable PCM devices.
func dacUnit(card int, id, model, serial, chain string) string {
	vid, pid, _ := strings.Cut(model, ":")
	dir := dacUSBRoot + "/1-1/1-" + chain
	return fmt.Sprintf("usbdev %s vendor=%s product=%s serial=%s devnum=10\nusbcard %d %s/1-%s:1.0\nproccard %d %s \"Synthetic DAC\" usb=%s pcm=0.0,1.0\n", dir, vid, pid, serial, card, dir, chain, card, id, model)
}

func outputDACTree(t *testing.T) (string, outputTrees) {
	t.Helper()
	root := sysfstest.BuildText(t, "pci sys/devices/pci0000:00/0000:01:00.0\nroothub "+dacUSBRoot+" speed=480\nusbdev "+dacUSBRoot+"/1-1 vendor=1234 product=0001 class=09\n"+dacUnit(5, "Audio", "20b1:3009", outputDACSerial, "1.2"))
	dacStreams(t, root, 5)
	// Put both sensitive strings in the longname as well as the identity/sysfs.
	cards := filepath.Join(root, "proc/asound/cards")
	raw, err := os.ReadFile(cards)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	lines[1] = "                      " + outputDACSerial + " at " + dacLongPort + ", high speed " + dacPort
	if err := os.WriteFile(cards, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	return root, outputTrees{proc: filepath.Join(root, "proc"), sysfs: filepath.Join(root, "sys")}
}

func dacStreams(t *testing.T, root string, card int) {
	t.Helper()
	for device, stream := range []string{
		"Playback:\n  Status: Stop\n  Interface 1\n    Altset 1\n    Format: S16_LE\n    Channels: 2\n    Rates: 44100, 48000\n",
		"Playback:\n  Status: Stop\n  Interface 1\n    Altset 1\n    Format: S24_3LE\n    Channels: 2\n    Rates: 96000, 192000\n  Interface 1\n    Altset 2\n    Format: DSD_U32_BE\n    Channels: 2\n    Rates: 176400, 352800\n",
	} {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("proc/asound/card%d/stream%d", card, device)), []byte(stream), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func dacBinding(kind string) config.PlayerDevice {
	d := config.PlayerDevice{Transport: "usb", Binding: kind, PCMDevice: 1}
	switch kind {
	case config.BindingSerial:
		d.VendorID, d.ProductID, d.Serial = "20b1", "3009", usbid.New(outputDACSerial)
	case config.BindingUniqueModel:
		d.VendorID, d.ProductID = "20b1", "3009"
	case config.BindingPort:
		d.Port = dacPort
	}
	return d
}

type dacEventSource struct {
	notes chan struct{}
	done  chan struct{}
	lost  atomic.Bool
}

func (*dacEventSource) Events() <-chan hotplug.Uevent { return nil }
func (s *dacEventSource) Notes() <-chan struct{}      { return s.notes }
func (s *dacEventSource) TakeLost() bool              { return s.lost.Swap(false) }
func (s *dacEventSource) Done() <-chan struct{}       { return s.done }
func (*dacEventSource) Status() hotplug.SourceStatus {
	return hotplug.SourceStatus{Mode: hotplug.ModePolling}
}

// dacFeed publishes a real watcher snapshot, then can mark it stale on demand.
// Its immutable inventory keeps filesystem changes deterministic between the
// snapshot and the request's fresh reading; no socket or host tree is used.
func dacFeed(t *testing.T, trees outputTrees, dev config.PlayerDevice) (*hotplug.Feed, func()) {
	t.Helper()
	inv, err := trees.observer().Observe()
	if err != nil {
		t.Fatal(err)
	}
	feed := hotplug.NewFeed()
	sub := feed.Subscribe()
	t.Cleanup(sub.Close)
	source := &dacEventSource{notes: make(chan struct{}, 1), done: make(chan struct{})}
	close(source.done)
	var fail atomic.Bool
	timing := hotplug.DefaultTiming()
	timing.Periodic = time.Hour
	svc := hotplug.NewService(hotplug.Config{Feed: feed, Settings: func() hotplug.Settings { return hotplug.Settings{Enabled: true, Device: dev} },
		Observe: func(context.Context) (binding.Inventory, error) {
			if fail.Load() {
				return binding.Inventory{}, errors.New("unreadable")
			}
			return inv, nil
		},
		OpenSource: func(context.Context) hotplug.Source { return source }, Timing: timing,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); svc.Run(ctx) }()
	drainLoopOnCleanup(t, cancel, done, "the DAC test's hotplug watcher")
	select {
	case <-sub.C():
	case <-serveGiveUp(t):
		t.Fatal("no first snapshot")
	}
	return feed, func() {
		fail.Store(true)
		source.lost.Store(true)
		source.notes <- struct{}{}
		for {
			select {
			case s := <-sub.C():
				if _, stale := s.Stale(); stale {
					return
				}
			case <-serveGiveUp(t):
				t.Fatal("no stale snapshot")
			}
		}
	}
}

// dacView starts the production player/view wiring with the real engine and a
// backend that opens no device. The watcher feed is already published.
func dacView(t *testing.T, trees outputTrees, dev config.PlayerDevice) (*bridgeOutput, func()) {
	t.Helper()
	needsDirectorySync(t)
	feed, stale := dacFeed(t, trees, dev)
	cfg := playerOutputConfig(t.TempDir(), func(p *config.PlayerConfig) { p.Device = dev })
	plan := planServePlayer(cfg, "linux", true)
	p := startServePlayer(plan, servePlayerWiring{Cfg: cfg, Hold: newDataDirHold(func() {}), Store: emptyLibrary{}, Paths: emptyLibrary{}, Feed: feed, Trees: trees, Backend: newPlayerStandIn().build})
	t.Cleanup(p.stop)
	awaitReady(t, p)
	if p.eng == nil {
		t.Fatalf("player failed: %s", p.failed)
	}
	availableOutput(t, p.eng)
	return newBridgeOutput(plan, p, cfg), stale
}

// dacBody drives the actual /v1/outputs handler without binding a socket.
func dacBody(t *testing.T, b *bridgeOutput) (api.OutputInfo, []byte) {
	t.Helper()
	store, err := auth.OpenStore(filepath.Join(t.TempDir(), tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(&config.Config{ListenAddress: ":7788", LibraryName: "T", LibraryRoots: []string{t.TempDir()}}, store, nil, "fp").
		WithLocalOutputs(api.LocalOutputsConfig{GOOS: "linux", Output: func() config.OutputConfig { return config.OutputConfig{MPD: "off"} }}).WithBridgeOutput(b)
	req := httptest.NewRequest("GET", "https://bridge/v1/outputs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	for _, private := range []string{outputDACSerial, dacPort, dacLongPort, `"serial"`, `"port"`} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("private identity reached JSON: %s", w.Body.Bytes())
		}
	}
	var body api.OutputsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Outputs) != 1 {
		t.Fatalf("outputs: %+v", body)
	}
	return body.Outputs[0], w.Body.Bytes()
}

func requireDAC(t *testing.T, d *api.OutputDAC, card int, id, usb string) {
	t.Helper()
	want := &api.OutputDAC{Card: card, ID: id, Name: "Synthetic DAC", USBID: usb, Capabilities: api.OutputCapabilities{MaxPCMRate: 192000, PCMBitDepths: []int{24}, NativeDSD: true, NativeDSDRates: []int{5644800, 11289600}, DSDFormats: []string{"DSD_U32_BE"}}}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("DAC = %+v; want %+v", d, want)
	}
}

func TestBridgeDACBindingsAndPrivacy(t *testing.T) {
	for _, kind := range []string{config.BindingSerial, config.BindingUniqueModel, config.BindingPort} {
		t.Run(kind, func(t *testing.T) {
			_, trees := outputDACTree(t)
			dev := dacBinding(kind)
			inv, err := trees.observer().Observe()
			if err != nil {
				t.Fatal(err)
			}
			res := binding.Resolve(dev, inv)
			if res.State != binding.StateAvailable || res.Identity.Port != dacPort || !res.Identity.Serial.Equal(usbid.New(outputDACSerial)) {
				t.Fatalf("fixture resolution: %+v", res)
			}
			b, _ := dacView(t, trees, dev)
			el, _ := dacBody(t, b)
			requireDAC(t, el.DAC, 5, "Audio", "20b1:3009")
		})
	}
}

func TestBridgeDACReplug(t *testing.T) {
	for _, tc := range []struct {
		name, kind, otherModel string
		swapIDs                bool
	}{
		{"index drift occupied by different card", config.BindingSerial, "1234:abcd", false},
		{"identical models swap ALSA ids", config.BindingSerial, "20b1:3009", true},
		{"different model at bound port", config.BindingPort, "1234:abcd", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, trees := outputDACTree(t)
			if tc.swapIDs {
				sysfstest.Apply(t, root, dacUnit(7, "Audio_1", tc.otherModel, "OTHER-UNIT", "1.3"))
				dacStreams(t, root, 7)
			}
			b, _ := dacView(t, trees, dacBinding(tc.kind))
			if tc.swapIDs {
				sysfstest.Apply(t, root, "unplug "+dacUSBRoot+"/1-1/1-1.3\n")
			}
			sysfstest.Apply(t, root, "unplug "+dacUSBRoot+"/1-1/1-1.2\n")
			model, id := "20b1:3009", "Moved"
			if tc.kind == config.BindingPort {
				model = "4321:dcba"
			}
			if tc.swapIDs {
				id = "Audio_1"
			}
			sysfstest.Apply(t, root, dacUnit(7, id, model, outputDACSerial, "1.2")+dacUnit(5, "Audio", tc.otherModel, "OTHER-UNIT", "1.3"))
			dacStreams(t, root, 5)
			dacStreams(t, root, 7)
			el, _ := dacBody(t, b)
			requireDAC(t, el.DAC, 7, id, model)
		})
	}
}

func TestBridgeDACReadFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, string, *bridgeOutput)
	}{
		{"absent", func(t *testing.T, r string, _ *bridgeOutput) {
			sysfstest.Apply(t, r, "unplug "+dacUSBRoot+"/1-1/1-1.2\n")
		}},
		{"unreadable proc", func(t *testing.T, r string, _ *bridgeOutput) {
			if err := os.RemoveAll(filepath.Join(r, "proc/asound")); err != nil {
				t.Fatal(err)
			}
		}},
		{"enumeration unreadable", func(t *testing.T, r string, _ *bridgeOutput) {
			sysfstest.Apply(t, r, "file "+dacUSBRoot+"/1-1/1-1.2/devnum invalid\n")
		}},
		{"stream missing", func(t *testing.T, r string, _ *bridgeOutput) {
			if err := os.Remove(filepath.Join(r, "proc/asound/card5/stream1")); err != nil {
				t.Fatal(err)
			}
		}},
		{"stream unreadable", func(t *testing.T, r string, _ *bridgeOutput) {
			sysfstest.Apply(t, r, "remove proc/asound/card5/stream1\ndir proc/asound/card5/stream1\n")
		}},
		{"no playback", func(t *testing.T, r string, _ *bridgeOutput) {
			sysfstest.Apply(t, r, "file proc/asound/card5/stream1 \"Capture:\\n  Status: Stop\\n\"\n")
		}},
		{"truncated stream", func(t *testing.T, r string, _ *bridgeOutput) {
			sysfstest.Apply(t, r, "file proc/asound/card5/stream1 \"Playback:\\n  Interface 1\\n    Altset 1\\n    Format: S24_3LE\\n\"\n")
		}},
		{"partial card", func(t *testing.T, r string, _ *bridgeOutput) {
			sysfstest.Apply(t, r, "link proc/asound/card5/pcm9p !/outside-b543\n")
		}},
		{"no first snapshot", func(_ *testing.T, _ string, b *bridgeOutput) { b.p.feed = hotplug.NewFeed() }},
		{"ambiguous unit", func(t *testing.T, r string, _ *bridgeOutput) {
			sysfstest.Apply(t, r, dacUnit(7, "Audio_1", "20b1:3009", outputDACSerial, "1.3"))
			dacStreams(t, r, 7)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, trees := outputDACTree(t)
			b, _ := dacView(t, trees, dacBinding(config.BindingSerial))
			tc.change(t, root, b)
			el, _ := dacBody(t, b)
			if el.DAC != nil || el.Bridge.State != "stopped" || el.Bridge.Reason != nil {
				t.Fatalf("failure changed state or gave DAC: %+v %+v", el.DAC, el.Bridge)
			}
		})
	}
}

func TestBridgeDACUnavailableAndStale(t *testing.T) {
	_, trees := outputDACTree(t)
	b, stale := dacView(t, trees, dacBinding(config.BindingSerial))
	// A valid positive control ensures each unavailable reason has something to withhold.
	requireDAC(t, b.Facts(context.Background()).DAC, 5, "Audio", "20b1:3009")
	for i, reason := range append(outputReasons(), reasonOutputUnavailable) {
		t.Run(reason, func(t *testing.T) {
			// Output reports exercise the real Facts/state path for every output reason.
			// The existing plan/start tests assert nil DAC through wantFacts.
			if err := b.p.eng.ReportOutput(context.Background(), engine.OutputReport{Generation: uint64(100 + i), Reason: reason}); err != nil {
				t.Fatal(err)
			}
			f := b.Facts(context.Background())
			if f.DAC != nil || f.State != "unavailable" || f.Reason != outputReason(reason) {
				t.Fatalf("facts: %+v", f)
			}
		})
	}
	if err := b.p.eng.ReportOutput(context.Background(), engine.OutputReport{Generation: 1000, Available: true}); err != nil {
		t.Fatal(err)
	}
	requireDAC(t, b.Facts(context.Background()).DAC, 5, "Audio", "20b1:3009")
	stale()
	f := b.Facts(context.Background())
	if f.DAC != nil || f.State != "stopped" || f.Reason != "" {
		t.Fatalf("stale changed state: %+v", f)
	}
	b.p.feed, _ = dacFeed(t, trees, b.p.device)
	b.p.stop()
	f = b.Facts(context.Background())
	if f.DAC != nil || f.Reason != "closed" {
		t.Fatalf("closed: %+v", f)
	}
}

func TestBridgeDACStalledReadIsBounded(t *testing.T) {
	_, trees := outputDACTree(t)
	b, _ := dacView(t, trees, dacBinding(config.BindingSerial))
	productionRead := b.dacRead
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var calls atomic.Int32
		b.dacRead = func(context.Context) *api.OutputDAC {
			calls.Add(1)
			<-release
			return &api.OutputDAC{Card: 99}
		}
		defer func() { close(release); synctest.Wait() }()
		// All callers exhaust their own bound while the same read stays held.
		for range 2 {
			results := make(chan api.BridgeOutputFacts, 25)
			for range 25 {
				go func() { results <- b.Facts(context.Background()) }()
			}
			synctest.Wait()
			time.Sleep(501 * time.Millisecond)
			synctest.Wait()
			for range 25 {
				select {
				case f := <-results:
					if f.DAC != nil {
						t.Fatal("stalled read gave a DAC")
					}
				default:
					t.Fatal("caller exceeded its 500 ms bound")
				}
			}
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("%d readers accumulated", n)
		}
		// Release before restoring the seam; Wait joins the reader's completion too.
		release <- struct{}{}
		synctest.Wait()
		b.dacRead = productionRead
		requireDAC(t, b.Facts(context.Background()).DAC, 5, "Audio", "20b1:3009")
	})
}

func TestBridgeDACReadSlotReleasedAfterPanic(t *testing.T) {
	_, trees := outputDACTree(t)
	b, _ := dacView(t, trees, dacBinding(config.BindingSerial))
	productionRead := b.dacRead
	b.dacRead = func(context.Context) *api.OutputDAC { panic("reader failed") }
	if f := b.Facts(context.Background()); f.DAC != nil {
		t.Fatal("panic gave a DAC")
	}
	b.dacRead = productionRead
	requireDAC(t, b.Facts(context.Background()).DAC, 5, "Audio", "20b1:3009")
}

func TestBridgeDACConcurrentCallersShareRead(t *testing.T) {
	_, trees := outputDACTree(t)
	b, _ := dacView(t, trees, dacBinding(config.BindingSerial))
	productionRead := b.dacRead
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer func() { close(release); synctest.Wait() }()
		var calls atomic.Int32
		b.dacRead = func(ctx context.Context) *api.OutputDAC { calls.Add(1); <-release; return productionRead(ctx) }
		results := make(chan api.BridgeOutputFacts, 2)
		go func() { results <- b.Facts(context.Background()) }()
		synctest.Wait()
		go func() { results <- b.Facts(context.Background()) }()
		synctest.Wait()
		select {
		case <-results:
			t.Fatal("caller did not wait for the in-flight read")
		default:
		}
		time.Sleep(100 * time.Millisecond)
		release <- struct{}{}
		synctest.Wait()
		for range 2 {
			requireDAC(t, (<-results).DAC, 5, "Audio", "20b1:3009")
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("%d concurrent readers", n)
		}
	})
}

func TestBridgeDACNeedsBootSnapshot(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint("foreign=", foreign), func(t *testing.T) {
			root, trees := outputDACTree(t)
			dev := dacBinding(config.BindingSerial)
			b, _ := dacView(t, trees, dev)
			if foreign {
				dev = dacBinding(config.BindingUniqueModel)
			} else {
				sysfstest.Apply(t, root, "unplug "+dacUSBRoot+"/1-1/1-1.2\n")
			}
			feed, _ := dacFeed(t, trees, dev)
			// The request reader would yield a valid DAC: only the snapshot gate withholds it.
			b.p.feed = feed
			b.dacRead = func(context.Context) *api.OutputDAC {
				t.Error("read despite foreign/unavailable snapshot")
				return &api.OutputDAC{Card: 99}
			}
			if f := b.Facts(context.Background()); f.DAC != nil || f.State != "stopped" || f.Reason != "" {
				t.Fatalf("snapshot changed state or supplied DAC: %+v", f)
			}
		})
	}
}

func TestBridgeDACModelAmbiguity(t *testing.T) {
	root, trees := outputDACTree(t)
	b, _ := dacView(t, trees, dacBinding(config.BindingUniqueModel))
	sysfstest.Apply(t, root, dacUnit(7, "OtherID", "20b1:3009", "OTHER-UNIT", "1.3"))
	dacStreams(t, root, 7)
	el, _ := dacBody(t, b)
	if el.DAC != nil {
		t.Fatalf("two units of bound model: %+v", el.DAC)
	}
}

// TestBridgeDACStartupReasons keeps an available snapshot behind each startup
// refusal, so the state's DAC gate, rather than an empty fixture, withholds it.
func TestBridgeDACStartupReasons(t *testing.T) {
	_, trees := outputDACTree(t)
	base, _ := dacView(t, trees, dacBinding(config.BindingSerial))
	for _, reason := range []string{refusedNotConfigured, refusedConfigInvalid, reasonStarting, reasonStartFailed} {
		t.Run(reason, func(t *testing.T) {
			ready := make(chan struct{})
			plan := base.plan
			if reason != reasonStarting {
				close(ready)
			}
			if reason == refusedNotConfigured || reason == refusedConfigInvalid {
				plan.start = false
				plan.refused = reason
			}
			p := &bridgePlayer{device: base.p.device, feed: base.p.feed, trees: trees, ready: ready}
			cfg := playerOutputConfig(base.dataDir, nil)
			b := newBridgeOutput(plan, p, cfg)
			f := b.Facts(context.Background())
			if !f.Listed || f.State != "unavailable" || f.Reason != reason || f.DAC != nil {
				t.Fatalf("%s: %+v", reason, f)
			}
		})
	}
	// Before the engine's first output report, a fresh hardware snapshot alone
	// must not publish a DAC. This player has its own empty report feed.
	cfg := playerOutputConfig(t.TempDir(), func(p *config.PlayerConfig) { p.Device = base.p.device })
	plan := planServePlayer(cfg, "linux", true)
	p := startPlayerDirect(t, plan, cfg, nil, nil)
	awaitReady(t, p)
	p.feed = base.p.feed
	p.trees = trees
	f := newBridgeOutput(plan, p, cfg).Facts(context.Background())
	if f.Reason != reasonOutputUnknown || f.DAC != nil {
		t.Fatalf("unknown output: %+v", f)
	}
}

// TestBridgeDACIdentityStableAcrossProc swaps only sysfs after proc has been
// captured. The old proc facts must never be assigned to the new occupant.
func TestBridgeDACIdentityStableAcrossProc(t *testing.T) {
	for _, change := range []string{"identical swap", "enumeration"} {
		t.Run(change, func(t *testing.T) {
			root, trees := outputDACTree(t)
			sysfstest.Apply(t, root, dacUnit(7, "Other", "20b1:3009", "OTHER-UNIT", "1.3"))
			dacStreams(t, root, 7)
			dev := dacBinding(config.BindingSerial)
			requireDAC(t, readPlayerDAC(context.Background(), trees, dev), 5, "Audio", "20b1:3009")
			observer := trees.observer()
			observer.AfterProc = func() {
				if change == "enumeration" {
					sysfstest.Apply(t, root, "file "+dacUSBRoot+"/1-1/1-1.2/devnum 42\n")
				} else {
					sysfstest.Apply(t, root, "usbcard 7 "+dacUSBRoot+"/1-1/1-1.2/1-1.2:1.0\nusbcard 5 "+dacUSBRoot+"/1-1/1-1.3/1-1.3:1.0\n")
				}
			}
			if d := readPlayerDACObserved(context.Background(), observer, dev); d != nil {
				t.Fatalf("changed identity carried stale proc facts: %+v", d)
			}
		})
	}
}

func TestBridgeDACRedactsIdentityInsideNameAndID(t *testing.T) {
	for _, field := range []string{"name", "id"} {
		for _, private := range []struct{ name, value string }{{"serial", outputDACSerial}, {"port", dacPort}} {
			t.Run(field+"/"+private.name, func(t *testing.T) {
				root, trees := outputDACTree(t)
				b, _ := dacView(t, trees, dacBinding(config.BindingSerial))
				value := "prefix-" + private.value + "-suffix"
				path := filepath.Join(root, "proc/asound/cards")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				old := "Synthetic DAC"
				if field == "id" {
					old = "Audio"
				}
				if err := os.WriteFile(path, []byte(strings.Replace(string(raw), old, value, 1)), 0600); err != nil {
					t.Fatal(err)
				}

				el, _ := dacBody(t, b)
				if el.DAC == nil {
					t.Fatal("redaction withheld DAC")
				}
				got := el.DAC.Name
				if field == "id" {
					got = el.DAC.ID
				}
				if got != "prefix-[redacted]-suffix" {
					t.Fatalf("redacted %s = %q", field, got)
				}
			})
		}
	}
}

func TestBridgeDACCapabilityListsAreArrays(t *testing.T) {
	for _, format := range []string{"S16_LE", "DSD_U32_BE"} {
		t.Run(format, func(t *testing.T) {
			root, trees := outputDACTree(t)
			path := filepath.Join(root, "proc/asound/card5/stream1")
			stream := "Playback:\n  Status: Stop\n  Interface 1\n    Altset 1\n    Format: " + format + "\n    Channels: 2\n    Rates: 48000\n"
			if err := os.WriteFile(path, []byte(stream), 0600); err != nil {
				t.Fatal(err)
			}
			b, _ := dacView(t, trees, dacBinding(config.BindingSerial))
			el, body := dacBody(t, b)
			if el.DAC == nil {
				t.Fatal("missing DAC")
			}
			empty := []string{"nativeDSDRates", "dsdFormats"}
			if format == "DSD_U32_BE" {
				empty = []string{"pcmBitDepths"}
			}
			for _, key := range empty {
				if !strings.Contains(string(body), `"`+key+`":[]`) {
					t.Fatalf("%s is not []: %s", key, body)
				}
			}
		})
	}
}

func TestBridgeDACCallerCancellationKeepsSharedRead(t *testing.T) {
	_, trees := outputDACTree(t)
	b, _ := dacView(t, trees, dacBinding(config.BindingSerial))
	productionRead := b.dacRead
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer func() { close(release); synctest.Wait() }()
		var calls atomic.Int32
		b.dacRead = func(ctx context.Context) *api.OutputDAC { calls.Add(1); <-release; return productionRead(ctx) }
		first, second := make(chan api.BridgeOutputFacts, 1), make(chan api.BridgeOutputFacts, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		go func() { first <- b.Facts(ctx) }()
		synctest.Wait()
		time.Sleep(50 * time.Millisecond)
		go func() { second <- b.Facts(context.Background()) }()
		synctest.Wait()
		time.Sleep(51 * time.Millisecond)
		synctest.Wait()
		if f := <-first; f.DAC != nil {
			t.Fatal("expired caller got DAC")
		}
		select {
		case <-second:
			t.Fatal("second caller lost its own wait budget")
		default:
		}
		release <- struct{}{}
		synctest.Wait()
		requireDAC(t, (<-second).DAC, 5, "Audio", "20b1:3009")
		if n := calls.Load(); n != 1 {
			t.Fatalf("%d readers", n)
		}
	})
}
