package updater

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := State{
		Status:          "installing",
		TargetVersion:   "0.2.0",
		PreviousVersion: "0.1.0",
		AttemptedAt:     time.Now().UTC().Truncate(time.Second), // JSON can't keep nanos cleanly across ISO8601
	}
	if err := SaveState(dir, want); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	got, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if got.Status != want.Status || got.TargetVersion != want.TargetVersion ||
		got.PreviousVersion != want.PreviousVersion || !got.AttemptedAt.Equal(want.AttemptedAt) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

func TestLoadStateMissingFileReturnsZero(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState on missing file: %v", err)
	}
	if got.Status != "" {
		t.Errorf("missing file should produce zero State, got %+v", got)
	}
}

func TestLoadStateMalformedFileTreatedAsAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(dir)
	if err != nil {
		t.Fatalf("LoadState on malformed file: %v (should swallow + log)", err)
	}
	if got.Status != "" {
		t.Errorf("malformed file should produce zero State, got %+v", got)
	}
}

func TestClearStateRemovesFile(t *testing.T) {
	dir := t.TempDir()
	if err := SaveState(dir, State{Status: "installing", AttemptedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := ClearState(dir); err != nil {
		t.Fatalf("ClearState: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, stateFileName)); !os.IsNotExist(err) {
		t.Errorf("state file still present after clear: err=%v", err)
	}
}

func TestClearStateMissingFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	if err := ClearState(dir); err != nil {
		t.Errorf("ClearState on missing: %v (should be nil)", err)
	}
}

func TestDecideBootAction(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		st        State
		serverVer string
		want      BootAction
	}{
		{
			name: "no marker → noop",
			st:   State{}, want: BootNoop,
			serverVer: "0.1.0",
		},
		{
			name: "installing + version-match → success",
			st: State{
				Status:        "installing",
				TargetVersion: "0.2.0",
				AttemptedAt:   now.Add(-time.Minute),
			},
			serverVer: "0.2.0",
			want:      BootInstallSucceeded,
		},
		{
			name: "installing + swapped + version-mismatch → failed",
			st: State{
				Status:        "installing",
				TargetVersion: "0.2.0",
				AttemptedAt:   now.Add(-time.Minute),
				SwapStarted:   true,
			},
			serverVer: "0.1.0",
			want:      BootInstallFailed,
		},
		{
			// F2: the marker is armed BEFORE swapBinary, and on Windows
			// swapBinary opens with an SCM stop that can hold for 15 s
			// before any rename. A kill in there leaves this exact state
			// — and .bak still holds the version BEFORE the one running,
			// so the blind restore silently downgraded by two versions.
			name: "installing + never swapped + version-mismatch → clear, no restore",
			st: State{
				Status:        "installing",
				TargetVersion: "0.2.0",
				AttemptedAt:   now.Add(-time.Minute),
			},
			serverVer: "0.1.0",
			want:      BootClearNotSwapped,
		},
		{
			// Upgrade-boundary guard: a marker written by a pre-fix
			// binary has no swapStarted field (false after unmarshal),
			// but if we're demonstrably RUNNING TargetVersion the swap
			// obviously happened — the success stamp must still fire, or
			// InstalledAt is never set and .bak is never reclaimed.
			name: "installing + never swapped + version-match → success",
			st: State{
				Status:        "installing",
				TargetVersion: "0.2.0",
				AttemptedAt:   now.Add(-time.Minute),
			},
			serverVer: "0.2.0",
			want:      BootInstallSucceeded,
		},
		{
			name: "installing + abandoned (older than recency window) → clear",
			st: State{
				Status:        "installing",
				TargetVersion: "0.2.0",
				AttemptedAt:   now.Add(-7 * time.Hour),
			},
			serverVer: "0.1.0",
			want:      BootClearAbandoned,
		},
		{
			name: "installed + InstalledAt set → cleanup .bak",
			st: State{
				Status:      "installed",
				InstalledAt: now.Add(-time.Hour),
			},
			serverVer: "0.2.0",
			want:      BootCleanupBak,
		},
		{
			name: "installed + InstalledAt zero → noop (defensive)",
			st: State{
				Status: "installed",
			},
			serverVer: "0.2.0",
			want:      BootNoop,
		},
		{
			name: "v-prefix on TargetVersion still matches",
			st: State{
				Status:        "installing",
				TargetVersion: "v0.2.0",
				AttemptedAt:   now.Add(-time.Minute),
			},
			serverVer: "0.2.0",
			want:      BootInstallSucceeded,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DecideBootAction(c.st, c.serverVer, now); got != c.want {
				t.Errorf("DecideBootAction = %v, want %v", got, c.want)
			}
		})
	}
}
