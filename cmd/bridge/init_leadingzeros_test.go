package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestLeadingZeroInitRecovery drives all three rewrite validation paths, then saves their result.
func TestLeadingZeroInitRecovery(t *testing.T) {
	for _, tc := range []struct{ name, top, other string }{
		{"alone", "", ""},
		{"unrelated decode error", "unknown: true\n", ""},
		{"malformed output neighbour", "", "  endpoint: [wrong, type]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			head := fmt.Sprintf("libraryRoots: [%q]\ndataDir: %q\n", dir, filepath.Join(dir, "data"))
			raw := []byte(head + tc.top + "output:\n  loopbackPort: 07795\n  mpd: off\n  password: kept-password\n" + tc.other)
			path := filepath.Join(dir, "bridge.yaml")
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			kept, notKept := priorOutput(raw)
			// Check this first: the negative control must identify the silent load.
			if kept.LoopbackPort != nil {
				t.Fatalf("leading-zero value loaded without error as %d; want it dropped with a warning", *kept.LoopbackPort)
			}
			line := strings.Count(head+tc.top+"output:\n", "\n") + 1
			want := fmt.Sprintf("output.loopbackPort (line %d): it does not read as that setting", line)
			if len(notKept) == 0 || notKept[0] != want {
				t.Fatalf("got %q; want first warning %q", notKept, want)
			}
			if kept.MPD != "off" || kept.Password != "kept-password" {
				t.Fatalf("lost valid neighbours: %+v", kept)
			}
			code, out := loopbackInit(t, dir, dir, "Rewritten", "--force")
			if code != 0 {
				t.Fatalf("rewrite exit %d: %s", code, out)
			}
			if !strings.Contains(outputNotKeptWarning(out), want) {
				t.Fatalf("rewrite warning missing: %s", out)
			}
			saved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(saved), "loopbackPort:") {
				t.Fatalf("saved the interpreted port:\n%s", saved)
			}
			loaded, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Output.LoopbackPort != nil || loaded.Output.MPD != "off" || loaded.Output.Password != "kept-password" {
				t.Fatalf("reload lost the recovered settings: %+v", loaded.Output)
			}
		})
	}
}
