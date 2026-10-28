package playback

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// policySpec is one policy of an approved fixture record: its ID and tuple,
// the constraints it carries and the controls its reports were taken
// under.
type policySpec struct {
	id          string
	tuple       qualification.Tuple
	constraints []alsactl.Constraint
	controls    []alsactl.Control
}

// fixtureEnvironment is the environment every approved fixture's reports
// name: DoP evidence checks it against its own platform, Go and kernel.
var fixtureEnvironment = qualification.Digest([]string{"linux/arm64", "test-go", "test-kernel"})

// nativeSpec and dopSpec are approved fixture policies: the kernel's pause,
// a 1 ms tail, no prefill, no constraint.
func nativeSpec(id, f string, rate int) policySpec {
	return policySpec{id: id, tuple: qualification.Tuple{Format: f, BitRate: rate, Pause: "kernel", TailMS: 1}}
}

func dopSpec(id string, rate int) policySpec {
	return policySpec{id: id, tuple: qualification.Tuple{Transport: "dop", Format: "S32_LE", BitRate: rate, Pause: "kernel", TailMS: 1}}
}

// specKey is the key a spec's policy has in an approval's Edges.
func specKey(s policySpec) string {
	return qualification.NativeKey(qualification.ApprovedPolicy{ID: s.id, Tuple: s.tuple})
}

// approvedDSDFixture is approvedBackendFixture's box and owner-approved
// record, holding specs in their order with their matrix and
// backend-lifecycle reports (a DoP policy's with DoP's schema, geometry and
// observations), and edges, each executed in every lifecycle report, read
// under mode and allowDoP.
func approvedDSDFixture(t *testing.T, mode string, allowDoP bool, edges []qualification.Edge, specs ...policySpec) (Config, qualification.Approval) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("production DSD approval uses Unix ownership; player is Linux only")
	}
	loc, root := goBarLocator(t)
	for k, v := range map[string][]byte{"manufacturer": []byte("iFi"), "product": []byte("GO bar"), "bcdDevice": []byte("0123"), "descriptors": {9, 4, 0, 0, 0, 1, 1, 0, 0}} {
		if e := os.WriteFile(filepath.Join(root, goBar, k), v, 0600); e != nil {
			t.Fatal(e)
		}
	}
	a := &Authorization{Mode: mode, AllowDoP: allowDoP, StateDir: t.TempDir(), Record: "approved.json", Sysfs: loc.Observer.Sysfs, Controls: alsactl.Reader{Open: func(int) (alsactl.Device, error) { return emptyControls{}, nil }}}
	u, e := loc.Locate(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	id, e := a.identity(u)
	if e != nil {
		t.Fatal(e)
	}
	var policies []qualification.ApprovedPolicy
	for _, s := range specs {
		p := qualification.ApprovedPolicy{ID: s.id, Tuple: s.tuple, Channels: 2, Ordering: "msb", PauseReleaseSeconds: 30, Constraints: s.constraints}
		snap := alsactl.Snapshot{Controls: s.controls}
		for _, scenario := range []string{"matrix", "backend-lifecycle"} {
			r := map[string]any{"QualificationVersion": qualification.Version, "Tuple": p.Tuple, "NativeTuple": p.Tuple, "NativeCompleted": 1, "Identity": id, "ManifestHash": qualification.Digest("fixtures"), "Environment": fixtureEnvironment, "Scenario": scenario, "End": "complete", "Completed": []bool{true}, "PauseReleaseSeconds": 30, "PCMTimePolicy": [2]int{}, "BuildSHA": "fixture", "FeatureUnit": false, "Before": snap, "After": snap, "LifecycleActions": []string{"natural", "next", "previous", "pause", "resume", "released", "reopened", "stop"}}
			if scenario == "backend-lifecycle" {
				r["PassedEdges"] = edges
			}
			if p.Tuple.Transport == "dop" {
				yes := true
				carrier := p.Tuple.BitRate / 16
				r["SchemaVersion"] = 2
				r["PackingVersion"] = 1
				r["SourceRate"] = p.Tuple.BitRate
				r["CarrierRate"] = carrier
				r["Kernel"] = "test-kernel"
				r["Platform"] = "linux/arm64"
				r["GoVersion"] = "test-go"
				r["Opened"] = []map[string]any{{"Format": "S32_LE", "RateHz": carrier, "Channels": 2, "Msbits": 32, "PeriodFrames": carrier / 20, "BufferFrames": carrier / 5, "RateNum": carrier, "RateDen": 1}}
				r["DoP"] = &qualification.DoPObservations{Indicator: &yes, Channels: &yes, PitchDuration: &yes}
			}
			b, _ := json.Marshal(r)
			name := s.id + "-" + scenario + ".json"
			if e := os.WriteFile(filepath.Join(a.StateDir, name), b, 0600); e != nil {
				t.Fatal(e)
			}
			ev := qualification.Evidence{Path: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(b))}
			if scenario == "matrix" {
				p.Matrix = ev
			} else {
				p.Lifecycle = ev
			}
		}
		policies = append(policies, p)
	}
	record := qualification.Approval{Body: qualification.ApprovalBody{SchemaVersion: 1, QualificationVersion: qualification.Version, Confirmation: qualification.ApprovalPhrase, Revision: "r1", Identity: id, ManifestHash: qualification.Digest("fixtures"), Environment: fixtureEnvironment, Policies: policies, Edges: edges}}
	saveApprovedFixture(t, a, record)
	if _, e := qualification.ReadApproval(a.StateDir, a.Record, id); e != nil {
		t.Fatal("the approved fixture record does not read:", e)
	}
	return Config{Locator: loc, Authorization: a}, record
}

