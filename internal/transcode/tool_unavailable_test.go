package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// soxLookupFailure is the error Run returns when sox is not on PATH, spelled
// as Run spells it: exec.Command's own lookup failure, wrapped by the
// sox-direct exit.
func soxLookupFailure() error {
	return fmt.Errorf("sox: %w (stderr: )", &exec.Error{Name: "sox", Err: exec.ErrNotFound})
}

// TestUnavailableToolClassifiesWhatExecReports runs real commands and asks
// the classifier about what each returned. A pinned string in the classifier
// ("fork/exec") is only as good as the os package's agreement with it, so the
// failures here are real ones, not constructed values.
func TestUnavailableToolClassifiesWhatExecReports(t *testing.T) {
	run := func(cmd *exec.Cmd) error {
		t.Helper()
		err := cmd.Run()
		if err == nil {
			t.Fatalf("%v ran cleanly, want a failure", cmd.Args)
		}
		// Wrapped, as every exit of Run wraps it.
		return fmt.Errorf("sox: %w (stderr: )", err)
	}
	for _, tc := range []struct {
		name     string
		err      func(t *testing.T) error
		wantTool string // "" means the failure may be about the source
	}{
		{
			name:     "not on PATH",
			err:      func(t *testing.T) error { return run(exec.Command("b43-no-such-tool")) },
			wantTool: "b43-no-such-tool",
		},
		{
			// POSIX: os.StartProcess's fork/exec error. Windows: the lookup
			// of the absolute path's extension, an *exec.Error.
			name:     "absolute path that is not there",
			err:      func(t *testing.T) error { return run(exec.Command(filepath.Join(t.TempDir(), "sox"))) },
			wantTool: "sox",
		},
		{
			// The test binary itself, handed a flag it does not define: a
			// program that started, ran and exited 2. Its verdict may be
			// about the file, so it is not classified.
			name: "a tool that ran and exited non-zero",
			err: func(t *testing.T) error {
				err := run(exec.Command(os.Args[0], "-b43-no-such-flag"))
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("want an *exec.ExitError from a program that ran, got %v", err)
				}
				return err
			},
		},
		{
			name: "a context that ended before the start",
			err: func(t *testing.T) error {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return run(exec.CommandContext(ctx, os.Args[0], "-test.run=^$"))
			},
		},
		{name: "a plain error", err: func(*testing.T) error { return errors.New("sox FAIL formats: bad header") }},
		{
			// Built by hand, with no cause: classified, and no panic in the
			// worker that asks.
			name:     "an exec error with no cause",
			err:      func(*testing.T) error { return &exec.Error{Name: "sox"} },
			wantTool: "sox",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err(t)
			got, ok := unavailableTool(err)
			if tc.wantTool == "" {
				if ok {
					t.Fatalf("unavailableTool(%v) = %+v, want no classification: it may be about the source", err, got)
				}
				return
			}
			if !ok || got.name != tc.wantTool || got.reason == "" {
				t.Fatalf("unavailableTool(%v) = (%+v, %v), want tool %q with a reason", err, got, ok, tc.wantTool)
			}
		})
	}
}

// TestUnavailableToolReadsTheRouteMarkAndPrefersTheToolThatDidNotStart pins
// the two ways a mark and an exec failure can meet. A mark alone answers its
// own tool; a mark wrapping a tool that could not start (sox gone between the
// decoder probe and the run) answers the tool that could not start.
func TestUnavailableToolReadsTheRouteMarkAndPrefersTheToolThatDidNotStart(t *testing.T) {
	marked := markToolUnavailable(toolFFmpeg, "ffmpeg and ffprobe not found on PATH",
		fmt.Errorf("%w (route none, source %q)", ErrDSDDecodeUnavailable, "01.dsf"))
	got, ok := unavailableTool(marked)
	if !ok || got.name != toolFFmpeg || got.reason != "ffmpeg and ffprobe not found on PATH" {
		t.Errorf("unavailableTool(route mark) = (%+v, %v), want ffmpeg with the mark's reason", got, ok)
	}
	if !errors.Is(marked, ErrDSDDecodeUnavailable) || marked.Error() != fmt.Sprintf("%v (route none, source %q)", ErrDSDDecodeUnavailable, "01.dsf") {
		t.Errorf("the mark changed what the error says or is: %q", marked)
	}

	both := markToolUnavailable(toolFFmpeg, "sox has no MP4 reader, and ffmpeg not found on PATH", soxLookupFailure())
	if got, ok := unavailableTool(both); !ok || got.name != toolSox {
		t.Errorf("unavailableTool(mark over sox's lookup failure) = (%+v, %v), want sox", got, ok)
	}
}

