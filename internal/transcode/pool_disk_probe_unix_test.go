//go:build !windows

package transcode

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/google/uuid"
)

// scratchDeletingSox writes a whole 48 kHz 16-bit FLAC, then removes the
// directory --temp named. Run creates that scratch before sox and
// guardTempShort probes it afterwards, so the probe finds it gone.
var scratchDeletingSox = `#!/bin/sh
temp=""; out=""; prev=""; prev2=""
for a in "$@"; do
  if [ "$prev" = "--temp" ]; then temp="$a"; fi
  if [ "$prev2" = "-t" ] && [ "$prev" = "flac" ]; then out="$a"; fi
  prev2="$prev"; prev="$a"
done
[ -n "$out" ] || { echo "stand-in sox: no output argument" >&2; exit 1; }
` + shWrites("$out", stereoFLAC(48000, 16)) + `
rm -rf "$temp"
exit 0
`

// longDurationFFprobe reports a source far longer than one FLAC block, so
// renditionLengthDisagrees is set and guardTempShort probes the scratch.
// probeDuration asks for a bare float (ffprobe -of default=nw=1:nk=1).
const longDurationFFprobe = "#!/bin/sh\nprintf '100\\n'\n"

// TestAShortRenditionWhoseScratchIsGoneNamesNoAbsoluteDirectory drives the
// real pool and Run. The disk probe's missing-directory warning must say
// the directory is the scratch and that it is missing, and must not name
// the absolute path.
func TestAShortRenditionWhoseScratchIsGoneNamesNoAbsoluteDirectory(t *testing.T) {
	temp := t.TempDir()
	bin := standInTools(t, map[string]string{
		"sox":     scratchDeletingSox,
		"ffprobe": longDurationFFprobe,
	})
	// /bin stays on PATH so the stand-in can rm the scratch. sox and ffprobe
	// resolve to the stand-ins, which come first.
	t.Setenv("PATH", bin+":/bin:/usr/bin")
	rec := loggingtest.Record(t)

	store := openTempStoreForPool(t)
	const rel = "Album/01.flac"
	src := filepath.Join(t.TempDir(), "lib", "01.flac")
	writeSource(t, src, 4096)
	spec := stampedAsScanned(JobSpec{
		SourceAbsPath: src, SourceLibraryRel: rel,
		SourceSampleRate: 96000, SourceBits: 24,
		TargetSampleRate: 48000, TargetBits: 16,
		Kind: JobKindOptimize, Quality: QualityVeryHigh,
		OutputDir: t.TempDir(), TempDir: temp,
	})
	rate, bits := 96000.0, 24
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: rel, Size: spec.SourceSize, ModTime: time.Unix(0, spec.SourceMTimeNS),
		Codec: "FLAC", SampleRate: &rate, BitsPerSample: &bits,
	}); err != nil {
		t.Fatal(err)
	}

	p := NewPool(store, 1, 4)
	t.Cleanup(p.Stop)
	p.fsyncFn = noopFsync
	done := make(chan struct{}, 1)
	finish := func() {
		select {
		case done <- struct{}{}:
		default:
		}
	}
	p.SetOnJobFailed(func(string, string, string, float64, uuid.UUID, time.Time) { finish() })
	p.SetOnJobComplete(func(string, string, int, int, float64, uuid.UUID, time.Time) { finish() })
	if err := p.Enqueue(spec); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("the job never finished")
	}

	scratch := renderScratchDir(temp)
	const msg = "disk probe: directory missing; probing nearest existing ancestor"
	lines := rec.Lines(msg)
	if len(lines) == 0 {
		t.Fatalf("the warning never fired\n%s", strings.Join(rec.All(), "\n"))
	}
	named := false
	for _, line := range lines {
		if strings.Contains(line, scratch) || strings.Contains(line, temp) {
			t.Errorf("the warning names an absolute directory: %s", line)
		}
		if strings.Contains(line, "role=scratch") {
			named = true
		}
	}
	if !named {
		t.Errorf("the warning does not name the scratch: %s", strings.Join(lines, "\n"))
	}
}
