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

// TestProductionStillPlaysAVersion35RecordAfterTheBump: after a bump past
// qualification.LegacyVersion, an owner's record of that version, its
// reports with it, still plays in production (B728 B1): serve's and output
// play's backend (New) reads it, publishes its tuple, admits a session's
// check of it and selects its policy for DSD64, as before the bump.
func TestProductionStillPlaysAVersion35RecordAfterTheBump(t *testing.T) {
	if qualification.Version <= qualification.LegacyVersion {
		t.Fatalf("qualificationVersion %d is not past LegacyVersion %d: no bump, so this proves nothing", qualification.Version, qualification.LegacyVersion)
	}
	cfg, record := approvedBackendFixture(t)
	cfg.Resolver = edgeContent(nil)
	recordAtVersion(t, cfg.Authorization, &record, qualification.LegacyVersion)
	b, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	token, e := b.readPermission(context.Background())
	if e != nil {
		t.Fatalf("production refused an owner's record of version %d after the bump to %d: %v", qualification.LegacyVersion, qualification.Version, e)
	}
	b.RefreshAuthorization(context.Background())
	if p := b.Policy(); len(p.Tuples) != 1 || p.Tuples[0].PolicyID != "p64" || p.Revision != "r1" || p.Reason != "" {
		t.Fatalf("published %+v; want the record's DSD64 policy", p)
	}
	if e = b.checkPermission(context.Background(), token, nil, qualification.NativeKey(record.Body.Policies[0])); e != nil {
		t.Fatalf("a session's check refused the record: %v", e)
	}
	plan, e := token.selectNative(format.NativeDSD{BitRateHz: 2822400, Channels: 2})
	if e != nil || plan.PolicyID != "p64" {
		t.Fatalf("DSD64 selected %+v, %v; want the record's policy", plan, e)
	}
}

// TestTheHarnessBackendsReadOnlyTheCurrentVersion: the CLI's candidate and
// transition backends (NewQualificationBackend,
// NewTransitionQualificationBackend) read through ReadCandidate and
// ReadApproval, which take the current version alone, so a record of
// qualification.LegacyVersion, which production plays, grants them
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
