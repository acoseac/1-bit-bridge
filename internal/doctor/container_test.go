package doctor

import (
	"context"
	"strings"
	"testing"
)

// TestInContainerSkipsTheHostOperatorChecks — backlog B190, measured in the
// Docker image: a healthy container's report ended "14 ok, 3 warn", and two of
// the warns were service-manager ("no user systemd session", use `bridge init
// --no-service`) and browser-opener ("no opener found"). The container
// runtime starts the bridge, and the image has no browser, so both told the
// reader to fix a host they are not on.
func TestInContainerSkipsTheHostOperatorChecks(t *testing.T) {
	ctx := context.Background()
	prevSM, prevBO := serviceManagerOnHost, browserOpenerOnHost
	t.Cleanup(func() { serviceManagerOnHost, browserOpenerOnHost = prevSM, prevBO })

	for _, host := range []struct {
		name    string
		sm, bo  Check
		failing bool
	}{
		{"a host where both checks fail", warn(checkNameServiceManager, "no user systemd session", "use `bridge init --no-service`"),
			warn(checkNameBrowserOpener, "no opener found", "install one"), true},
		{"a host where both checks pass (a system container with its own systemd, a shared opener)",
			ok(checkNameServiceManager, "systemctl --user reachable"), ok(checkNameBrowserOpener, "xdg-open"), false},
	} {
		t.Run(host.name, func(t *testing.T) {
			serviceManagerOnHost = func(context.Context) Check { return host.sm }
			browserOpenerOnHost = func() Check { return host.bo }

			in := Deps{InContainer: true}
			for name, pair := range map[string][2]Check{
				"service-manager": {checkServiceManager(ctx, in), host.sm},
				"browser-opener":  {checkBrowserOpener(ctx, in), host.bo},
			} {
				c, hostCheck := pair[0], pair[1]
				if c.Status != OK || c.Hint != "" {
					t.Errorf("%s in a container: %q %q (hint %q), want ok with no advice", name, c.Status, c.Summary, c.Hint)
				}
				if host.failing && !strings.Contains(c.Summary, "container") {
					t.Errorf("%s: the failing host check was not reported skipped for the container: %q", name, c.Summary)
				}
				if !host.failing && c.Summary != hostCheck.Summary {
					t.Errorf("%s: the container hid a passing check (%q, want %q)", name, c.Summary, hostCheck.Summary)
				}
			}

			// NEGATIVE CONTROL: outside a container the host's own answer,
			// advice included, is what the report says.
			out := Deps{}
			for name, pair := range map[string][2]Check{
				"service-manager": {checkServiceManager(ctx, out), host.sm},
				"browser-opener":  {checkBrowserOpener(ctx, out), host.bo},
			} {
				if pair[0] != pair[1] {
					t.Errorf("%s outside a container: %+v, want the host's %+v", name, pair[0], pair[1])
				}
			}
		})
	}
}

// TestInContainerKeepsTheChecksItsOperatorCanActOn — a container's operator
// has a shell (`docker exec`), unlike a managed tenant's reader, so only the
// two checks above are skipped. The log-size check is the one Managed also
// skips; here it still reports a log nothing rotates.
func TestInContainerKeepsTheChecksItsOperatorCanActOn(t *testing.T) {
	c := checkLogSize(context.Background(), Deps{InContainer: true, LogPath: mustBigLog(t)})
	if strings.Contains(c.Summary, "skipped") {
		t.Errorf("log-file-size skipped in a container: %q", c.Summary)
	}
	if c.Status != Warn {
		t.Errorf("log-file-size on an unrotated log in a container: status = %q, want warn", c.Status)
	}
}
