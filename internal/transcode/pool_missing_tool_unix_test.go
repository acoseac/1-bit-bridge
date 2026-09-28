//go:build !windows

package transcode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Stand-in tools, for the cases that need a tool to be THERE: shell scripts,
// so these tests are POSIX-only (the missing-tool cases in
// pool_missing_tool_test.go run everywhere). They use shell builtins alone,
// because PATH holds nothing but the stand-ins.

// standInSoxHelp answers `sox --help` (ProbeSox) with a formats listing
// that holds no MP4 reader, as every stock build's does.
const standInSoxHelp = `#!/bin/sh
if [ "$1" = "--help" ]; then
  printf 'sox: SoX v14.4.2\n\nAUDIO FILE FORMATS: flac wav\n\nEFFECTS: rate dither\n'
  exit 0
fi
`

// standInSoxWrites writes its output argument (the one after -t flac), the
// way a run that succeeds leaves its temp file.
const standInSoxWrites = standInSoxHelp + `out=""; prev=""; prev2=""
for a in "$@"; do
  if [ "$prev2" = "-t" ] && [ "$prev" = "flac" ]; then out="$a"; break; fi
  prev2="$prev"; prev="$a"
done
[ -n "$out" ] || { echo "stand-in sox: no output argument" >&2; exit 1; }
printf 'fLaC' > "$out"
`

// standInSoxRefuses refuses its input, as sox refuses a file it cannot read.
const standInSoxRefuses = standInSoxHelp + `echo "sox FAIL formats: can't open input file: bad header" >&2
exit 2
`

// standInFFmpeg lists the four dsd_* decoders (ProbeFFmpeg), and otherwise
// exits at once: the case it serves never gets as far as reading its output.
const standInFFmpeg = `#!/bin/sh
if [ "$2" = "-decoders" ]; then
  printf 'Decoders:\n A..... = Audio\n ------\n A....D dsd_lsbf DSD\n A....D dsd_lsbf_planar DSD\n A....D dsd_msbf DSD\n A....D dsd_msbf_planar DSD\n'
fi
exit 0
`

// standInFFprobe reports a DSD64 stereo source's decoder geometry.
const standInFFprobe = `#!/bin/sh
printf 'sample_rate=352800\nchannels=2\nduration=1.000000\n'
`

// standInTools writes the named scripts into a fresh directory and returns it,
// to be the whole of PATH.
func standInTools(t *testing.T, scripts map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write stand-in %s: %v", name, err)
		}
	}
	return dir
}

// TestAToolThatRanAndRefusedTheFileStillStrikesIt is the positive control for
// every no-strike test in these two files: a sox that ran and refused the
// file reached a verdict on it, and three of those suppress the file, as the
// debounce was built to. Without it, a pool that struck nothing at all would
// pass the others.
func TestAToolThatRanAndRefusedTheFileStillStrikesIt(t *testing.T) {
	const rel = "Music/Album/01.flac"
	a := newToolFreePool(t, standInTools(t, map[string]string{"sox": standInSoxRefuses}), rel)
	a.run(t, sourceFile(t, rel), 3)

	if suppressed, records := a.strikes(t, rel); suppressed != 1 || records != 1 {
		t.Errorf("after three refusals: %d suppressed, %d strike record(s), want 1 and 1", suppressed, records)
	}
	if n := len(a.log.Failures("pool: sox failed")); n != 3 {
		t.Errorf("%d \"pool: sox failed\" warnings, want one per refusal (3)", n)
	}
	if n := len(a.log.Lines(logToolUnavailable)); n != 0 {
		t.Errorf("%d %q line(s) for a tool that ran, want none", n, logToolUnavailable)
	}
}

// TestAJobWhoseToolIsBrokenOrMissingOnTheWayStrikesNoSource covers the
// shapes a missing tool takes past the first exec: the ALAC route the probe
// found no decoder for (sox has no MP4 reader and ffmpeg is missing, so sox
// is handed the file and refuses it for want of a reader, not for the file),
// the DSD pipe starting ffmpeg and then failing to find sox, and a sox on
// PATH that the system cannot start (a missing interpreter, what a broken
// install looks like: exec's lookup accepts it, fork/exec does not).
func TestAJobWhoseToolIsBrokenOrMissingOnTheWayStrikesNoSource(t *testing.T) {
	for _, tc := range []struct {
		name, rel, tool, reason string
		tools                   map[string]string
		spec                    func(*testing.T, string) JobSpec
	}{
		{name: "ALAC with no decoder", rel: "Music/Album/01.m4a", tool: toolFFmpeg, reason: "sox has no MP4 reader",
			tools: map[string]string{"sox": standInSoxRefuses}, spec: sourceFile},
		{name: "DSD with sox missing", rel: "Music/Album/01.dsf", tool: toolSox,
			tools: map[string]string{"ffmpeg": standInFFmpeg, "ffprobe": standInFFprobe}, spec: dsdSource},
		{name: "sox that cannot start", rel: "Music/Album/01.flac", tool: toolSox,
			tools: map[string]string{"sox": "#!/b43/no/such/interpreter\n"}, spec: sourceFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newToolFreePool(t, standInTools(t, tc.tools), tc.rel)
			a.run(t, tc.spec(t, tc.rel), 3)

			if suppressed, records := a.strikes(t, tc.rel); suppressed != 0 || records != 0 {
				t.Errorf("%d suppressed, %d strike record(s), want none: the job could not run %s",
					suppressed, records, tc.tool)
			}
			warns := a.log.Failures()
			if len(warns) != 1 || !strings.Contains(warns[0], "tool="+tc.tool) || !strings.Contains(warns[0], tc.reason) {
				t.Errorf("warnings:\n%s\nwant exactly one, naming %s (reason containing %q)",
					strings.Join(warns, "\n"), tc.tool, tc.reason)
			}
		})
	}
}

// TestInstallingTheToolBringsTheSourceBackAtTheNextJob is the reproduction's
// second half, through the real runner: after three jobs fail for want of sox
// the source is not suppressed, so the first job once sox is installed
// converts it, and that job reports sox back. A later outage is reported
// afresh.
func TestInstallingTheToolBringsTheSourceBackAtTheNextJob(t *testing.T) {
	const rel = "Music/Album/01.flac"
	noTools := t.TempDir()
	a := newToolFreePool(t, noTools, rel)
	spec := sourceFile(t, rel)
	a.run(t, spec, 3)

	t.Setenv("PATH", standInTools(t, map[string]string{"sox": standInSoxWrites}))
	if got := a.run(t, spec, 1); got[0] != "done" {
		t.Fatalf("the first job with sox installed announced %q, want done", got[0])
	}
	t.Setenv("PATH", noTools)
	a.run(t, spec, 1)

	back := a.lines("INFO", logToolBack)
	if len(back) != 1 || !strings.Contains(back[0], "tool=sox") || !strings.Contains(back[0], "failedJobs=3") {
		t.Errorf("recovery lines:\n%s\nwant one, sox back after 3 failed jobs", strings.Join(back, "\n"))
	}
	if warns := a.lines("WARN", logToolUnavailable); len(warns) != 2 {
		t.Errorf("Warn lines:\n%s\nwant two: the first outage and the second", strings.Join(warns, "\n"))
	}
}
