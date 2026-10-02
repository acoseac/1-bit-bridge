//go:build !windows

package transcode

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/flactest"
)

// standInSoxForStageC is a stand-in sox for a whole DSD render: it answers
// `--i -s` with scratchSamples (sox's count of what Stage A wrote), writes a
// scratch for Stage A, and writes stageC, a FLAC, as Stage C's rendition.
func standInSoxForStageC(scratchSamples uint64, stageC []byte) string {
	return standInSoxHelp + `if [ "$1" = "--i" ]; then echo ` + strconv.FormatUint(scratchSamples, 10) + `; exit 0; fi
for a in "$@"; do
  case "$a" in
    *` + renderScratchSuffix + `) printf 'x' > "$a" || exit 2 ;;
    *` + sidecarTmpSuffix + `) ` + shWrites("$a", stageC) + ` || exit 2 ;;
  esac
done
`
}

// TestADSDRenditionHoldsWhatItsScratchHeld: Stage C's gain and dither keep
// the scratch's length to the sample, so a whole rendition that holds
// another length is not the render of that scratch and is not published.
// The positive control holds exactly the scratch's samples. (The stand-in
// ffprobe reports a 1 s DSD64 source; 176,400 samples at the faithful
// tier's rate is that second, which Stage A's own guard takes.)
func TestADSDRenditionHoldsWhatItsScratchHeld(t *testing.T) {
	const scratch = 176400
	for _, tc := range []struct {
		name   string
		stageC []byte
		ok     bool
	}{
		{name: "the scratch's length", stageC: flactest.Stream(176400, 2, 24, scratch, 0), ok: true},
		{name: "a block short", stageC: flactest.Stream(176400, 2, 24, scratch-flactest.Block, 0)},
		{name: "a sample long", stageC: flactest.Stream(176400, 2, 24, scratch+1, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", standInTools(t, map[string]string{
				"sox": standInSoxForStageC(scratch, tc.stageC), "ffmpeg": standInFFmpeg, "ffprobe": standInFFprobe,
			}))
			resetFFmpegSnapshotForTest()
			t.Cleanup(resetFFmpegSnapshotForTest)
			spec := dsdSource(t, "Album/01.dsf")
			_, err := Run(context.Background(), spec)
			_, statErr := os.Stat(spec.SidecarPath())
			if tc.ok {
				if err != nil || statErr != nil {
					t.Fatalf("Run = %v, stat %v; want the rendition published", err, statErr)
				}
				return
			}
			if !errors.Is(err, ErrRenditionIncomplete) || !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("Run = %v, stat %v; want ErrRenditionIncomplete and nothing published", err, statErr)
			}
			if _, marked := unwritableOutput(err); marked {
				t.Errorf("Run = %v: marked the output side's; a whole rendition of the wrong length is the run's", err)
			}
		})
	}
}
