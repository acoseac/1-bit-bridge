package playback

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

type emptyControls struct{}

func (emptyControls) Controls() ([]alsactl.Control, error) { return nil, nil }
func (emptyControls) Close() error                         { return nil }
func approvedBackendFixture(t *testing.T) (Config, qualification.Approval) {
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
	a := &Authorization{Mode: "native", StateDir: t.TempDir(), Record: "approved.json", Sysfs: loc.Observer.Sysfs, Controls: alsactl.Reader{Open: func(int) (alsactl.Device, error) { return emptyControls{}, nil }}}
	u, e := loc.Locate(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	id, e := a.identity(u)
	if e != nil {
		t.Fatal(e)
	}
	p := qualification.ApprovedPolicy{ID: "p64", Tuple: qualification.Tuple{Format: "DSD_U32_BE", BitRate: 2822400, Pause: "kernel"}, Channels: 2, Ordering: "msb", PauseReleaseSeconds: 30}
	edge := qualification.Edge{From: qualification.NativeKey(p), To: qualification.NativeKey(p)}
	for _, scenario := range []string{"matrix", "backend-lifecycle"} {
		r := map[string]any{"QualificationVersion": qualification.Version, "Tuple": p.Tuple, "Identity": id, "ManifestHash": qualification.Digest("fixtures"), "Environment": "fixture", "Scenario": scenario, "End": "complete", "Completed": []bool{true}, "PauseReleaseSeconds": 30, "PCMTimePolicy": [2]int{}, "BuildSHA": "fixture", "FeatureUnit": false, "LifecycleActions": []string{"natural", "next", "previous", "pause", "resume", "released", "reopened", "stop"}, "PassedEdges": []qualification.Edge{edge}}
		b, _ := json.Marshal(r)
		name := scenario + ".json"
		if e := os.WriteFile(filepath.Join(a.StateDir, name), b, 0600); e != nil {
			t.Fatal(e)
		}
		evidence := qualification.Evidence{Path: name, SHA256: fmt.Sprintf("%x", sha256.Sum256(b))}
		if scenario == "matrix" {
			p.Matrix = evidence
		} else {
			p.Lifecycle = evidence
		}
	}
	record := qualification.Approval{Body: qualification.ApprovalBody{SchemaVersion: 1, QualificationVersion: qualification.Version, Confirmation: qualification.ApprovalPhrase, Revision: "r1", Identity: id, ManifestHash: qualification.Digest("fixtures"), Environment: "fixture", Policies: []qualification.ApprovedPolicy{p}, Edges: []qualification.Edge{edge}}}
	saveApprovedFixture(t, a, record)
	return Config{Locator: loc, Authorization: a}, record
}
func saveApprovedFixture(t *testing.T, a *Authorization, r qualification.Approval) {
	t.Helper()
	r.SHA256 = qualification.Digest(r.Body)
	b, _ := json.Marshal(r)
	if e := os.WriteFile(a.path(), b, 0600); e != nil {
		t.Fatal(e)
	}
}
func TestProductionApprovalRefresh(t *testing.T) {
	cfg, record := approvedBackendFixture(t)
	cfg.Resolver = edgeContent(nil)
	b, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	b.RefreshAuthorization(context.Background())
	p := b.Policy()
	if len(p.Tuples) != 1 || p.Revision != "r1" {
		t.Fatalf("valid approved capability missing: %+v", p)
	}
	token, e := b.readPermission(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if e = b.checkPermission(context.Background(), token, nil); e != nil {
		t.Fatal(e)
	}
	// Replacing the record with identical bytes still invalidates an old token.
	original := cfg.Authorization.path()
	if e = os.Rename(original, original+".old"); e != nil {
		t.Fatal(e)
	}
	saveApprovedFixture(t, cfg.Authorization, record)
	if e = b.checkPermission(context.Background(), token, nil); e == nil {
		t.Fatal("replacement retained old permission")
	}
	if e = os.Remove(original); e != nil {
		t.Fatal(e)
	}
	b.RefreshAuthorization(context.Background())
	if len(b.Policy().Tuples) != 0 {
		t.Fatal("deleted record retained capability")
	}
	saveApprovedFixture(t, cfg.Authorization, record)
	b.RefreshAuthorization(context.Background())
	if len(b.Policy().Tuples) != 1 || b.Stats().Sessions != 0 {
		t.Fatal("recovery failed or autostarted")
	}
}
func TestProductionApprovalHealthSameInventory(t *testing.T) {
	cfg, _ := approvedBackendFixture(t)
	cfg.Resolver = edgeContent(nil)
	healthy := true
	cfg.Authorization.Healthy = func() bool { return healthy }
	b, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	b.RefreshAuthorization(context.Background())
	generation := b.Policy().Generation
	healthy = false
	b.RefreshAuthorization(context.Background())
	p := b.Policy()
	if len(p.Tuples) != 0 || p.Generation <= generation {
		t.Fatal("stale health ignored")
	}
	healthy = true
	b.RefreshAuthorization(context.Background())
	if len(b.Policy().Tuples) != 1 || b.Stats().Sessions != 0 {
		t.Fatal("health recovery started playback")
	}
}

type listControls []alsactl.Control

func (v listControls) Controls() ([]alsactl.Control, error) {
	return append([]alsactl.Control(nil), v...), nil
}
func (v listControls) Close() error { return nil }
func TestProductionApprovalSemanticMixer(t *testing.T) {
	cfg, record := approvedBackendFixture(t)
	cfg.Resolver = edgeContent(nil)
	volume := alsactl.Control{Interface: 2, Name: "Volume", Type: 2, Count: 1, Access: 1, Values: []byte{1, 0, 0, 0, 0, 0, 0, 0}}
	record.Body.Policies[0].Constraints = []alsactl.Constraint{{Interface: 2, Name: "Volume", Value: volume.Values}}
	for _, evidence := range []*qualification.Evidence{&record.Body.Policies[0].Matrix, &record.Body.Policies[0].Lifecycle} {
		name := filepath.Join(cfg.Authorization.StateDir, evidence.Path)
		data, e := os.ReadFile(name)
		if e != nil {
			t.Fatal(e)
		}
		var r map[string]any
		if e = json.Unmarshal(data, &r); e != nil {
			t.Fatal(e)
		}
		r["Before"] = alsactl.Snapshot{Card: 9, Controls: []alsactl.Control{volume}}
		r["After"] = r["Before"]
		data, _ = json.Marshal(r)
		if e = os.WriteFile(name, data, 0600); e != nil {
			t.Fatal(e)
		}
		evidence.SHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	saveApprovedFixture(t, cfg.Authorization, record)
	current := listControls{volume, {Interface: 2, Name: "unrelated", ReadError: "unreadable"}}
	cfg.Authorization.Controls = alsactl.Reader{Open: func(int) (alsactl.Device, error) { return current, nil }}
	b, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"unrelated_unreadable", "unrelated_changed", "disappeared", "constrained_unreadable"} {
		t.Run(mode, func(t *testing.T) {
			bad := false
			switch mode {
			case "unrelated_changed":
				current[1].ReadError = ""
				current[1].Values = []byte{9}
			case "disappeared":
				current = listControls{current[1]}
				bad = true
			case "constrained_unreadable":
				v := volume
				v.ReadError = "failed"
				current = listControls{v}
				bad = true
			}
			_, e := b.readPermission(context.Background())
			if (e != nil) != bad {
				t.Fatalf("semantic authorization=%v", e)
			}
		})
	}
}
