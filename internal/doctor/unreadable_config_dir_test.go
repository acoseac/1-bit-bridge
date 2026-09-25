package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configDirShape is a directory config-dir can be handed, built so each of
// its two probes, the create and the write, has a known answer without a
// permission bit. The tests below therefore run as root and on Windows,
// where mode bits deny nothing.
type configDirShape struct {
	name string
	make func(t *testing.T) string
	// probed is config-dir's verdict when it runs its probes on the
	// directory. A fail's hint starts with failHint; an ok names the
	// directory.
	probed   Status
	failHint string
}

// configDirShapes are the three answers config-dir's probes can give.
var configDirShapes = []configDirShape{
	{"the create fails", fileWhereTheDirectoryShouldBe, Fail, "can't create: "},
	{"the write fails", directoryWhereTheProbeShouldBe, Fail, "not writable: "},
	{"both pass", func(t *testing.T) string { return t.TempDir() }, OK, ""},
}

// fileWhereTheDirectoryShouldBe returns a path holding a regular file,
// which MkdirAll refuses to make a directory of.
func fileWhereTheDirectoryShouldBe(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "1-bit-bridge")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// directoryWhereTheProbeShouldBe returns a directory holding a directory
// with the probe file's name, which the write refuses.
func directoryWhereTheProbeShouldBe(t *testing.T) string {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".doctor-probe"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestConfigDirIsNotCheckedForAUserWhoCannotReadTheConfig pins config-dir's
// answer when the config in the directory is not readable by this user.
//
// config-dir exists to vouch that the BRIDGE can create and write the
// directory its config lives in, and both of its probes answer for whoever
// ran doctor. A user who may not read the config is not the user the bridge
// runs as, since the bridge reads it at every start, so what either probe
// says is a fact about the run. Measured on 2026-09-25 with `bridge serve`
// as uid 1000 and `bridge doctor --config <its config>` as uid 1001:
// config-file warned, both port lines said "not checked" (#1022), and
// config-dir FAILed "not writable: open …/.doctor-probe: permission
// denied", so the run exited 1 and told the operator to fix it or pass
// `bridge init --skip-doctor`.
//
// So the line says it was not checked, and why, and config-file gives the
// one verdict about the run. That holds whatever the probes would have
// answered: the old check FAILs the first two shapes, and the third, a
// directory this user can write, it vouched for, which is no more the
// bridge's answer than the fails were.
func TestConfigDirIsNotCheckedForAUserWhoCannotReadTheConfig(t *testing.T) {
	for _, shape := range configDirShapes {
		t.Run(shape.name, func(t *testing.T) {
			c := checkConfigDir(t.Context(), Deps{
				ConfigDir:  shape.make(t),
				ConfigFile: &ConfigFile{Path: ungradedConfigPath, LoadErr: unreadableConfig.err},
			})
			if c.Status != OK || !strings.HasPrefix(c.Summary, "not checked: ") ||
				!strings.Contains(c.Summary, unreadableConfig.reason) {
				t.Errorf("config-dir = %s %q (hint %q) for a config this user cannot read, "+
					"want ok \"not checked: …%s\"", c.Status, c.Summary, c.Hint, unreadableConfig.reason)
			}
		})
	}
}

// TestConfigDirStillProbesForAUserWhoCanReadTheConfigOrIsAboutToWriteIt is
// the control for the test above. The probes answer for the bridge whenever
// the user running doctor is one it could be: a user who read the config
// (it loaded, or it did not load and the error is in the file, which is the
// runbook's validate-before-restart run by the bridge's own user), and the
// user of a run that found no config, who is about to run `bridge init`
// into this directory. `bridge init`'s preflight looks nothing up (a nil
// lookup) for the same reason. The directory comes from the config's PATH,
// never its contents, so a config that does not load names it as well as
// one that does.
//
// The launcher's row is the last case: its user is the one about to run
// `bridge init` here even when the row finds a config this user cannot
// read, and Setup's preflight then probes this directory as that user. A
// row that declined would read "all clear." over the directory Setup then
// refuses.
func TestConfigDirStillProbesForAUserWhoCanReadTheConfigOrIsAboutToWriteIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		file *ConfigFile
	}{
		{"a config that loaded", &ConfigFile{Path: ungradedConfigPath}},
		{"a config that does not load", &ConfigFile{Path: ungradedConfigPath, LoadErr: brokenConfig.err}},
		{"nothing named, nothing found", &ConfigFile{Tried: []string{ungradedConfigPath}}},
		{"no lookup", nil},
		{"the launcher's row over a config this user cannot read",
			&ConfigFile{Path: ungradedConfigPath, LoadErr: unreadableConfig.err, PreSetup: true}},
	} {
		for _, shape := range configDirShapes {
			t.Run(tc.name+", "+shape.name, func(t *testing.T) {
				dir := shape.make(t)
				c := checkConfigDir(t.Context(), Deps{ConfigDir: dir, ConfigFile: tc.file})
				switch {
				case c.Status != shape.probed:
					t.Errorf("config-dir = %s %q (hint %q), want %s: the probes did not run",
						c.Status, c.Summary, c.Hint, shape.probed)
				case c.Status == OK && c.Summary != dir:
					t.Errorf("config-dir = ok %q, want ok naming %s", c.Summary, dir)
				case c.Status == Fail && !strings.HasPrefix(c.Hint, shape.failHint):
					t.Errorf("config-dir = fail %q (hint %q), want the hint to start %q",
						c.Summary, c.Hint, shape.failHint)
				}
			})
		}
	}
}
