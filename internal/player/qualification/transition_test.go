package qualification

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// transitionFixture is a schema-2 owner-approved record of an approved
// native policy (n64) and an approved DoP policy (dop64) at DSD64, each
// with its matrix and backend-lifecycle reports, and one transition report
// (TransitionScenario) that played the two in both directions, its edges
// approved; it returns the state directory, the record, the identity and
// the two cross edges, native to DoP first.
func transitionFixture(t *testing.T) (string, Approval, Identity, [2]Edge) {
	t.Helper()
	dir, a, id := approvalFixture(t)
	environment := Digest([]string{"linux/arm64", "test-go", "test-kernel"})
	dopFields := func(r map[string]any, tuple Tuple) {
		r["SchemaVersion"] = 2
		r["PackingVersion"] = 1
		r["SourceRate"] = tuple.BitRate
		r["CarrierRate"] = tuple.BitRate / 16
		r["Kernel"] = "test-kernel"
		r["Platform"] = "linux/arm64"
		r["GoVersion"] = "test-go"
		r["Opened"] = []map[string]any{{"Format": "S32_LE", "RateHz": tuple.BitRate / 16, "Channels": 2, "Msbits": 32, "PeriodFrames": 8820, "BufferFrames": 35280, "RateNum": tuple.BitRate / 16, "RateDen": 1}}
		r["DoP"] = dopObserved()
	}
	native := a.Body.Policies[0]
	native.ID = "n64"
	dop := native
	dop.ID = "dop64"
	dop.Tuple.Transport, dop.Tuple.Format = "dop", "S32_LE"
	for _, p := range []*ApprovedPolicy{&native, &dop} {
		for _, ev := range []*Evidence{&p.Matrix, &p.Lifecycle} {
			scenario := "matrix"
			if ev == &p.Lifecycle {
				scenario = "backend-lifecycle"
			}
			name := p.ID + "-" + scenario + ".json"
			b, e := os.ReadFile(filepath.Join(dir, ev.Path))
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
				t.Fatal(e)
			}
			ev.Path = name
			tuple := p.Tuple
			rewriteReport(t, dir, ev, func(r map[string]any) {
				r["Tuple"] = tuple
				r["NativeTuple"] = tuple
				r["Environment"] = environment
				if tuple.Transport == "dop" {
					dopFields(r, tuple)
				}
			})
		}
	}
	edges := [2]Edge{{From: NativeKey(native), To: NativeKey(dop)}, {From: NativeKey(dop), To: NativeKey(native)}}
	report := map[string]any{"QualificationVersion": Version, "Tuple": dop.Tuple, "NativeTuple": native.Tuple, "NativeCompleted": 3, "Identity": id, "ManifestHash": a.Body.ManifestHash, "Environment": environment, "Scenario": TransitionScenario, "End": "complete", "Completed": []bool{true, true, true}, "PauseReleaseSeconds": 30, "PCMTimePolicy": [2]int{}, "BuildSHA": "test-build", "FeatureUnit": false, "Before": map[string]any{}, "After": map[string]any{},
		"LifecycleActions": []string{"natural", "next", "previous", "stop"}, "PassedEdges": edges[:],
		"Endpoints": []PolicyRef{{ID: native.ID, SHA256: Digest(native)}, {ID: dop.ID, SHA256: Digest(dop)}}}
	dopFields(report, dop.Tuple)
	b, e := json.Marshal(report)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "transition.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	a.Body.SchemaVersion = 2
	a.Body.QualifiedOn = "2026-10-28"
	a.Body.Environment = environment
	a.Body.Policies = []ApprovedPolicy{native, dop}
	a.Body.Edges = edges[:]
	a.Body.Transitions = []Evidence{{Path: "transition.json", SHA256: hashBytes(b)}}
	a.SHA256 = Digest(a.Body)
	writeApproval(t, dir, a)
	return dir, a, id, edges
}

// TestApprovalTransitionEvidence: an edge between an approved native and an
// approved DoP policy is approved only by a transition report (schema 2's
// Transitions) that executed it between those very policies: executed, the
// record reads; an edge no report executed is unexecuted; a transition
// report whose endpoints are not the record's policies, byte for byte, is
// refused for its endpoints.
func TestApprovalTransitionEvidence(t *testing.T) {
	for _, name := range []string{"executed", "unexecuted", "policy_mismatch"} {
		t.Run(name, func(t *testing.T) {
			dir, a, id, _ := transitionFixture(t)
			want := ""
			switch name {
			case "unexecuted":
				a.Body.Transitions = nil
				want = "unexecuted transition edge"
			case "policy_mismatch":
				other := a.Body.Policies[1]
				other.Tuple.TailMS = 25
				rewriteReport(t, dir, &a.Body.Transitions[0], func(r map[string]any) {
					r["Endpoints"] = []PolicyRef{{ID: a.Body.Policies[0].ID, SHA256: Digest(a.Body.Policies[0])}, {ID: other.ID, SHA256: Digest(other)}}
				})
				want = "transition endpoints do not match approved policies"
			}
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, a)
			_, e := ReadApproval(dir, "approved.json", id)
			if want == "" {
				if e != nil {
					t.Fatalf("an approval whose native↔DoP edges a transition report executed was refused: %v", e)
				}
				return
			}
			refusedFor(t, e, want)
		})
	}
}
