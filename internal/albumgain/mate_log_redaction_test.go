package albumgain

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestAMateThatCannotBeMeasuredIsLoggedLibraryRelative pins the one line the
// survey logs for an album-mate it gives up on. Its error is the mate's
// decode failing (MeasureDSDPeak: ffmpeg's and sox's stderr, the render
// scratch's mkdir), which names the mate's absolute path and the configured
// tempDir: families a failed job's own message is redacted of
// (redactSoxErr). The line takes the same redaction, by the mate's own spec,
// so it names the mate library-relative, as the privacy page promises.
func TestAMateThatCannotBeMeasuredIsLoggedLibraryRelative(t *testing.T) {
	var logs bytes.Buffer
	loggingtest.SetDefault(t, slog.New(slog.NewTextHandler(&logs, nil)))

	refs := album(3)
	h := newHarness(t, refs...)
	j := compactSpec(refs[0].Path, dsd64)
	j.TempDir = filepath.Join(t.TempDir(), "operator-temp")
	j.OutputDir = filepath.Join(t.TempDir(), "operator-variants")
	mate := refs[1].Path
	mateAbs := "/lib/" + mate // what the harness's SpecFor gives the mate
	h.meas.errs[mate] = fmt.Errorf("mkdir render scratch dir: mkdir %s: permission denied; %s: Invalid data found when processing input",
		j.TempDir, mateAbs)
	h.meas.peaks[refs[2].Path] = fp(-5.0)
	if _, _, err := h.r.AlbumGainDB(context.Background(), j, fp(-8.0)); err != nil {
		t.Fatalf("AlbumGainDB: %v", err)
	}

	var line string
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "could not be measured") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no line for the mate that could not be measured; the fixture no longer reaches it:\n%s", logs.String())
	}
	for _, abs := range []string{mateAbs, j.TempDir} {
		if strings.Contains(line, abs) {
			t.Errorf("the line carries the absolute path %q: %s", abs, line)
		}
	}
	if !strings.Contains(line, "Invalid data found") {
		t.Errorf("the line lost the decoder's reason: %s", line)
	}
}
