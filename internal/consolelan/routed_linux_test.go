//go:build linux

package consolelan_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/consolelan"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
)

// The routed client's qualification (the plan's §9.5, F4, F8): network
// namespaces put a client on A's link and a routed client on another link
// whose address lies in A's prefix, the shape of a VPN that hands out home
// addresses, and the real plan, hold and handler run in the box's namespace
// on its real interfaces. It needs root, and runs only where
// BRIDGE_QUALIFY_ROUTED=1, which only its own CI job sets (gate.yml's
// console-lan-routed); elsewhere it skips. The test never switches a
// namespace from Go: each side is this binary re-executed under
// `ip netns exec`, one namespace for its whole life.

// helperEnv selects a helper process's role; its other variables carry the
// address, the port and whether the hold is made.
const (
	helperEnv     = "BRIDGE_CONSOLELAN_HELPER"
	helperAddrEnv = "BRIDGE_CONSOLELAN_ADDR"
	helperPortEnv = "BRIDGE_CONSOLELAN_PORT"
	helperHoldEnv = "BRIDGE_CONSOLELAN_HOLD"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "box":
		os.Exit(runBox())
	case "dial":
		os.Exit(runDial())
	}
	os.Exit(m.Run())
}

// TestARoutedClientInsideThePrefixGetsNoConsole: for an RFC 1918 A and a ULA
// alike, the client on A's link gets the console's 200, and the routed
// client's connect is refused by the hold; with the hold removed, the routed
// client gets the 200, so the source policy and the Host check admit it and
// only the hold keeps it out (D1).
func TestARoutedClientInsideThePrefixGetsNoConsole(t *testing.T) {
	if os.Getenv("BRIDGE_QUALIFY_ROUTED") != "1" {
		t.Skip("BRIDGE_QUALIFY_ROUTED=1 is set only by the console-lan-routed CI job, which runs this as root")
	}
	if os.Geteuid() != 0 {
		t.Fatal("BRIDGE_QUALIFY_ROUTED=1 needs root: network namespaces and veth pairs need CAP_NET_ADMIN")
	}
	ip, err := exec.LookPath("ip")
	if err != nil {
		t.Fatalf("ip: %v", err)
	}
	ip, err = filepath.Abs(ip)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, fam := range []struct {
		name                     string
		a, client, routed, other string
		bits                     int
		v6                       bool
	}{
		{"rfc1918", "10.77.1.1", "10.77.1.2", "10.77.1.50", "10.77.2.1", 24, false},
		{"ula", "fd77:1::1", "fd77:1::2", "fd77:1::50", "fd77:2::1", 64, true},
	} {
		t.Run(fam.name, func(t *testing.T) {
			n := newNamespaces(t, ip, fam.name)
			n.link(t, fam.a, fam.client, fam.routed, fam.other, fam.bits, fam.v6)
			for _, held := range []bool{true, false} {
				port := 7789
				if !held {
					port = 7790
				}
				box := n.startBox(t, self, fam.a, port, held)
				url := "http://" + net.JoinHostPort(fam.a, strconv.Itoa(port)) + "/api/console"
				if got := n.dial(t, self, "client", url); got != "200" {
					t.Fatalf("held=%v: the client on A's link got %q, want the console's 200 (box: %s)", held, got, box)
				}
				got := n.dial(t, self, "routed", url)
				switch {
				case held && got == "200":
					t.Fatalf("the routed client inside A's prefix got the console's 200 through the hold")
				case held && !strings.Contains(got, "connection refused"):
					t.Fatalf("held: the routed client's connect was answered %q, want it refused", got)
				case !held && got != "200":
					t.Fatalf("without the hold the routed client got %q: the source policy and the Host check must admit it, so the hold alone refuses it", got)
				}
			}
		})
	}
}

// namespaces is one case's three namespaces and the commands that made them.
type namespaces struct {
	ip, box, client, routed string
}

