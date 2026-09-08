package config

import (
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestManagedControlsRoundTripThroughYAML — the list is written by a
// control plane into bridge.yaml, so the key name IS the contract with
// another program. A rename here is a silent un-managing of every field
// on every deployed tenant.
func TestManagedControlsRoundTripThroughYAML(t *testing.T) {
	const src = `
deployment:
  mode: public
  managedControls:
    - restart
    - updates
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Deployment.ManagedControls; len(got) != 2 {
		t.Fatalf("managedControls = %v, want 2 entries — is the yaml key still `managedControls`?", got)
	}
	if !cfg.Deployment.IsManagedControl(ManagedControlRestart) {
		t.Error("restart did not read back as managed")
	}
	if cfg.Deployment.IsManagedControl(ManagedControlRoots) {
		t.Error("roots read as managed without being listed")
	}
	if !cfg.Deployment.IsManaged() {
		t.Error("IsManaged false with two controls listed")
	}
}

// TestUnknownManagedControlsAreNamed pins the deliberate choice: an
// unrecognised name manages NOTHING, and the only thing standing between
// that and silence is this list. See the docblock for why it is not an
// error — a rollback to an older binary must not take every tenant on a
// host offline.
func TestUnknownManagedControlsAreNamed(t *testing.T) {
	d := DeploymentConfig{ManagedControls: []string{"restart", "restarts", "Updates"}}

	unknown := d.UnknownManagedControls()
	if !slices.Contains(unknown, "restarts") {
		t.Errorf("unknown = %v, want the typo `restarts` named", unknown)
	}
	// Exact comparison, same rule as ManagedSettings: a fold-insensitive
	// match would let a control plane's casing decide what is locked.
	if !slices.Contains(unknown, "Updates") {
		t.Errorf("unknown = %v, want `Updates` named — comparison must stay exact", unknown)
	}
	if slices.Contains(unknown, "restart") {
		t.Errorf("unknown = %v names a control this build does know", unknown)
	}
	// The typo really is permissive — that is the risk being accepted.
	if d.IsManagedControl("restart") && d.IsManagedControl("updates") {
		t.Error("`restarts` somehow managed `updates` — comparison is not exact")
	}

	if len(DeploymentConfig{}.UnknownManagedControls()) != 0 {
		t.Error("an empty list reported unknown names")
	}
}

// TestEveryKnownManagedControlIsSpelledOnce — the names are a two-repo
// contract (the control plane writes them, this binary reads them), so a
// duplicate or an empty entry in the table would make one of them
// unreachable in a way nothing else notices.
func TestEveryKnownManagedControlIsSpelledOnce(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range KnownManagedControls() {
		if strings.TrimSpace(n) == "" {
			t.Error("empty control name in the known set")
		}
		if seen[n] {
			t.Errorf("control %q listed twice", n)
		}
		seen[n] = true
		if (DeploymentConfig{ManagedControls: []string{n}}).UnknownManagedControls() != nil {
			t.Errorf("control %q is in the known set but reads as unknown", n)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no known controls — the table is empty")
	}
}

// TestManagedControlsSurviveClone — Config is copy-on-write behind an
// atomic pointer, so a slice shared with the previous generation is a
// data race on a live security boundary. TestConfigCloneIsDeep sweeps
// this generically; this names the field, because the generic test
// reports "shared backing array" without saying what it protects.
func TestManagedControlsSurviveClone(t *testing.T) {
	rc := NewRuntimeConfig(&Config{
		Deployment: DeploymentConfig{ManagedControls: []string{ManagedControlRestart}},
	})
	clone := rc.Clone()
	clone.Deployment.ManagedControls[0] = ManagedControlRoots

	if got := rc.Load().Deployment.ManagedControls[0]; got != ManagedControlRestart {
		t.Fatalf("mutating the clone changed the live config: %q", got)
	}
}
