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

	in := Deps{InContainer: true}
	for name, c := range map[string]Check{
		"service-manager": checkServiceManager(ctx, in),
		"browser-opener":  checkBrowserOpener(ctx, in),
	} {
		if c.Status != OK {
			t.Errorf("%s in a container: status = %q, want ok", name, c.Status)
		}
		if !strings.Contains(c.Summary, "skipped") || !strings.Contains(c.Summary, "container") {
			t.Errorf("%s summary = %q, want it to say the check was skipped because of the container", name, c.Summary)
		}
		if c.Hint != "" {
			t.Errorf("%s still carries advice for the host: %q", name, c.Hint)
		}
	}

	// NEGATIVE CONTROL: outside a container both checks run. Their verdict
	// depends on the machine the suite runs on, so the assertion is that
	// they answered about THIS host rather than skipping.
	out := Deps{}
	for name, c := range map[string]Check{
		"service-manager": checkServiceManager(ctx, out),
		"browser-opener":  checkBrowserOpener(ctx, out),
	} {
		if strings.Contains(c.Summary, "skipped") {
			t.Errorf("%s skipped outside a container: %q", name, c.Summary)
		}
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
