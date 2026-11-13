package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/adminauth"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The tests in this file drive a console login, and a login link's
// redemption, through the public console's real handler chain, and park each
// in the window between the check that let it in and the session it admits
// (testAfterLoginCheck), where a rotation or a sign-out written by another
// process lands in production (adminauth.Admission, backlog B881). The other
// process is a store of its own on the same file, as `bridge admin
// reset-password` and `sign-out-everywhere` open.

// resetPassword is the password the reset in these tests rotates to.
const resetPassword = "the password it was reset to"

// loginPark holds every login that reaches testAfterLoginCheck until the test
// lets them go.
type loginPark struct {
	arrived chan struct{}
	release chan struct{}
	once    sync.Once
}

// parkLogins installs a loginPark on srv. Its cleanup lets every parked login
// go, so register the server's Close before calling it: cleanups run last
// first, and a Close waits for the handlers it would otherwise hold.
func parkLogins(t *testing.T, srv *Server) *loginPark {
	t.Helper()
	p := &loginPark{arrived: make(chan struct{}, 16), release: make(chan struct{})}
	hook := func() {
		p.arrived <- struct{}{}
		<-p.release
	}
	srv.testAfterLoginCheck.Store(&hook)
	t.Cleanup(p.letGo)
	return p
}

// waitParked waits for n logins to reach the park.
func (p *loginPark) waitParked(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-p.arrived:
		case <-playerGiveUp(t):
			t.Fatalf("%d of %d logins reached the window between their check and their session", i, n)
		}
	}
}

func (p *loginPark) letGo() { p.once.Do(func() { close(p.release) }) }

