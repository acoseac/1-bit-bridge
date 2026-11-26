package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/backup"
)

// backupKeepInstall is backupInstall with backupYAML (a `backup:` block, or
// "") at the end of its config, BRIDGE_BACKUP_KEEP cleared for the test and
// then set to env where env is not "", so only the file and env set the
// config's retention. It answers the config's path and the backups folder.
func backupKeepInstall(t *testing.T, backupYAML, env string) (cfgPath, root string) {
	t.Helper()
	t.Setenv("BRIDGE_BACKUP_KEEP", "")
	os.Unsetenv("BRIDGE_BACKUP_KEEP")
	if env != "" {
		t.Setenv("BRIDGE_BACKUP_KEEP", env)
	}
	cfgPath, _, root = backupInstall(t)
	t.Cleanup(func() { awaitHoldsLetGo(t, root) })
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(data, backupYAML...), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, root
}

// runBackups runs bridge backup n times, args after its --config, each
// required to exit 0 with its prune run, and answers the folder name of the
// snapshot each run wrote, in order, and the last run's stdout.
func runBackups(t *testing.T, cfgPath string, n int, args ...string) (written []string, last string) {
	t.Helper()
	for i := 1; i <= n; i++ {
		var stdout, stderr bytes.Buffer
		code := backupCmd(append([]string{"--config", cfgPath}, args...), &stdout, &stderr)
		out := stdout.String()
		_, dir, found := strings.Cut(out, "Snapshot written: ")
		if code != 0 || !found || strings.Contains(out, "Prune skipped") {
			t.Fatalf("run %d: bridge backup = %d\nstdout %s\nstderr %s", i, code, out, stderr.String())
		}
		dir, _, _ = strings.Cut(dir, "\n")
		written = append(written, filepath.Base(dir))
		last = out
	}
	return written, last
}

// snapshotNames is the names of the snapshot folders under root, sorted.
func snapshotNames(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	for _, dir := range snapshotDirs(t, root) {
		names = append(names, filepath.Base(dir))
	}
	slices.Sort(names)
	return names
}

// TestBridgeBackupPrunesByTheConfigsRetention: with no --keep, eight runs of
// bridge backup prune as serve's ticker and the console's snapshots do, by
// the config's backup.keep as EffectiveKeep reads it, BRIDGE_BACKUP_KEEP over
// the file: retention off (-1) keeps all eight and still sweeps an aged crash
// orphan; 3 keeps exactly the three newest, beside them the older failed
// snapshot recovery keeps as evidence, and says it kept 3; an omitted keep
// and 0 are the default, 7; and the variable's 2 over the file's 5 keeps two.
// Red at 0a8d39f3, whose command pruned by its flag's default of 7 and asked
// the config for no retention (backlog B1035).
func TestBridgeBackupPrunesByTheConfigsRetention(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env string
		kept            int    // how many of the runs' snapshots survive, the newest
		failed          bool   // an older failed snapshot is there first, and survives
		orphan          bool   // an aged crash orphan is there first, and is swept
		says            string // in the last run's stdout, where not ""
	}{
		{name: "retention off", yaml: "backup:\n  keep: -1\n", kept: 8, orphan: true},
		{name: "three", yaml: "backup:\n  keep: 3\n", kept: 3, failed: true,
			says: "Pruned 1 older snapshot(s) (kept 3 most recent, and what recovery needs)."},
		{name: "omitted", kept: 7, says: "kept 7 most recent"},
		{name: "zero", yaml: "backup:\n  keep: 0\n", kept: 7, says: "kept 7 most recent"},
		{name: "the variable over the file", yaml: "backup:\n  keep: 5\n", env: "2", kept: 2, says: "kept 2 most recent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, root := backupKeepInstall(t, tc.yaml, tc.env)
			var want []string
			if tc.failed {
				dir := writeSnapshotDirAt(t, root, time.Now().UTC().Add(-time.Hour), &backup.Verification{Format: 1,
					DatabaseState: backup.StateFailed, DatabaseReasons: []backup.Reason{backup.ReasonSchemaNewer}, FilesState: backup.StateVerified})
				want = append(want, filepath.Base(dir))
			}
			orphan := filepath.Join(root, "2020-01-01T00-00-00Z")
			if tc.orphan {
				if err := os.MkdirAll(orphan, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(orphan, backup.ManifestDBFileName), []byte("partial"), 0o600); err != nil {
					t.Fatal(err)
				}
				aged := time.Now().Add(-48 * time.Hour)
				if err := os.Chtimes(orphan, aged, aged); err != nil {
					t.Fatal(err)
				}
			}
			written, last := runBackups(t, cfgPath, 8)
			want = append(want, written[len(written)-tc.kept:]...)
			slices.Sort(want)
			if got := snapshotNames(t, root); !slices.Equal(got, want) {
				t.Errorf("eight runs left %d snapshots %v, want %d: %v", len(got), got, len(want), want)
			}
			if tc.says != "" && !strings.Contains(last, tc.says) {
				t.Errorf("the last run's stdout lacks %q:\n%s", tc.says, last)
			}
			if _, err := os.Lstat(orphan); tc.orphan && !os.IsNotExist(err) {
				t.Errorf("the crash orphan survived retention off (lstat err = %v): the sweep runs whatever the retention", err)
			}
		})
	}
}

