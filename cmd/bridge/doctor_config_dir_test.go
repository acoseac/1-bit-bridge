package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// TestDoctorGradesTheConfigDirOnlyForAUserWhoCanReadTheConfig drives
// buildDoctorDepsFor and doctor.Run, as a user the mode bits deny, over
// each shape in which a config reaches doctor, in a directory this user
// cannot write or create.
//
// config-dir exists to vouch that the BRIDGE can create and write the
// directory beside its config, and its two probes, a MkdirAll and a write,
// answer for whoever runs doctor. Measured on 2026-09-25 with `bridge
// serve` as uid 1000 and `bridge doctor --config <its config>` as uid
// 1001, the config dir 0700 as `bridge init` makes it: config-file warned
// that the file is not readable by this user, both port lines said "not
// checked" (#1022), and config-dir FAILed "not writable: open
// …/.doctor-probe: permission denied", so the run exited 1 and told the
// operator to fix it or bypass `bridge init`'s preflight. A user who
// cannot read the config is not the user the bridge runs as, since the
// bridge reads it at every start, so that verdict was about the run.
// config-dir now says it was not checked, and why, for every shape of that
// state: a config found in the working directory or the platform dir, one
// named under a directory this user cannot traverse (where the write
// fails) or below one (where the create does), and the launcher's row over
// a platform config this user cannot reach. The last decline row is a
// directory this user CAN write, which the old check vouched for, ok,
// which was no more the bridge's answer.
//
// The rest are the controls, which keep the FAIL where the probe answers
// for the bridge. A user who can read the config could be the one it runs
// as, whether the config loads or not (a config that does not load still
// names its directory, which comes from its path). A run that finds no
// config, from the CLI or the launcher's row, is by the user about to run
// `bridge init` into the directory.
func TestDoctorGradesTheConfigDirOnlyForAUserWhoCanReadTheConfig(t *testing.T) {
	skipUnlessModeBitsDeny(t, "root reads a mode-0000 config, traverses a mode-0000 directory and "+
		"creates files in a mode-0500 one, so the configs below that this user must not read load, "+
		"and config-dir's create and write succeed; this test needs a user the mode bits deny, as "+
		"they deny another user a service-owned config")
	const notChecked = "not checked: the config in it is not readable by this user"
	for _, tc := range []struct {
		name string
		// install lays out the config and its directory, with the modes
		// the row needs, and returns what buildDoctorDepsFor is handed.
		install    func(t *testing.T, cwd, platform string) (cfgPath string, absentIsPreSetup bool)
		configFile doctor.Status
		configDir  doctor.Status
		// has starts config-dir's summary on an ok and its hint on a fail.
		has string
	}{
		{"a found config this user cannot read, in a directory it cannot write",
			unreadableConfigInReadOnlyWorkingDir, doctor.Warn, doctor.OK, notChecked},
		{"a platform config this user cannot read, in a directory it cannot write",
			unreadableConfigInReadOnlyPlatformDir, doctor.Warn, doctor.OK, notChecked},
		{"a named config in a directory this user cannot traverse",
			namedConfigInUntraversableDir, doctor.Warn, doctor.OK, notChecked},
		{"a named config below a directory this user cannot traverse",
			namedConfigBelowUntraversableDir, doctor.Warn, doctor.OK, notChecked},
		{"the launcher's row over a platform config this user cannot reach",
			launcherRowOverUnreachablePlatformConfig, doctor.Warn, doctor.OK, notChecked},
		{"a found config this user cannot read, in a directory it can write",
			unreadableConfigInWritableWorkingDir, doctor.Warn, doctor.OK, notChecked},

		{"a config this user can read, in a directory it cannot write",
			readableConfigInReadOnlyWorkingDir, doctor.OK, doctor.Fail, "not writable: "},
		{"a config this user can read that does not load, in a directory it cannot write",
			brokenConfigInReadOnlyWorkingDir, doctor.Fail, doctor.Fail, "not writable: "},
		{"no config named or found, and a platform dir this user cannot write",
			readOnlyPlatformDirBeforeSetup, doctor.OK, doctor.Fail, "not writable: "},
		{"no config named or found, and a platform dir this user cannot create",
			uncreatablePlatformDirBeforeSetup, doctor.OK, doctor.Fail, "can't create: "},
		{"the launcher's row before setup, and a platform dir this user cannot write",
			launcherRowOverReadOnlyPlatformDirBeforeSetup, doctor.OK, doctor.Fail, "not writable: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd, platform := isolateConfigEnv(t)
			cfgPath, absentIsPreSetup := tc.install(t, cwd, platform)
			rep := doctor.Run(context.Background(), buildDoctorDepsFor(cfgPath, absentIsPreSetup))

			if c := findCheck(t, rep, "config-file"); c.Status != tc.configFile {
				t.Errorf("config-file = %s %q, want %s", c.Status, c.Summary, tc.configFile)
			}
			c := findCheck(t, rep, "config-dir")
			if c.Status != tc.configDir {
				t.Fatalf("config-dir = %s %q (hint %q), want %s …%s", c.Status, c.Summary, c.Hint, tc.configDir, tc.has)
			}
			// A fail must be the one the mode bits give: a fixture that
			// denied nothing would fail for another reason, or not at all.
			if tc.configDir == doctor.Fail && (!strings.HasPrefix(c.Hint, tc.has) || !strings.Contains(c.Hint, "permission denied")) {
				t.Errorf("config-dir = fail %q (hint %q), want a hint starting %q about a permission denied",
					c.Summary, c.Hint, tc.has)
			}
			if tc.configDir == doctor.OK && !strings.HasPrefix(c.Summary, tc.has) {
				t.Errorf("config-dir = ok %q, want it to start %q", c.Summary, tc.has)
			}
		})
	}
}