// consoleClient is a browser at ts's console: no redirect followed, on a
// transport of ts's own, since a redemption's 302 carries no body (the
// transport bullet under Build, CI, and test discipline).
func consoleClient(ts *httptest.Server) *http.Client {
	return &http.Client{
		Transport:     ts.Client().Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// consoleAnswer is what the console answered a login or a redemption.
type consoleAnswer struct {
	status   int
	body     string
	location string
	session  string // the session cookie set, "" for none
	err      error
}

// do sends req and reads the answer. It makes no call on a testing.T, so a
// goroutine of the test's own may run it.
func do(client *http.Client, req *http.Request) consoleAnswer {
	resp, err := client.Do(req)
	if err != nil {
		return consoleAnswer{err: err}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	a := consoleAnswer{status: resp.StatusCode, body: string(body), location: resp.Header.Get("Location"), err: err}
	if c := sessionCookie(resp); c != nil {
		a.session = c.Value
	}
	return a
}

// postLogin is the login form's submit, as the console's script sends it.
func postLogin(client *http.Client, base, username, password string) consoleAnswer {
	body, err := json.Marshal(loginRequest{Username: username, Password: password})
	if err != nil {
		return consoleAnswer{err: err}
	}
	req, err := http.NewRequest(http.MethodPost, base+"/login", bytes.NewReader(body))
	if err != nil {
		return consoleAnswer{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", publicOrigin)
	return do(client, req)
}

// postTicket is the login link interstitial's Continue, as redeemTicket sends
// it.
func postTicket(client *http.Client, base, ticket string) consoleAnswer {
	req, err := http.NewRequest(http.MethodPost, base+"/login/ticket?t="+ticket, nil)
	if err != nil {
		return consoleAnswer{err: err}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", publicFixtureOrigin)
	return do(client, req)
}

// answered waits for a parked request's answer.
func answered(t *testing.T, answer <-chan consoleAnswer) consoleAnswer {
	t.Helper()
	select {
	case a := <-answer:
		if a.err != nil {
			t.Fatal(a.err)
		}
		return a
	case <-playerGiveUp(t):
		t.Fatal("the parked request never answered")
		return consoleAnswer{}
	}
}

// elsewhere is a command another process runs against the console's
// credential file while the bridge serves.
type elsewhere struct {
	name string
	run  func(t *testing.T, path string)
	// password is the one the file holds afterwards.
	password string
	// keeps is whether the sessions signed in before it stay signed in.
	keeps bool
}

func resetElsewhere(action adminauth.SessionAction) func(t *testing.T, path string) {
	return func(t *testing.T, path string) {
		t.Helper()
		b, err := adminauth.OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.ResetPassword("admin", resetPassword, action); err != nil {
			t.Fatal(err)
		}
	}
}

func signOutEverywhereElsewhere(t *testing.T, path string) {
	t.Helper()
	b, err := adminauth.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SignOutEverywhere(); err != nil {
		t.Fatal(err)
	}
}

var (
	resetDefault     = elsewhere{"bridge admin reset-password", resetElsewhere(adminauth.EndSessions), resetPassword, false}
	resetKeeping     = elsewhere{"bridge admin reset-password --keep-sessions", resetElsewhere(adminauth.KeepSessions), resetPassword, true}
	signOutElsewhere = elsewhere{"bridge admin sign-out-everywhere", signOutEverywhereElsewhere, signInPassword, false}
)

// takenInTheWindow is another console request reaching the serving bridge
// while the login is parked, with the session signed in before: its check
// reads the file, the rotation or the sign-out with it, and ends the session
// unless the command kept it. That request is what kept a login checked
// before a reset signed in until B881.
func takenInTheWindow(t *testing.T, ts *httptest.Server, established string, cmd elsewhere) {
	t.Helper()
	want := http.StatusUnauthorized
	if cmd.keeps {
		want = http.StatusOK
	}
	if status, body := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/api/stats", established); status != want {
		t.Fatalf("the request in the window after %s = %d, want %d: %s", cmd.name, status, want, body)
	}
}

// requireSessions requires that the serving bridge and the file, which a
// restart signs in, hold n sessions.
func requireSessions(t *testing.T, store *adminauth.Store, path string, n int, after string) {
	t.Helper()
	if got := store.SessionCount(); got != n {
		t.Errorf("after %s, the serving bridge holds %d sessions, want %d", after, got, n)
	}
	reopened, err := adminauth.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.SessionCount(); got != n {
		t.Errorf("after %s, the file holds %d sessions, which a restart signs in; want %d", after, got, n)
	}
}

// TestAConsoleLoginCheckedBeforeAResetIsRefused is backlog B881 through the
// login route: a password checked against the credential `bridge admin
// reset-password` then rotated away, or checked before `sign-out-everywhere`,
// is refused once the reset or the sign-out has landed, however the serving
// bridge took it. The answer is the one a wrong password gets, byte for byte,
// no cookie is set, no session is kept in the bridge or the file, and the user
// signs in again with the password the file holds. A reset that keeps the
// sessions moves no sign-out marker, so its row is the credential's half of
// the check alone: the session signed in before it stays.
func TestAConsoleLoginCheckedBeforeAResetIsRefused(t *testing.T) {
	for _, cmd := range []elsewhere{resetDefault, resetKeeping, signOutElsewhere} {
		t.Run(cmd.name, func(t *testing.T) {
			srv, store, _, path := newPublicTestServerAt(t, signInPassword)
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)
			client := consoleClient(ts)
			established := signIn(t, store)
			wrong := postLogin(client, ts.URL, "admin", "not the password")
			if wrong.err != nil || wrong.status != http.StatusUnauthorized {
				t.Fatalf("a wrong password = %d %v, want 401", wrong.status, wrong.err)
			}

			park := parkLogins(t, srv)
			answer := make(chan consoleAnswer, 1)
			go func() { answer <- postLogin(client, ts.URL, "admin", signInPassword) }()
			park.waitParked(t, 1)
			cmd.run(t, path)
			takenInTheWindow(t, ts, established, cmd)
			park.letGo()
			got := answered(t, answer)

			if got.status != wrong.status || got.body != wrong.body {
				t.Errorf("a login checked before %s = %d %q, want what a wrong password gets, %d %q",
					cmd.name, got.status, got.body, wrong.status, wrong.body)
			}
			if got.session != "" {
				t.Errorf("a login checked before %s was handed a session cookie", cmd.name)
			}
			kept := 0
			if cmd.keeps {
				kept = 1
				if status, _ := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/api/stats", established); status != http.StatusOK {
					t.Errorf("the session %s kept = %d, want 200", cmd.name, status)
				}
			}
			requireSessions(t, store, path, kept, cmd.name)

			// The user signs in again.
			srv.testAfterLoginCheck.Store(nil)
			again := postLogin(client, ts.URL, "admin", cmd.password)
			if again.err != nil || again.status != http.StatusOK || again.session == "" {
				t.Fatalf("signing in again after %s = %d %v (cookie set: %t), want 200 with a session", cmd.name, again.status, again.err, again.session != "")
			}
			if status, _ := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/api/stats", again.session); status != http.StatusOK {
				t.Errorf("the session signed in again after %s = %d, want 200", cmd.name, status)
			}
		})
	}
}

// TestALoginLinkRedeemedBeforeAResetIsRefused is the same window on the login
// link's route: POST /login/ticket redeems the ticket, which checks its account
// and spends it, and makes the session after. A reset or a sign-out landing
// between the two refuses the session, and the spent link answers as any
// spent link does, with the stale-link page. A reset that keeps the sessions
// does not refuse it: a ticket is not bound to the password (a ticket minted
// before a rotation redeems after it), and that row is green before B881 and
// after.
func TestALoginLinkRedeemedBeforeAResetIsRefused(t *testing.T) {
	for _, cmd := range []elsewhere{resetDefault, signOutElsewhere, resetKeeping} {
		t.Run(cmd.name, func(t *testing.T) {
			srv, store, _, path := newPublicTestServerAt(t, signInPassword)
			ts := httptest.NewServer(srv.Handler())
			t.Cleanup(ts.Close)
			client := consoleClient(ts)
			established := signIn(t, store)
			// `bridge admin login-link`, a process of its own.
			cli, err := adminauth.OpenStore(path)
			if err != nil {
				t.Fatal(err)
			}
			ticket, err := cli.MintLoginTicket("admin")
			if err != nil {
				t.Fatal(err)
			}

			park := parkLogins(t, srv)
			answer := make(chan consoleAnswer, 1)
			go func() { answer <- postTicket(client, ts.URL, ticket) }()
			park.waitParked(t, 1)
			cmd.run(t, path)
			takenInTheWindow(t, ts, established, cmd)
			park.letGo()
			got := answered(t, answer)

			if cmd.keeps {
				if got.status != http.StatusFound || got.location != "/" || got.session == "" {
					t.Fatalf("a link redeemed before %s = %d to %q (cookie set: %t), want 302 to / with a session",
						cmd.name, got.status, got.location, got.session != "")
				}
				if status, _ := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/api/stats", got.session); status != http.StatusOK {
					t.Errorf("the link's session = %d, want 200", status)
				}
				requireSessions(t, store, path, 2, cmd.name)
				return
			}
			if got.status != http.StatusFound || got.location != loginStaleLinkRedirect {
				t.Errorf("a link redeemed before %s = %d to %q, want 302 to %s", cmd.name, got.status, got.location, loginStaleLinkRedirect)
			}
			if got.session != "" {
				t.Errorf("a link redeemed before %s was handed a session cookie", cmd.name)
			}
			requireSessions(t, store, path, 0, cmd.name)
		})
	}
}

// TestConsoleLoginsRacingNothingButEachOtherSignIn is the guard, green before
// B881 and after: a login racing nothing signs in, and so do two logins whose
// checks both pass before either session is made. The check holds a login to
// the credential and the sign-out marker, never to the file's bytes, which the
// other login's session write changes.
func TestConsoleLoginsRacingNothingButEachOtherSignIn(t *testing.T) {
	srv, store, _, path := newPublicTestServerAt(t, signInPassword)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := consoleClient(ts)

	alone := postLogin(client, ts.URL, "admin", signInPassword)
	if alone.err != nil || alone.status != http.StatusOK || alone.session == "" {
		t.Fatalf("a login racing nothing = %d %v (cookie set: %t), want 200 with a session", alone.status, alone.err, alone.session != "")
	}

	park := parkLogins(t, srv)
	answers := make(chan consoleAnswer, 2)
	for range 2 {
		go func() { answers <- postLogin(client, ts.URL, "admin", signInPassword) }()
	}
	park.waitParked(t, 2)
	park.letGo()
	sessions := map[string]bool{alone.session: true}
	for i := 0; i < 2; i++ {
		got := answered(t, answers)
		if got.status != http.StatusOK || got.session == "" {
			t.Errorf("a login checked together with another = %d %q (cookie set: %t), want 200 with a session", got.status, got.body, got.session != "")
			continue
		}
		sessions[got.session] = true
	}
	if len(sessions) != 3 {
		t.Fatalf("three logins signed in %d distinct sessions, want 3", len(sessions))
	}
	for session := range sessions {
		if status, _ := consoleRequest(t, ts, publicOrigin, http.MethodGet, "/api/stats", session); status != http.StatusOK {
			t.Errorf("a session the logins signed in = %d, want 200", status)
		}
	}
	requireSessions(t, store, path, 3, "three logins")
}

// TestAConsoleLoginBesideAHeldStoreLockAnswersAsAStoreFailure: a login whose
// session's write waited out its bound for the store's lock, which another
// process holds (adminauth.ErrStoreBusy, backlog B881's second round), is
// answered as any store failure: the 500 that says no session was made, and
// no cookie. Once the lock is let go, the same login signs in.
func TestAConsoleLoginBesideAHeldStoreLockAnswersAsAStoreFailure(t *testing.T) {
	srv, store, _, path := newPublicTestServerAt(t, signInPassword)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := consoleClient(ts)
	lock, err := fsutil.OpenLockFile(filepath.Dir(path), adminauth.LockFileName)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.TryLock(lock); err != nil {
		lock.Close()
		t.Fatalf("take the store's lock for the test: %v", err)
	}
	var released sync.Once
	release := func() {
		released.Do(func() {
			_ = fsutil.Unlock(lock)
			_ = lock.Close()
		})
	}
	t.Cleanup(release)

	got := postLogin(client, ts.URL, "admin", signInPassword)
	var envelope struct{ Error, Message string }
	if got.err != nil || got.status != http.StatusInternalServerError || json.Unmarshal([]byte(got.body), &envelope) != nil ||
		envelope.Error != "internal" || envelope.Message != "could not create session" {
		t.Errorf("a login beside a held store lock = %d %q (%v), want the 500 a store failure gets", got.status, got.body, got.err)
	}
	if got.session != "" {
		t.Error("a login whose session was never written was handed a cookie")
	}
	if n := store.SessionCount(); n != 0 {
		t.Errorf("the serving bridge holds %d sessions after the login that was never written, want none", n)
	}

	release()
	again := postLogin(client, ts.URL, "admin", signInPassword)
	if again.err != nil || again.status != http.StatusOK || again.session == "" {
		t.Errorf("the login once the lock is let go = %d %v (cookie set: %t), want 200 with a session", again.status, again.err, again.session != "")
	}
}

// TestARefusedConsoleLoginIsCountedAsAFailedOne: a login refused because its
// check no longer holds is answered as a wrong password in every respect, the
// rate limiter's count included. The attempt was reserved before its check, as
// every attempt is, and the refusal records no success, so it stays counted:
// otherwise the limiter would answer the next attempts differently after a
// password that was right a moment ago. Four wrong passwords and the refused
// login fill the bucket, as five wrong passwords do.
func TestARefusedConsoleLoginIsCountedAsAFailedOne(t *testing.T) {
	srv, _, limiter, path := newPublicTestServerAt(t, signInPassword)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := consoleClient(ts)
	for i := 0; i < adminauth.RateLimitMaxAttempts-1; i++ {
		if got := postLogin(client, ts.URL, "admin", "not the password"); got.err != nil || got.status != http.StatusUnauthorized {
			t.Fatalf("wrong password %d = %d %v, want 401", i+1, got.status, got.err)
		}
	}

	park := parkLogins(t, srv)
	answer := make(chan consoleAnswer, 1)
	go func() { answer <- postLogin(client, ts.URL, "admin", signInPassword) }()
	park.waitParked(t, 1)
	resetDefault.run(t, path)
	park.letGo()
	if got := answered(t, answer); got.status != http.StatusUnauthorized {
		t.Fatalf("a login checked before a reset = %d %q, want 401", got.status, got.body)
	}
	clientIP := mustParseURL(t, ts.URL).Hostname()
	if limiter.Allow(clientIP, "admin") {
		t.Error("four wrong passwords and a login refused for a reset left the (IP, user) open: the refusal was not counted as a failed login")
	}
}