// gateControl is a mixer control named name at value v, and gateConstraint
// the constraint that holds it there.
func gateControl(name string, v byte) alsactl.Control {
	return alsactl.Control{Interface: 2, Name: name, Type: 2, Count: 1, Access: 1, Values: []byte{v, 0, 0, 0, 0, 0, 0, 0}}
}

func gateConstraint(name string, v byte) alsactl.Constraint {
	return alsactl.Constraint{Interface: 2, Name: name, Value: []byte{v, 0, 0, 0, 0, 0, 0, 0}}
}

// dsdContent is the DSD golden lsb_dsd64_partial_block.dsf with its sample
// rate set to rate, bits bits long when bits is above 0.
func dsdContent(t *testing.T, rate, bits int) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("..", "decode", "testdata", "dsd-goldens", "lsb_dsd64_partial_block.dsf"))
	if e != nil {
		t.Fatal(e)
	}
	binary.LittleEndian.PutUint32(b[56:60], uint32(rate))
	if bits > 0 {
		binary.LittleEndian.PutUint64(b[64:72], uint64(bits))
	}
	return b
}

// dsdHardware counts what a backend asks of the hardware: the binding's
// locates beside the authorization's reads of its health (one before each
// locate the authorization makes for the DAC's identity, so a locate past
// them is a locate to open), and every open, native or PCM, each refused.
type dsdHardware struct {
	locator         Locator
	locates, health atomic.Int32
	mu              sync.Mutex
	native          []alsapcm.DSDConfig
	pcm             []alsapcm.SinkConfig
}

func (h *dsdHardware) Locate(ctx context.Context) (Unit, error) {
	h.locates.Add(1)
	return h.locator.Locate(ctx)
}

// counted makes cfg's backend locate, open and read its health through h.
func (h *dsdHardware) counted(cfg *Config) {
	h.locator = cfg.Locator
	cfg.Locator = h
	cfg.Authorization.Healthy = func() bool { h.health.Add(1); return true }
	cfg.OpenSink = func(_ context.Context, c alsapcm.SinkConfig) (*alsapcm.Sink, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.pcm = append(h.pcm, c)
		return nil, errors.New("the test's DAC refuses every open")
	}
	cfg.OpenNative = func(_ context.Context, c alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.native = append(h.native, c)
		return nil, errors.New("the test's DAC refuses every open")
	}
}

// opens are the native and the PCM opens asked.
func (h *dsdHardware) opens() ([]alsapcm.DSDConfig, []alsapcm.SinkConfig) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]alsapcm.DSDConfig(nil), h.native...), append([]alsapcm.SinkConfig(nil), h.pcm...)
}

// untouched fails the test unless nothing was opened and every locate was
// the authorization's.
func (h *dsdHardware) untouched(t *testing.T) {
	t.Helper()
	native, pcm := h.opens()
	if len(native) != 0 || len(pcm) != 0 {
		t.Errorf("the refusal opened the DAC: %d native, %d PCM opens", len(native), len(pcm))
	}
	if l, r := h.locates.Load(), h.health.Load(); l != r {
		t.Errorf("the refusal located the DAC to open it: %d locates beside the authorization's %d reads", l, r)
	}
}

// startDSD starts session s on the reference ref, and answers Start's
// error, the session released and the backend's goroutines joined.
func startDSD(t *testing.T, b *Backend, s engine.SessionID, ref string) error {
	t.Helper()
	r, _ := engine.NewReference(ref)
	err := b.Start(context.Background(), engine.StartRequest{Session: s, First: engine.Occurrence{ID: engine.OccurrenceID(s), Ref: r}, Events: &dsdNoEvents{}})
	if e := b.Release(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	if e := b.Wait(context.Background()); e != nil {
		t.Fatal(e)
	}
	return err
}

// refusedAs fails the test unless err is an *engine.Unplayable with reason.
func refusedAs(t *testing.T, err error, reason engine.Reason) {
	t.Helper()
	var u *engine.Unplayable
	if !errors.As(err, &u) || u.Reason != reason {
		t.Fatalf("Start answered %v, want the refusal %s", err, reason)
	}
}

// dsdFiles is a resolver of named contents.
type dsdFiles map[string][]byte

func (f dsdFiles) Open(_ context.Context, r engine.Reference) (Content, error) {
	b, ok := f[r.String()]
	if !ok {
		return Content{}, errors.New("no such fixture content")
	}
	return edgeContent(b).Open(context.Background(), r)
}

// dsdNoEvents takes a session's events and keeps none.
type dsdNoEvents struct{}

func (dsdNoEvents) Prepared(engine.SessionID, engine.OccurrenceID, engine.Join)         {}
func (dsdNoEvents) Refused(engine.SessionID, engine.OccurrenceID, engine.Unplayable)    {}
func (dsdNoEvents) Submitted(engine.SessionID, engine.OccurrenceID)                     {}
func (dsdNoEvents) Audible(engine.SessionID, engine.OccurrenceID)                       {}
func (dsdNoEvents) Position(engine.SessionID, engine.Position)                          {}
func (dsdNoEvents) Fault(engine.SessionID, engine.Fault)                                {}
func (dsdNoEvents) Finished(engine.SessionID)                                           {}
func (dsdNoEvents) DeviceReleased(engine.SessionID)                                     {}
func (dsdNoEvents) Released(engine.SessionID)                                           {}
func (dsdNoEvents) Details(engine.SessionID, engine.OccurrenceID, engine.Details)       {}
func (dsdNoEvents) NativePlan(engine.SessionID, engine.OccurrenceID, engine.NativePlan) {}