// TestBridgeBackupsKeepOverridesTheConfig: --keep given on the command line
// is the retention whatever the config says, its 0 or less retention off,
// told by its presence and never by its value: --keep 2 over a backup.keep
// of 5 keeps two, --keep 0 over 3 keeps all eight, --keep 7 over 3 keeps
// seven, and --keep 2 over BRIDGE_BACKUP_KEEP's -1 keeps two.
func TestBridgeBackupsKeepOverridesTheConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env, keep string
		kept                  int
	}{
		{name: "two over five", yaml: "backup:\n  keep: 5\n", keep: "2", kept: 2},
		{name: "zero over three", yaml: "backup:\n  keep: 3\n", keep: "0", kept: 8},
		{name: "seven over three", yaml: "backup:\n  keep: 3\n", keep: "7", kept: 7},
		{name: "two over the variable's off", env: "-1", keep: "2", kept: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, root := backupKeepInstall(t, tc.yaml, tc.env)
			written, _ := runBackups(t, cfgPath, 8, "--keep", tc.keep)
			want := slices.Clone(written[len(written)-tc.kept:])
			slices.Sort(want)
			if got := snapshotNames(t, root); !slices.Equal(got, want) {
				t.Errorf("eight runs at --keep %s left %d snapshots %v, want %d: %v", tc.keep, len(got), got, len(want), want)
			}
		})
	}
}

// TestBridgeBackupsHelpSaysKeepOverridesTheConfig: the flag's help and the
// usage line say that --keep overrides the config's backup.keep, and the help
// prints no default of the flag's own, which would read as the retention a
// run without it applies.
func TestBridgeBackupsHelpSaysKeepOverridesTheConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "bridge.yaml")
	run := func(args ...string) string {
		var stdout, stderr bytes.Buffer
		if code := backupCmd(append([]string{"--config", cfgPath}, args...), &stdout, &stderr); code != 2 {
			t.Errorf("bridge backup %v = %d, want 2", args, code)
		}
		return stderr.String()
	}
	help := run("-h")
	_, keepHelp, found := strings.Cut(help, "  -keep ")
	keepHelp, _, _ = strings.Cut(keepHelp, "\n  -")
	if !found || !strings.Contains(keepHelp, "overrides the config's backup.keep") || strings.Contains(keepHelp, "(default") {
		t.Errorf("--keep's help is %q, in:\n%s", keepHelp, help)
	}
	if usage := run("stray"); !strings.Contains(usage, "overrides the config's backup.keep") {
		t.Errorf("the usage line says:\n%s", usage)
	}
}
