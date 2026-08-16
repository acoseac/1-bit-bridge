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
