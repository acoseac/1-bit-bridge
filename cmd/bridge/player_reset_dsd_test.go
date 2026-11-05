package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
)

// TestResetPlayerKeepsThePlayersDSDState: bridge reset-player gives up the
// player's identity and its queue, and keeps its DSD state byte for byte, the
// --discard-floor reset too: a choice and what was heard belong to the DAC
// model, which the identity is not (B728 B3's decision), and the state's
// marker keeps the legacy keys from migrating again over what was chosen
// since, which a removed state would let them do.
func TestResetPlayerKeepsThePlayersDSDState(t *testing.T) {
	for _, args := range [][]string{{"--yes"}, {"--discard-floor", "--yes"}} {
		t.Run(args[0], func(t *testing.T) {
			cfg, dir := resetInstall(t, "127.0.0.1:0")
			state := filepath.Join(dir, dsdstate.FileName)
			if err := os.WriteFile(state, []byte(dsdStateBeforeTheBackup), 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errs bytes.Buffer
			if code := run(t.Context(), append([]string{"reset-player", "--config", cfg}, args...), &out, &errs); code != 0 {
				t.Fatalf("reset-player %v = %d: %s", args, code, &errs)
			}
			if got := readFileT(t, state); string(got) != dsdStateBeforeTheBackup {
				t.Fatalf("reset-player %v changed the DSD state:\n%s", args, got)
			}
		})
	}
}
