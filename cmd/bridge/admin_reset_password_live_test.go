package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
)

// TestResetPasswordTakesOnARunningPublicBridge drives `bridge admin
// reset-password` against a real public-mode `bridge serve`, which is how the
// command is run: beside the bridge, by an operator whose password leaked, or
// by the hosted control plane's `bridge-tenant passwd`, which restarts the
// tenant straight after.
//
// The running bridge held the credential it loaded at start and wrote it back
// over the rotation at its next write of the file. The first run below ends in
// the write that did it on the path the command itself advised, the shutdown
// flush of a restart; the second is that restart, and then a rotation the
// running bridge must take with no restart at all.
//
// The first rotation keeps the sessions (--keep-sessions), so the console
// signed in before it has activity for the shutdown flush to land. The second
// takes the default, and signs that console out at its next request, with no
// restart; a console signed in after it stays in.
func TestResetPasswordTakesOnARunningPublicBridge(t *testing.T) {
	const (
		rotated = "the password it was rotated to"
		third   = "and rotated again while it ran"
	)
	cfgDir := t.TempDir()
	lib := filepath.Join(cfgDir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	ports := pickPublicInitPorts(t)
	code, out := publicInit(t, cfgDir, ports, "Live", "--library", lib)
	if code != 0 {
		t.Fatalf("bridge init --public = %d:\n%s", code, out)
	}
	leaked := mintedPassword(t, out)
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	storePath := filepath.Join(cfgDir, "data", "adminauth.json")
	// UDP is not what this is about, and the port picked free for TCP is
	// not known to be free for UDP.
	appendToFile(t, cfgPath, "disableHttp3: true\n")
	console := newLiveConsole(ports.admin)

	// First run: a console signed in with the password that leaked, the
	// rotation beside it, one more console request, and the stop.
	ctx1, cancel1 := context.WithCancel(context.Background())
	stdout1, stderr1 := &safeBuffer{}, &safeBuffer{}
	done1 := make(chan int, 1)
	exited1 := make(chan struct{})
	go func() {
		defer close(exited1)
		done1 <- run(ctx1, []string{"serve", "--config", cfgPath}, stdout1, stderr1)
	}()
	drainServeOnCleanup(t, cancel1, exited1, done1, stderr1)
	waitForAdminReady(t, console.addr, done1, stderr1)

	signedIn := console.login(t, leaked, http.StatusOK, stderr1)
	resetPasswordLive(t, cfgPath, rotated, "--keep-sessions")
	// Inside the session debounce, so this request writes nothing and leaves
	// the shutdown flush something to land: a restart after any console
	// request in the last 30 seconds.
	console.get(t, "/api/stats", signedIn, http.StatusOK, stderr1)
	stopLiveServe(t, console, cancel1, exited1, stderr1)
	requireCredentialOnDisk(t, storePath, rotated, leaked, "the shutdown flush")

	// Second run: the restart.
	ctx2, cancel2 := context.WithCancel(context.Background())
	stdout2, stderr2 := &safeBuffer{}, &safeBuffer{}
	done2 := make(chan int, 1)
	exited2 := make(chan struct{})
	go func() {
		defer close(exited2)
		done2 <- run(ctx2, []string{"serve", "--config", cfgPath}, stdout2, stderr2)
	}()
	drainServeOnCleanup(t, cancel2, exited2, done2, stderr2)
	waitForAdminReady(t, console.addr, done2, stderr2)

	console.login(t, leaked, http.StatusUnauthorized, stderr2)
	afterRestart := console.login(t, rotated, http.StatusOK, stderr2)
	// A rotation with --keep-sessions, and the restart, both leave a
	// signed-in console signed in, which is what reset-password says.
	console.get(t, "/api/stats", signedIn, http.StatusOK, stderr2)

	// A rotation while it runs, with the default: taken at the next login,
	// with no restart, and kept through the login's write and a logout's.
	// Every console signed in before it is refused at its next request.
	resetPasswordLive(t, cfgPath, third)
	console.get(t, "/api/stats", signedIn, http.StatusUnauthorized, stderr2)
	console.get(t, "/api/stats", afterRestart, http.StatusUnauthorized, stderr2)
	console.login(t, rotated, http.StatusUnauthorized, stderr2)
	signedInAfter := console.login(t, third, http.StatusOK, stderr2)
	console.get(t, "/api/stats", signedInAfter, http.StatusOK, stderr2)
	// A logout, so the logout's write lands over the rotation too.
	loggedOut := console.login(t, third, http.StatusOK, stderr2)
	console.logout(t, loggedOut, stderr2)
	requireCredentialOnDisk(t, storePath, third, rotated, "a login and a logout on the running bridge")

	stopLiveServe(t, console, cancel2, exited2, stderr2)
	requireCredentialOnDisk(t, storePath, third, rotated, "the second shutdown flush")
	requireSessionsOnDisk(t, storePath, "the second shutdown flush",
		sessionOnDisk{"signed in before the first rotation", signedIn, adminauth.ErrSessionNotFound},
		sessionOnDisk{"signed in after the restart", afterRestart, adminauth.ErrSessionNotFound},
		sessionOnDisk{"signed in after the second rotation", signedInAfter, nil})
}

// TestSignOutEverywhereOnARunningPublicBridge drives the two ways to end
// console sessions without changing the password against a real public-mode
// `bridge serve`: the Devices page's "Sign out all other sessions", and
// `bridge admin sign-out-everywhere` run beside the bridge. Each takes at the
// signed-out console's next request, with no restart, and the restart after
// does not bring anything back. Before 2026-09-27 neither existed, and nothing
// could end a console session another browser held.
func TestSignOutEverywhereOnARunningPublicBridge(t *testing.T) {
	cfgDir := t.TempDir()
	lib := filepath.Join(cfgDir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	ports := pickPublicInitPorts(t)
	code, out := publicInit(t, cfgDir, ports, "Live", "--library", lib)
	if code != 0 {
		t.Fatalf("bridge init --public = %d:\n%s", code, out)
	}
	password := mintedPassword(t, out)
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	storePath := filepath.Join(cfgDir, "data", "adminauth.json")
	appendToFile(t, cfgPath, "disableHttp3: true\n")
	console := newLiveConsole(ports.admin)

	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- run(ctx, []string{"serve", "--config", cfgPath}, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	waitForAdminReady(t, console.addr, done, stderr)

	// The console's button: every other browser out, this one kept.
	here := console.login(t, password, http.StatusOK, stderr)
	elsewhere := console.login(t, password, http.StatusOK, stderr)
	console.signOutOthers(t, here, 1, stderr)
	console.get(t, "/api/stats", elsewhere, http.StatusUnauthorized, stderr)
	console.get(t, "/api/stats", here, http.StatusOK, stderr)

	// The command: every browser out, the password kept.
	again := console.login(t, password, http.StatusOK, stderr)
	signOutEverywhereLive(t, cfgPath)
	console.get(t, "/api/stats", here, http.StatusUnauthorized, stderr)
	console.get(t, "/api/stats", again, http.StatusUnauthorized, stderr)
	after := console.login(t, password, http.StatusOK, stderr)
	console.get(t, "/api/stats", after, http.StatusOK, stderr)

	stopLiveServe(t, console, cancel, exited, stderr)
	requireSessionsOnDisk(t, storePath, "the shutdown flush",
		sessionOnDisk{"the button signed out", elsewhere, adminauth.ErrSessionNotFound},
		sessionOnDisk{"that pressed the button", here, adminauth.ErrSessionNotFound},
		sessionOnDisk{"signed in before the command", again, adminauth.ErrSessionNotFound},
		sessionOnDisk{"signed in after the command", after, nil})
}

// liveConsole signs in to a running bridge's admin console the way the
// console's own page does, over plain HTTP (--admin-tls-proxy).
type liveConsole struct {
	addr   string
	client *http.Client
}

func newLiveConsole(adminPort int) *liveConsole {
	return &liveConsole{
		addr: fmt.Sprintf("127.0.0.1:%d", adminPort),
		client: &http.Client{
			Timeout: 30 * time.Second,
			// A 302 to /login is an answer, not something to follow.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// login posts the credential and returns the session the bridge set, or ""
// when want is not 200.
func (c *liveConsole) login(t *testing.T, password string, want int, stderr *safeBuffer) string {
	t.Helper()
	body := fmt.Sprintf(`{"username":"admin","password":%q}`, password)
	resp := c.do(t, http.MethodPost, "/login", body, "", stderr)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("POST /login with %q = %d, want %d: %s\nstderr=%s", password, resp.StatusCode, want, raw, stderr.String())
	}
	if want != http.StatusOK {
		return ""
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "bridge_admin_session" && ck.Value != "" {
			return ck.Value
		}
	}
	t.Fatalf("POST /login answered 200 and set no session cookie")
	return ""
}

func (c *liveConsole) get(t *testing.T, path, session string, want int, stderr *safeBuffer) {
	t.Helper()
	resp := c.do(t, http.MethodGet, path, "", session, stderr)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("GET %s = %d, want %d: %s", path, resp.StatusCode, want, raw)
	}
}

// signOutOthers presses the Devices page's "Sign out all other sessions" as
// the browser signed in with session, and requires that it ended wantEnded.
func (c *liveConsole) signOutOthers(t *testing.T, session string, wantEnded int, stderr *safeBuffer) {
	t.Helper()
	resp := c.do(t, http.MethodPost, "/api/console-sessions/sign-out-others", "", session, stderr)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/console-sessions/sign-out-others = %d: %s", resp.StatusCode, raw)
	}
	var got struct {
		Ended int `json:"ended"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.Ended != wantEnded {
		t.Fatalf("sign-out-others answered %s (err=%v), want ended=%d", raw, err, wantEnded)
	}
}

func (c *liveConsole) logout(t *testing.T, session string, stderr *safeBuffer) {
	t.Helper()
	resp := c.do(t, http.MethodPost, "/logout", "", session, stderr)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /logout = %d: %s", resp.StatusCode, raw)
	}
}

func (c *liveConsole) do(t *testing.T, method, path, body, session string, stderr *safeBuffer) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+c.addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if session != "" {
		// By hand: the cookie is Secure in public mode, and a jar would not
		// send it over the plain HTTP an --admin-tls-proxy console serves.
		req.AddCookie(&http.Cookie{Name: "bridge_admin_session", Value: session})
	}
	resp, err := c.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v; stderr=%s", method, path, err, stderr.String())
	}
	return resp
}

// resetPasswordLive runs `bridge admin reset-password --from-stdin` against the
// config, as the hosted control plane does, with flags after the password.
func resetPasswordLive(t *testing.T, cfgPath, password string, flags ...string) {
	t.Helper()
	var out, errOut bytes.Buffer
	args := append([]string{"reset-password", "--config", cfgPath, "--from-stdin"}, flags...)
	if code := adminCmd(args, strings.NewReader(password+"\n"), &out, &errOut); code != 0 {
		t.Fatalf("bridge admin reset-password = %d: %s%s", code, out.String(), errOut.String())
	}
	// What it says about a running bridge and its sessions is what the tests
	// show: the rotation takes with no restart, and the consoles signed in are
	// signed out unless kept, a restart neither way changing that.
	want := []string{"no restart", "signed out", "refuses their sessions at their next request"}
	if slices.Contains(flags, "--keep-sessions") {
		want = []string{"no restart", "stay signed in", "bridge admin sign-out-everywhere"}
	}
	for _, w := range want {
		if !strings.Contains(out.String(), w) {
			t.Errorf("reset-password %v's output does not say %q:\n%s", flags, w, out.String())
		}
	}
}

// signOutEverywhereLive runs `bridge admin sign-out-everywhere` against the
// config.
func signOutEverywhereLive(t *testing.T, cfgPath string) {
	t.Helper()
	var out, errOut bytes.Buffer
	if code := adminCmd([]string{"sign-out-everywhere", "--config", cfgPath}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("bridge admin sign-out-everywhere = %d: %s%s", code, out.String(), errOut.String())
	}
	for _, w := range []string{"signed out", "refuses their sessions at their next request", "password is unchanged"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("sign-out-everywhere's output does not say %q:\n%s", w, out.String())
		}
	}
}

// stopLiveServe stops a serve started by the test and waits for it to exit, so
// what the file holds afterwards includes the shutdown flush.
func stopLiveServe(t *testing.T, c *liveConsole, cancel context.CancelFunc, exited <-chan struct{}, stderr *safeBuffer) {
	t.Helper()
	// The client closes its connections first, so no socket on the console's
	// port waits in TIME_WAIT when the next run binds it.
	c.client.CloseIdleConnections()
	cancel()
	select {
	case <-exited:
	case <-serveGiveUp(t):
		t.Fatalf("serve did not exit before the test's deadline; stderr=%s\nserve's goroutines:\n%s",
			stderr.String(), serveStacks())
	}
	if strings.Contains(stderr.String(), "adminauth: flush sessions on shutdown") {
		t.Errorf("the shutdown flush failed:\n%s", stderr.String())
	}
}

// requireCredentialOnDisk opens the credential file as the next start of the
// bridge would, and requires that it accepts want and refuses old.
func requireCredentialOnDisk(t *testing.T, storePath, want, old, after string) {
	t.Helper()
	s, err := adminauth.OpenStore(storePath)
	if err != nil {
		t.Fatalf("after %s, the credential file does not open: %v", after, err)
	}
	if err := s.Verify("admin", want); err != nil {
		t.Errorf("after %s, the file refuses the current password (%v): the running bridge "+
			"wrote the credential it held in memory back over the rotation", after, err)
	}
	if err := s.Verify("admin", old); !errors.Is(err, adminauth.ErrInvalidCredentials) {
		t.Errorf("after %s, the file still accepts the password rotated away from (err=%v)", after, err)
	}
}

// sessionOnDisk is a console session and what the next start of the bridge
// must answer for it: nil for one still signed in, ErrSessionNotFound for one
// signed out, which must not come back.
type sessionOnDisk struct {
	name, raw string
	want      error
}

// requireSessionsOnDisk opens the credential file as the next start of the
// bridge would, and requires each session's answer.
func requireSessionsOnDisk(t *testing.T, storePath, after string, sessions ...sessionOnDisk) {
	t.Helper()
	s, err := adminauth.OpenStore(storePath)
	if err != nil {
		t.Fatalf("after %s, the credential file does not open: %v", after, err)
	}
	for _, sess := range sessions {
		if _, err := s.ValidateSession(sess.raw); !errors.Is(err, sess.want) {
			t.Errorf("after %s, the session %s on the next start = %v, want %v", after, sess.name, err, sess.want)
		}
	}
}

func appendToFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
