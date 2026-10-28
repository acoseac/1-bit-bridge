package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// qualifyGoBar is the GO bar's USB unit in the Pi 5 tree.
const qualifyGoBar = "devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1/1-1"

// qualifyBox is a box for the dsd-test scenarios that run the real engine
// and backend (backend-lifecycle and approved-native-dop-native): the Pi 5
// tree with the iFi GO bar at card 5, its USB strings and descriptors laid
// out for the authorization's identity, and no mixer control; a state
// directory holding two-second signed fixtures (PCM at DoP64's carrier and
// at 44.1 kHz, DSD64 in DSF), their manifest, the records and the reports;
// and the env the harness runs on there, which counts every open.
type qualifyBox struct {
	t            *testing.T
	dir          string
	env          outputDSDTestEnv
	sys          alsa.SysfsReader
	identity     qualification.Identity
	attachment   qualification.Attachment
	manifestHash string
	// kernel and environment are what the box's synthetic reports were
	// taken on: this host and the fake kernel, as the harness here reads them.
	kernel, environment string
	// opens counts the calls of the harness's own openers, which the
	// backend scenarios never make, and of the player's, which a test
	// replaces to open the fake kernel.
	harnessOpens, playerOpens atomic.Int32
}

// qualifyNoControls is a DAC with no mixer control.
type qualifyNoControls struct{}

func (qualifyNoControls) Controls() ([]alsactl.Control, error) { return nil, nil }
func (qualifyNoControls) Close() error                         { return nil }

// qualifyWAV is a two-second 32-bit stereo WAV at rate.
func qualifyWAV(rate int) []byte {
	frames := rate * 2
	b := make([]byte, 44+frames*8)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 2)
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*8))
	binary.LittleEndian.PutUint16(b[32:], 8)
	binary.LittleEndian.PutUint16(b[34:], 32)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(frames*8))
	return b
}

