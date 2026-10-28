// Package playbacktest builds owner-approval fixtures over synthetic USB trees.
// It is imported only by tests and never opens a hardware node.
package playbacktest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

type noControls struct{}

// Controls returns the synthetic DAC's empty control set.
func (noControls) Controls() ([]alsactl.Control, error) { return nil, nil }

// Close releases no host resources.
func (noControls) Close() error { return nil }

type noContent struct{}

// Open refuses content; this fixture observes authorization only.
func (noContent) Open(context.Context, engine.Reference) (playback.Content, error) {
	return playback.Content{}, fmt.Errorf("fixture opens no content")
}

// Approved returns the real production backend after refreshing the named
// authorization state under dsdMode native. The valid state is the positive
// counterpart of every case.
func Approved(t *testing.T, state string, builders ...func(playback.Config) (*playback.Backend, error)) *playback.Backend {
	t.Helper()
	return ApprovedWith(t, state, Options{}, builders...)
}

// Options are what ApprovedWith builds the record and the authorization
// from, beside the state.
type Options struct {
	// Mode is the authorization's dsdMode: "" is native; the state off is
	// off whatever Mode says.
	Mode string
	// AllowDoP is the authorization's allowDoP.
	AllowDoP bool
	// Schema is the record's SchemaVersion, 0 for 1; QualifiedOn its
	// qualification date (schema 2's).
	Schema      int
	QualifiedOn string
}

