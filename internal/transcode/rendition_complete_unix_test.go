//go:build !windows

package transcode

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
)

// TestACutStreamIsClassifiedByWhatTheVolumeAnswers pins classifyCut on the
// errno causes a probe write can meet: those of the host's table are the
// output side's, by their kind; EFBIG (a file larger than the volume can
// hold) is the file's own size and keeps the job's strike, as B211 decided.
func TestACutStreamIsClassifiedByWhatTheVolumeAnswers(t *testing.T) {
	cut := errors.New("cut short")
	for _, tc := range []struct {
		errno syscall.Errno
		kind  outputFaultKind
	}{
		{syscall.ENOSPC, outputFull},
		{syscall.EDQUOT, outputQuota},
		{syscall.EROFS, outputReadOnly},
		{syscall.EIO, outputGone},
		{syscall.ENOTCONN, outputGone},
		{syscall.ESTALE, outputGone},
		{syscall.EFBIG, 0},
		{syscall.EINVAL, 0},
	} {
		probe := &os.PathError{Op: "write", Path: "x.tmp", Err: tc.errno}
		err := classifyCut(outputVariants, "/variants/Album", cut, probe)
		f, marked := unwritableOutput(err)
		if marked != (tc.kind != 0) || (marked && (f.kind != tc.kind || f.reason != tc.errno.Error())) {
			t.Errorf("%v: marked %v as %+v, want kind %v", tc.errno, marked, f, tc.kind)
		}
		if !errors.Is(err, cut) || !errors.Is(err, tc.errno) {
			t.Errorf("%v: %v does not wrap the cut and the volume's answer", tc.errno, err)
		}
	}
}

// standInSoxShortScratch is a stand-in sox for a DSD render whose Stage A
// leaves a scratch shorter than the source (it answers `--i -s` with a
// thousand samples of the second the stand-in ffprobe reports) and, when
// lock is set, makes that scratch refuse more bytes: the shape a full
// scratch volume leaves after sox's write failed and ffmpeg had finished.
func standInSoxShortScratch(lock bool) string {
	chmod := ""
	if lock {
		chmod = `; /bin/chmod 444 "$a"`
	}
	return standInSoxHelp + `if [ "$1" = "--i" ]; then echo 1000; exit 0; fi
for a in "$@"; do
  case "$a" in
    *` + renderScratchSuffix + `) printf 'x' > "$a" 2>/dev/null` + chmod + ` ;;
  esac
done
exit 0
`
}

// TestAStageAThatLeftAShortScratchAsksTheScratchVolume: a scratch shorter
// than its source is the scratch volume's fault when that volume refuses
// more bytes (here a scratch this user may no longer write, which the probe
// meets as a permission), and the source's otherwise, which strikes as the
// completeness guard always has.
func TestAStageAThatLeftAShortScratchAsksTheScratchVolume(t *testing.T) {
	skipAsRoot(t)
	for _, lock := range []bool{true, false} {
		t.Run(map[bool]string{true: "the volume refuses", false: "the volume takes more"}[lock], func(t *testing.T) {
			t.Setenv("PATH", standInTools(t, map[string]string{
				"sox": standInSoxShortScratch(lock), "ffmpeg": standInFFmpeg, "ffprobe": standInFFprobe,
			}))
			resetFFmpegSnapshotForTest()
			t.Cleanup(resetFFmpegSnapshotForTest)
			_, err := Run(context.Background(), dsdSource(t, "Album/01.dsf"))
			if !errors.Is(err, ErrFFmpegDecodeIncomplete) {
				t.Fatalf("Run = %v, want ErrFFmpegDecodeIncomplete", err)
			}
			f, marked := unwritableOutput(err)
			if marked != lock || (lock && (f.where != outputScratch || f.kind != outputDenied)) {
				t.Errorf("Run = %v: marked %v (%+v), want %v, the scratch's", err, marked, f, lock)
			}
		})
	}
}