func newQualifyBox(t *testing.T) *qualifyBox {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("owner approval uses Unix ownership, and the GO bar's sysfs tree cannot be laid out on Windows; the player is Linux only")
	}
	persistentMedium(t)
	q := &qualifyBox{t: t, dir: t.TempDir()}
	tree := sysfstest.Build(t, filepath.Join("..", "..", "internal", "alsa", "testdata", "sysfs", "pi5-go-bar.txt"))
	for k, v := range map[string][]byte{"manufacturer": []byte("iFi"), "product": []byte("GO bar"), "bcdDevice": []byte("0123"), "descriptors": {9, 4, 0, 0, 0, 1, 1, 0, 0}} {
		if e := os.WriteFile(filepath.Join(tree, "sys", qualifyGoBar, k), v, 0600); e != nil {
			t.Fatal(e)
		}
	}
	q.sys = alsa.SysfsReader{Root: filepath.Join(tree, "sys")}
	qi, e := q.sys.Qualification(5)
	if e != nil {
		t.Fatal(e)
	}
	q.identity = qualification.Identity{Vendor: "20b1", ProductID: "3009", Manufacturer: qi.Manufacturer, Product: qi.Product, BCDDevice: qi.BCDDevice, Port: qi.Port, Interface: qi.Interface}
	q.attachment = qualification.Attachment{BootID: "boot", Port: qi.Port, Manufacturer: qi.Manufacturer, Product: qi.Product, BCDDevice: qi.BCDDevice, Card: 5, DevNum: max(qi.DevNum, 1), Interface: qi.Interface}
	q.reportsOn("fake-kernel")
	var dsd bytes.Buffer
	if e := dsdtone.WriteDSF(&dsd, 2822400, 2, bytes.Repeat([]byte{0x69}, 2822400/8*2), 2822400*2); e != nil {
		t.Fatal(e)
	}
	files := map[string][]byte{"pcm176.wav": qualifyWAV(176400), "pcm44.wav": qualifyWAV(44100), "dsd64.dsf": dsd.Bytes()}
	for name, b := range files {
		if e := os.WriteFile(filepath.Join(q.dir, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	hash := func(name string) string { return fmt.Sprintf("%x", sha256.Sum256(files[name])) }
	m := qualification.Manifest{SchemaVersion: 1, Calibration: qualification.Calibration{Reference: "test-only; never live audio", PeakDBFS: -30, LeadingSilenceMS: 1000, TrailingSilenceMS: 1000, Attestation: qualification.CalibrationPhrase}, Fixtures: []qualification.Fixture{
		{ID: "pcm176", Path: "pcm176.wav", SHA256: hash("pcm176.wav"), Container: "WAV", Order: "LR", Rate: 176400, Channels: 2, Frames: 176400 * 2},
		{ID: "pcm44", Path: "pcm44.wav", SHA256: hash("pcm44.wav"), Container: "WAV", Order: "LR", Rate: 44100, Channels: 2, Frames: 44100 * 2},
		{ID: "dsda", Path: "dsd64.dsf", SHA256: hash("dsd64.dsf"), Container: "DSF", Order: "LR", PCMReference: "pcm176", Rate: 2822400, Channels: 2, Frames: 2822400 * 2},
		{ID: "dsdb", Path: "dsd64.dsf", SHA256: hash("dsd64.dsf"), Container: "DSF", Order: "LR", PCMReference: "pcm176", Rate: 2822400, Channels: 2, Frames: 2822400 * 2},
	}}
	b, _ := json.Marshal(m)
	q.manifestHash = fmt.Sprintf("%x", sha256.Sum256(b))
	if e := os.WriteFile(filepath.Join(q.dir, "manifest.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(q.dir, "mixer.txt"), []byte("# card=5\nno controls\n"), 0600); e != nil {
		t.Fatal(e)
	}
	refuse := errors.New("the box opens nothing the test did not give it")
	q.env = outputDSDTestEnv{goos: "linux", now: time.Now, at: func(at time.Time) (<-chan time.Time, func()) {
		timer := time.NewTimer(time.Until(at))
		return timer.C, func() { timer.Stop() }
	}, stdin: strings.NewReader(""),
		controls:   func(int) (alsactl.Snapshot, error) { return alsactl.Snapshot{}, nil },
		attachment: func(int) (qualification.Attachment, error) { return q.attachment, nil },
		resolve: func(context.Context, string) (nativeEndpoint, func(alsapcm.NodeInfo) error, error) {
			return nativeEndpoint{Endpoint: alsapcm.Endpoint{Card: 5, Device: 0}}, nil, nil
		},
		identity: func(card, device, sub int) (qualification.Identity, error) {
			id := q.identity
			id.Device, id.Subdevice = device, sub
			return id, nil
		},
		gain:   q.sys.QualificationGain,
		kernel: func() (string, error) { return "fake-kernel", nil },
		build:  func() (string, bool) { return "test-build", false },
		open: func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error) {
			q.harnessOpens.Add(1)
			return nil, refuse
		},
		openPCM: func(context.Context, alsapcm.SinkConfig) (nativePCMSink, error) {
			q.harnessOpens.Add(1)
			return nil, refuse
		},
		player: nativePlayerSeams{sysfs: q.sys, controls: alsactl.Reader{Open: func(int) (alsactl.Device, error) { return qualifyNoControls{}, nil }},
			openSink: func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error) {
				q.playerOpens.Add(1)
				return nil, refuse
			},
			openNative: func(context.Context, alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
				q.playerOpens.Add(1)
				return nil, refuse
			}},
	}
	return q
}

// reportsOn makes the box's synthetic reports from now on a host's of this
// platform and Go running kernel.
func (q *qualifyBox) reportsOn(kernel string) {
	q.kernel = kernel
	q.environment = qualification.Digest([]string{runtime.GOOS + "/" + runtime.GOARCH, runtime.Version(), kernel})
}

// dopTuple and nativeTuple are the box's DSD64 tuples: the kernel's pause,
// a 1 ms tail, no prefill.
func (q *qualifyBox) dopTuple() qualification.Tuple {
	return qualification.Tuple{Transport: "dop", Format: "S32_LE", BitRate: 2822400, Pause: "kernel", TailMS: 1}
}

func (q *qualifyBox) nativeTuple() qualification.Tuple {
	return qualification.Tuple{Format: "DSD_U32_BE", BitRate: 2822400, Pause: "kernel", TailMS: 1}
}

// contact is a fresh, complete contact receipt of tuple on the box.
func (q *qualifyBox) contact(tuple qualification.Tuple) qualification.Contact {
	c := qualification.Contact{Device: new(int), Subdevice: new(int), SchemaVersion: 2, QualificationVersion: qualification.Version, Tuple: tuple, Attachment: q.attachment, MixerHash: alsactl.Snapshot{}.Hash(), ManifestHash: q.manifestHash, KnobPosition: "1", PCMAudible: qualification.PCMConfirmation, DSDMatched: qualification.DSDConfirmation}
	if tuple.Transport == "dop" {
		yes := true
		c.SchemaVersion = 3
		c.DoP = &qualification.DoPObservations{Indicator: &yes, Channels: &yes, PitchDuration: &yes}
	}
	return c
}

// write writes v as JSON to name in the state directory, and answers its
// path and its bytes' SHA-256.
func (q *qualifyBox) write(name string, v any) (string, string) {
	q.t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		q.t.Fatal(e)
	}
	path := filepath.Join(q.dir, name)
	if e = os.WriteFile(path, b, 0600); e != nil {
		q.t.Fatal(e)
	}
	return path, fmt.Sprintf("%x", sha256.Sum256(b))
}

