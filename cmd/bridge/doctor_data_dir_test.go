package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// Backlog B659, B606's follow-up: bridge doctor had no line on the data
// directory as such. The player-storage line, which fails a data directory
// serve refuses since B606, exists only for a config that asks for the player
// on Linux, so with the player off a data directory serve will not start on
// went unreported (tls-cert found no pair there, "absent (init will mint)"),
// and the report passed. The data-dir line says, for every config that loads,
// whether serve can prepare the data directory (prepareServeDataDir), the
// same verdict on the path's shapes, and it is "not checked", a warn, for a
// config that cannot be loaded.

// dataDirCheckSlug is the data-dir line's name, the slug the report prints.
const dataDirCheckSlug = "data-dir"

// writeDataDirConfig writes base/bridge.yaml with its data directory at
// dataDir, MPD off and the bridge's own player on where player is, and
// returns its path.
func writeDataDirConfig(t *testing.T, base, dataDir string, player bool) string {
	t.Helper()
	lib := filepath.Join(base, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\ndataDir: " + dataDir +
		"\noutput:\n  mpd: off\n"
	if player {
		body += "  player:\n    enabled: true\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// doctorsSaid is what bridge doctor and the console's doctor said of one
// config: bridge doctor's exit code and its report as printed, and each
// report's lines by name.
type doctorsSaid struct {
	code    int
	printed string
	cli     map[string]doctor.Check
	console map[string]doctor.Check
}

// askBothDoctors runs bridge doctor over the config at path (doctorCmd, and
// the Deps it builds, buildDoctorDeps) and the console's doctor
// (adminDoctorRunner), and returns what they said.
func askBothDoctors(t *testing.T, path string) doctorsSaid {
	t.Helper()
	var out, errOut strings.Builder
	said := doctorsSaid{code: doctorCmd([]string{"--config", path}, &out, &errOut),
		cli: map[string]doctor.Check{}, console: map[string]doctor.Check{}}
	said.printed = stripANSI(out.String() + errOut.String())
	for _, c := range doctor.Run(context.Background(), buildDoctorDeps(path)).Checks {
		said.cli[c.Name] = c
	}
	for _, c := range adminDoctorRunner(path, nil, nil)(context.Background()).Checks {
		said.console[c.Name] = doctor.Check{Name: c.Name, Status: doctor.Status(c.Status), Summary: c.Summary, Hint: c.Hint}
	}
	return said
}

// requireLine requires both doctors to carry the line name, word for word,
// with status, and returns it.
func (s doctorsSaid) requireLine(t *testing.T, name string, status doctor.Status) doctor.Check {
	t.Helper()
	cli, inCLI := s.cli[name]
	console, inConsole := s.console[name]
	switch {
	case !inCLI || !inConsole:
		t.Fatalf("bridge doctor has a %s line: %v, the console's doctor: %v, want one in each; bridge doctor printed\n%s", name, inCLI, inConsole, s.printed)
	case cli != console:
		t.Errorf("the console's doctor says %+v, bridge doctor %+v", console, cli)
	case cli.Status != status:
		t.Errorf("the %s line is %s %q, want %s", name, cli.Status, cli.Summary, status)
	}
	return cli
}

// requirePrinted requires bridge doctor's report to print c as its line,
// with its hint under it where c is no ok.
func (s doctorsSaid) requirePrinted(t *testing.T, c doctor.Check) {
	t.Helper()
	line := stripANSI(fmt.Sprintf("%s %-18s  %s\n", badgeForStatus(c.Status), c.Name, c.Summary))
	if c.Status != doctor.OK {
		line += "    ↳ " + c.Hint + "\n"
	}
	if !strings.Contains(s.printed, line) {
		t.Errorf("bridge doctor printed\n%s\nwant the lines\n%s", s.printed, line)
	}
}

// TestBothDoctorsFailEveryDataDirectoryServeRefuses runs bridge doctor and
// the console's doctor over a config with the player off, for each shape of a
// data directory serve refuses to prepare (serve's own table,
// unusableDataDirs: a regular file, a link to nothing and a link to a regular
// file at the data directory; an ancestor that is a regular file, a link to
// one or a link to nothing): the data-dir line fails in both, word for word,
// saying bridge serve will not start, naming the data directory and the entry
// in its way as serve's refusal does, and bridge doctor exits 1, while
// prepareServeDataDir refuses the same data directory; no player-storage line
// says it in its place. Both reports passed before B659.
func TestBothDoctorsFailEveryDataDirectoryServeRefuses(t *testing.T) {
	for _, shape := range unusableDataDirs {
		t.Run(shape.name, func(t *testing.T) {
			base := t.TempDir()
			dataDir, why := shape.lay(t, base)
			said := askBothDoctors(t, writeDataDirConfig(t, base, dataDir, false))
			c := said.requireLine(t, dataDirCheckSlug, doctor.Fail)
			if !strings.HasPrefix(c.Summary, "bridge serve will not start: ") || !strings.Contains(c.Summary, why) || !strings.Contains(c.Summary, dataDir) {
				t.Errorf("the data-dir line says %q, want it to say bridge serve will not start, %q, and name %s", c.Summary, why, dataDir)
			}
			if said.code != 1 {
				t.Errorf("bridge doctor exited %d over a data directory serve refuses, want 1", said.code)
			}
			if c, ok := said.cli[playerStorageCheckName]; ok {
				t.Errorf("a config that leaves the player off has the line %+v", c)
			}
			var unusable *dataDirUnusableError
			if err := prepareServeDataDir(dataDir); !errors.As(err, &unusable) || !strings.Contains(err.Error(), why) {
				t.Errorf("prepareServeDataDir(%s) = %v, want serve's refusal naming %q", dataDir, err, why)
			}
		})
	}
}

// moveAsideHint and pointTheLinkHint are the data-dir line's fixes for an
// entry in the data directory's way that is no link, and for one that is.
const (
	moveAsideHint    = "move it aside, or set dataDir in bridge.yaml to a directory serve can make or use"
	pointTheLinkHint = "point the link at a directory, or set dataDir in bridge.yaml to one"
)

// TestTheDataDirLineSaysWhatStandsInTheWayAndItsFix pins the data-dir line's
// words for each shape serve refuses, through both doctors: the data directory
// that is a regular file, or leads to one, is named as such; a link to nothing
// is one serve cannot make the data directory through; an entry above the data
// directory that is a regular file is one serve cannot make it below, and one
// that is a link, to a regular file or to nothing, one it cannot make it
// through; each with the fix for a link or for an entry that is none, printed
// under the line.
func TestTheDataDirLineSaysWhatStandsInTheWayAndItsFix(t *testing.T) {
	const wont = "bridge serve will not start: "
	for _, tc := range []struct {
		name string
		// lay lays the case out below base, and returns the data directory
		// and what the line must say and advise.
		lay func(t *testing.T, base string) (dataDir, says, hint string)
	}{
		{"a regular file at the data directory", func(t *testing.T, base string) (string, string, string) {
			d := filepath.Join(base, "data")
			writeFileOrFail(t, d)
			return d, wont + "the data directory " + d + " is a regular file, not a directory", moveAsideHint
		}},
		{"a link to a regular file at the data directory", func(t *testing.T, base string) (string, string, string) {
			file := filepath.Join(base, "file")
			writeFileOrFail(t, file)
			d := filepath.Join(base, "data")
			symlinkOrSkip(t, file, d)
			return d, wont + "the data directory " + d + " leads to " + resolvedOrFail(t, d) + ", a regular file, not a directory", pointTheLinkHint
		}},
		{"a link to nothing at the data directory", func(t *testing.T, base string) (string, string, string) {
			d := filepath.Join(base, "data")
			symlinkOrSkip(t, filepath.Join(base, "nothing"), d)
			return d, wont + d + " is a symbolic link to nothing, and serve cannot make " + d + " through it", pointTheLinkHint
		}},
		{"an ancestor that is a regular file", func(t *testing.T, base string) (string, string, string) {
			a := filepath.Join(base, "config")
			writeFileOrFail(t, a)
			d := filepath.Join(a, "data")
			return d, wont + a + " is a regular file, not a directory, and serve cannot make " + d + " below it", moveAsideHint
		}},
		{"an ancestor that is a link to a regular file", func(t *testing.T, base string) (string, string, string) {
			file := filepath.Join(base, "file")
			writeFileOrFail(t, file)
			a := filepath.Join(base, "config")
			symlinkOrSkip(t, file, a)
			d := filepath.Join(a, "sub", "data")
			return d, wont + a + " leads to " + resolvedOrFail(t, a) + ", a regular file, not a directory, and serve cannot make " + d + " through it", pointTheLinkHint
		}},
		{"an ancestor that is a link to nothing", func(t *testing.T, base string) (string, string, string) {
			a := filepath.Join(base, "config")
			symlinkOrSkip(t, filepath.Join(base, "nothing"), a)
			d := filepath.Join(a, "data")
			return d, wont + a + " is a symbolic link to nothing, and serve cannot make " + d + " through it", pointTheLinkHint
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			dataDir, says, hint := tc.lay(t, base)
			said := askBothDoctors(t, writeDataDirConfig(t, base, dataDir, false))
			c := said.requireLine(t, dataDirCheckSlug, doctor.Fail)
			if c.Summary != says || c.Hint != hint {
				t.Errorf("the data-dir line says %q, hint %q; want %q, hint %q", c.Summary, c.Hint, says, hint)
			}
			said.requirePrinted(t, c)
		})
	}
}

// resolvedOrFail is where path's links lead.
func resolvedOrFail(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// TestBothDoctorsTakeADataDirectoryServeCanPrepare: a data directory that is
// a directory, or a link to one, and one not made yet below a directory or a
// link to one, are ok in both doctors, word for word, with no hint: the first
// two named as a directory, the last two as not made yet, which serve makes.
// The doctors make nothing (a data directory not made yet is still not there
// after both have run), and serve's preparation takes each.
func TestBothDoctorsTakeADataDirectoryServeCanPrepare(t *testing.T) {
	for _, tc := range []struct {
		name string
		// lay lays the case out below base, and returns the data directory
		// and whether it is there.
		lay func(t *testing.T, base string) (dataDir string, there bool)
	}{
		{"a directory", func(t *testing.T, base string) (string, bool) {
			d := filepath.Join(base, "data")
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			return d, true
		}},
		{"a link to a directory", func(t *testing.T, base string) (string, bool) {
			target := filepath.Join(base, "target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			d := filepath.Join(base, "data")
			symlinkOrSkip(t, target, d)
			return d, true
		}},
		{"not made yet, below a directory", func(t *testing.T, base string) (string, bool) {
			return filepath.Join(base, "config", "data"), false
		}},
		{"not made yet, below a link to a directory", func(t *testing.T, base string) (string, bool) {
			target := filepath.Join(base, "target")
			if err := os.Mkdir(target, 0o700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(base, "link")
			symlinkOrSkip(t, target, link)
			return filepath.Join(link, "sub", "data"), false
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			dataDir, there := tc.lay(t, base)
			said := askBothDoctors(t, writeDataDirConfig(t, base, dataDir, false))
			c := said.requireLine(t, dataDirCheckSlug, doctor.OK)
			want := dataDir + " is a directory"
			if !there {
				want = dataDir + " does not exist yet; bridge serve makes it"
			}
			if c.Summary != want || c.Hint != "" {
				t.Errorf("the data-dir line says %q, hint %q; want %q and no hint", c.Summary, c.Hint, want)
			}
			said.requirePrinted(t, c)
			if _, err := os.Lstat(dataDir); !there && !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the doctors made the data directory %s (lstat: %v), want nothing made", dataDir, err)
			}
			if err := prepareServeDataDir(dataDir); err != nil {
				t.Errorf("prepareServeDataDir(%s) = %v, want the data directory taken", dataDir, err)
			}
		})
	}
}

// notLoadedHint is the data-dir line's fix where the config cannot be
// loaded: config-file's.
const notLoadedHint = "config-file above says why; the data directory is checked once the config loads"

// TestTheDataDirLineIsNotCheckedWhereTheConfigCannotBeLoaded: a config that
// is there and does not load, and a named config that is not there, give
// bridge doctor no data directory it can trust, so the data-dir line says it
// was not checked and why, a warn, in both doctors (Codex's check on the
// brief), never a verdict on a data directory it guessed; and a run that finds
// no config at all, before bridge init, has no such line, since there is no
// config for serve to refuse.
func TestTheDataDirLineIsNotCheckedWhereTheConfigCannotBeLoaded(t *testing.T) {
	t.Run("a config that does not load", func(t *testing.T) {
		base := t.TempDir()
		path := writeDataDirConfig(t, base, filepath.Join(base, "data"), false)
		if err := os.WriteFile(path, []byte(mustRead(t, path)+"noSuchSetting: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		said := askBothDoctors(t, path)
		c := said.requireLine(t, dataDirCheckSlug, doctor.Warn)
		if want := "not checked: the config that sets the data directory does not load"; c.Summary != want || c.Hint != notLoadedHint {
			t.Errorf("the data-dir line says %q, hint %q; want %q, hint %q", c.Summary, c.Hint, want, notLoadedHint)
		}
		said.requirePrinted(t, c)
	})
	t.Run("a named config that is not there", func(t *testing.T) {
		said := askBothDoctors(t, filepath.Join(t.TempDir(), "bridge.yaml"))
		c := said.requireLine(t, dataDirCheckSlug, doctor.Warn)
		if want := "not checked: the named config does not exist"; c.Summary != want || c.Hint != notLoadedHint {
			t.Errorf("the data-dir line says %q, hint %q; want %q, hint %q", c.Summary, c.Hint, want, notLoadedHint)
		}
		said.requirePrinted(t, c)
	})
	t.Run("no config found, before bridge init", func(t *testing.T) {
		isolateConfigEnv(t)
		if out := runDoctor(t); reportLine(out, dataDirCheckSlug) != "" || !strings.Contains(out, "none found") {
			t.Errorf("a run that finds no config printed\n%s\nwant config-file's none found and no %s line", out, dataDirCheckSlug)
		}
	})
}
