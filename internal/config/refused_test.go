package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDataDirOfIsTheDataDirLoadGives holds the early rollback's lenient read
// of the data dir to Load's own answer, for every way a config names it: the
// key absent, empty or null, relative or absolute, through an alias or a
// merge key, tagged, and BRIDGE_DATA_DIR over it, set or set empty, for a
// config named by a relative path, an absolute one, and one through a link.
func TestDataDirOfIsTheDataDirLoadGives(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(dir, "elsewhere", "data")
	settings := map[string]string{
		"no key":       "",
		"empty":        "dataDir: \"\"\n",
		"null":         "dataDir: null\n",
		"relative":     "dataDir: state\n",
		"dot relative": "dataDir: ./a/../state\n",
		"absolute":     "dataDir: " + yamlStr(abs) + "\n",
		"tagged":       "dataDir: !!str state\n",
		"a number":     "dataDir: 5\n",
		"an alias":     "dataDir: &d state\nlibraryName: *d\n",
	}
	envs := map[string]*string{"no env": nil, "env set": ptr(filepath.Join(dir, "env-data")), "env relative": ptr("env-rel"), "env empty": ptr("")}
	for name, body := range settings {
		for envName, env := range envs {
			t.Run(name+", "+envName, func(t *testing.T) {
				if env != nil {
					t.Setenv("BRIDGE_DATA_DIR", *env)
				}
				sub := t.TempDir()
				cfg := filepath.Join(sub, "bridge.yaml")
				if err := os.WriteFile(cfg, []byte("libraryRoots:\n  - "+yamlStr(lib)+"\n"+body), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(t.TempDir(), "linked")
				paths := []string{cfg}
				if os.Symlink(sub, link) == nil {
					paths = append(paths, filepath.Join(link, "bridge.yaml"))
				}
				for _, p := range paths {
					loaded, err := Load(p)
					if err != nil {
						t.Fatalf("Load(%s): %v", p, err)
					}
					got, err := DataDirOf(p)
					if err != nil {
						t.Fatalf("DataDirOf(%s): %v", p, err)
					}
					if got != loaded.DataDir {
						t.Errorf("DataDirOf(%s) = %s, Load gives %s", p, got, loaded.DataDir)
					}
				}
			})
		}
	}
	t.Run("a relative config name", func(t *testing.T) {
		sub := t.TempDir()
		if err := os.WriteFile(filepath.Join(sub, "bridge.yaml"), []byte("libraryRoots:\n  - "+yamlStr(lib)+"\ndataDir: state\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(sub)
		loaded, err := Load("bridge.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if got, err := DataDirOf("bridge.yaml"); err != nil || got != loaded.DataDir {
			t.Errorf("DataDirOf(bridge.yaml) = %s, %v; Load gives %s", got, err, loaded.DataDir)
		}
	})
	t.Run("a merge key", func(t *testing.T) {
		sub := t.TempDir()
		cfg := filepath.Join(sub, "bridge.yaml")
		if err := os.WriteFile(cfg, []byte("libraryRoots:\n  - "+yamlStr(lib)+"\n<<: {dataDir: merged}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := DataDirOf(cfg); err != nil || got != loaded.DataDir || got != filepath.Join(sub, "merged") {
			t.Errorf("DataDirOf = %s, %v; Load gives %s", got, err, loaded.DataDir)
		}
	})
}

// TestDataDirOfReadsTheDataDirOfAConfigLoadRefuses: the setting a version
// refuses is what brings the early rollback to this read, so it must not
// stop it.
func TestDataDirOfReadsTheDataDirOfAConfigLoadRefuses(t *testing.T) {
	for name, bad := range map[string]string{
		"a leading zero":             "scanIntervalSec: 010\n",
		"a fraction":                 "scanIntervalSec: 21600.5\n",
		"a key it does not know":     "notASetting: 1\n",
		"a setting Validate refuses": "listenAddress: \":99999\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := filepath.Join(dir, "bridge.yaml")
			if err := os.WriteFile(cfg, []byte("dataDir: state\n"+bad), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(cfg); err == nil {
				t.Fatal("Load took the setting")
			}
			if got, err := DataDirOf(cfg); err != nil || got != filepath.Join(dir, "state") {
				t.Errorf("DataDirOf = %s, %v; want %s", got, err, filepath.Join(dir, "state"))
			}
		})
	}
}

// TestDataDirOfRefusesADataDirItCannotBeSureOf: where the data dir is not
// certain, the read says so and names none.
func TestDataDirOfRefusesADataDirItCannotBeSureOf(t *testing.T) {
	for name, body := range map[string]string{
		"written twice":     "dataDir: a\ndataDir: b\n",
		"a list":            "dataDir: [a, b]\n",
		"a map":             "dataDir: {a: b}\n",
		"not YAML":          "dataDir: [a\n",
		"an empty file":     "",
		"a list at the top": "- dataDir: a\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "bridge.yaml")
			if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := DataDirOf(cfg); err == nil {
				t.Errorf("DataDirOf = %s, want an error", got)
			}
		})
	}
	if _, err := DataDirOf(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Error("DataDirOf of a missing file answered")
	}
}

// TestALoadRefusalGivesItsReasonsWithoutTheFilesName: what Load says is what
// it always said, and the same refusal's reasons, one a line, name no path,
// for the settings check's lines.
func TestALoadRefusalGivesItsReasonsWithoutTheFilesName(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		reasons    []string
	}{
		{"two leading zeros", "scanIntervalSec: 010\nbackup:\n  keep: 07\n", []string{
			"line 1: a whole number is written with a leading zero, " + leadingZeroRemedy,
			"line 3: a whole number is written with a leading zero, " + leadingZeroRemedy}},
		{"a key it does not know", "notASetting: 1\n", []string{"line 1: that key is not a field of type config.Config"}},
		{"not YAML", "dataDir: [a\n", nil},
		{"a setting Validate refuses", "listenAddress: \":99999\"\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lib := filepath.Join(dir, "Music")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(dir, "bridge.yaml")
			if err := os.WriteFile(cfg, []byte(tc.body+"libraryRoots:\n  - "+yamlStr(lib)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(cfg)
			var refused *RefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("Load = %v, want a RefusedError", err)
			}
			reasons := refused.Reasons()
			if len(reasons) == 0 {
				t.Fatal("no reasons")
			}
			if tc.reasons != nil && strings.Join(reasons, "\n") != strings.Join(tc.reasons, "\n") {
				t.Errorf("Reasons = %q, want %q", reasons, tc.reasons)
			}
			for _, r := range reasons {
				if strings.Contains(r, dir) || strings.Contains(r, "\n") {
					t.Errorf("a reason names a path or spans lines: %q", r)
				}
			}
		})
	}
	t.Run("a file it cannot read is no refusal", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
		var refused *RefusedError
		if err == nil || errors.As(err, &refused) {
			t.Errorf("Load of a missing file = %v, want an error that is no RefusedError", err)
		}
	})
}