// bundle writes a contact bundle of receipts for tuples to name.
func (q *qualifyBox) bundle(name string, tuples ...qualification.Tuple) string {
	q.t.Helper()
	var b contactBundle
	for _, tuple := range tuples {
		c := q.contact(tuple)
		b.Receipts = append(b.Receipts, contactReceipt{Contact: c, SHA256: qualification.Digest(c)})
	}
	path, _ := q.write(name, b)
	return path
}

// report is a complete synthetic report of scenario for tuple, as the
// harness writes one on the box, written to name: a DoP tuple's with DoP's
// schema, geometry and observations.
func (q *qualifyBox) report(name, scenario string, tuple qualification.Tuple, edges []qualification.Edge) qualification.Evidence {
	q.t.Helper()
	unit := false
	r := nativeReport{SchemaVersion: 1, QualificationVersion: qualification.Version, Tuple: tuple, NativeTuple: tuple, NativeCompleted: 1, Identity: q.identity, ManifestHash: q.manifestHash, Environment: q.environment,
		Scenario: scenario, End: "complete", Completed: []bool{true}, PauseReleaseSeconds: 30, BuildSHA: "test-build", FeatureUnit: &unit, Kernel: q.kernel, Platform: runtime.GOOS + "/" + runtime.GOARCH, GoVersion: runtime.Version(),
		LifecycleActions: []string{"natural", "next", "previous", "pause", "resume", "released", "reopened", "stop"}, PassedEdges: edges, Attachment: q.attachment}
	if tuple.Transport == "dop" {
		yes := true
		r.SchemaVersion, r.PackingVersion, r.SourceRate, r.CarrierRate = 2, 1, tuple.BitRate, tuple.BitRate/16
		r.Opened = []alsapcm.Params{{Format: "S32_LE", Channels: 2, RateHz: tuple.BitRate / 16, RateNum: uint32(tuple.BitRate / 16), RateDen: 1, Msbits: 32, PeriodFrames: tuple.BitRate / 320, BufferFrames: tuple.BitRate / 80}}
		r.DoP = &qualification.DoPObservations{Indicator: &yes, Channels: &yes, PitchDuration: &yes}
	}
	_, sum := q.write(name, r)
	return qualification.Evidence{Path: name, SHA256: sum}
}

// policy is an approved policy of tuple with its synthetic matrix and
// backend-lifecycle reports, the lifecycle having passed edges.
func (q *qualifyBox) policy(id string, tuple qualification.Tuple, edges []qualification.Edge) qualification.ApprovedPolicy {
	return qualification.ApprovedPolicy{ID: id, Tuple: tuple, Channels: 2, Ordering: "msb", PauseReleaseSeconds: 30,
		Matrix: q.report(id+"-matrix.json", "matrix", tuple, nil), Lifecycle: q.report(id+"-lifecycle.json", "backend-lifecycle", tuple, edges)}
}

// record writes an approval of policies and edges to name, confirmed with
// confirmation; schema 2 carries the qualification date.
func (q *qualifyBox) record(name, confirmation string, schema int, policies []qualification.ApprovedPolicy, edges []qualification.Edge, transitions []qualification.Evidence) string {
	q.t.Helper()
	a := qualification.Approval{Body: qualification.ApprovalBody{SchemaVersion: schema, QualificationVersion: qualification.Version, Confirmation: confirmation, Revision: "r1", ManifestHash: q.manifestHash, Environment: q.environment, Identity: q.identity, Policies: policies, Edges: edges, Transitions: transitions}}
	if schema == 2 {
		a.Body.QualifiedOn = "2026-10-28"
	}
	a.SHA256 = qualification.Digest(a.Body)
	path, _ := q.write(name, a)
	return path
}

// approvedEndpoints writes the owner-approved record the transition scenario
// reads, approved.json, a schema-1 record (promotion r1 may write either
// schema): an approved native policy n64 and an approved DoP policy dop64
// at DSD64, each lifecycle having executed its own self edge.
func (q *qualifyBox) approvedEndpoints() (qualification.ApprovedPolicy, qualification.ApprovedPolicy) {
	q.t.Helper()
	nativeKey := qualification.NativeKey(qualification.ApprovedPolicy{ID: "n64", Tuple: q.nativeTuple()})
	dopKey := qualification.NativeKey(qualification.ApprovedPolicy{ID: "dop64", Tuple: q.dopTuple()})
	self := []qualification.Edge{{From: nativeKey, To: nativeKey}, {From: dopKey, To: dopKey}}
	native := q.policy("n64", q.nativeTuple(), self[:1])
	dop := q.policy("dop64", q.dopTuple(), self[1:])
	q.record("approved.json", qualification.ApprovalPhrase, 1, []qualification.ApprovedPolicy{native, dop}, self, nil)
	if _, e := qualification.ReadApproval(q.dir, "approved.json", q.identity); e != nil {
		q.t.Fatal("the box's approved record does not read:", e)
	}
	return native, dop
}

