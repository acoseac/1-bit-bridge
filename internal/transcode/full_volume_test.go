package transcode

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
)

// smallVolumeEnv names a directory on a small filesystem the tests may fill,
// for a host where they cannot make one themselves: on Linux, a tmpfs a root
// shell mounted and gave to the user the tests run as, e.g.
//
//	mount -t tmpfs -o size=16m tmpfs /mnt/small && chown 1000 /mnt/small
//	BRIDGE_TEST_SMALL_VOLUME=/mnt/small go test ./internal/transcode/ \
//	  -run TestARenditionThatCouldNotBeWrittenWholeIsNotPublished
//
// (gate.yml's dsd-measure job does this). On macOS the tests make their
// own, an HFS+ disk image (hdiutil).
const smallVolumeEnv = "BRIDGE_TEST_SMALL_VOLUME"

// smallVolume returns a fresh directory on a filesystem of about sizeMB
// megabytes that the test may fill, or skips: an HFS+ image the test attaches
// on macOS (and detaches when it ends), the directory smallVolumeEnv names
// elsewhere.
func smallVolume(t *testing.T, sizeMB int) string {
	t.Helper()
	if dir := os.Getenv(smallVolumeEnv); dir != "" {
		d, err := os.MkdirTemp(dir, "b264-")
		if err != nil {
			t.Fatalf("%s=%s: %v", smallVolumeEnv, dir, err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		return d
	}
	if runtime.GOOS != "darwin" {
		t.Skipf("no small volume to fill: set %s to a directory on a small filesystem (a tmpfs)", smallVolumeEnv)
	}
	hdiutil, err := exec.LookPath("hdiutil")
	if err != nil {
		t.Skip("no hdiutil to make a small volume")
	}
	root := t.TempDir()
	img, mnt := filepath.Join(root, "small.dmg"), filepath.Join(root, "mnt")
	if out, err := exec.Command(hdiutil, "create", "-quiet", "-size", strconv.Itoa(sizeMB)+"m", "-fs", "HFS+",
		"-layout", "NONE", "-volname", "b264", img).CombinedOutput(); err != nil {
		t.Skipf("hdiutil create: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(hdiutil, "attach", "-quiet", "-nobrowse", "-noautoopen", "-mountpoint", mnt, img).CombinedOutput(); err != nil {
		t.Skipf("hdiutil attach: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	t.Cleanup(func() {
		if out, err := exec.Command(hdiutil, "detach", "-force", mnt).CombinedOutput(); err != nil {
			t.Errorf("hdiutil detach %s: %v (%s)", mnt, err, strings.TrimSpace(string(out)))
		}
	})
	d := filepath.Join(mnt, "work")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// fillVolume writes a filler file into dir until the volume holding it has
// about leave bytes free.
func fillVolume(t *testing.T, dir string, leave int64) {
	t.Helper()
	free, err := AvailableDiskSpaceNearest(dir)
	if err != nil {
		t.Fatalf("free space of %s: %v", dir, err)
	}
	f, err := os.Create(filepath.Join(dir, "filler"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	chunk := make([]byte, 64<<10)
	for want := free - leave; want > 0; {
		n := min(int64(len(chunk)), want)
		if _, err := f.Write(chunk[:n]); err != nil {
			if errors.Is(err, syscall.ENOSPC) {
				break
			}
			t.Fatalf("fill %s: %v", dir, err)
		}
		want -= n
	}
	if err := f.Sync(); err != nil && !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("fill %s: %v", dir, err)
	}
}

// realSource makes a source with the real sox (or, for an .m4a, sox and then
// ffmpeg's ALAC encoder): seconds of noise at 44.1 kHz / 16 bit stereo, so
// it compresses as little as music does. A DSD source is a tone.
func realSource(t *testing.T, dir, name string, seconds float64) string {
	t.Helper()
	p := filepath.Join(dir, name)
	switch filepath.Ext(name) {
	case ".dsf":
		if _, err := dsdtone.MintDSF(p, dsdtone.Tone{RateHz: 2822400, Seconds: seconds, AmplitudeDBFS: -20}); err != nil {
			t.Fatal(err)
		}
	case ".m4a":
		wav := filepath.Join(dir, "alac-source.wav")
		runTool(t, "sox", "-R", "-n", "-r", "44100", "-b", "16", "-c", "2", wav, "synth", secondsArg(seconds), "whitenoise", "vol", "0.5")
		runTool(t, "ffmpeg", "-v", "error", "-y", "-i", wav, "-c:a", "alac", p)
	default:
		runTool(t, "sox", "-R", "-n", "-r", "44100", "-b", "16", "-c", "2", p, "synth", secondsArg(seconds), "whitenoise", "vol", "0.5")
	}
	return p
}

func secondsArg(s float64) string { return strconv.FormatFloat(s, 'f', -1, 64) }

// runTool runs a real tool, named by the absolute path PATH gives it.
func runTool(t *testing.T, name string, args ...string) {
	t.Helper()
	bin, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v (%s)", name, args, err, strings.TrimSpace(string(out)))
	}
}

// fullVolumeCase is one row of
// TestARenditionThatCouldNotBeWrittenWholeIsNotPublished: a source rendered
// onto a volume with no room for one of the files its render writes.
type fullVolumeCase struct {
	name, src string
	seconds   float64
	// fullScratch puts the render scratch, not the variants directory, on
	// the full volume.
	fullScratch bool
	need        func(*testing.T)
	spec        func(src, outDir, tempDir string) JobSpec
	where       string
}

// pcmUpscaleSpec is the job a 44.1 kHz / 16-bit row at rel asks for: 192 kHz
// / 24 bit.
func pcmUpscaleSpec(rel string) func(src, outDir, tempDir string) JobSpec {
	return func(src, outDir, tempDir string) JobSpec {
		return JobSpec{SourceAbsPath: src, SourceLibraryRel: rel, SourceSampleRate: 44100, SourceBits: 16,
			TargetSampleRate: 192000, TargetBits: 24, Quality: QualityVeryHigh, OutputDir: outDir, TempDir: tempDir}
	}
}

// dsdFaithfulSpec is the faithful tier's job for a 3 s DSD64 stereo source.
func dsdFaithfulSpec(src, outDir, tempDir string) JobSpec {
	return JobSpec{SourceAbsPath: src, SourceLibraryRel: "Album/01.dsf", SourceSampleRate: 2822400,
		SourceIsDSD: true, SourceChannels: 2, SourceDurationSec: 3, TargetSampleRate: 176400, TargetBits: 24,
		Quality: QualityVeryHigh, OutputDir: outDir, TempDir: tempDir, Kind: JobKindPCMRender}
}

// TestARenditionThatCouldNotBeWrittenWholeIsNotPublished renders through the
// real tools and the real pool onto a volume with no room for the rendition:
// sox 14.4.2 prints "error writing output file: No space left on device"
// and EXITS 0, leaving the file it began. Nothing may be published, and the
// source must take no strike: a full volume is a fact about the host.
//
// Each route's output is the one on the full volume: the sidecar for the
// sox-direct, ALAC and DSD renders (Stage C writes it), the Stage A scratch
// for the last case.
func TestARenditionThatCouldNotBeWrittenWholeIsNotPublished(t *testing.T) {
	requireSox(t)
	for _, tc := range []fullVolumeCase{
		{name: "sox-direct", src: "01.flac", seconds: 8, need: requireSox, where: outputVariants,
			spec: pcmUpscaleSpec("Album/01.flac")},
		{name: "ALAC pipe", src: "01.m4a", seconds: 8, need: requireSoxAndFFmpeg, where: outputVariants,
			spec: pcmUpscaleSpec("Album/01.m4a")},
		{name: "DSD stage C", src: "01.dsf", seconds: 3, need: requireDSDToolchain, where: outputVariants,
			spec: dsdFaithfulSpec},
		{name: "DSD stage A", src: "01.dsf", seconds: 3, fullScratch: true, need: requireDSDToolchain, where: outputScratch,
			spec: dsdFaithfulSpec},
	} {
		t.Run(tc.name, tc.run)
	}
}

func (tc fullVolumeCase) run(t *testing.T) {
	tc.need(t)
	full := smallVolume(t, 16)
	roomy := t.TempDir()
	src := realSource(t, t.TempDir(), tc.src, tc.seconds)
	outDir, tempDir := filepath.Join(full, "variants"), filepath.Join(roomy, "tmp")
	if tc.fullScratch {
		outDir, tempDir = filepath.Join(roomy, "variants"), filepath.Join(full, "tmp")
	}
	spec := stampedAsScanned(tc.spec(src, outDir, tempDir))
	fillVolume(t, full, 256<<10)

	resetFFmpegSnapshotForTest()
	t.Cleanup(resetFFmpegSnapshotForTest)
	a := newAnnouncingPool(t, spec.SourceLibraryRel)
	tc.check(t, a, spec, a.run(t, spec, 1))
}

// check asserts that the render published nothing and left no temp sidecar,
// that its job failed without striking the source, and that the outage was
// reported once, naming the output and the operating system's reason: the
// volume still has no room when the bridge asks (the file the tool left
// holds it).
func (tc fullVolumeCase) check(t *testing.T, a *announcingPool, spec JobSpec, got []string) {
	t.Helper()
	if _, err := os.Stat(spec.SidecarPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a rendition was published at its sidecar path (stat err %v): the tool could not write it whole", err)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "failed: ") {
		t.Fatalf("job announced %q, want a failure", got)
	}
	t.Logf("announced: %s", got[0])
	if suppressed, records := a.strikes(t, spec.SourceLibraryRel); records != 0 {
		t.Errorf("%d strike record(s) (%d suppressed), want none: a volume with no room is a fact about the host", records, suppressed)
	}
	warns := a.lines("WARN", logOutputUnavailable)
	if len(warns) != 1 || !strings.Contains(warns[0], "output="+tc.where) ||
		!strings.Contains(warns[0], "reason="+syscall.ENOSPC.Error()) {
		t.Errorf("%q warnings:\n%s\nwant one naming output=%s and the reason %q",
			logOutputUnavailable, strings.Join(warns, "\n"), tc.where, syscall.ENOSPC.Error())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(spec.SidecarPath()), "*"+sidecarTmpSuffix)); len(leftovers) != 0 {
		t.Errorf("temp sidecars left behind: %v", leftovers)
	}
}
