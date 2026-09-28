package doctor

import (
	"net"
	"strconv"
	"strings"
	"testing"
)

// TestAnAbandonedPortIsNotProbed: a port the caller's run is about to stop
// binding (Deps.AbandonedPorts, `bridge init --yes --force` moving off it) is
// answered "not checked" without a bind probe, whoever holds it, and the
// check beside it, whose port is not listed, is graded as ever: the list is
// matched by port, never by check.
func TestAnAbandonedPortIsNotProbed(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.Addr().(*net.TCPAddr).Port

	d := Deps{APIPort: port, AdminPort: port, AbandonedPorts: []int{port}}
	orig := listenFunc
	t.Cleanup(func() { listenFunc = orig })
	probes := 0
	listenFunc = func(network, address string) (net.Listener, error) {
		probes++
		return orig(network, address)
	}
	for _, c := range RunPortChecks(t.Context(), d, true, true).Checks {
		if c.Status != OK || !strings.HasPrefix(c.Summary, "not checked: ") ||
			!strings.Contains(c.Summary, ":"+strconv.Itoa(port)) {
			t.Errorf("%s = %s %q on an abandoned port another process holds; want ok \"not checked\", naming it",
				c.Name, c.Status, c.Summary)
		}
	}
	if probes != 0 {
		t.Errorf("the checks probed an abandoned port %d time(s)", probes)
	}

	// The same port unlisted, beside an abandoned one, is the conflict it is:
	// with no pid file, another process's.
	d.AbandonedPorts = []int{port + 1}
	if c := checkAPIPort(t.Context(), d); c.Status != Fail {
		t.Errorf("port-api = %s %q on a held port that is not abandoned; want fail", c.Status, c.Summary)
	}
}