func newNamespaces(t *testing.T, ip, tag string) *namespaces {
	t.Helper()
	sfx := fmt.Sprintf("%s%d", tag, os.Getpid()%10000)
	n := &namespaces{ip: ip, box: "b989box" + sfx, client: "b989cli" + sfx, routed: "b989rtd" + sfx}
	for _, ns := range []string{n.box, n.client, n.routed} {
		run(t, ip, "netns", "add", ns)
		ns := ns
		t.Cleanup(func() { _ = exec.Command(ip, "netns", "delete", ns).Run() })
		run(t, ip, "-n", ns, "link", "set", "lo", "up")
	}
	return n
}

// link makes A's link (box-a to the client) and the other link (box-b to
// the routed client), the routed client holding an address in A's prefix
// with a host route each way.
func (n *namespaces) link(t *testing.T, a, client, routed, other string, bits int, v6 bool) {
	t.Helper()
	ip := n.ip
	run(t, ip, "link", "add", "b989a", "netns", n.box, "type", "veth", "peer", "name", "b989ac", "netns", n.client)
	run(t, ip, "link", "add", "b989b", "netns", n.box, "type", "veth", "peer", "name", "b989br", "netns", n.routed)
	family := "-4"
	full, nodad := "32", []string{}
	if v6 {
		family, full, nodad = "-6", "128", []string{"nodad"}
	}
	addr := func(ns, dev, a string, bits string) {
		args := append([]string{"-n", ns, family, "addr", "add", a + "/" + bits, "dev", dev}, nodad...)
		run(t, ip, args...)
		run(t, ip, "-n", ns, "link", "set", dev, "up")
	}
	addr(n.box, "b989a", a, strconv.Itoa(bits))
	addr(n.client, "b989ac", client, strconv.Itoa(bits))
	otherBits := "24"
	if v6 {
		otherBits = "64"
	}
	addr(n.box, "b989b", other, otherBits)
	addr(n.routed, "b989br", routed, full)
	// The routed client reaches A over its own link, through the box's
	// address there as its next hop (IPv6's neighbour discovery answers only
	// for an address of the link it arrives on), and the box answers it
	// there: a host route each way.
	run(t, ip, "-n", n.routed, family, "route", "add", other+"/"+full, "dev", "b989br")
	run(t, ip, "-n", n.routed, family, "route", "add", a+"/"+full, "via", other, "dev", "b989br")
	run(t, ip, "-n", n.box, family, "route", "add", routed+"/"+full, "dev", "b989b")
	if !v6 {
		for _, dev := range []string{"all", "b989b"} {
			run(t, ip, "netns", "exec", n.box, "sysctl", "-qw", "net.ipv4.conf."+dev+".rp_filter=0")
		}
	}
}

// startBox runs the console in the box's namespace on A:port, held to A's
// interface where held, and waits for it to say it listens.
func (n *namespaces) startBox(t *testing.T, self, a string, port int, held bool) string {
	t.Helper()
	cmd := exec.Command(n.ip, "netns", "exec", n.box, self, "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=box", helperAddrEnv+"="+a, helperPortEnv+"="+strconv.Itoa(port),
		helperHoldEnv+"="+strconv.FormatBool(held))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "state ") {
				line <- sc.Text()
				break
			}
		}
		_, _ = io.Copy(io.Discard, stdout)
	}()
	select {
	case l := <-line:
		var st consolelan.State
		if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "state ")), &st); err != nil {
			t.Fatal(err)
		}
		if st.State != consolelan.StateListening {
			t.Fatalf("the box's console did not listen on its LAN address: %+v", st)
		}
		if held && st.Hold != consolelan.HoldBindToIfindex {
			t.Fatalf("the box's console listens with hold %q", st.Hold)
		}
		return l
	case <-time.After(30 * time.Second):
		t.Fatal("the box's console said nothing within 30 s")
	}
	return ""
}

