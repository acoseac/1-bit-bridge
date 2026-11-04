package qualification

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
)

// atVersion puts a record and the reports its policies and transitions name
// at version v, as an owner's record made at v reads at this version: each
// policy's matrix and lifecycle report, each transition report (its
// endpoints bound again to the policies, whose digests change with their
// evidence), then the record. A DoP experiment's version is
// dopExperimentFixture's.
func atVersion(t *testing.T, dir string, a *Approval, v int) {
	t.Helper()
	for i := range a.Body.Policies {
		p := &a.Body.Policies[i]
		for _, ev := range []*Evidence{&p.Matrix, &p.Lifecycle} {
			rewriteReport(t, dir, ev, func(r map[string]any) { r["QualificationVersion"] = v })
		}
	}
	for i := range a.Body.Transitions {
		rewriteReport(t, dir, &a.Body.Transitions[i], func(r map[string]any) {
			r["QualificationVersion"] = v
			var refs []PolicyRef
			for _, ref := range r["Endpoints"].([]any) {
				for _, p := range a.Body.Policies {
					if p.ID == ref.(map[string]any)["ID"] {
						refs = append(refs, PolicyRef{ID: p.ID, SHA256: Digest(p)})
					}
				}
			}
			r["Endpoints"] = refs
		})
	}
	a.Body.QualificationVersion = v
	a.SHA256 = Digest(a.Body)
	writeApproval(t, dir, *a)
}

// dopExperimentFixture is dopApprovalFixture's record on a DAC with a USB
// feature unit: its policy constrains the one mixer control its reports
// read, and its matrix and lifecycle reports name the two level-check
// experiments CheckDoPExperiments asks of such a DAC, one attenuated and
// one restored, each with its DoP contact. change, when given, alters each
// experiment before it is written.
func dopExperimentFixture(t *testing.T, change func(state string, r *executedReport)) (string, Approval, Identity) {
	t.Helper()
	dir, a, id := dopApprovalFixture(t)
	p := &a.Body.Policies[0]
	volume := alsactl.Control{Interface: 2, Name: "Playback Volume", Type: 2, Count: 1, Access: 1, Values: make([]byte, 8)}
	p.Constraints = []alsactl.Constraint{{Interface: volume.Interface, Name: volume.Name, Value: volume.Values}}
	matrix, e := os.ReadFile(filepath.Join(dir, p.Matrix.Path))
	if e != nil {
		t.Fatal(e)
	}
	var refs []Evidence
	for i, state := range []string{"nonzero-attenuation", "restored"} {
		var r executedReport
		if e = json.Unmarshal(matrix, &r); e != nil {
			t.Fatal(e)
		}
		level := volume
		level.Values = []byte{byte(i + 1), 0, 0, 0, 0, 0, 0, 0}
		snap := alsactl.Snapshot{Controls: []alsactl.Control{level}}
		yes := true
		r.FeatureUnit, r.Scenario, r.ExperimentState, r.Before, r.After = &yes, "level-check", state, snap, snap
		r.Contact = &Contact{SchemaVersion: 3, QualificationVersion: Version, Device: new(int), Subdevice: new(int), Tuple: p.Tuple, Attachment: Attachment{BootID: "boot", Port: id.Port, Manufacturer: id.Manufacturer, Product: id.Product, BCDDevice: id.BCDDevice, Interface: id.Interface, DevNum: 1}, MixerHash: snap.Hash(), ManifestHash: a.Body.ManifestHash, KnobPosition: "1", PCMAudible: PCMConfirmation, DSDMatched: DSDConfirmation, DoP: dopObserved()}
		if change != nil {
			change(state, &r)
		}
		data, e := json.Marshal(r)
		if e != nil {
			t.Fatal(e)
		}
		name := "experiment-" + state + ".json"
		if e = os.WriteFile(filepath.Join(dir, name), data, 0600); e != nil {
			t.Fatal(e)
		}
		refs = append(refs, Evidence{Path: name, SHA256: hashBytes(data)})
	}
	for _, ev := range []*Evidence{&p.Matrix, &p.Lifecycle} {
		rewriteReport(t, dir, ev, func(r map[string]any) {
			r["FeatureUnit"] = true
			r["Experiments"] = refs
			r["Before"] = alsactl.Snapshot{Controls: []alsactl.Control{volume}}
			r["After"] = r["Before"]
		})
	}
	a.SHA256 = Digest(a.Body)
	writeApproval(t, dir, a)
	return dir, a, id
}

