package dsdstate

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification/approvaltest"
)

// t0 is the tests' clock: every time a step stamps is it.
var t0 = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

func newKey(t *testing.T, vendor, product uint16, iface *int, name *string) dacprofile.Key {
	t.Helper()
	k, err := dacprofile.NewKey(vendor, product, iface, name)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func ptr[T any](v T) *T { return &v }

// goBar is the owner's DAC, the model the records are of; brooklyn and
// hugo are two others.
func goBar(t *testing.T) dacprofile.Key {
	return newKey(t, 0x20b1, 0x3009, ptr(1), ptr("GO bar"))
}
func brooklyn(t *testing.T) dacprofile.Key {
	return newKey(t, 0x152a, 0x85de, ptr(2), ptr("Brooklyn DAC+"))
}
func hugo(t *testing.T) dacprofile.Key { return newKey(t, 0x262a, 0x9302, nil, nil) }

// goBarIdentity is the GO bar as the legacy reader compares it, at a port
// and on a firmware.
func goBarIdentity(port, bcd string) qualification.Identity {
	return qualification.Identity{Vendor: "20b1", ProductID: "3009", Manufacturer: "iFi", Product: "GO bar", BCDDevice: bcd, Port: port, Interface: 1}
}

// bound is the GO bar bound now: at another port than any record names, on
// the firmware the records were made on.
func bound(t *testing.T) *Bound {
	return &Bound{Key: goBar(t), Identity: goBarIdentity("platform-xhci-hcd.1.auto-usb2-2", "0123")}
}

// durable gives a test the real durable writer over a directory barrier and
// a medium of its own: the test's directory may be on tmpfs, which the
// writer refuses, and Windows has no barrier. Both seams are process-global:
// a test that uses it never runs in parallel.
func durable(t *testing.T) {
	t.Helper()
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
}

// openAt opens the state in dir on the tests' clock.
func openAt(t *testing.T, dir string, o options) *Store {
	t.Helper()
	if o.now == nil {
		o.now = func() time.Time { return t0 }
	}
	s, err := open(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fileState decodes the state file in dir.
func fileState(t *testing.T, dir string) State {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Decode(b)
	if err != nil {
		t.Fatalf("the state file does not decode: %v\n%s", err, b)
	}
	return s
}

// recordSpec is an owner's approval record as recordFixture writes it.
type recordSpec struct {
	// version is the record's and every report's qualificationVersion: 0 is
	// qualification.LegacyVersion.
	version int
	// tuples are the approved policies' tuples: none is one native DSD64
	// policy.
	tuples []qualification.Tuple
	// schema is 1 or 2 (0: 1); qualifiedOn is schema 2's date.
	schema      int
	qualifiedOn string
	// identity is the unit the record and its reports were made on: the
	// GO bar at a port no unit is bound at now, on firmware 0123, when zero.
	identity qualification.Identity
}

var (
	nativeAt = func(rate int) qualification.Tuple {
		return qualification.Tuple{Format: "DSD_U32_BE", BitRate: rate, Pause: "kernel"}
	}
	dopAt = func(rate int) qualification.Tuple {
		return qualification.Tuple{Transport: "dop", Format: "S32_LE", BitRate: rate, Pause: "kernel"}
	}
)

// recordFixture writes, in a private directory, an owner's approval record
// the legacy reader takes for the unit spec names, with each policy's matrix
// and lifecycle report, as the harness writes them: returns the directory
// and the record, at approved.json in it.
func recordFixture(t *testing.T, spec recordSpec) (string, qualification.Approval) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("an owner's approval is trusted by Unix ownership; the player is Linux's alone")
	}
	if spec.version == 0 {
		spec.version = qualification.LegacyVersion
	}
	if len(spec.tuples) == 0 {
		spec.tuples = []qualification.Tuple{nativeAt(DSD64)}
	}
	if spec.schema == 0 {
		spec.schema = 1
	}
	if spec.identity == (qualification.Identity{}) {
		spec.identity = goBarIdentity("platform-1000120000.pcie-usb2-1", "0123")
	}
	dir := approvaltest.TempDir(t)
	environment := qualification.Digest([]string{"linux/arm64", "test-go", "test-kernel"})
	var policies []qualification.ApprovedPolicy
	for i, tuple := range spec.tuples {
		p := qualification.ApprovedPolicy{ID: fmt.Sprintf("p%d", i), Tuple: tuple, Channels: 2, Ordering: "msb", PauseReleaseSeconds: 30}
		for _, scenario := range []string{"matrix", "backend-lifecycle"} {
			r := map[string]any{"QualificationVersion": spec.version, "Tuple": tuple, "NativeTuple": tuple, "NativeCompleted": 1, "Identity": spec.identity,
				"ManifestHash": qualification.Digest("fixtures"), "Environment": environment, "Scenario": scenario, "End": "complete", "Completed": []bool{true},
				"PauseReleaseSeconds": 30, "BuildSHA": "test-build", "FeatureUnit": false, "Before": map[string]any{}, "After": map[string]any{},
				"LifecycleActions": []string{"natural", "next", "previous", "pause", "resume", "released", "reopened", "stop"}}
			if tuple.Transport == "dop" {
				yes := true
				r["SchemaVersion"], r["PackingVersion"], r["SourceRate"], r["CarrierRate"] = 2, 1, tuple.BitRate, tuple.BitRate/16
				r["Kernel"], r["Platform"], r["GoVersion"] = "test-kernel", "linux/arm64", "test-go"
				r["Opened"] = []map[string]any{{"Format": "S32_LE", "RateHz": tuple.BitRate / 16, "Channels": 2, "Msbits": 32, "PeriodFrames": 8820, "BufferFrames": 35280, "RateNum": tuple.BitRate / 16, "RateDen": 1}}
				r["DoP"] = &qualification.DoPObservations{Indicator: &yes, Channels: &yes, PitchDuration: &yes}
			}
			data, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			name := fmt.Sprintf("%s-%s.json", p.ID, scenario)
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				t.Fatal(err)
			}
			ev := qualification.Evidence{Path: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}
			if scenario == "matrix" {
				p.Matrix = ev
			} else {
				p.Lifecycle = ev
			}
		}
		policies = append(policies, p)
	}
	a := qualification.Approval{Body: qualification.ApprovalBody{SchemaVersion: spec.schema, QualificationVersion: spec.version, Confirmation: qualification.ApprovalPhrase,
		Revision: "r1", ManifestHash: qualification.Digest("fixtures"), Environment: environment, Identity: spec.identity, Policies: policies, QualifiedOn: spec.qualifiedOn}}
	writeRecord(t, dir, &a)
	return dir, a
}

// writeRecord takes a's digest and writes it at approved.json in dir.
func writeRecord(t *testing.T, dir string, a *qualification.Approval) {
	t.Helper()
	a.SHA256 = qualification.Digest(a.Body)
	data, err := json.Marshal(*a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "approved.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
