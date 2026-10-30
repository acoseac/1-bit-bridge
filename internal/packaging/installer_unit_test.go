package packaging

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The one-line installer (scripts/install.sh) finds the bridge it upgrades in
// the per-user unit bridge init writes: its read_unit takes the program and
// the config from the ExecStart line of this package's systemd template, and
// refuses any path the template had to escape, rather than read it wrong.
// This renders the template and runs the installer's own function on it.
func TestTheInstallerReadsTheUnitInitWrites(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the installer is a POSIX shell script for Linux")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		binary, config string
		read           bool
	}{
		{"/home/pi/.local/bin/bridge", "/home/pi/.config/1-bit-bridge/bridge.yaml", true},
		{"/home/pi/My Apps/bridge", "/home/pi/Bridge Settings/bridge.yaml", true},
		{"/srv/50% off/bridge", "/home/pi/.config/1-bit-bridge/bridge.yaml", false},
		{"/home/pi/.local/bin/bridge", "/srv/$HOME/bridge.yaml", false},
		{`/srv/a"b/bridge`, "/home/pi/.config/1-bit-bridge/bridge.yaml", false},
		{`/srv/a\b/bridge`, "/home/pi/.config/1-bit-bridge/bridge.yaml", false},
	}
	for _, c := range cases {
		t.Run(c.binary+" "+c.config, func(t *testing.T) {
			body, err := render("systemd.service.tmpl", Params{
				BinaryPath: c.binary,
				ConfigPath: c.config,
				WorkingDir: "/home/pi/.config/1-bit-bridge/data",
				LogPath:    "/home/pi/.local/state/1-bit-bridge/bridge.log",
			})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			unit := filepath.Join(t.TempDir(), ServiceLabel+systemdUnitSuffix)
			if err := os.WriteFile(unit, body, 0o644); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(sh, "-c", `. "$1" && read_unit "$2" && printf '%s\n%s\n' "$INSTALLED_BIN" "$INSTALLED_CONFIG"`, "sh", script, unit)
			cmd.Env = append(os.Environ(), "BRIDGE_INSTALL_TEST_SOURCE=1")
			out, err := cmd.Output()
			if !c.read {
				if err == nil {
					t.Errorf("the installer read a unit whose paths the template escaped as %q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("the installer refused the unit init writes: %v\n%s", err, body)
			}
			if got, want := string(out), c.binary+"\n"+c.config+"\n"; got != want {
				t.Errorf("the installer read %q from\n%s\nwant %q", got, body, want)
			}
			if !strings.Contains(string(body), `ExecStart="`) {
				t.Errorf("the template no longer quotes ExecStart's program")
			}
		})
	}
}