// legacyKinds are the records an owner can hold: a native one, a DoP one
// on a DAC with a feature unit, its experiments and their contacts with it,
// and a schema-2 one whose native↔DoP edges a transition report executed.
var legacyKinds = []string{"native", "dop_experiments", "transition"}

// versioned is a fixture of kind, its record and every report it holds at
// version v.
func versioned(t *testing.T, kind string, v int) (string, Approval, Identity) {
	t.Helper()
	var dir string
	var a Approval
	var id Identity
	switch kind {
	case "native":
		dir, a, id = approvalFixture(t)
	case "dop_experiments":
		dir, a, id = dopExperimentFixture(t, func(_ string, r *executedReport) { r.QualificationVersion, r.Contact.QualificationVersion = v, v })
	case "transition":
		dir, a, id, _ = transitionFixture(t)
	default:
		t.Fatalf("no fixture of kind %q", kind)
	}
	atVersion(t, dir, &a, v)
	return dir, a, id
}

// TestTheLegacyReaderRefusesBelowVersion35: the legacy reader takes a record
// whose version, and every report's it holds, is LegacyVersion, frozen at
// 35, or later, and refuses one below it, by the record's version check or
// by that of the report below it: each policy's matrix and lifecycle
// report, a transition report, a DoP experiment and an experiment's
// contact. The harness's readers still take the current version alone
// (B728 B1).
func TestTheLegacyReaderRefusesBelowVersion35(t *testing.T) {
	if LegacyVersion != 35 {
		t.Fatalf("LegacyVersion = %d; it is frozen at 35, the version every owner's record held when B728's design merged", LegacyVersion)
	}
	for _, kind := range legacyKinds {
		for _, v := range []int{LegacyVersion, Version, Version + 1} {
			t.Run(fmt.Sprintf("%s_at_%d", kind, v), func(t *testing.T) {
				dir, _, id := versioned(t, kind, v)
				if _, e := ReadLegacyApproval(dir, "approved.json", id, SameIdentity); e != nil {
					t.Fatalf("a record of version %d refused: %v", v, e)
				}
			})
		}
		t.Run(fmt.Sprintf("%s_at_%d", kind, LegacyVersion-1), func(t *testing.T) {
			dir, _, id := versioned(t, kind, LegacyVersion-1)
			_, e := ReadLegacyApproval(dir, "approved.json", id, SameIdentity)
			refusedFor(t, e, "stale approval version")
		})
	}
	below := func(r map[string]any) { r["QualificationVersion"] = LegacyVersion - 1 }
	for _, tc := range []struct {
		name, want string
		record     func(t *testing.T) (string, Approval, Identity)
	}{
		{name: "matrix_report", want: "report does not execute approved policy", record: func(t *testing.T) (string, Approval, Identity) {
			dir, a, id := versioned(t, "native", LegacyVersion)
			rewriteReport(t, dir, &a.Body.Policies[0].Matrix, below)
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, a)
			return dir, a, id
		}},
		{name: "lifecycle_report", want: "report does not execute approved policy", record: func(t *testing.T) (string, Approval, Identity) {
			dir, a, id := versioned(t, "native", LegacyVersion)
			rewriteReport(t, dir, &a.Body.Policies[0].Lifecycle, below)
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, a)
			return dir, a, id
		}},
		{name: "transition_report", want: "transition report does not execute approved policies", record: func(t *testing.T) (string, Approval, Identity) {
			dir, a, id := versioned(t, "transition", LegacyVersion)
			rewriteReport(t, dir, &a.Body.Transitions[0], below)
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, a)
			return dir, a, id
		}},
		{name: "experiment", want: "DoP experiment context mismatch", record: func(t *testing.T) (string, Approval, Identity) {
			dir, a, id := dopExperimentFixture(t, func(state string, r *executedReport) {
				r.QualificationVersion, r.Contact.QualificationVersion = LegacyVersion, LegacyVersion
				if state == "restored" {
					r.QualificationVersion = LegacyVersion - 1
				}
			})
			atVersion(t, dir, &a, LegacyVersion)
			return dir, a, id
		}},
		{name: "contact", want: "contact version mismatch", record: func(t *testing.T) (string, Approval, Identity) {
			dir, a, id := dopExperimentFixture(t, func(state string, r *executedReport) {
				r.QualificationVersion, r.Contact.QualificationVersion = LegacyVersion, LegacyVersion
				if state == "restored" {
					r.Contact.QualificationVersion = LegacyVersion - 1
				}
			})
			atVersion(t, dir, &a, LegacyVersion)
			return dir, a, id
		}},
	} {
		t.Run(tc.name+"_below", func(t *testing.T) {
			dir, _, id := tc.record(t)
			_, e := ReadLegacyApproval(dir, "approved.json", id, SameIdentity)
			refusedFor(t, e, tc.want)
		})
	}
	t.Run("the_harness_readers", func(t *testing.T) {
		dir, a, id := versioned(t, "native", LegacyVersion)
		_, e := ReadApproval(dir, "approved.json", id)
		refusedFor(t, e, "stale approval version")
		a.Body.Confirmation = "CLI candidate; not approved"
		a.SHA256 = Digest(a.Body)
		writeApproval(t, dir, a)
		_, e = ReadCandidate(dir, "approved.json", id)
		refusedFor(t, e, "stale approval version")
		dir, a, id = versioned(t, "dop_experiments", LegacyVersion)
		var refs []Evidence
		for _, state := range []string{"nonzero-attenuation", "restored"} {
			name := "experiment-" + state + ".json"
			b, e := os.ReadFile(filepath.Join(dir, name))
			if e != nil {
				t.Fatal(e)
			}
			refs = append(refs, Evidence{Path: name, SHA256: hashBytes(b)})
		}
		refusedFor(t, CheckDoPExperiments(dir, refs, a.Body.Policies[0].Tuple, id, a.Body.ManifestHash, a.Body.Environment), "DoP experiment context mismatch")
		tuple := Tuple{Transport: "dop", Format: "S32_LE", BitRate: 2822400, Pause: "kernel"}
		attach := Attachment{BootID: "boot", Port: "port", Manufacturer: "m", Product: "p", BCDDevice: "1", DevNum: 1}
		c := Contact{SchemaVersion: 3, QualificationVersion: LegacyVersion, Device: new(int), Subdevice: new(int), Tuple: tuple, Attachment: attach, MixerHash: "m", ManifestHash: "f", KnobPosition: "1", PCMAudible: PCMConfirmation, DSDMatched: DSDConfirmation, DoP: dopObserved()}
		refusedFor(t, c.Validate(tuple, attach, 0, 0, "m", "f"), "contact version mismatch")
		c.QualificationVersion = Version
		if e = c.Validate(tuple, attach, 0, 0, "m", "f"); e != nil {
			t.Fatalf("a contact of the current version refused: %v", e)
		}
	})
}

