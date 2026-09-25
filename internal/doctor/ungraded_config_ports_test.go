package doctor

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// ungradedConfigPath is where the configs below claim to live. The port
// checks never read it: they classify the lookup's error, as config-file
// does.
const ungradedConfigPath = "/srv/bridge/bridge.yaml"

// ungradedConfig is a config that was named or found and did not load,
// as cmd/bridge's lookup records it, with the words each port line must
// give as its reason.
type ungradedConfig struct {
	name   string
	err    error
	reason string
}

// unreadableConfig is config.Load's wrapping of an open this user may not
// make: the reported case, a service-owned config read by another user.
var unreadableConfig = ungradedConfig{"not readable by this user",
	fmt.Errorf("read config %q: %w", ungradedConfigPath,
		&fs.PathError{Op: "open", Path: ungradedConfigPath, Err: fs.ErrPermission}),
	"not readable by this user"}

// missingConfig is os.Stat's answer for a --config naming a file that is
// not there.
var missingConfig = ungradedConfig{"named and not there",
	&fs.PathError{Op: "stat", Path: ungradedConfigPath, Err: fs.ErrNotExist},
	"does not exist"}

// brokenConfig is a decode error: the runbook's validate-before-restart
// run on a config edit with a typo in it.
var brokenConfig = ungradedConfig{"there and does not load",
	errors.New(`parse config "/srv/bridge/bridge.yaml": yaml: unmarshal errors:
  line 6: field libraryNmae not found in type config.Config`),
	"does not load"}

// TestPortChecksDoNotGradeTheDefaultsOfAConfigThatDidNotLoad pins what the
// port lines say when a config was named or found and did not load.
//
// cmd/bridge seeds Deps with the default ports 7788 / 7789 and replaces
// them only from a config that loads, and the pid file's path comes from
// the same config. So the ports here are a guess, with no pid file to
// recognise the bridge by. `bridge doctor` run by a user who cannot read
// a service-owned config FAILed both ports against the live bridge's own
// listeners, "another process owns this port", on every host (#1021's
// table). On an install that does not use the defaults it reported them
// free instead, a pass about ports nothing binds. Both verdicts are about
// a guess, so the lines say they were not checked, and why, whether the
// guessed port is bound or free.
//
// ok, not warn: config-file reports the fact behind all three lines once,
// at the severity #985 chose for it (a warn for a config this user cannot
// read, a fail for one that is not there or does not load).
func TestPortChecksDoNotGradeTheDefaultsOfAConfigThatDidNotLoad(t *testing.T) {
	for _, cfg := range []ungradedConfig{unreadableConfig, missingConfig, brokenConfig} {
		for _, port := range []struct {
			name string
			take func(*testing.T) int
		}{
			// The reported case: something, the bridge itself in the
			// report, holds the guessed port.
			{"guessed port bound", bindPort},
			// The pass beside it: a free guess says nothing about the
			// port the config sets.
			{"guessed port free", mustFreePort},
		} {
			t.Run(cfg.name+", "+port.name, func(t *testing.T) {
				p := port.take(t)
				d := Deps{
					ConfigFile: &ConfigFile{Path: ungradedConfigPath, LoadErr: cfg.err},
					APIPort:    p,
					AdminPort:  p,
				}
				for _, c := range []Check{checkAPIPort(t.Context(), d), checkAdminPort(t.Context(), d)} {
					assertNotChecked(t, c, cfg.reason, p)
				}
			})
		}
	}

	// An in-process caller (the console, inside `bridge serve`) that bound
	// the guessed port itself. It still says nothing about the port the
	// config would set, so the answer is the same, not "bound by this
	// bridge".
	t.Run("the guessed port is one the caller bound", func(t *testing.T) {
		p := bindPort(t)
		d := Deps{
			ConfigFile: &ConfigFile{Path: ungradedConfigPath, LoadErr: brokenConfig.err},
			APIPort:    p,
			AdminPort:  p,
			OwnedPorts: []int{p},
		}
		for _, c := range []Check{checkAPIPort(t.Context(), d), checkAdminPort(t.Context(), d)} {
			assertNotChecked(t, c, brokenConfig.reason, p)
		}
	})
}

// assertNotChecked requires c to be the "not checked" answer, giving
// reason, and not naming the guessed port, which would read as the port
// the install uses.
func assertNotChecked(t *testing.T, c Check, reason string, guessed int) {
	t.Helper()
	if c.Status != OK || !strings.HasPrefix(c.Summary, "not checked: ") || !strings.Contains(c.Summary, reason) {
		t.Errorf("%s = %s %q (hint %q), want ok \"not checked: …%s…\"", c.Name, c.Status, c.Summary, c.Hint, reason)
	}
	if strings.Contains(c.Summary, strconv.Itoa(guessed)) {
		t.Errorf("%s names the guessed port %d: %q", c.Name, guessed, c.Summary)
	}
}

// TestPortChecksStillGradeTheDefaultsWhenNoConfigFailedToLoad is the
// control for the test above: only a config that was named or found and
// did not load turns the port checks off. With nothing found and nothing
// named, `bridge doctor` runs before `bridge init`, and the defaults are
// the ports init will write, so a held one is still a conflict. `bridge
// init`'s preflight and its second port pass look nothing up (a nil
// lookup) and grade the ports they were handed. A config that loaded
// names its own ports, and a held one with no pid of ours behind it
// FAILs as before.
func TestPortChecksStillGradeTheDefaultsWhenNoConfigFailedToLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		file *ConfigFile
	}{
		{"nothing named, nothing found", &ConfigFile{Tried: []string{"/home/b/.config/1-bit-bridge/bridge.yaml"}}},
		{"no lookup", nil},
		{"a config that loaded", &ConfigFile{Path: ungradedConfigPath}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := bindPort(t)
			d := Deps{ConfigFile: tc.file, APIPort: p, AdminPort: p}
			for _, c := range []Check{checkAPIPort(t.Context(), d), checkAdminPort(t.Context(), d)} {
				if c.Status != Fail {
					t.Errorf("%s = %s %q with :%d held and no pid of ours, want fail", c.Name, c.Status, c.Summary, p)
				}
			}
		})
	}
}
