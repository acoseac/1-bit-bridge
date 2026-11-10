package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVacuumModeRefusesADirectoryJunction(t *testing.T) {
	target := t.TempDir()
	junction := filepath.Join(t.TempDir(), "output")
	cmd, err := exec.LookPath("cmd.exe")
	if err != nil {
		t.Skipf("junction creation unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, cmd, "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("create directory junction: %v: %s", err, out)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := chmodVacuumOutput(junction); err == nil || !strings.Contains(err.Error(), "vacuum output is not a regular file") {
		t.Errorf("directory junction refusal = %v, want not a regular file", err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() {
		t.Errorf("changed junction target mode: %v -> %v", before.Mode(), after.Mode())
	}
}
