package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAHandedListenerCannotBeTakenBeforeServeBinds is the collision the
// close-then-reuse helpers lost to in CI. The port is bound and kept, a
// second listen on it fails, and serve still comes up because it is handed
// the listener it would otherwise have had to bind (backlog B313).
func TestAHandedListenerCannotBeTakenBeforeServeBinds(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	lan, admin := holdLoopback(t), holdLoopback(t)
	for _, addr := range []string{lan.addr, admin.addr} {
		squatter, err := net.Listen("tcp", addr)
		if err == nil {
			_ = squatter.Close()
			t.Fatalf("a second listen on %s succeeded; the held port can still be taken", addr)
		}
	}

	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\ndataDir: " + filepath.Join(dir, "data") +
		"\nadminAddress: " + admin.addr + "\ndisableHttp3: true\nmdns:\n  enabled: false\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	b := launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, serveOpts{
			configPath: cfgPath, addrOverride: lan.addr,
			lanListener: lan.ln, adminListener: admin.ln,
		}, stdout, stderr)
	})
	waitForAdminReady(t, admin.addr, b.done, b.stderr)
}

// TestAHandedListenerOnAnotherAddressIsAStartupError refuses a listener
// whose address is not the one the config names. Serving on it would
// answer a port the config, the banner and the tests do not.
func TestAHandedListenerOnAnotherAddressIsAStartupError(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	handed, named := holdLoopback(t), holdLoopback(t)
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\ndataDir: " + filepath.Join(dir, "data") +
		"\nlistenAddress: " + named.addr + "\nadminAddress: 127.0.0.1:0\n" +
		"disableHttp3: true\nmdns:\n  enabled: false\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{
			configPath: cfgPath, lanListener: handed.ln,
		}, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)

	var code int
	select {
	case code = <-done:
	case <-serveGiveUp(t):
		t.Fatalf("serve did not exit on the mismatched listener\n%s", stderr.String())
	}
	out := stderr.String()
	if code == 0 {
		t.Fatalf("serve exited 0 on a listener whose address is not the config's\n%s", out)
	}
	if !strings.Contains(out, "handed listener") {
		t.Fatalf("serve did not name the handed listener\n%s", out)
	}
	if strings.Contains(stdout.String(), "listening on") {
		t.Fatalf("serve printed its banner for a listener it was meant to refuse\n%s", stdout.String())
	}
}

// TestARefusedHandedUDPSocketLeavesTheLANPortFree is the leak a UDP
// refusal had: runServe bound the LAN TCP port, then returned on the
// socket before ServeTLS owned that listener. The port has to be free
// once serve has returned (backlog B313).
func TestARefusedHandedUDPSocketLeavesTheLANPortFree(t *testing.T) {
	t.Run("mismatched address", func(t *testing.T) {
		assertRefusedUDPLeavesTheLANPortFree(t, false)
	})
	t.Run("http3 disabled", func(t *testing.T) {
		assertRefusedUDPLeavesTheLANPortFree(t, true)
	})
}

func assertRefusedUDPLeavesTheLANPortFree(t *testing.T, http3Disabled bool) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	// Serve binds this port itself. Closed here so the bind is serve's;
	// the refusal must not leave it held.
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lanAddr := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = udp.Close() })
	if udp.LocalAddr().String() == lanAddr {
		t.Fatal("the handed UDP socket landed on the LAN port")
	}

	http3 := ""
	if http3Disabled {
		http3 = "disableHttp3: true\n"
	}
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\ndataDir: " + filepath.Join(dir, "data") +
		"\nlistenAddress: " + lanAddr + "\nadminAddress: 127.0.0.1:0\n" +
		http3 + "mdns:\n  enabled: false\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{
			configPath: cfgPath, lanPacket: udp,
		}, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)

	var code int
	select {
	case code = <-done:
	case <-serveGiveUp(t):
		t.Fatalf("serve did not exit on the refused UDP socket\n%s", stderr.String())
	}
	out := stderr.String()
	if code == 0 {
		t.Fatalf("serve exited 0 on a UDP socket it cannot adopt\n%s", out)
	}
	if !strings.Contains(out, "handed UDP socket") {
		t.Fatalf("serve did not name the handed UDP socket\n%s", out)
	}
	if http3Disabled && !strings.Contains(out, "HTTP/3 disabled") {
		t.Fatalf("serve did not name HTTP/3 as the reason\n%s", out)
	}
	again, err := net.Listen("tcp", lanAddr)
	if err != nil {
		t.Fatalf("LAN port %s stayed taken after serve refused the UDP socket: %v\n%s", lanAddr, err, out)
	}
	if err := again.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "listening on") {
		t.Fatalf("serve printed its banner for a socket it was meant to refuse\n%s", stdout.String())
	}
	if err := udp.SetDeadline(time.Now()); err != nil {
		t.Fatalf("serve closed the UDP socket it refused: %v", err)
	}
}

// TestTheAdminReadinessCheckWaitsUntilServeAdoptsTheListener holds
// serve just before ServeListener. The port is already accepting,
// which is all a dial can see; the console has not answered yet
// (backlog B313).
func TestTheAdminReadinessCheckWaitsUntilServeAdoptsTheListener(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	lan, admin := holdLoopback(t), holdLoopback(t)
	cfgPath := filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\ndataDir: " + filepath.Join(dir, "data") +
		"\nadminAddress: " + admin.addr + "\ndisableHttp3: true\nmdns:\n  enabled: false\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	b := launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, serveOpts{
			configPath: cfgPath, addrOverride: lan.addr,
			lanListener: lan.ln, adminListener: admin.ln,
			beforeAdminServe: func() {
				close(entered)
				<-release
			},
		}, stdout, stderr)
	})
	t.Cleanup(letGo)
	select {
	case <-entered:
	case <-serveGiveUp(t):
		t.Fatal("serve never reached ServeListener")
	}

	ready := make(chan struct{})
	go func() {
		waitForAdminReady(t, admin.addr, b.done, b.stderr)
		close(ready)
	}()
	select {
	case <-ready:
		t.Fatal("the admin readiness check returned before ServeListener")
	case <-time.After(time.Second):
	}
	letGo()
	select {
	case <-ready:
	case <-serveGiveUp(t):
		t.Fatal("the admin readiness check did not return once the console answered")
	}
}

func TestAdoptHandedListenerMatchesTheAddress(t *testing.T) {
	if got, err := adoptHandedListener(nil, "127.0.0.1:1"); got != nil || err != nil {
		t.Fatalf("nil listener = %v, %v", got, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got, err := adoptHandedListener(ln, ln.Addr().String())
	if err != nil || got != ln {
		t.Fatalf("matching listener = %v, %v", got, err)
	}
	if _, err := adoptHandedListener(ln, "127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "handed listener") {
		t.Fatalf("mismatched listener error = %v", err)
	}
}

func TestAdoptHandedPacketMatchesTheAddress(t *testing.T) {
	if got, err := adoptHandedPacket(nil, "127.0.0.1:1"); got != nil || err != nil {
		t.Fatalf("nil socket = %v, %v", got, err)
	}
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	got, err := adoptHandedPacket(conn, conn.LocalAddr().String())
	if err != nil || got != conn {
		t.Fatalf("matching socket = %v, %v", got, err)
	}
	if _, err := adoptHandedPacket(conn, "127.0.0.1:1"); err == nil || !strings.Contains(err.Error(), "handed UDP socket") {
		t.Fatalf("mismatched socket error = %v", err)
	}
}
