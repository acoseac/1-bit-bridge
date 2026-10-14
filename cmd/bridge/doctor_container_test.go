package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// TestDoctorAndInitCarryTheContainerVerdict — backlog B190: both reports that
// grade this host take the container verdict from one seam, so `bridge
// doctor` (and the console, which builds its report the same way) and init's
// preflight cannot answer the same container differently. In a container the
// doctor's real report reads the two host-shell checks as skipped.
func TestDoctorAndInitCarryTheContainerVerdict(t *testing.T) {
	_, platform := isolateConfigEnv(t)
	prev := inContainerFunc
	t.Cleanup(func() { inContainerFunc = prev })

	for _, want := range []bool{true, false} {
		inContainerFunc = func() bool { return want }
		d := buildDoctorDepsFor(filepath.Join(platform, "bridge.yaml"), true)
		if d.InContainer != want {
			t.Errorf("doctor's report: InContainer = %v, want %v", d.InContainer, want)
		}
		if got := initPreflightDeps(t.TempDir(), t.TempDir(), nil, 7788, 7789).InContainer; got != want {
			t.Errorf("init's preflight: InContainer = %v, want %v", got, want)
		}
		if !want {
			continue
		}
		rep := doctor.Run(context.Background(), d)
		for _, name := range []string{"service-manager", "browser-opener"} {
			if c := findCheck(t, rep, name); c.Status != doctor.OK || !strings.Contains(c.Summary, "container") {
				t.Errorf("%s in a container's report: %q %q, want ok and skipped for the container", name, c.Status, c.Summary)
			}
		}
	}
}
