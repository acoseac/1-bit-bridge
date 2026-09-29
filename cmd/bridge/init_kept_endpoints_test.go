package main

import (
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A loopback rewrite of a loopback install keeps its customEndpoints (#1040)
// and writes the run's ports, so a kept endpoint can name the port the
// rewrite moves the API off. Measured with the real binary on 2026-09-29
// (backlog B61): an install on 127.0.0.1:X listing https://nas…:X, rewritten
// onto :Z, kept the endpoint without a word, and the restarted bridge's
// /v1/health advertised https://nas…:X beside its :Z addresses, an alternate
// every device puts into its failover rotation.
//
// The rewrite warns and keeps the endpoint. It cannot tell a port that
// reaches this bridge directly from one a router or proxy forwards: the
// endpoint is the operator's word for what is reachable, and a forward from
// that port stays right once it is pointed at the new one.

// TestInitRewriteWarnsAboutAKeptEndpointOnThePortItMovesOff: the rewrite
// names the endpoint, the port it names and the port the API moves to, and
// keeps the endpoint. An endpoint on another port is not named.
func TestInitRewriteWarnsAboutAKeptEndpointOnThePortItMovesOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		// flags move the API off the install's port; newPort is where to,
		// given the port the flags pick.
		flags   func(picked int) []string
		newPort func(picked int) int
	}{
		{"to the port --listen-address names", func(p int) []string {
			return []string{"--listen-address", loopbackAddr(p)}
		}, func(p int) int { return p }},
		{"to the default, with no --listen-address", func(int) []string { return nil },
			func(int) int { return 7788 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			lib := testLibrary(t)
			old := freeLoopbackPort(t)
			writeLoopbackInstall(t, cfgDir, lib, old, freeLoopbackPort(t))
			moved := "https://nas.example.test:" + strconv.Itoa(old)
			const elsewhere = "https://music.example.test"
			appendToConfig(t, cfgDir, "customEndpoints:\n    - "+moved+"\n    - "+elsewhere+"\n")

			picked := freeLoopbackPortOtherThan(t, old, 7788)
			code, out := loopbackInit(t, cfgDir, lib, "", append([]string{"--force"}, tc.flags(picked)...)...)
			defer logRunOnFailure(t, out)
			if code != 0 {
				t.Fatalf("the rewrite exited %d", code)
			}
			checkKeptEndpointWarning(t, keptEndpointWarning(out), moved, elsewhere, old, tc.newPort(picked))
			if got := loadInstallConfig(t, cfgDir).CustomEndpoints; !slices.Equal(got, []string{moved, elsewhere}) {
				t.Errorf("customEndpoints = %v, want both kept as they were", got)
			}
		})
	}
}

// checkKeptEndpointWarning checks the warning w a rewrite moving the API from
// :from to :to gave about its kept endpoints: it names moved, the endpoint on
// :from, and both ports, and not elsewhere, an endpoint on another port.
func checkKeptEndpointWarning(t *testing.T, w, moved, elsewhere string, from, to int) {
	t.Helper()
	if w == "" {
		t.Fatalf("the rewrite said nothing about %s, which names :%d, the port it moves the API off", moved, from)
	}
	for _, want := range []string{moved, ":" + strconv.Itoa(from), ":" + strconv.Itoa(to)} {
		if !strings.Contains(w, want) {
			t.Errorf("the warning does not name %q", want)
		}
	}
	if strings.Contains(w, elsewhere) {
		t.Errorf("the warning names %s, which names no port the rewrite moves off", elsewhere)
	}
}

// freeLoopbackPortOtherThan is freeLoopbackPort, never one of taken.
func freeLoopbackPortOtherThan(t *testing.T, taken ...int) int {
	t.Helper()
	for range 100 {
		if p := freeLoopbackPort(t); !slices.Contains(taken, p) {
			return p
		}
	}
	t.Fatalf("100 free loopback ports were all among %v", taken)
	return 0
}

// TestInitRewriteKeepingThePortSaysNothingOfItsEndpoints is the control: a
// rewrite that keeps the API on the install's port moves nothing, so an
// endpoint naming that port is as right as it was.
func TestInitRewriteKeepingThePortSaysNothingOfItsEndpoints(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "cfg")
	lib := testLibrary(t)
	port := freeLoopbackPort(t)
	writeLoopbackInstall(t, cfgDir, lib, port, freeLoopbackPort(t))
	appendToConfig(t, cfgDir, "customEndpoints:\n    - https://nas.example.test:"+strconv.Itoa(port)+"\n")
	code, out := loopbackInit(t, cfgDir, lib, "", "--force", "--listen-address", loopbackAddr(port))
	defer logRunOnFailure(t, out)
	if code != 0 {
		t.Fatalf("the rewrite exited %d", code)
	}
	if w := keptEndpointWarning(out); w != "" {
		t.Errorf("a rewrite keeping :%d warned about an endpoint naming it: %q", port, w)
	}
}

// TestEndpointsNamingAMovedPort pins which endpoints name the port a rewrite
// moves the API off: by their own port, or by their scheme's where they give
// none, and nothing where the port does not move or names nothing a client
// dials.
func TestEndpointsNamingAMovedPort(t *testing.T) {
	endpoints := []string{
		"https://nas.example.test:9090",
		"https://nas.example.test:9090/bridge",
		"https://[fd00::1]:9090",
		"https://music.example.test",
		"http://plain.example.test",
		"https://other.example.test:7788",
		// A port no client dials, so an install listening on :0 has no
		// endpoint on the port it moves off.
		"https://zero.example.test:0",
	}
	for _, tc := range []struct {
		name     string
		from, to string
		want     []string
	}{
		{"off 9090", ":9090", "127.0.0.1:7788", endpoints[:3]},
		{"off 443, which https names with no port", "0.0.0.0:443", ":7788", []string{"https://music.example.test"}},
		{"off 80, which http names with no port", ":80", ":7788", []string{"http://plain.example.test"}},
		{"keeping the port", "127.0.0.1:9090", ":9090", nil},
		{"off port 0, which no client dials", ":0", ":7788", nil},
		{"off an address that does not parse", "nas:port", ":7788", nil},
		{"onto an address that does not parse", ":9090", "7788", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := endpointsNamingAMovedPort(tc.from, tc.to, endpoints)
			if !slices.Equal(got, tc.want) {
				t.Errorf("endpointsNamingAMovedPort(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
			}
		})
	}
}

// keptEndpointWarning returns the warning out gives about a kept custom
// endpoint on a port the rewrite moves off, with the lines under it, or "".
func keptEndpointWarning(out string) string {
	const opening = "warning: customEndpoints kept from the config"
	_, after, ok := strings.Cut(out, opening)
	if !ok {
		return ""
	}
	// The warning runs to the first blank line.
	block, _, _ := strings.Cut(after, "\n\n")
	return opening + block
}
