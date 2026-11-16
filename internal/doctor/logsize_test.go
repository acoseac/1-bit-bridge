package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSized(t *testing.T, size int64) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bridge.log")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	// Sparse: Truncate sets the size without writing the bytes, so the
	// oversized case costs no disk and no time.
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCheckLogSizeQuietWhenAbsent pins the no-op states. A foreground
// `bridge serve` has no log file at all, and reporting on a file that does not
// exist would be reporting on nothing — it belongs in the OK column, not as a
// warning an operator has to learn to ignore.
func TestCheckLogSizeQuietWhenAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps Deps
	}{
		{"no path configured", Deps{}},
		{"path set but no file yet", Deps{LogPath: filepath.Join(t.TempDir(), "nope.log")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkLogSize(context.Background(), tc.deps)
			if got.Status != OK {
				t.Errorf("status = %v, want OK (%s)", got.Status, got.Summary)
			}
		})
	}
}

func TestCheckLogSizeOKBelowThreshold(t *testing.T) {
	d := Deps{LogPath: writeSized(t, logSizeWarnBytes-1)}
	got := checkLogSize(context.Background(), d)
	if got.Status != OK {
		t.Errorf("status = %v, want OK just under the threshold: %s", got.Status, got.Summary)
	}
}

// TestCheckLogSizeWarnsAboveThreshold also pins that the hint names the
// PLATFORM'S tool. "Rotate your log" is advice an operator cannot act on
// without going to look something up; naming newsyslog / logrotate is the
// difference between a warning and a next step.
func TestCheckLogSizeWarnsAboveThreshold(t *testing.T) {
	d := Deps{LogPath: writeSized(t, logSizeWarnBytes+1)}
	got := checkLogSize(context.Background(), d)
	if got.Status != Warn {
		t.Fatalf("status = %v, want Warn over the threshold: %s", got.Status, got.Summary)
	}
	if !strings.Contains(got.Summary, "MiB") && !strings.Contains(got.Summary, "GiB") {
		t.Errorf("summary does not state a size: %q", got.Summary)
	}
	if got.Hint == "" {
		t.Fatal("no hint — a warning with no next step is noise")
	}
	for _, want := range []string{"newsyslog", "logrotate", "scheduled task"} {
		if strings.Contains(got.Hint, want) {
			return
		}
	}
	t.Errorf("hint names no platform rotation tool: %q", got.Hint)
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{301 << 20, "301.0 MiB"},
		{1 << 30, "1.0 GiB"},
		{1 << 40, "1.0 TiB"},
		// Past the table's last unit it keeps scaling in TiB rather than
		// indexing off the end of "KMGT".
		{1 << 50, "1024.0 TiB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestALogNoteIsCarriedWhateverTheLineFinds (backlog B618): where the caller
// says how the log was chosen, on Windows where the install check could not
// tell whether the bridge's service is installed, the line's summary carries
// it whatever the line found, so the table, which prints a hint under a warn
// or a fail alone, shows it on an ok too.
func TestALogNoteIsCarriedWhateverTheLineFinds(t *testing.T) {
	const note = "cannot tell whether the bridge's Windows service is installed (the test's reason), so this is its log"
	for _, tc := range []struct {
		name string
		path string
		want Status
	}{
		{"a log below the threshold", writeSized(t, 1024), OK},
		{"a log past the threshold", writeSized(t, logSizeWarnBytes+1), Warn},
		{"no log file yet", filepath.Join(t.TempDir(), "nope.log"), OK},
		{"a directory", t.TempDir(), Warn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkLogSize(context.Background(), Deps{LogPath: tc.path, LogNote: note})
			if got.Status != tc.want || !strings.HasSuffix(got.Summary, "; "+note) {
				t.Errorf("got %s %q, want %s with the note after its summary", got.Status, got.Summary, tc.want)
			}
			bare := checkLogSize(context.Background(), Deps{LogPath: tc.path})
			if got.Summary != bare.Summary+"; "+note || got.Hint != bare.Hint {
				t.Errorf("with the note the line is %q (hint %q), without it %q (hint %q): the note changed more than the summary's end",
					got.Summary, got.Hint, bare.Summary, bare.Hint)
			}
		})
	}
}

// TestALogThatCouldNotBeNamedIsNeverTheTerminalsOK (backlog B618): a log whose
// path could not be made, or that the install check could not place, is a
// warn saying why, its note the hint, never the ok "no log file configured
// (logs go to the terminal)" that an empty path alone reads as.
func TestALogThatCouldNotBeNamedIsNeverTheTerminalsOK(t *testing.T) {
	terminal := checkLogSize(context.Background(), Deps{}).Summary
	for _, tc := range []struct{ name, unresolved, note string }{
		{"a path that cannot be made", "the log's path cannot be made (the test's home is not set)", ""},
		{"neither log there", `neither C:\a\bridge.log nor C:\b\bridge.log exists`,
			"cannot tell whether the bridge's Windows service is installed (the test's reason)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkLogSize(context.Background(), Deps{LogUnresolved: tc.unresolved, LogNote: tc.note})
			if got.Status != Warn || got.Summary != "not checked: "+tc.unresolved || got.Hint != tc.note {
				t.Errorf("got %s %q (hint %q), want warn %q, its note the hint", got.Status, got.Summary, got.Hint,
					"not checked: "+tc.unresolved)
			}
			if got.Summary == terminal {
				t.Errorf("a log that could not be named reads as a bridge that logs to its terminal: %q", got.Summary)
			}
		})
	}
	// A managed bridge's log is still the host's, asked first.
	if got := checkLogSize(context.Background(), Deps{Managed: true, LogUnresolved: "x"}); got.Status != OK ||
		!strings.Contains(got.Summary, "skipped") {
		t.Errorf("a managed bridge's line is %s %q, want the host's skip", got.Status, got.Summary)
	}
}

// TestCheckLogSizeWarnsOnDirectory: os.Stat succeeds on a directory and
// reports a small size, so without an explicit IsDir test the check reports OK
// for a path that can never hold a log — telling the operator logging is fine
// while the service's redirect fails. Reported by Gemini on PR #708.
func TestCheckLogSizeWarnsOnDirectory(t *testing.T) {
	got := checkLogSize(context.Background(), Deps{LogPath: t.TempDir()})
	if got.Status != Warn {
		t.Fatalf("status = %v for a directory path, want Warn: %s", got.Status, got.Summary)
	}
	if !strings.Contains(got.Summary, "directory") {
		t.Errorf("summary does not say the path is a directory: %q", got.Summary)
	}
}
