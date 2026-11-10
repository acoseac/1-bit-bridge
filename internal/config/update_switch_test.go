package config

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The update check's off switch (backlog B761): update.checkEnabled, on where
// the file writes no key, and BRIDGE_UPDATE_CHECK_ENABLED, the name the struct
// derives for it, which reads as the enrichment opt-outs' variables do
// (switchEnvPaths): as the file reads the key, without regard to case, and a
// value that reads as neither on nor off as off, since an opt-out misread as
// on asks GitHub what the operator asked the bridge to stop asking.

const updateCheckEnv = "BRIDGE_UPDATE_CHECK_ENABLED"

// clearUpdateCheckEnv removes the switch's variable for the test, so a value
// a developer's shell carries cannot decide a case; Setenv first restores
// whatever was there when the test ends.
func clearUpdateCheckEnv(t *testing.T) {
	t.Helper()
	t.Setenv(updateCheckEnv, "")
	os.Unsetenv(updateCheckEnv)
}

// TestTheUpdateCheckSwitchLoads: the key loads and reads as it says, on where
// the file writes none, and leaves the auto-installer's own setting as the
// file writes it; its variable, by the name the struct derives, sets it over
// the file in every spelling of on and off the file takes, says nothing when
// empty or white space alone, and turns it off, with one Warn naming the
// variable and not its value, where it reads as neither.
func TestTheUpdateCheckSwitchLoads(t *testing.T) {
	type switchCase struct {
		name        string
		yaml        string
		env         *string
		on          bool
		autoInstall bool
		unread      bool
	}
	value := func(s string) *string { return &s }
	cases := []switchCase{
		{name: "no key", on: true},
		{name: "checkEnabled: false", yaml: "update:\n  checkEnabled: false\n"},
		{name: "checkEnabled: true", yaml: "update:\n  checkEnabled: true\n", on: true},
		{name: "the file's off", yaml: "update:\n  checkEnabled: off\n"},
		{name: "the file's No", yaml: "update:\n  checkEnabled: No\n"},
		{name: "the file's ON", yaml: "update:\n  checkEnabled: ON\n", on: true},
		{name: "the file's yes", yaml: "update:\n  checkEnabled: yes\n", on: true},
		{name: "off keeps the auto-installer's setting", yaml: "update:\n  checkEnabled: false\n  autoInstall: true\n", autoInstall: true},
	}
	for _, val := range switchEnvOn {
		cases = append(cases, switchCase{name: updateCheckEnv + "=" + val + " over the file's false",
			yaml: "update:\n  checkEnabled: false\n", env: value(val), on: true})
	}
	for _, val := range switchEnvOff {
		cases = append(cases, switchCase{name: updateCheckEnv + "=" + val, env: value(val)})
	}
	for _, val := range []string{"", "   "} {
		cases = append(cases,
			switchCase{name: updateCheckEnv + "=" + val + " says nothing over the file's false",
				yaml: "update:\n  checkEnabled: false\n", env: value(val)},
			switchCase{name: updateCheckEnv + "=" + val + " says nothing over no key", env: value(val), on: true})
	}
	for _, val := range switchEnvUnread {
		cases = append(cases, switchCase{name: updateCheckEnv + "=" + val + " over the file's true",
			yaml: "update:\n  checkEnabled: true\n", env: value(val), unread: true})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearUpdateCheckEnv(t)
			if c.env != nil {
				t.Setenv(updateCheckEnv, *c.env)
			}
			rec := loggingtest.Record(t)
			cfg, err := Load(writeEnrichTestConfig(t, c.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Update.EffectiveCheckEnabled(); got != c.on {
				t.Errorf("update checks on = %v, want %v", got, c.on)
			}
			if cfg.Update.AutoInstall != c.autoInstall {
				t.Errorf("update.autoInstall = %v, want %v", cfg.Update.AutoInstall, c.autoInstall)
			}
			lines := rec.Lines(unreadableSwitchEnv)
			switch {
			case !c.unread && len(lines) != 0:
				t.Errorf("a value that reads warned:\n%s", strings.Join(lines, "\n"))
			case c.unread && (len(lines) != 1 || !strings.Contains(lines[0], "var="+updateCheckEnv)):
				t.Errorf("an unreadable %s warned %d time(s), want once naming it:\n%s", updateCheckEnv, len(lines), strings.Join(lines, "\n"))
			}
			if c.unread {
				if got := rec.Carrying(*c.env); len(got) != 0 {
					t.Errorf("a line carries the unreadable value:\n%s", strings.Join(got, "\n"))
				}
			}
		})
	}
}