// TestTheLegacyReaderKeepsEveryOtherCheck: at LegacyVersion, where the
// legacy reader's version rule alone admits a record, every other check
// ReadApproval makes still refuses one, each with its own words: a body
// changed after its digest was taken, a byte of a report changed after the
// record named its digest, an edge no report executed, a CLI candidate in
// the approved record's place, a record a group may write, and, every field
// compared as production compares them until B7, the unit on another port.
func TestTheLegacyReaderKeepsEveryOtherCheck(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(t *testing.T, dir string, a *Approval, live *Identity)
	}{
		{name: "untampered"},
		{name: "tampered_body", want: "approval hash mismatch", change: func(t *testing.T, dir string, a *Approval, _ *Identity) {
			a.Body.Revision = "r2"
			writeApproval(t, dir, *a)
		}},
		{name: "changed_evidence_byte", want: "evidence hash mismatch", change: func(t *testing.T, dir string, a *Approval, _ *Identity) {
			name := filepath.Join(dir, a.Body.Policies[0].Matrix.Path)
			b, e := os.ReadFile(name)
			if e != nil {
				t.Fatal(e)
			}
			if strings.Count(string(b), `"test-build"`) != 1 {
				t.Fatalf("the matrix report names its build %d times, want once", strings.Count(string(b), `"test-build"`))
			}
			if e = os.WriteFile(name, []byte(strings.Replace(string(b), `"test-build"`, `"test-builD"`, 1)), 0600); e != nil {
				t.Fatal(e)
			}
		}},
		{name: "unexecuted_edge", want: "unexecuted transition edge", change: func(t *testing.T, dir string, a *Approval, _ *Identity) {
			p := a.Body.Policies[0]
			a.Body.Edges = []Edge{{From: NativeKey(p), To: NativeKey(p)}}
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, *a)
		}},
		{name: "candidate", want: "owner approval incomplete", change: func(t *testing.T, dir string, a *Approval, _ *Identity) {
			a.Body.Confirmation = "CLI candidate; not approved"
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, *a)
		}},
		{name: "group_writable", want: "untrusted approval owner or mode", change: func(t *testing.T, dir string, _ *Approval, _ *Identity) {
			if e := os.Chmod(filepath.Join(dir, "approved.json"), 0660); e != nil {
				t.Fatal(e)
			}
		}},
		{name: "another_port", want: "stale approval identity", change: func(_ *testing.T, _ string, _ *Approval, live *Identity) {
			live.Port = "platform-test/usb-0:1:2.0"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, a, live := versioned(t, "native", LegacyVersion)
			if tc.change != nil {
				tc.change(t, dir, &a, &live)
			}
			_, e := ReadLegacyApproval(dir, "approved.json", live, SameIdentity)
			if tc.want == "" {
				if e != nil {
					t.Fatalf("a record of version %d refused: %v", LegacyVersion, e)
				}
				return
			}
			refusedFor(t, e, tc.want)
		})
	}
}