// unreadableConfigInReadOnlyWorkingDir writes an install in cwd whose
// bridge.yaml this user may not read, in a directory it may not write, and
// names none: the working directory's is found.
func unreadableConfigInReadOnlyWorkingDir(t *testing.T, cwd, _ string) (string, bool) {
	chmodForTest(t, writeInstallAt(t, cwd, "local-track.flac"), 0)
	denyDirForTest(t, cwd, 0o500)
	return "", false
}

// unreadableConfigInReadOnlyPlatformDir is the same install in the platform
// dir, which a run from a working directory with no config finds.
func unreadableConfigInReadOnlyPlatformDir(t *testing.T, _, platform string) (string, bool) {
	chmodForTest(t, writeInstallAt(t, platform, "platform-track.flac"), 0)
	denyDirForTest(t, platform, 0o500)
	return "", false
}

// namedConfigInUntraversableDir names the bridge.yaml of an install in a
// directory this user may not traverse, the shape `bridge init`'s 0700
// config dir has for any other user. The directory exists, so the create
// passes and the write fails.
func namedConfigInUntraversableDir(t *testing.T, _, _ string) (string, bool) {
	locked := filepath.Join(t.TempDir(), "locked")
	named := writeInstallAt(t, locked, "local-track.flac")
	denyDirForTest(t, locked, 0)
	return named, false
}

// namedConfigBelowUntraversableDir names a bridge.yaml one directory below
// one this user may not traverse, so the config's own directory cannot be
// looked at, and the create fails.
func namedConfigBelowUntraversableDir(t *testing.T, _, _ string) (string, bool) {
	locked := filepath.Join(t.TempDir(), "locked")
	named := writeInstallAt(t, filepath.Join(locked, "cfg"), "local-track.flac")
	denyDirForTest(t, locked, 0)
	return named, false
}

// launcherRowOverUnreachablePlatformConfig is the launcher's doctor row
// (actDoctor) over a platform install this user may not reach: another
// user's, say a root-owned one `sudo bridge init` wrote. The launcher
// offers the row because it cannot see the install. absentIsPreSetup makes
// only an ABSENT config the pre-setup state, so this one is reported as not
// readable, like a --config naming it.
func launcherRowOverUnreachablePlatformConfig(t *testing.T, _, platform string) (string, bool) {
	named := writeInstallAt(t, platform, "platform-track.flac")
	denyDirForTest(t, platform, 0)
	return named, true
}

// unreadableConfigInWritableWorkingDir writes an install in cwd whose
// bridge.yaml this user may not read, in a directory it MAY write.
func unreadableConfigInWritableWorkingDir(t *testing.T, cwd, _ string) (string, bool) {
	chmodForTest(t, writeInstallAt(t, cwd, "local-track.flac"), 0)
	return "", false
}

// readableConfigInReadOnlyWorkingDir writes an install in cwd that this
// user may read, in a directory it may not write: a bridge whose own user
// cannot write beside its config.
func readableConfigInReadOnlyWorkingDir(t *testing.T, cwd, _ string) (string, bool) {
	writeInstallAt(t, cwd, "local-track.flac")
	denyDirForTest(t, cwd, 0o500)
	return "", false
}

// brokenConfigInReadOnlyWorkingDir writes a bridge.yaml in cwd that this
// user may read and config.Load refuses, in a directory it may not write.
func brokenConfigInReadOnlyWorkingDir(t *testing.T, cwd, _ string) (string, bool) {
	installConfigThatDoesNotLoad(t, cwd)
	denyDirForTest(t, cwd, 0o500)
	return "", false
}

// readOnlyPlatformDirBeforeSetup leaves no config anywhere, with a platform
// dir, where `bridge init` would write, that this user may not write.
func readOnlyPlatformDirBeforeSetup(t *testing.T, _, platform string) (string, bool) {
	denyDirForTest(t, platform, 0o500)
	return "", false
}

// uncreatablePlatformDirBeforeSetup leaves no config anywhere, with a
// platform dir that does not exist yet, under a parent this user may not
// write, so it cannot be created.
func uncreatablePlatformDirBeforeSetup(t *testing.T, _, _ string) (string, bool) {
	parent := t.TempDir()
	platform := filepath.Join(parent, "1-bit-bridge")
	// isolateConfigEnv's cleanup restores the production lookup.
	defaultConfigDirFn = func() (string, error) { return platform, nil }
	denyDirForTest(t, parent, 0o500)
	return "", false
}

// launcherRowOverReadOnlyPlatformDirBeforeSetup is the launcher's doctor
// row before setup: the platform config is not there, and its directory is
// one this user may not write.
func launcherRowOverReadOnlyPlatformDirBeforeSetup(t *testing.T, _, platform string) (string, bool) {
	denyDirForTest(t, platform, 0o500)
	return filepath.Join(platform, defaultConfigPath), true
}

// denyDirForTest sets dir's mode for the rest of the test, and restores
// one RemoveAll can clear. Registered after the t.TempDir that made dir,
// the restore runs first.
func denyDirForTest(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	chmodForTest(t, dir, mode)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}