// transitionArgs are an approved-native-dop-native invocation on the box:
// the DoP endpoint's flags, the native endpoint by its ID, the bundle and
// the fixtures native, DoP, native.
func (q *qualifyBox) transitionArgs(fixtures ...string) []string {
	if len(fixtures) == 0 {
		fixtures = []string{"dsda", "dsdb", "dsda"}
	}
	return append([]string{"--qualification=" + filepath.Join(q.dir, "approved.json"), "--native-tuple-id=n64", "--transport=dop", "--format=S32_LE", "--bit-rate=2822400",
		"--pause-method=kernel", "--tail-ms=1", "--prefill-ms=0", "--seconds=600", "--ack-level=contact-position", "--ack-dop-noise",
		"--manifest=" + filepath.Join(q.dir, "manifest.json"), "--fixtures=" + q.dir, "--contact-report=" + filepath.Join(q.dir, "contacts.json"),
		"--mixer-snapshot=" + filepath.Join(q.dir, "mixer.txt"), "--report=" + filepath.Join(q.dir, "report.json"), "--scenario=approved-native-dop-native", "--config=fake"}, fixtures...)
}

// TestApprovedTransitionScenarioNeverProbes: approved-native-dop-native reads
// both endpoints through ReadApproval and refuses, before anything is opened
// or a report written, an invocation whose endpoints, fixtures or contacts
// the owner-approved record does not support, each with its own line; a
// valid one goes on to open the DAC.
func TestApprovedTransitionScenarioNeverProbes(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		code       int
		setup      func(q *qualifyBox, args []string) []string
	}{
		{"valid", "", 1, nil},
		{"candidate", "--qualification: owner approval incomplete", 1, func(q *qualifyBox, args []string) []string {
			native, dop := q.approvedEndpoints()
			q.record("approved.json", "CLI candidate; not approved", 1, []qualification.ApprovedPolicy{native, dop}, nil, nil)
			return args
		}},
		{"native_id_unknown", "--native-tuple-id names no approved native policy", 1, func(q *qualifyBox, args []string) []string {
			return setNativeArg(args, "native-tuple-id", "n128")
		}},
		{"native_id_dop", "--native-tuple-id names no approved native policy", 1, func(q *qualifyBox, args []string) []string {
			return setNativeArg(args, "native-tuple-id", "dop64")
		}},
		{"dop_flags", "--qualification approves no DoP policy for these flags", 1, func(q *qualifyBox, args []string) []string {
			return setNativeArg(args, "tail-ms", "25")
		}},
		{"shape", "approved-native-dop-native plays three DSD fixtures: native, DoP, native", 1, func(q *qualifyBox, args []string) []string {
			return append(args, "dsda")
		}},
		{"pcm_fixture", "approved-native-dop-native plays three DSD fixtures: native, DoP, native", 1, func(q *qualifyBox, args []string) []string {
			return append(args[:len(args)-1], "pcm176")
		}},
		{"contact", "--contact-report: contact for approved tuple missing", 1, func(q *qualifyBox, args []string) []string {
			q.bundle("contacts.json", q.dopTuple())
			return args
		}},
		{"environment", "--qualification was approved for another manifest or environment", 1, func(q *qualifyBox, args []string) []string {
			q.reportsOn("another-kernel")
			q.approvedEndpoints()
			return args
		}},
		{"native_transport", "approved-native-dop-native names its DoP policy by --transport dop and the policy flags", 2, func(q *qualifyBox, args []string) []string {
			return setNativeArg(setNativeArg(args, "transport", "native"), "format", "DSD_U32_BE")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newQualifyBox(t)
			q.approvedEndpoints()
			q.bundle("contacts.json", q.nativeTuple(), q.dopTuple())
			args := q.transitionArgs()
			if tc.setup != nil {
				args = tc.setup(q, args)
			}
			var errout bytes.Buffer
			code := runOutputDSDTest(context.Background(), args, q.env, io.Discard, &errout)
			if tc.want == "" {
				if q.playerOpens.Load() == 0 {
					t.Fatalf("a valid approved transition never reached the DAC: exit %d, %q", code, errout.String())
				}
				return
			}
			if code != tc.code || errout.String() != tc.want+"\n" {
				t.Fatalf("exit %d, %q; want %d and the refusal %q", code, errout.String(), tc.code, tc.want)
			}
			if n, m := q.harnessOpens.Load(), q.playerOpens.Load(); n != 0 || m != 0 {
				t.Fatalf("the refused invocation opened the DAC: %d harness and %d player opens", n, m)
			}
			if _, e := os.Stat(filepath.Join(q.dir, "report.json")); !errors.Is(e, os.ErrNotExist) {
				t.Fatalf("the refused invocation wrote a report: %v", e)
			}
		})
	}
}
