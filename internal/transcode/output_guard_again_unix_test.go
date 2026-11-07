//go:build !windows

package transcode

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/flactest"
)

// parkingGuard reads a variants directory as the mount-loss probe's missing
// case does (not there: away), and parks its first answer until released:
// the window between the guard asked at Run's start and the guard asked just
// before the sidecar's directory is made, where the volume can go (the
// decoder probes, the scratch, and on DSD the geometry run in it).
type parkingGuard struct {
	asked   atomic.Int32
	parked  chan struct{}
	release chan struct{}
}

func newParkingGuard() *parkingGuard {
	return &parkingGuard{parked: make(chan struct{}), release: make(chan struct{})}
}

func (g *parkingGuard) CheckOutput(_ context.Context, dir string) error {
	var err error
	if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
		err = &VariantsDirAwayError{Found: "is not there"}
	}
	if g.asked.Add(1) == 1 {
		close(g.parked)
		<-g.release
	}
	return err
}

// TestTheGuardIsAskedAgainJustBeforeTheSidecarDirectoryIsMade: the variants
// directory is on a volume mounted at mnt, there when Run asks its guard at
// the start, and taken away while Run is parked after that answer, the bare
// mount point left. Run asks the guard again just before it makes the
// sidecar's directory, on the PCM path and on the DSD chain's, so the render
// is refused as the output's fault and nothing is made under mnt. Asked once,
// at the start alone, the render made the directory under the bare mount
// point and published there.
func TestTheGuardIsAskedAgainJustBeforeTheSidecarDirectoryIsMade(t *testing.T) {
	const scratch = 176400
	for _, tc := range []struct {
		name  string
		tools map[string]string
		spec  func(t *testing.T) JobSpec
	}{
		{"pcm", map[string]string{"sox": standInSoxWrites},
			func(t *testing.T) JobSpec { return sourceFile(t, "Album/01.flac") }},
		{"dsd", map[string]string{
			"sox":     standInSoxForStageC(scratch, flactest.Stream(176400, 2, 24, scratch, 0)),
			"ffmpeg":  standInFFmpeg,
			"ffprobe": standInFFprobe,
		}, func(t *testing.T) JobSpec { return dsdSource(t, "Album/01.dsf") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", standInTools(t, tc.tools))
			resetFFmpegSnapshotForTest()
			t.Cleanup(resetFFmpegSnapshotForTest)
			spec := tc.spec(t)
			mnt := filepath.Join(t.TempDir(), "usb")
			spec.OutputDir = filepath.Join(mnt, "variants")
			if err := os.MkdirAll(spec.OutputDir, 0o755); err != nil {
				t.Fatal(err)
			}
			g := newParkingGuard()
			spec.OutputGuard = g
			ran := make(chan error, 1)
			go func() {
				_, err := Run(context.Background(), spec)
				ran <- err
			}()
			select {
			case <-g.parked:
			case err := <-ran:
				t.Fatalf("Run returned %v before it asked its guard", err)
			}
			// The volume goes: its tree away, the bare mount point left.
			if err := os.Rename(mnt, mnt+".volume"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(mnt, 0o755); err != nil {
				t.Fatal(err)
			}
			close(g.release)
			err := <-ran

			var away *VariantsDirAwayError
			if !errors.As(err, &away) {
				t.Errorf("Run = %v, want the guard's refusal", err)
			}
			if f, marked := unwritableOutput(err); !marked || f.kind != outputAway {
				t.Errorf("Run = %v: the output side's fault = %+v, %v; want the variants directory away", err, f, marked)
			}
			if n := g.asked.Load(); n != 2 {
				t.Errorf("the guard was asked %d time(s), want twice: at the start and before the sidecar's directory", n)
			}
			entries, err := os.ReadDir(mnt)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				var names []string
				_ = filepath.WalkDir(mnt, func(p string, _ fs.DirEntry, _ error) error {
					if r, err := filepath.Rel(mnt, p); err == nil && r != "." {
						names = append(names, filepath.ToSlash(r))
					}
					return nil
				})
				t.Errorf("the render made %v under the mount point of the volume that went", names)
			}
		})
	}
}