// TestTheLegacyReaderComparesTheIdentityItIsGiven: the legacy reader asks its
// caller's comparison of the record's identity and of every report's, so a
// comparison without the port (as the migration's of the model alone will
// be, B6) takes a record of LegacyVersion on the unit read at another port,
// with its reports, a DoP record's experiments and a transition, where
// every field compared refuses it; a report whose identity that comparison
// does not take is refused all the same; and an experiment's contact is
// compared with its own experiment's identity, which the comparison has
// matched to the unit's. With no comparison at all it refuses the record.
func TestTheLegacyReaderComparesTheIdentityItIsGiven(t *testing.T) {
	withoutPort := func(recorded, live Identity) bool {
		recorded.Port, live.Port = "", ""
		return recorded == live
	}
	elsewhere := func(id Identity) Identity { id.Port = "platform-test/usb-0:1:2.0"; return id }
	t.Run("no_comparison", func(t *testing.T) {
		dir, _, id := versioned(t, "native", LegacyVersion)
		_, e := ReadLegacyApproval(dir, "approved.json", id, nil)
		refusedFor(t, e, "no identity comparison")
	})
	for _, kind := range legacyKinds {
		t.Run(kind, func(t *testing.T) {
			dir, _, id := versioned(t, kind, LegacyVersion)
			_, e := ReadLegacyApproval(dir, "approved.json", elsewhere(id), SameIdentity)
			refusedFor(t, e, "stale approval identity")
			if _, e = ReadLegacyApproval(dir, "approved.json", elsewhere(id), withoutPort); e != nil {
				t.Fatalf("a comparison without the port refused the record on another port: %v", e)
			}
			_, e = ReadLegacyApproval(dir, "approved.json", id, func(Identity, Identity) bool { return false })
			refusedFor(t, e, "stale approval identity")
		})
	}
	other := func(r map[string]any) { r["Identity"].(map[string]any)["Product"] = "another product" }
	for _, tc := range []struct {
		name, want string
		record     func(t *testing.T) (string, Approval, Identity)
	}{
		{name: "lifecycle_report", want: "report does not execute approved policy", record: func(t *testing.T) (string, Approval, Identity) {
			dir, a, id := versioned(t, "native", LegacyVersion)
			rewriteReport(t, dir, &a.Body.Policies[0].Lifecycle, other)
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, a)
			return dir, a, id
		}},
		{name: "transition_report", want: "transition report does not execute approved policies", record: func(t *testing.T) (string, Approval, Identity) {
			dir, a, id := versioned(t, "transition", LegacyVersion)
			rewriteReport(t, dir, &a.Body.Transitions[0], other)
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, a)
			return dir, a, id
		}},
		{name: "experiment", want: "DoP experiment context mismatch", record: func(t *testing.T) (string, Approval, Identity) {
			dir, a, id := dopExperimentFixture(t, func(state string, r *executedReport) {
				r.QualificationVersion, r.Contact.QualificationVersion = LegacyVersion, LegacyVersion
				if state == "restored" {
					r.Identity.Product = "another product"
				}
			})
			atVersion(t, dir, &a, LegacyVersion)
			return dir, a, id
		}},
	} {
		t.Run(tc.name+"_of_another_product", func(t *testing.T) {
			dir, _, id := tc.record(t)
			_, e := ReadLegacyApproval(dir, "approved.json", elsewhere(id), withoutPort)
			refusedFor(t, e, tc.want)
		})
	}
}
