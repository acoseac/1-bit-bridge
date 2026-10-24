//go:build linux

package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/mpd/conf"
)

func TestHintOmitsLiveProcessAliasesAtHostRoot(t *testing.T) {
	for _, kind := range []string{"proc_fd", "dev_fd", "cwd"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			f, err := os.Create(filepath.Join(dir, "mpd.conf"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.Close() })
			target := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
			if kind == "dev_fd" {
				target = fmt.Sprintf("/dev/fd/%d", f.Fd())
			}
			if kind == "cwd" {
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				rel, err := filepath.Rel(cwd, f.Name())
				if err != nil {
					t.Fatal(err)
				}
				// Preserve the cwd/.. sequence for actual kernel resolution.
				target = "/proc/self/cwd/" + rel
			}
			alias := filepath.Join(dir, "alias.conf")
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			_, info, err := conf.StatHost("/", alias)
			if err != nil {
				t.Fatal(err)
			}
			original, err := f.Stat()
			if err != nil || !os.SameFile(info, original) {
				t.Fatalf("fixture did not reach held file: %v", err)
			}
			r, _, _ := provenanceRig(t)
			o := newLocalOutputs(r.cfg)
			t.Cleanup(o.close)
			s := o.takeReading(context.Background())
			s.cards.ProcRoot = "/proc"
			// Real /proc for this test process supplies stable start and root identity.
			// Captured ALSA ownership still drives the production hint admission path.
			for ci := range s.cards.Cards {
				for di := range s.cards.Cards[ci].Devices {
					for si := range s.cards.Cards[ci].Devices[di].Substreams {
						owner := s.cards.Cards[ci].Devices[di].Substreams[si].Owner
						if owner != nil && owner.PID != nil {
							*owner.PID = os.Getpid()
						}
					}
				}
			}
			o.cmdline = func(string, string, int64) ([]byte, error) { return []byte("mpd\x00" + alias + "\x00"), nil }
			// An ordinary operand proves all other admission evidence works at root /.
			if got := o.processConfigHint(s, &s.picks[0], "/", r.confPath); got != nil {
				t.Fatalf("process-relative %s alias emitted %+v", kind, got)
			}
			o.cmdline = func(string, string, int64) ([]byte, error) { return []byte("mpd\x00" + f.Name() + "\x00"), nil }
			if got := o.processConfigHint(s, &s.picks[0], "/", r.confPath); got == nil || got.Argument != "different" {
				t.Fatalf("ordinary operand lost hint: %+v", got)
			}
		})
	}
}