// ApprovedWith is Approved under opts. Beside Approved's states, mixed is a
// record whose DoP policy at DSD64 is listed before a native DSD_U32_BE
// policy at DSD128, and candidate a CLI candidate in the approved record's
// place, which no production backend takes.
func ApprovedWith(t *testing.T, state string, opts Options, builders ...func(playback.Config) (*playback.Backend, error)) *playback.Backend {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix owner-approval fixture")
	}
	_, here, _, _ := runtime.Caller(0)
	root := sysfstest.Build(t, filepath.Join(filepath.Dir(here), "../../../alsa/testdata/sysfs/pi5-go-bar.txt"))
	const unit = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1/1-1"
	write := func(name string, b []byte) {
		t.Helper()
		if e := os.WriteFile(name, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	for k, v := range map[string][]byte{"manufacturer": []byte("iFi"), "product": []byte("GO bar"), "bcdDevice": []byte("0123"), "descriptors": {9, 4, 0, 0, 0, 1, 1, 0, 0}} {
		write(filepath.Join(root, unit, k), v)
	}
	sys := alsa.SysfsReader{Root: filepath.Join(root, "sys")}
	loc := playback.BindingLocator{Device: config.PlayerDevice{Transport: "usb", Binding: config.BindingSerial, VendorID: "20b1", ProductID: "3009", Serial: usbid.New("GOBAR-7Q2X-91KZ-4471")}, Observer: binding.Observer{Proc: alsa.Reader{ProcRoot: filepath.Join(root, "proc")}, Sysfs: sys}}
	q, e := sys.Qualification(5)
	if e != nil {
		t.Fatal(e)
	}
	id := qualification.Identity{Vendor: "20b1", ProductID: "3009", Manufacturer: q.Manufacturer, Product: q.Product, BCDDevice: q.BCDDevice, Port: q.Port, Interface: q.Interface}
	native := qualification.ApprovedPolicy{ID: "p64", Tuple: qualification.Tuple{Format: "DSD_U32_BE", BitRate: 2822400, Pause: "kernel"}, Channels: 2, Ordering: "msb", PauseReleaseSeconds: 30}
	dop := native
	dop.ID, dop.Tuple.Format, dop.Tuple.Transport = "dop64", "S32_LE", "dop"
	policies := []qualification.ApprovedPolicy{native}
	switch state {
	case "dop_only":
		dop.ID = "p64"
		policies = []qualification.ApprovedPolicy{dop}
	case "mixed":
		native.ID, native.Tuple.BitRate = "p128", 5644800
		policies = []qualification.ApprovedPolicy{dop, native}
	}
	withDoP := state == "dop_only" || state == "mixed"
	environment := "test"
	if withDoP {
		environment = qualification.Digest([]string{"linux/arm64", "test-go", "test-kernel"})
	}
	dir := t.TempDir()
	for i := range policies {
		p := &policies[i]
		for _, scenario := range []string{"matrix", "backend-lifecycle"} {
			r := map[string]any{"QualificationVersion": qualification.Version, "Tuple": p.Tuple, "NativeTuple": p.Tuple, "NativeCompleted": 1, "Identity": id, "ManifestHash": qualification.Digest("fixtures"), "Environment": environment, "Scenario": scenario, "End": "complete", "Completed": []bool{true}, "PauseReleaseSeconds": 30, "BuildSHA": "test", "FeatureUnit": false, "LifecycleActions": []string{"natural", "next", "previous", "pause", "resume", "released", "reopened", "stop"}}
			if p.Tuple.Transport == "dop" {
				yes := true
				r["SchemaVersion"] = 2
				r["PackingVersion"] = 1
				r["SourceRate"] = p.Tuple.BitRate
				r["CarrierRate"] = p.Tuple.BitRate / 16
				r["Kernel"] = "test-kernel"
				r["Platform"] = "linux/arm64"
				r["GoVersion"] = "test-go"
				r["Opened"] = []map[string]any{{"Format": "S32_LE", "RateHz": p.Tuple.BitRate / 16, "Channels": 2, "Msbits": 32, "PeriodFrames": 8820, "BufferFrames": 35280, "RateNum": p.Tuple.BitRate / 16, "RateDen": 1}}

				r["DoP"] = &qualification.DoPObservations{Indicator: &yes, Channels: &yes, PitchDuration: &yes}
			}
			data, _ := json.Marshal(r)
			name := p.ID + "-" + scenario + ".json"
			if len(policies) == 1 {
				name = scenario + ".json"
			}
			write(filepath.Join(dir, name), data)
			ev := qualification.Evidence{Path: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
			if scenario == "matrix" {
				p.Matrix = ev
			} else {
				p.Lifecycle = ev
			}
		}
	}
	schema := opts.Schema
	if schema == 0 {
		schema = 1
	}
	a := qualification.Approval{Body: qualification.ApprovalBody{SchemaVersion: schema, QualificationVersion: qualification.Version, Confirmation: qualification.ApprovalPhrase, Revision: "r1", Identity: id, ManifestHash: qualification.Digest("fixtures"), Environment: environment, Policies: policies, QualifiedOn: opts.QualifiedOn}}
	switch state {
	case "stale":
		a.Body.QualificationVersion--
	case "binding_mismatch":
		a.Body.Identity.BCDDevice = "ffff"
	case "empty":
		a.Body.Policies = nil
	case "candidate":
		a.Body.Confirmation = "CLI candidate; not approved"
	}
	a.SHA256 = qualification.Digest(a.Body)
	data, _ := json.Marshal(a)
	name := filepath.Join(dir, "approved.json")
	write(name, data)
	mode := opts.Mode
	if mode == "" {
		mode = "native"
	}
	if state == "off" {
		mode = "off"
	}
	if state == "missing" {
		if e = os.Remove(name); e != nil {
			t.Fatal(e)
		}
	}
	build := playback.New
	if len(builders) > 0 {
		build = builders[0]
	}
	if withDoP && schema == 1 {
		if _, e := qualification.ReadApproval(dir, "approved.json", id); e != nil {
			t.Fatal("fresh DoP approval refused", e)
		}
	}
	b, e := build(playback.Config{Resolver: noContent{}, Locator: loc, Authorization: &playback.Authorization{Mode: mode, AllowDoP: opts.AllowDoP, StateDir: dir, Record: "approved.json", Sysfs: sys, Controls: alsactl.Reader{Open: func(int) (alsactl.Device, error) { return noControls{}, nil }}}})
	if e != nil {
		t.Fatal(e)
	}
	b.RefreshAuthorization(context.Background())
	if state == "revoked" {
		if len(b.Policy().Tuples) != 1 {
			t.Fatal("revocation fixture was never authorized")
		}
		if e = os.Remove(name); e != nil {
			t.Fatal(e)
		}
		b.RefreshAuthorization(context.Background())
	}
	return b
}