func TestToolNameIsTheBaseNameWithoutExe(t *testing.T) {
	for in, want := range map[string]string{
		"sox":                                    "sox",
		filepath.Join("usr", "local", "sox"):     "sox",
		"ffprobe.exe":                            "ffprobe",
		"FFMPEG.EXE":                             "FFMPEG",
		filepath.Join("opt", "tools", "sox.exe"): "sox",
	} {
		if got := toolName(in); got != want {
			t.Errorf("toolName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMissingDecodeToolNamesWhatTheHostLacks walks the ffmpeg snapshots a
// source can route nowhere under.
func TestMissingDecodeToolNamesWhatTheHostLacks(t *testing.T) {
	for _, tc := range []struct {
		name              string
		ff                FFmpegInfo
		class             decodeClass
		wantTool, wantWhy string
	}{
		{"both binaries missing, DSD", FFmpegInfo{MissingBinaries: []string{"ffmpeg", "ffprobe"}}, classDSD,
			toolFFmpeg, "ffmpeg and ffprobe not found on PATH"},
		{"ffprobe missing, ALAC", FFmpegInfo{MissingBinaries: []string{"ffprobe"}}, classMP4,
			toolFFprobe, "sox has no MP4 reader, and ffprobe not found on PATH"},
		{"listing unreadable, DSD", FFmpegInfo{Path: "/x/ffmpeg", ProbeErr: "ffmpeg -decoders timed out after 2s"}, classDSD,
			toolDSDDecoders, "the ffmpeg decoder listing could not be read: ffmpeg -decoders timed out after 2s"},
		{"a build without the decoders, DSD", FFmpegInfo{Path: "/x/ffmpeg", DecodersKnown: true}, classDSD,
			toolDSDDecoders, "this ffmpeg build lacks the dsd_* decoders"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool, why := missingDecodeTool(tc.ff, tc.class)
			if tool != tc.wantTool || why != tc.wantWhy {
				t.Errorf("missingDecodeTool = (%q, %q), want (%q, %q)", tool, why, tc.wantTool, tc.wantWhy)
			}
		})
	}
}

// TestToolsProvenByReadsTheRouteTheRunTook pins which outages a success ends:
// the settings' decoder is the route the run took, and the settings each
// route writes are taken from the code that writes them.
func TestToolsProvenByReadsTheRouteTheRunTook(t *testing.T) {
	spec := JobSpec{TargetSampleRate: 176400, TargetBits: 24, Quality: QualityVeryHigh, OutputDir: t.TempDir(),
		SourceLibraryRel: "A/01.flac"}
	settingsFor := func(r decodeRoute) string {
		_, s, _, _ := spec.soxArgsFrom([]string{"in"}, r.String())
		return s
	}
	dsd, err := spec.dsdSettings(sourceGeometry{SampleRate: 352800, Channels: 2}, 3, nil, GainScopeTrack, 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, settings string
		want           []string
	}{
		{"sox-direct", settingsFor(routeSoxDirect), []string{toolSox}},
		{"no route, read by sox after all", settingsFor(routeNone), []string{toolSox}},
		{"the ffmpeg pipe", settingsFor(routeFFmpegPipe), []string{toolSox, toolFFmpeg, toolFFprobe}},
		{"the DSD chain", dsd, []string{toolSox, toolFFmpeg, toolFFprobe, toolDSDDecoders}},
		{"settings that do not parse", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toolsProvenBy(tc.settings)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("toolsProvenBy(%s) = %v, want %v", tc.settings, got, tc.want)
			}
		})
	}
}