// dial runs a GET of url from the namespace's process and answers its result.
func (n *namespaces) dial(t *testing.T, self, who, url string) string {
	t.Helper()
	ns := n.client
	if who == "routed" {
		ns = n.routed
	}
	cmd := exec.Command(n.ip, "netns", "exec", ns, self, "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"=dial", helperAddrEnv+"="+url)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s's dial: %v", who, err)
	}
	return strings.TrimSpace(string(out))
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// saveBoxConfig saves the box's config in a turn of its lock.
func saveBoxConfig(cfg *config.Config, path string) error {
	lock, err := config.LockFile(path)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	return cfg.Save(lock)
}

// runDial is the dialling helper: one GET, its status or its error, printed.
func runDial() int {
	url := os.Getenv(helperAddrEnv)
	c := &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get(url)
	if err != nil {
		fmt.Println("error " + err.Error())
		return 0
	}
	_ = resp.Body.Close()
	fmt.Println(strconv.Itoa(resp.StatusCode))
	return 0
}

// runBox is the box helper: the real console, its plan read from the
// namespace's own interfaces and held by consolelan.Hold (or by nothing,
// where the case removes the hold), served until stdin closes.
func runBox() int {
	a := netip.MustParseAddr(os.Getenv(helperAddrEnv))
	port, _ := strconv.Atoi(os.Getenv(helperPortEnv))
	held := os.Getenv(helperHoldEnv) == "true"
	tmp, err := os.MkdirTemp("", "b989box")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(tmp)
	lib := filepath.Join(tmp, "Music")
	_ = os.MkdirAll(lib, 0o755)
	cfg := &config.Config{
		LibraryRoots:    []string{lib},
		ListenAddress:   "127.0.0.1:7788",
		AdminAddress:    "127.0.0.1:" + strconv.Itoa(port),
		AdminLANAddress: a.String(),
		DataDir:         filepath.Join(tmp, "data"),
		ScanIntervalSec: 3600,
		LibraryName:     "Routed",
	}
	_ = os.MkdirAll(cfg.DataDir, 0o755)
	cfgPath := filepath.Join(tmp, "bridge.yaml")
	if err := saveBoxConfig(cfg, cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	store, err := manifest.OpenStore(manifest.DefaultDBPath(cfg.DataDir))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer store.Close()
	tokens, err := auth.OpenStore(filepath.Join(cfg.DataDir, "tokens.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	srv, err := admin.New(admin.Deps{
		CfgHolder: config.NewRuntimeConfig(cfg),
		CfgPath:   cfgPath,
		Auth:      tokens,
		Manifest:  store,
		Scanner:   manifest.NewScanner(cfg.LibraryRoots, store, ""),
		Resolver:  bridgefs.New(cfg.LibraryRoots),
		StartedAt: time.Now(),
		Restart:   func() {},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	hold := consolelan.Hold
	if !held {
		hold = func(int, bool) func(string, string, syscall.RawConn) error {
			return func(string, string, syscall.RawConn) error { return nil }
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := srv.OpenConsoleLAN(ctx, &admin.ConsoleLANPlan{
		Address: a, Port: port, Source: consolelan.SourceFile,
		FileKey: config.LANKey{State: config.LANKeyAddress, Address: a.String()},
		Read: func() ([]consolelan.Interface, error) {
			ifs, err := lan.ReadInterfaces()
			if err != nil {
				return nil, err
			}
			out := make([]consolelan.Interface, 0, len(ifs))
			for _, in := range ifs {
				out = append(out, consolelan.Interface{Name: in.Name, Index: in.Index, Flags: in.Flags, Addrs: in.Addrs})
			}
			return out, nil
		},
		Hold: hold,
	})
	b, _ := json.Marshal(st)
	fmt.Println("state " + string(b))
	loop, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	_ = srv.ServeListener(ctx, loop)
	return 0
}
