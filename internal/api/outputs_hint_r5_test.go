package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/mpd/conf"
	"github.com/acoseac/1-bit-bridge/internal/signalpath"
)

func TestStalledHintCannotSpendSongLookupTime(t *testing.T) {
	read := func(stall bool) OutputReading {
		r, _, _ := provenanceRig(t)
		track := r.cfg.Track
		r.cfg.Track = func(ctx context.Context, path string) (OutputTrack, bool, error) {
			select {
			case <-time.After(120 * time.Millisecond):
				return track(ctx, path)
			case <-ctx.Done():
				return OutputTrack{}, false, ctx.Err()
			}
		}
		return provenanceReading(t, r, func(o *localOutputs) {
			o.limits.build = 600 * time.Millisecond
			o.limits.file = 400 * time.Millisecond
			o.limits.hint = time.Second
			release := make(chan struct{})
			done := make(chan struct{})
			o.cmdline = func(string, string, int64) ([]byte, error) {
				defer close(done)
				if stall {
					<-release
				}
				return nil, errors.New("no hint")
			}
			t.Cleanup(func() {
				close(release)
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("hint did not finish")
				}
			})
		})
	}
	baseline := read(false)
	got := read(true)
	if baseline.Song == nil || baseline.Song.Row == nil || baseline.Verdict.Value != signalpath.FormatPreserved {
		t.Fatalf("baseline is not a known preserved source: %+v", baseline)
	}
	if got.Song == nil || !reflect.DeepEqual(got.Song.Row, baseline.Song.Row) || !reflect.DeepEqual(got.Verdict, baseline.Verdict) {
		t.Fatalf("hint changed song/verdict: song=%+v verdict=%+v; baseline song=%+v verdict=%+v", got.Song, got.Verdict, baseline.Song, baseline.Verdict)
	}
	wantHint(t, got, "", "")
}

func TestReadingOmitsIntermediateProcessAliases(t *testing.T) {
	for _, alias := range []string{"proc/self/cwd", "proc/self/fd/7", "dev/fd/7", "dev/stdin", "dev/stdout", "dev/stderr"} {
		t.Run(alias, func(t *testing.T) {
			r, root, pid := provenanceRig(t)
			// Every chain ends at a real, ordinary file outside the special tree.
			// The old operand/end-only check therefore publishes a different hint.
			writeRigFile(t, root, "srv/bridge/mpd.conf", "volume_normalization \"yes\"\n")
			target := filepath.Join(root, "srv/bridge/mpd.conf")
			suffix := ""
			if alias == "proc/self/cwd" {
				target = filepath.Dir(target)
				suffix = "/mpd.conf"
			}
			link := filepath.Join(root, filepath.FromSlash(alias))
			if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skip(err)
			}
			operand := "/etc/alias.conf"
			writeRigFile(t, root, "etc/placeholder", "")
			if err := os.Symlink("../"+alias+suffix, filepath.Join(root, "etc/alias.conf")); err != nil {
				t.Fatal(err)
			}
			// Prove the refusal cannot come from the ordinary confined stat.
			if _, info, err := conf.StatHost(root, filepath.Join(root, "etc/alias.conf")); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("alias fixture: %v", err)
			}
			writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00"+operand+"\x00")
			wantConfigAndHint(t, provenanceReading(t, r), "", "", "no")
		})
	}
}

func TestReadingHintLinkHopLimit(t *testing.T) {
	r, root, pid := provenanceRig(t)
	writeRigFile(t, root, "etc/end.conf", "volume_normalization \"yes\"\n")
	// EvalSymlinks permits this chain, but hints deliberately stop at 40 links.
	for i := 40; i >= 0; i-- {
		next := "end.conf"
		if i < 40 {
			next = fmt.Sprintf("hop%d.conf", i+1)
		}
		if err := os.Symlink(next, filepath.Join(root, "etc", fmt.Sprintf("hop%d.conf", i))); err != nil {
			t.Skip(err)
		}
	}
	if _, _, err := conf.StatHost(root, filepath.Join(root, "etc/hop0.conf")); err != nil {
		t.Fatalf("ordinary resolution: %v", err)
	}
	writeRigFile(t, r.procRoot, strconv.Itoa(pid)+"/cmdline", "mpd\x00/etc/hop0.conf\x00")
	wantConfigAndHint(t, provenanceReading(t, r), "", "", "no")
}
