package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/flactest"
)

// The test binary stands in for sox and ffprobe, on every platform: a copy
// of it named for the tool is the whole of PATH, and TestMain runs it as
// that tool when standInToolEnv is set. The shell stand-ins elsewhere are
// POSIX-only; this one reaches the completeness check on Windows too.
//
// What the stand-ins do is scripted by files in the directory standInToolEnv
// names: sox writes standInRendition to its output argument (the one after
// `-t flac`) and exits 0, whatever it wrote, as sox 14.4.2 does after a
// write it could not make; ffprobe prints standInProbe, or fails when there
// is none.
const (
	standInToolEnv   = "TRANSCODE_TEST_STAND_IN"
	standInRendition = "rendition"
	standInProbe     = "ffprobe"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(standInToolEnv); dir != "" {
		tool := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
		os.Exit(runStandInTool(dir, tool, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// runStandInTool is the stand-in's whole behaviour; it returns the exit
// status.
func runStandInTool(dir, tool string, args []string) int {
	switch tool {
	case "sox":
		if len(args) > 0 && args[0] == "--help" {
			fmt.Print("sox: SoX v14.4.2\n\nAUDIO FILE FORMATS: flac wav\n\nEFFECTS: rate dither\n")
			return 0
		}
		for i := 2; i < len(args); i++ {
			if args[i-2] == "-t" && args[i-1] == "flac" {
				b, err := os.ReadFile(filepath.Join(dir, standInRendition))
				if err == nil {
					err = os.WriteFile(args[i], b, 0o644)
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, "stand-in sox:", err)
					return 2
				}
				return 0
			}
		}
		fmt.Fprintln(os.Stderr, "stand-in sox: no output argument")
		return 1
	case "ffprobe":
		b, err := os.ReadFile(filepath.Join(dir, standInProbe))
		if err != nil {
			return 1
		}
		_, _ = os.Stdout.Write(b)
		return 0
	}
	fmt.Fprintln(os.Stderr, "stand-in: no such tool", tool)
	return 1
}

// standInToolsScripted makes PATH a directory holding the test binary as sox
// and as ffprobe, and returns the directory that scripts them.
func standInToolsScripted(t *testing.T) (script string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, script := t.TempDir(), t.TempDir()
	windows := runtime.GOOS == "windows"
	suffix := ""
	if windows {
		suffix = ".exe"
	}
	for _, tool := range []string{"sox", "ffprobe"} {
		dst := filepath.Join(bin, tool+suffix)
		// A copy on Windows: a hard link to the running test binary cannot
		// be deleted while it runs, so TempDir's cleanup fails on it.
		if windows || os.Link(exe, dst) != nil {
			copyFileForTest(t, exe, dst)
		}
	}
	t.Setenv("PATH", bin)
	t.Setenv(standInToolEnv, script)
	resetFFmpegSnapshotForTest()
	t.Cleanup(resetFFmpegSnapshotForTest)
	return script
}

func copyFileForTest(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeScript writes one of the stand-ins' scripts.
func writeScript(t *testing.T, script, name string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(script, name), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// standInsWriting makes the stand-ins the whole of PATH, sox writing
// rendition and ffprobe printing probe ("" fails it).
func standInsWriting(t *testing.T, rendition []byte, probe string) {
	t.Helper()
	script := standInToolsScripted(t)
	writeScript(t, script, standInRendition, rendition)
	if probe != "" {
		writeScript(t, script, standInProbe, []byte(probe))
	}
}

// checkRunVerdict asserts what a Run left: want published at the sidecar
// path when want is not nil, and otherwise ErrRenditionIncomplete, nothing
// at the sidecar path or in a temp sidecar, and the failure marked as the
// output side's exactly when outputs.
func checkRunVerdict(t *testing.T, spec JobSpec, err error, want []byte, outputs bool) {
	t.Helper()
	got, statErr := os.ReadFile(spec.SidecarPath())
	if want != nil {
		if err != nil || statErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("Run = %v, published %d bytes (%v); want the rendition published", err, len(got), statErr)
		}
		return
	}
	if !errors.Is(err, ErrRenditionIncomplete) {
		t.Fatalf("Run = %v, want ErrRenditionIncomplete", err)
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("a rendition was published (%d bytes, err %v): Run returned %v", len(got), statErr, err)
	}
	if _, marked := unwritableOutput(err); marked != outputs {
		t.Errorf("Run = %v: the output side's fault = %v, want %v", err, marked, outputs)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(spec.SidecarPath()), "*"+sidecarTmpSuffix)); len(left) != 0 {
		t.Errorf("temp sidecars left behind: %v", left)
	}
}

// checkStruck asserts the jobs a pool ran all failed, that they struck the
// source (one record, suppressed) exactly when struck and left no record
// otherwise, and that an output outage was reported exactly when they did not.
func checkStruck(t *testing.T, a *announcingPool, rel string, announced []string, struck bool) {
	t.Helper()
	for i, g := range announced {
		if !strings.HasPrefix(g, "failed: ") {
			t.Errorf("job #%d announced %q, want a failure", i+1, g)
		}
	}
	want := 0
	if struck {
		want = 1
	}
	if suppressed, records := a.strikes(t, rel); suppressed != want || records != int64(want) {
		t.Errorf("%d suppressed, %d strike record(s); want struck = %v", suppressed, records, struck)
	}
	if warns := a.lines("WARN", logOutputUnavailable); (len(warns) == 1) == struck {
		t.Errorf("%q warnings:\n%s\nwant one exactly when the output side failed", logOutputUnavailable, strings.Join(warns, "\n"))
	}
}

// TestRunPublishesOnlyAWholeRendition drives Run with a sox that exits 0
// whatever it wrote, on every platform. Only the positive control may be
// published. A stream cut short (what a full volume left) is the output
// side's fault, which strikes nothing. A stream at another bit depth is the
// host's sox: Run still refuses it, and the pool strikes nothing (the pool
// test). The rest is the run's, which strikes: a stream the tool finished
// at a length the source disagrees with while the temp volume can hold the
// -G file, at another rate, or no FLAC at all. The same short stream is the
// temp volume's
// when the probed duration asks for a guard file larger than the free
// space.
func TestRunPublishesOnlyAWholeRendition(t *testing.T) {
	block := flactest.Stream(176400, 2, 24, flactest.Block, 0)
	// One block at 176.4 kHz is 4096/176400 s; ffprobe prints six decimals,
	// and the value alone (probeDuration asks it for no key).
	const blockSeconds = "0.023220\n"
	for _, tc := range []struct {
		name      string
		rendition []byte
		probe     string // what ffprobe says of the source; "" fails it
		published bool
		// outputs: the failure is the output side's (no strike).
		outputs bool
	}{
		{name: "whole, at the source's length", rendition: block, probe: blockSeconds, published: true},
		{name: "whole, ffprobe missing (no verdict on the length)", rendition: block, published: true},
		{name: "cut short", rendition: block[:len(block)-5], probe: blockSeconds, outputs: true},
		{name: "cut short, ffprobe missing", rendition: block[:len(block)-1], outputs: true},
		{name: "whole, shorter than the source", rendition: block, probe: "1.000000\n"},
		// 1e9 s at 176.4 kHz stereo is more than any temp volume holds, so
		// the same short file is the gain guard's and strikes nothing. The
		// 1 s row above is the control: that volume has room, and it strikes.
		{name: "whole, shorter, temp volume below the guard's need", rendition: block,
			probe: "1000000000.000000\n", outputs: true},
		{name: "whole, longer than the source", rendition: flactest.Stream(176400, 2, 24, 3*flactest.Block, 0),
			probe: blockSeconds},
		{name: "another rate", rendition: flactest.Stream(44100, 2, 24, flactest.Block, 0)},
		// Whole, the asked rate, the asked length, and 16-bit samples where
		// the job asked for 24. sox 14.4.2 does this for a 32-bit target:
		// it warns, exits 0, and writes a shallower stream (backlog B271).
		{name: "another bit depth", rendition: flactest.Stream(176400, 2, 16, flactest.Block, 0),
			probe: blockSeconds},
		{name: "not FLAC", rendition: []byte("fLaC")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			standInsWriting(t, tc.rendition, tc.probe)
			spec := sourceFile(t, "Album/01.flac") // sox-direct, to 176.4 kHz / 24 bit
			res, err := Run(context.Background(), spec)
			var want []byte
			if tc.published {
				want = tc.rendition
			}
			checkRunVerdict(t, spec, err, want, tc.outputs)
			if _, marked := depthMismatch(err); marked != (tc.name == "another bit depth") {
				t.Errorf("depth mismatch classified = %v", marked)
			}
			if !tc.published {
				return
			}
			stream, rerr := readFLACStream(spec.SidecarPath())
			if rerr != nil {
				t.Fatalf("read published stream: %v", rerr)
			}
			if res.BitsPerSample != stream.info.bitsPerSample {
				t.Fatalf("RunResult.BitsPerSample = %d, STREAMINFO = %d", res.BitsPerSample, stream.info.bitsPerSample)
			}
		})
	}
}

// TestACutRenditionStrikesNothingAndAShortOneStrikes is the pool's half, on
// every platform: three jobs whose stream was cut short leave the source
// unstruck and report one output outage; three whose stream was whole but
// short strike it while the temp volume can hold the guard file. The same
// short stream leaves the source unstruck when the probed duration asks
// for a guard file larger than the free space.
func TestACutRenditionStrikesNothingAndAShortOneStrikes(t *testing.T) {
	block := flactest.Stream(176400, 2, 24, flactest.Block, 0)
	for _, tc := range []struct {
		name      string
		rendition []byte
		probe     string
		struck    bool
	}{
		{name: "cut short", rendition: block[:len(block)-7]},
		{name: "whole and short", rendition: block, probe: "1.000000\n", struck: true},
		{name: "whole and short, temp volume below the guard's need", rendition: block,
			probe: "1000000000.000000\n", struck: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			standInsWriting(t, tc.rendition, tc.probe)
			spec := sourceFile(t, "Album/01.flac")
			a := newAnnouncingPool(t, spec.SourceLibraryRel)
			checkStruck(t, a, spec.SourceLibraryRel, a.run(t, spec, 3), tc.struck)
		})
	}
}

// TestARenditionOfAnotherBitDepthIsNotPublished is the pool's half of the
// mislabel. The stand-in sox exits 0 after writing a whole 16-bit stream at
// the rate and length a 32-bit job asked for, which is what sox 14.4.2 does
// after "flac can't encode to 32-bit". Three such jobs fail, strike nothing,
// and log the outage once. A later job whose target is the depth the writer
// can make publishes that file.
func TestARenditionOfAnotherBitDepthIsNotPublished(t *testing.T) {
	body := flactest.Stream(176400, 2, 16, flactest.Block, 0)
	standInsWriting(t, body, "0.023220\n")
	spec := sourceFile(t, "Album/01.flac")
	spec.TargetBits = 32
	a := newAnnouncingPool(t, spec.SourceLibraryRel)
	announced := a.run(t, spec, 3)
	for i, line := range announced {
		if !strings.HasPrefix(line, "failed:") {
			t.Fatalf("job %d announced %q, want a failure", i, line)
		}
	}
	suppressed, records := a.strikes(t, spec.SourceLibraryRel)
	if suppressed != 0 || records != 0 {
		t.Fatalf("%d suppressed, %d strike record(s); want the source unstruck", suppressed, records)
	}
	warns := a.lines("WARN", logDepthMismatch)
	if len(warns) != 1 || !strings.Contains(warns[0], FLAC32EncodeFix) {
		t.Fatalf("depth outage warns = %q, want one naming the fix", warns)
	}
	if n := len(a.lines("DEBUG", logDepthMismatch)); n != 2 {
		t.Fatalf("depth outage debug lines = %d, want 2", n)
	}
	if n := len(a.lines("WARN", "pool: sox failed")); n != 0 {
		t.Fatalf("strike warns = %d, want 0", n)
	}
	rows, err := a.store.ListVariantsForPath(context.Background(), spec.SourceLibraryRel)
	if err != nil {
		t.Fatalf("ListVariantsForPath: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("stored %d variant(s); want none", len(rows))
	}
	spec16 := spec
	spec16.TargetBits = 16
	if announced = a.run(t, spec16, 1); len(announced) != 1 || announced[0] != "done" {
		t.Fatalf("16-bit job announced %v, want done", announced)
	}
	rows, err = a.store.ListVariantsForPath(context.Background(), spec.SourceLibraryRel)
	if err != nil {
		t.Fatalf("ListVariantsForPath: %v", err)
	}
	if len(rows) != 1 || rows[0].BitsPerSample != 16 {
		t.Fatalf("published %+v, want one 16-bit row", rows)
	}
	if n := len(a.lines("INFO", logDepthBack)); n != 0 {
		t.Fatalf("a 16-bit publish ended the 32-bit outage (%d info lines)", n)
	}
}

// TestAWholeRenditionIsLabelledFromItsStream publishes a 24-bit file a
// 24-bit job asked for and records the depth STREAMINFO names, which is
// the depth the job asked for.
func TestAWholeRenditionIsLabelledFromItsStream(t *testing.T) {
	body := flactest.Stream(176400, 2, 24, flactest.Block, 0)
	standInsWriting(t, body, "0.023220\n")
	spec := sourceFile(t, "Album/01.flac")
	a := newAnnouncingPool(t, spec.SourceLibraryRel)
	if announced := a.run(t, spec, 1); len(announced) != 1 || announced[0] != "done" {
		t.Fatalf("announced %v, want done", announced)
	}
	rows, err := a.store.ListVariantsForPath(context.Background(), spec.SourceLibraryRel)
	if err != nil {
		t.Fatalf("ListVariantsForPath: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("stored %d variant(s), want 1", len(rows))
	}
	stream, err := readFLACStream(rows[0].SidecarPath)
	if err != nil {
		t.Fatalf("read published stream: %v", err)
	}
	if rows[0].BitsPerSample != stream.info.bitsPerSample || rows[0].BitsPerSample != spec.TargetBits {
		t.Fatalf("row bits = %d, STREAMINFO = %d, target = %d", rows[0].BitsPerSample, stream.info.bitsPerSample, spec.TargetBits)
	}
}
