package config

import (
	"fmt"
	"os"
	"slices"
	"testing"
)

// tailscale.uploadDiagnosticLogs (backlog B749): the embedded node uploads
// Tailscale's diagnostic logs only where the config says true, and every save
// the console and the CLI make keeps what the file says.

// TestTheUploadConsentIsOffUnlessTheConfigSaysTrue: absent and false are off,
// true is on, and a value that is no boolean is refused as every boolean
// key's is.
func TestTheUploadConsentIsOffUnlessTheConfigSaysTrue(t *testing.T) {
	for _, c := range []struct {
		body string
		want bool
	}{
		{"", false},
		{"tailscale:\n  mode: tsnet\n", false},
		{"tailscale:\n  mode: tsnet\n  uploadDiagnosticLogs: false\n", false},
		{"tailscale:\n  mode: tsnet\n  uploadDiagnosticLogs: true\n", true},
	} {
		if got := loadAt(t, envSaveConfig(t, c.body)).Tailscale.UploadDiagnosticLogs; got != c.want {
			t.Errorf("%q: uploadDiagnosticLogs = %v, want %v", c.body, got, c.want)
		}
	}
	if _, err := Load(envSaveConfig(t, "tailscale:\n  mode: tsnet\n  uploadDiagnosticLogs: please\n")); err == nil {
		t.Error("a word for uploadDiagnosticLogs loaded")
	}
}

// TestTheUploadConsentSurvivesEverySave: the console's saves (the live
// holder's UpdateChecked, which every console writer of bridge.yaml goes
// through) and the CLI's (config.Edit, and a save in a turn of the file's
// lock, which bind and init's check make) keep the key the file holds while
// they change another setting.
func TestTheUploadConsentSurvivesEverySave(t *testing.T) {
	const body = "libraryName: Before\ntailscale:\n  mode: tsnet\n  uploadDiagnosticLogs: true\n"
	for name, save := range map[string]func(t *testing.T, path string){
		"the console's holder": func(t *testing.T, path string) {
			if err := bootHolder(t, path).UpdateChecked(path, func(c *Config, _ func() error) error {
				c.LibraryName = "After"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		},
		"config.Edit": func(t *testing.T, path string) {
			if err := Edit(path, func(c *Config) (bool, error) {
				c.LibraryName = "After"
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
		},
		"a save in a turn of the lock": func(t *testing.T, path string) {
			l, err := LockFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer l.Unlock()
			cfg, err := l.LoadFound()
			if err != nil {
				t.Fatal(err)
			}
			cfg.LibraryName = "After"
			if err := cfg.Save(l); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := envSaveConfig(t, body)
			save(t, path)
			cfg := loadAt(t, path)
			if cfg.LibraryName != "After" || !cfg.Tailscale.UploadDiagnosticLogs {
				t.Errorf("after the save: libraryName %q, uploadDiagnosticLogs %v; want the change and the consent kept",
					cfg.LibraryName, cfg.Tailscale.UploadDiagnosticLogs)
			}
			if v, ok := fileLeaf(t, path, "tailscale.uploadDiagnosticLogs"); !ok || v != true {
				t.Errorf("the saved file writes uploadDiagnosticLogs as %v (present %v), want true", v, ok)
			}
		})
	}
}

// TestAnEmptyVariableLeavesTheFilesConsent: compose.yaml forwards
// BRIDGE_TAILSCALE_UPLOAD_DIAGNOSTIC_LOGS as
// "${BRIDGE_TAILSCALE_UPLOAD_DIAGNOSTIC_LOGS:-}", so a container whose .env
// leaves it out gets it set and empty, never unset: the load takes that for
// no override, leaving what bridge.yaml says, a true and a false alike, and
// records no leaf for it. It pins what the overrides do (an empty variable is
// a no-op, applyEnvOverrides), not a change.
func TestAnEmptyVariableLeavesTheFilesConsent(t *testing.T) {
	const variable = "BRIDGE_TAILSCALE_UPLOAD_DIAGNOSTIC_LOGS"
	for _, file := range []bool{true, false} {
		t.Run(fmt.Sprintf("bridge.yaml says %v", file), func(t *testing.T) {
			path := envSaveConfig(t, fmt.Sprintf("tailscale:\n  mode: tsnet\n  uploadDiagnosticLogs: %v\n", file))
			t.Setenv(variable, "")
			if v, ok := os.LookupEnv(variable); !ok || v != "" {
				t.Fatalf("%s is %q (set: %v), want it set and empty", variable, v, ok)
			}
			cfg := loadAt(t, path)
			if got := cfg.Tailscale.UploadDiagnosticLogs; got != file {
				t.Errorf("with %s set and empty, uploadDiagnosticLogs = %v, want bridge.yaml's %v", variable, got, file)
			}
			if slices.Contains(cfg.envSet, "tailscale.uploadDiagnosticLogs") {
				t.Errorf("the load records the empty %s as setting the key", variable)
			}
		})
	}
}

// TestAConsentGivenByTheVariableIsNotWrittenByAnotherSave: with
// BRIDGE_TAILSCALE_UPLOAD_DIAGNOSTIC_LOGS=true at the start and no key in the
// file, a console save of another setting writes no key (backlog B761's rule),
// so a start without the variable uploads nothing.
func TestAConsentGivenByTheVariableIsNotWrittenByAnotherSave(t *testing.T) {
	const variable = "BRIDGE_TAILSCALE_UPLOAD_DIAGNOSTIC_LOGS"
	if got := envName("tailscale.uploadDiagnosticLogs"); got != variable {
		t.Fatalf("the key's variable is %s, want %s", got, variable)
	}
	path := envSaveConfig(t, "libraryName: Before\ntailscale:\n  mode: tsnet\n")
	t.Setenv(variable, "true")
	rc := bootHolder(t, path)
	if !rc.Load().Tailscale.UploadDiagnosticLogs {
		t.Fatal("the variable did not set the key at the start")
	}
	if err := rc.UpdateChecked(path, func(c *Config, _ func() error) error {
		c.LibraryName = "After"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if v, ok := fileLeaf(t, path, "tailscale.uploadDiagnosticLogs"); ok {
		t.Errorf("a save of another setting wrote the variable's consent into the file: %v", v)
	}
	withoutVariables(t, map[string]string{variable: "true"}, func() {
		if loadAt(t, path).Tailscale.UploadDiagnosticLogs {
			t.Error("a start without the variable loads the consent")
		}
	})
}
