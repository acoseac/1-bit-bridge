package qualification

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// kinds are the readers' refusal kinds (B728 B3).
var kinds = []error{ErrRecordMissing, ErrStaleVersion, ErrHashMismatch, ErrIdentityMismatch}

// refusedAs fails the test unless e is the refusal want names, its message
// exactly as the reader gave it before it was marked, and marked with kind
// alone (nil: no kind).
func refusedAs(t *testing.T, e error, want string, kind error) {
	t.Helper()
	refusedFor(t, e, want)
	for _, k := range kinds {
		if got := errors.Is(e, k); got != (k == kind) {
			t.Errorf("errors.Is(%q, %v) = %v", e, k, got)
		}
	}
}

// TestTheReadersRefusalsAreMarkedByKind: each refusal the readers made
// before B728's B3 is marked with the kind the migration's warning names,
// its message unchanged, so the migration gives a reason by errors.Is and
// never by the text: a record that does not exist (the state directory's or
// the record's absence; a filesystem error, which errors.As still finds), a
// digest that does not match (the body's, a report's, a transition
// report's, a DoP experiment's), a version the reader does not take (the
// record's, a schema it does not know, each report kind's, an experiment's,
// a contact's, and the harness's own readers'), and the record's identity
// (another port under every field compared, a unit whose identity is not
// whole). A report the record names that does not exist, a report of
// another unit in a record of this one, and an unexecuted edge are of no
// kind: the record does not read.
func TestTheReadersRefusalsAreMarkedByKind(t *testing.T) {
	read := func(dir string, id Identity) error {
		_, e := ReadLegacyApproval(dir, "approved.json", id, SameIdentity)
		return e
	}
	t.Run("record_missing", func(t *testing.T) {
		dir, _, id := versioned(t, "native", LegacyVersion)
		_, e := ReadLegacyApproval(dir, "absent.json", id, SameIdentity)
		var pe *fs.PathError
		if !errors.As(e, &pe) || !errors.Is(e, fs.ErrNotExist) {
			t.Fatalf("a missing record's refusal %v is no filesystem error any more", e)
		}
		refusedAs(t, e, pe.Error(), ErrRecordMissing)
		gone := filepath.Join(dir, "gone")
		_, e = ReadLegacyApproval(gone, "approved.json", id, SameIdentity)
		if !errors.Is(e, ErrRecordMissing) {
			t.Fatalf("a missing state directory's refusal %v is not a missing record", e)
		}
	})
	t.Run("a_missing_report", func(t *testing.T) {
		dir, a, id := versioned(t, "native", LegacyVersion)
		if e := os.Remove(filepath.Join(dir, a.Body.Policies[0].Lifecycle.Path)); e != nil {
			t.Fatal(e)
		}
		e := read(dir, id)
		if !errors.Is(e, fs.ErrNotExist) {
			t.Fatalf("a missing report's refusal is %v", e)
		}
		refusedAs(t, e, e.Error(), nil)
	})
	t.Run("hash_mismatch", func(t *testing.T) {
		dir, a, id := versioned(t, "native", LegacyVersion)
		a.Body.Revision = "r2"
		writeApproval(t, dir, a)
		refusedAs(t, read(dir, id), "approval hash mismatch", ErrHashMismatch)

		dir, a, id = versioned(t, "native", LegacyVersion)
		name := filepath.Join(dir, a.Body.Policies[0].Matrix.Path)
		b, e := os.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(name, []byte(strings.Replace(string(b), `"test-build"`, `"test-builD"`, 1)), 0o600); e != nil {
			t.Fatal(e)
		}
		refusedAs(t, read(dir, id), "evidence hash mismatch", ErrHashMismatch)

		dir, a, id = versioned(t, "transition", LegacyVersion)
		name = filepath.Join(dir, a.Body.Transitions[0].Path)
		if b, e = os.ReadFile(name); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(name, append(b, ' '), 0o600); e != nil {
			t.Fatal(e)
		}
		refusedAs(t, read(dir, id), "evidence hash mismatch", ErrHashMismatch)

		dir, _, id = dopExperimentFixture(t, func(_ string, r *executedReport) {
			r.QualificationVersion, r.Contact.QualificationVersion = Version, Version
		})
		name = filepath.Join(dir, "experiment-restored.json")
		if b, e = os.ReadFile(name); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(name, append(b, ' '), 0o600); e != nil {
			t.Fatal(e)
		}
		refusedAs(t, read(dir, id), "DoP experiment hash mismatch", ErrHashMismatch)
	})
	t.Run("stale_version", func(t *testing.T) {
		below := func(r map[string]any) { r["QualificationVersion"] = LegacyVersion - 1 }
		dir, _, id := versioned(t, "native", LegacyVersion-1)
		refusedAs(t, read(dir, id), "stale approval version", ErrStaleVersion)

		dir, a, id := versioned(t, "native", LegacyVersion)
		a.Body.SchemaVersion = 3
		a.SHA256 = Digest(a.Body)
		writeApproval(t, dir, a)
		refusedAs(t, read(dir, id), "stale approval version", ErrStaleVersion)

		for _, which := range []string{"matrix", "lifecycle"} {
			dir, a, id = versioned(t, "native", LegacyVersion)
			ev := &a.Body.Policies[0].Matrix
			if which == "lifecycle" {
				ev = &a.Body.Policies[0].Lifecycle
			}
			rewriteReport(t, dir, ev, below)
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, a)
			refusedAs(t, read(dir, id), "report does not execute approved policy", ErrStaleVersion)
		}

		dir, a, id = versioned(t, "transition", LegacyVersion)
		rewriteReport(t, dir, &a.Body.Transitions[0], below)
		a.SHA256 = Digest(a.Body)
		writeApproval(t, dir, a)
		refusedAs(t, read(dir, id), "transition report does not execute approved policies", ErrStaleVersion)

		for _, tc := range []struct {
			want   string
			change func(r *executedReport)
		}{
			{"DoP experiment context mismatch", func(r *executedReport) { r.QualificationVersion = LegacyVersion - 1 }},
			{"contact version mismatch", func(r *executedReport) { r.Contact.QualificationVersion = LegacyVersion - 1 }},
		} {
			dir, a, id = dopExperimentFixture(t, func(state string, r *executedReport) {
				r.QualificationVersion, r.Contact.QualificationVersion = LegacyVersion, LegacyVersion
				if state == "restored" {
					tc.change(r)
				}
			})
			atVersion(t, dir, &a, LegacyVersion)
			refusedAs(t, read(dir, id), tc.want, ErrStaleVersion)
		}

		dir, _, id = versioned(t, "native", LegacyVersion)
		_, e := ReadApproval(dir, "approved.json", id)
		refusedAs(t, e, "stale approval version", ErrStaleVersion)
	})
	t.Run("identity_mismatch", func(t *testing.T) {
		dir, _, id := versioned(t, "native", LegacyVersion)
		elsewhere := id
		elsewhere.Port = "platform-test/usb-0:1:2.0"
		refusedAs(t, read(dir, elsewhere), "stale approval identity", ErrIdentityMismatch)
		whole := id
		whole.Port = ""
		refusedAs(t, read(dir, whole), "stale approval identity", ErrIdentityMismatch)
	})
	t.Run("of_no_kind", func(t *testing.T) {
		dir, a, id := versioned(t, "native", LegacyVersion)
		rewriteReport(t, dir, &a.Body.Policies[0].Lifecycle, func(r map[string]any) { r["Identity"].(map[string]any)["Product"] = "another product" })
		a.SHA256 = Digest(a.Body)
		writeApproval(t, dir, a)
		refusedAs(t, read(dir, id), "report does not execute approved policy", nil)

		dir, a, id = versioned(t, "native", LegacyVersion)
		p := a.Body.Policies[0]
		a.Body.Edges = []Edge{{From: NativeKey(p), To: NativeKey(p)}}
		a.SHA256 = Digest(a.Body)
		writeApproval(t, dir, a)
		refusedAs(t, read(dir, id), "unexecuted transition edge", nil)

		dir, _, id = dopExperimentFixture(t, func(state string, r *executedReport) {
			r.QualificationVersion, r.Contact.QualificationVersion = LegacyVersion, LegacyVersion
			if state == "restored" {
				r.SchemaVersion = 1
			}
		})
		refusedAs(t, read(dir, id), "DoP experiment context mismatch", nil)
	})
}
