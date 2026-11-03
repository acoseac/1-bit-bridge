//go:build !windows

package installtest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoInitAdviceMatchesTheHost runs the real entry point without a download
// or a socket, and checks the commands as printed, including elogind's
// loginctl on a host without systemd. Download and installation are stubbed.
func TestNoInitAdviceMatchesTheHost(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		for _, systemd := range []bool{false, true} {
			for _, name := range []string{"tester", "1000", "", "lookup failed"} {
				t.Run(fmt.Sprintf("systemd=%t/name=%q", systemd, name), func(t *testing.T) {
					r := newRigFiles(t, sh)
					if !systemd {
						if err := os.RemoveAll(filepath.Join(r.root, "run", "systemd")); err != nil {
							t.Fatal(err)
						}
					}
					_, group := r.playbackNode(0o660)
					id := readFile(t, filepath.Join(r.bin, "id"))
					reply := "printf '%s\\n' \"$TEST_USER_NAME\""
					if name == "lookup failed" {
						reply = "printf '1000\\n'; exit 1"
					}
					id = strings.Replace(id, `printf '%s\n' "${STUB_USER:-tester}"`, reply, 1)
					writeFile(t, filepath.Join(r.bin, "id"), []byte(id), 0o755)
					r.set("TEST_USER_NAME", name, "STUB_USER", name)
					// Source through the existing hook, then run main: detection,
					// flags and advice stay real; nothing needs a release server.
					r.script = filepath.Join(r.dir, "advice.sh")
					writeFile(t, r.script, []byte(`BRIDGE_INSTALL_TEST_SOURCE=1
. "$TEST_INSTALLER"
download_release() { :; }
install_binary() { :; }
main "$@"
`), 0o755)
					r.set("TEST_INSTALLER", stampedScript)
					res := r.runHeldOpen("--no-init")
					mustExit(t, res, 0)
					init := []string{"bridge", "init"}
					if !systemd {
						init = append(init, "--no-service")
					}
					want := [][]string{init}
					if systemd {
						user := name
						if name != "tester" {
							user = "1000"
						}
						want = append(want, []string{"loginctl", "enable-linger", user})
					}
					join, restart := audioCommands(group)
					if name == "tester" {
						want = append(want, join)
					} else {
						mustSay(t, res, "add the account with uid 1000 to "+group+" by its user name")
						if strings.Contains(res.output(), "usermod") {
							t.Errorf("unresolved account got a usermod command:\n%s", res.output())
						}
					}
					if systemd {
						want = append(want, restart)
						mustSay(t, res, "    sudo systemctl restart user@1000.service   # or reboot")
					} else {
						mustSay(t, res, "then log in again (or reboot) before you start the bridge, so that it runs with the group.")
						for _, forbidden := range []string{"systemctl", "loginctl", "starts again with it", "starts when this box boots"} {
							if strings.Contains(res.output(), forbidden) {
								t.Errorf("non-systemd advice contains %q:\n%s", forbidden, res.output())
							}
						}
					}
					sameCalls(t, r.runPrinted(printed(res.output()), r.target()), want)
					r.ranNoAudioCommand()
					if calls := r.mutations(); len(calls) != 0 {
						t.Errorf("advice ran commands: %q", calls)
					}
				})
			}
		}
	})
}
