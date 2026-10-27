package qualification

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func approvalFixture(t *testing.T) (string, Approval, Identity) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("production DSD approval uses Unix ownership; player is Linux only")
	}
	dir := t.TempDir()
	id := Identity{Vendor: "20b1", ProductID: "300a", Manufacturer: "iFi", Product: "GO bar", BCDDevice: "0100", Port: "platform-test/usb-0:1:1.0", Device: 0}
	p := ApprovedPolicy{ID: "one", Tuple: Tuple{Format: "DSD_U32_BE", BitRate: 2822400, Pause: "kernel"}, Channels: 2, Ordering: "msb", PauseReleaseSeconds: 30}
	for _, scenario := range []string{"matrix", "backend-lifecycle"} {
		report := map[string]any{"QualificationVersion": Version, "Tuple": p.Tuple, "NativeTuple": p.Tuple, "NativeCompleted": 1, "Identity": id, "ManifestHash": Digest("fixtures"), "Environment": "test", "Scenario": scenario, "End": "complete", "LifecycleActions": []string{"natural", "next", "previous", "pause", "resume", "released", "reopened", "stop"}, "Completed": []bool{true}, "PauseReleaseSeconds": 30, "PCMTimePolicy": [2]int{}, "BuildSHA": "test-build", "FeatureUnit": false, "Before": map[string]any{}, "After": map[string]any{}}
		b, _ := json.Marshal(report)
		path := scenario + ".json"
		if err := os.WriteFile(filepath.Join(dir, path), b, 0600); err != nil {
			t.Fatal(err)
		}
		e := Evidence{Path: path, SHA256: hashBytes(b)}
		if scenario == "matrix" {
			p.Matrix = e
		} else {
			p.Lifecycle = e
		}
	}
	a := Approval{Body: ApprovalBody{SchemaVersion: 1, QualificationVersion: Version, Confirmation: ApprovalPhrase, Revision: "r1", ManifestHash: Digest("fixtures"), Environment: "test", Identity: id, Policies: []ApprovedPolicy{p}}}
	a.SHA256 = Digest(a.Body)
	writeApproval(t, dir, a)
	return dir, a, id
}
func hashBytes(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
func writeApproval(t *testing.T, dir string, a Approval) {
	t.Helper()
	b, _ := json.Marshal(a)
	if err := os.WriteFile(filepath.Join(dir, "approved.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestApprovedRecordTrust(t *testing.T) {
	dir, a, id := approvalFixture(t)
	if _, err := ReadApproval(dir, "approved.json", id); err != nil {
		t.Fatal("fresh approved record refused:", err)
	}
	for _, tc := range []string{"hash", "mode", "identity", "version", "duplicate", "policy", "missing_evidence", "provisional", "deleted"} {
		t.Run(tc, func(t *testing.T) {
			dir, a, id = approvalFixture(t)
			switch tc {
			case "hash":
				a.SHA256 = "broken"
			case "mode":
				if err := os.Chmod(filepath.Join(dir, "approved.json"), 0666); err != nil {
					t.Fatal(err)
				}
			case "identity":
				a.Body.Identity.BCDDevice = "0200"
			case "version":
				a.Body.QualificationVersion--
			case "duplicate":
				a.Body.Policies = append(a.Body.Policies, a.Body.Policies[0])
			case "policy":
				a.Body.Policies[0].Tuple.TailMS = 25
				// Keep native execution matching; only the report's declared
				// tuple is stale, so that comparison is independently pinned.
				ev := &a.Body.Policies[0].Lifecycle
				data, e := os.ReadFile(filepath.Join(dir, ev.Path))
				if e != nil {
					t.Fatal(e)
				}
				var report map[string]any
				if e = json.Unmarshal(data, &report); e != nil {
					t.Fatal(e)
				}
				report["NativeTuple"] = a.Body.Policies[0].Tuple
				data, _ = json.Marshal(report)
				if e = os.WriteFile(filepath.Join(dir, ev.Path), data, 0600); e != nil {
					t.Fatal(e)
				}
				ev.SHA256 = hashBytes(data)
			case "missing_evidence":
				os.Remove(filepath.Join(dir, a.Body.Policies[0].Lifecycle.Path))
			case "provisional":
				a.Body.Confirmation = "candidate"
			case "deleted":
				os.Remove(filepath.Join(dir, "approved.json"))
			}
			if tc != "mode" && tc != "deleted" && tc != "missing_evidence" {
				if tc != "hash" {
					a.SHA256 = Digest(a.Body)
				}
				writeApproval(t, dir, a)
			}
			if _, err := ReadApproval(dir, "approved.json", id); err == nil {
				t.Fatal("invalid approval accepted")
			}
		})
	}
}

func TestApprovalExecutedEvidence(t *testing.T) {
	for _, mode := range []string{"hash", "cancelled", "sink_only", "unverified", "incomplete", "lifecycle", "pcm_only", "negative_native", "wrong_native_tuple", "gain", "edge"} {
		t.Run(mode, func(t *testing.T) {
			dir, a, id := approvalFixture(t)
			name := a.Body.Policies[0].Lifecycle.Path
			b, e := os.ReadFile(filepath.Join(dir, name))
			if e != nil {
				t.Fatal(e)
			}
			var r map[string]any
			if e = json.Unmarshal(b, &r); e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "hash":
				r["ExtraEvidence"] = "tampered"
			case "cancelled":
				r["End"] = "cancelled"
			case "sink_only":
				r["Scenario"] = "matrix"
			case "unverified":
				r["BuildSHA"] = "unverified"
			case "incomplete":
				r["Completed"] = []bool{true, false}
			case "pcm_only":
				r["NativeCompleted"] = 0
			case "negative_native":
				r["NativeCompleted"] = -1
			case "wrong_native_tuple":
				r["NativeTuple"] = Tuple{Format: "DSD_U32_BE", BitRate: 5644800, Pause: "kernel"}
			case "lifecycle":
				r["LifecycleActions"] = []string{"natural"}
			case "gain":
				r["FeatureUnit"] = true
			case "edge":
				a.Body.Edges = []Edge{{From: "unexecuted", To: "unexecuted"}}
			}
			b, _ = json.Marshal(r)
			if e = os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
				t.Fatal(e)
			}
			if mode != "hash" {
				a.Body.Policies[0].Lifecycle.SHA256 = hashBytes(b)
			}
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, a)
			if _, e = ReadApproval(dir, "approved.json", id); e == nil {
				t.Fatal("unexecuted or tampered evidence approved")
			}
		})
	}
}
