package playback

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// recordAtVersion puts the approved fixture's record and both reports of
// each of its policies at version v, as an owner's record made at v reads
// at this version.
func recordAtVersion(t *testing.T, a *Authorization, record *qualification.Approval, v int) {
	t.Helper()
	for i := range record.Body.Policies {
		p := &record.Body.Policies[i]
		for _, ev := range []*qualification.Evidence{&p.Matrix, &p.Lifecycle} {
			name := filepath.Join(a.StateDir, ev.Path)
			data, e := os.ReadFile(name)
			if e != nil {
				t.Fatal(e)
			}
			var r map[string]any
			if e = json.Unmarshal(data, &r); e != nil {
				t.Fatal(e)
			}
			r["QualificationVersion"] = v
			if data, e = json.Marshal(r); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(name, data, 0600); e != nil {
				t.Fatal(e)
			}
			ev.SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
		}
	}
	record.Body.QualificationVersion = v
	saveApprovedFixture(t, a, *record)
}

// TestTheApprovalAdmissionTakesTheHarnesssVersionAlone (B728 B7): since the
// cutover the approval's admission (a backend built with an Authorization)
// is the harness's, and reads an owner's record through ReadApproval, at
// this build's Version alone: a record of qualification.LegacyVersion,
// which production played through the legacy reader until B7, and one of
// Version+1 are refused with the version's refusal and publish no tuple, and
// one of Version reads and publishes its policy. Production reads no record
// at all (TestServeReadsNoApprovalRecord); the legacy reader is the
// migration's alone.
func TestTheApprovalAdmissionTakesTheHarnesssVersionAlone(t *testing.T) {
	if qualification.Version <= qualification.LegacyVersion {
		t.Fatalf("qualificationVersion %d is not past LegacyVersion %d: no bump, so this proves nothing", qualification.Version, qualification.LegacyVersion)
	}
	for _, v := range []int{qualification.LegacyVersion, qualification.Version, qualification.Version + 1} {
		t.Run(fmt.Sprintf("version_%d", v), func(t *testing.T) {
			cfg, record := approvedBackendFixture(t)
			cfg.Resolver = edgeContent(nil)
			recordAtVersion(t, cfg.Authorization, &record, v)
			b, e := New(cfg)
			if e != nil {
				t.Fatal(e)
			}
			token, e := b.readPermission(context.Background())
			b.RefreshAuthorization(context.Background())
			p := b.Policy()
			if v == qualification.Version {
				if e != nil || len(p.Tuples) != 1 || p.Tuples[0].PolicyID != "p64" {
					t.Fatalf("a record of version %d: %v, published %+v; want it read and its DSD64 policy published", v, e, p)
				}
				if plan, e := token.selectNative(format.NativeDSD{BitRateHz: 2822400, Channels: 2}); e != nil || plan.PolicyID != "p64" {
					t.Fatalf("DSD64 selected %+v, %v; want the record's policy", plan, e)
				}
				return
			}
			if e == nil || e.Error() != "stale approval version" || len(p.Tuples) != 0 {
				t.Fatalf("a record of version %d under the harness's admission at %d: %v, %d tuples published; want the refusal \"stale approval version\" and none", v, qualification.Version, e, len(p.Tuples))
			}
		})
	}
}

// TestProductionConfiguresNoApprovalBesideItsPolicy (B728 B7): a backend
// takes the effective DSD policy (Config.DSD, production's) or the
// harness's approval admission (Config.Authorization), never both, so no
// production backend can read a record beside its policy.
func TestProductionConfiguresNoApprovalBesideItsPolicy(t *testing.T) {
	cfg, _ := approvedBackendFixture(t)
	cfg.Resolver = edgeContent(nil)
	cfg.DSD = &DSDPolicy{}
	if _, err := New(cfg); err == nil {
		t.Fatal("New took a config with the effective DSD policy and an approval admission")
	}
}

// TestTheHarnessBackendsReadOnlyTheCurrentVersion: the CLI's candidate and
// transition backends (NewQualificationBackend,
// NewTransitionQualificationBackend) read through ReadCandidate and
// ReadApproval, which take the current version alone, so a record of
// qualification.LegacyVersion, which the migration takes, grants them
// nothing, and the same record at the current version reads (B728 B1).
func TestTheHarnessBackendsReadOnlyTheCurrentVersion(t *testing.T) {
	for _, name := range []string{"candidate", "transition"} {
		t.Run(name, func(t *testing.T) {
			cfg, record := approvedBackendFixture(t)
			cfg.Resolver = edgeContent(nil)
			build := func() (*Backend, error) { return NewQualificationBackend(cfg) }
			if name == "candidate" {
				record.Body.Confirmation = "CLI candidate; not approved"
			} else {
				build = func() (*Backend, error) {
					return NewTransitionQualificationBackend(cfg, TransitionPlans{Policies: map[string]string{"a": "p64"}})
				}
			}
			recordAtVersion(t, cfg.Authorization, &record, qualification.LegacyVersion)
			b, e := build()
			if e != nil {
				t.Fatal(e)
			}
			if _, e = b.readPermission(context.Background()); e == nil || e.Error() != "stale approval version" {
				t.Fatalf("the %s backend read a record of version %d: %v; want the refusal \"stale approval version\"", name, qualification.LegacyVersion, e)
			}
			recordAtVersion(t, cfg.Authorization, &record, qualification.Version)
			if _, e = b.readPermission(context.Background()); e != nil {
				t.Fatalf("the %s backend refused the record at the current version: %v", name, e)
			}
		})
	}
}