// TestTheUpdateCheckSwitchSavesReloadsAndClones: the holder's save writes the
// switch it changes, off as false and on as no key, as the enrichment
// opt-outs are written, a start reads back what was written, and a clone
// holds a switch of its own.
func TestTheUpdateCheckSwitchSavesReloadsAndClones(t *testing.T) {
	clearUpdateCheckEnv(t)
	path := envSaveConfig(t, "")
	rc := bootHolder(t, path)
	for _, step := range []struct {
		name  string
		on    bool
		filed any
		there bool
	}{
		{"turned off", false, false, true},
		{"turned on", true, nil, false},
		{"turned off again", false, false, true},
	} {
		if err := rc.Update(path, func(next *Config) error {
			v := step.on
			next.Update.CheckEnabled = &v
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got, there := fileLeaf(t, path, "update.checkEnabled"); there != step.there || got != step.filed {
			t.Errorf("%s: the file holds update.checkEnabled = %#v (there %v), want %#v (there %v)",
				step.name, got, there, step.filed, step.there)
		}
		if got := rc.Load().Update.EffectiveCheckEnabled(); got != step.on {
			t.Errorf("%s: the holder publishes update checks %v, want %v", step.name, got, step.on)
		}
		if got := loadAt(t, path).Update.EffectiveCheckEnabled(); got != step.on {
			t.Errorf("%s: a start reads update checks %v, want %v", step.name, got, step.on)
		}
	}

	cfg := loadAt(t, path)
	clone := Clone(cfg)
	if clone.Update.CheckEnabled == cfg.Update.CheckEnabled {
		t.Fatal("a clone shares the switch's pointer with the config it was cloned from")
	}
	*clone.Update.CheckEnabled = true
	if cfg.Update.EffectiveCheckEnabled() {
		t.Error("turning a clone's switch on turned the original's on")
	}
}

// TestAConsoleSaveKeepsTheFilesOwnUpdateCheckSwitch: a console save of another
// setting writes the switch as the file holds it, read in the save's turn,
// whatever BRIDGE_UPDATE_CHECK_ENABLED made it at the start, and the running
// bridge keeps the variable's value; a save that changes the switch writes the
// change, off as false and on as no key, and publishes it (filedSwitches, the
// enrichment opt-outs' rule since B617). So the variable never lands in the
// file, a file-owned false is never lost, and a start without the variable
// finds what the operator chose.
func TestAConsoleSaveKeepsTheFilesOwnUpdateCheckSwitch(t *testing.T) {
	for _, c := range []struct {
		name  string
		file  string
		env   string
		live  bool
		filed any
		there bool
	}{
		{name: "the file's false, no variable", file: "update:\n  checkEnabled: false\n", live: false, filed: false, there: true},
		{name: "the variable off over a file that says nothing", env: "false", live: false},
		{name: "the variable off over the file's true", file: "update:\n  checkEnabled: true\n", env: "off", live: false, filed: true, there: true},
		{name: "the variable on over the file's false", file: "update:\n  checkEnabled: false\n", env: "on", live: true, filed: false, there: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			clearUpdateCheckEnv(t)
			if c.env != "" {
				t.Setenv(updateCheckEnv, c.env)
			}
			path := envSaveConfig(t, c.file)
			rc := bootHolder(t, path)
			if got := rc.Load().Update.EffectiveCheckEnabled(); got != c.live {
				t.Fatalf("booted, update checks are %v, want %v", got, c.live)
			}
			for i := 1; i <= 2; i++ {
				if err := rc.Update(path, func(next *Config) error {
					next.LibraryName = fmt.Sprintf("Saved %d", i)
					return nil
				}); err != nil {
					t.Fatalf("save %d: %v", i, err)
				}
				if got, there := fileLeaf(t, path, "update.checkEnabled"); there != c.there || got != c.filed {
					t.Errorf("save %d of another setting: the file holds update.checkEnabled = %#v (there %v), want the file's own %#v (there %v)",
						i, got, there, c.filed, c.there)
				}
				if got := rc.Load().Update.EffectiveCheckEnabled(); got != c.live {
					t.Errorf("save %d of another setting: the running switch is %v, want %v", i, got, c.live)
				}
			}
			withoutVariables(t, map[string]string{updateCheckEnv: c.env}, func() {
				want := c.there && c.filed == true || !c.there
				if got := loadAt(t, path).Update.EffectiveCheckEnabled(); got != want {
					t.Errorf("a start without the variable reads update checks %v, want the file's own %v", got, want)
				}
			})

			// A change is the operator's choice: written and run.
			if err := rc.Update(path, func(next *Config) error {
				v := !c.live
				next.Update.CheckEnabled = &v
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			wantFiled, wantThere := any(nil), false
			if c.live {
				wantFiled, wantThere = false, true
			}
			if got, there := fileLeaf(t, path, "update.checkEnabled"); there != wantThere || got != wantFiled {
				t.Errorf("the switch changed to %v: the file holds update.checkEnabled = %#v (there %v), want %#v (there %v)",
					!c.live, got, there, wantFiled, wantThere)
			}
			if got := rc.Load().Update.EffectiveCheckEnabled(); got != !c.live {
				t.Errorf("the switch changed: the running switch is %v, want %v", got, !c.live)
			}
		})
	}
}

// TestTheUpdatesControlOwnsTheUpdateCheckSwitch: a deployment whose control
// plane owns the updates control owns the switch too, so the console hides it
// and the settings save refuses it (managedControlSettings), as it owns the
// cadence and the auto-installer's settings: on a fleet that shares one
// binary, whether the bridge asks GitHub, and installs, is the fleet's.
func TestTheUpdatesControlOwnsTheUpdateCheckSwitch(t *testing.T) {
	d := DeploymentConfig{ManagedControls: []string{ManagedControlUpdates}}
	if !d.IsManagedSettingOrImplied("updateCheckEnabled") {
		t.Error("the updates control does not own updateCheckEnabled")
	}
	if !slices.Contains(d.EffectiveManagedSettings(), "updateCheckEnabled") {
		t.Errorf("the console is told the managed settings %v, without updateCheckEnabled", d.EffectiveManagedSettings())
	}
	if (DeploymentConfig{ManagedControls: []string{ManagedControlRestart}}).IsManagedSettingOrImplied("updateCheckEnabled") {
		t.Error("the restart control owns updateCheckEnabled: turning checks on or off restarts nothing")
	}
}
