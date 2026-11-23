package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/pairingcode"
)

// A pairing request whose context has ended when its handler acquires the
// console's lock (Server.mu) performs no credential mutation (backlog B997's
// second round): the mint and the rotation look at the request's context once
// they hold the lock, before Auth.Mint or Auth.Rotate, and return writing
// nothing. Round 1's case ends a request while its endpoint list waits; these
// end one while it waits for the lock, and send one already ended down the
// paths that read no list.

// pairingDial is the explicit pairing URL every case sends, one
// resolvePairingURL takes as it is.
const pairingDial = "https://box.local:7788"

// pairingRoutes are the two pairing writes.
var pairingRoutes = []string{"mint", "rotate"}

// issuedCodes is the pairing links' code store, counting the codes the
// console asks it for.
type issuedCodes struct {
	*pairingcode.Store
	issued atomic.Int32
}

func (c *issuedCodes) Issue(tokenID, tokenHash string) (string, error) {
	c.issued.Add(1)
	return c.Store.Issue(tokenID, tokenHash)
}

// pairingRig is a test console whose pairing codes are counted, with, for the
// rotation, a token minted beforehand and a code issued for it.
type pairingRig struct {
	srv    *Server
	cfg    *config.Config
	codes  *issuedCodes
	route  string
	raw    string
	before auth.Token
	code   string
}

// newPairingRig is the rig of one route.
func newPairingRig(t *testing.T, route string) *pairingRig {
	t.Helper()
	srv, cfg, _ := newTestServer(t)
	r := &pairingRig{srv: srv, cfg: cfg, codes: &issuedCodes{Store: pairingcode.New()}, route: route}
	srv.deps.PairingCodes = r.codes
	if route == "rotate" {
		var err error
		if r.raw, r.before, err = srv.deps.Auth.Mint("Phone"); err != nil {
			t.Fatal(err)
		}
		if r.code, err = r.codes.Store.Issue(r.before.ID, r.before.Hash); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// request is the route's POST on ctx, as the console's loopback socket takes
// it, its path value set for a handler called directly.
func (r *pairingRig) request(ctx context.Context) *http.Request {
	path, body := "/api/tokens", `{"name":"Phone","url":"`+pairingDial+`"}`
	if r.route == "rotate" {
		path, body = "/api/tokens/"+r.before.ID+"/rotate", `{"url":"`+pairingDial+`"}`
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	req.RemoteAddr, req.Host = "127.0.0.1:51000", "127.0.0.1:7789"
	req.Header.Set("Content-Type", "application/json")
	if r.route == "rotate" {
		req.SetPathValue("id", r.before.ID)
	}
	return req
}

// handler is the route's handler, to call directly.
func (r *pairingRig) handler() http.HandlerFunc {
	if r.route == "rotate" {
		return r.srv.apiTokensRotate
	}
	return r.srv.apiTokensMint
}

// WriteCallRecorder is an httptest.ResponseRecorder that notes, in order,
// every call that writes to the response: WriteHeader, Write, WriteString and
// Flush, each overriding the method the embedded recorder would promote, so
// none reaches the recorder unnoted. A check that a handler wrote nothing
// reads Writes, never the recorder alone: NewRecorder's Code is 200 before
// anything is written, and a WriteHeader(200) with no body moves neither Body,
// Flushed nor the header map. One mutex orders the calls and the check: each
// of the four holds it across its note and its call into the embedded
// recorder, which is not safe for concurrent use either, and Writes copies the
// notes under it, so a write a handler leaves to a goroutine of its own,
// during or after the handler, is read as a call made, never as a data race.
// Exported for the pairing discovery case of package admin_test.
type WriteCallRecorder struct {
	*httptest.ResponseRecorder
	// mu guards writes and every call into the embedded recorder.
	mu sync.Mutex
	// writes holds each call as it was made, its argument quoted.
	writes []string
}

// NewWriteCallRecorder is a WriteCallRecorder over httptest.NewRecorder.
func NewWriteCallRecorder() *WriteCallRecorder {
	return &WriteCallRecorder{ResponseRecorder: httptest.NewRecorder()}
}

// Writes is a copy of the calls noted so far, in the order they were made,
// taken under the recorder's mutex.
func (w *WriteCallRecorder) Writes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.writes)
}

func (w *WriteCallRecorder) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, fmt.Sprintf("WriteHeader(%d)", code))
	w.ResponseRecorder.WriteHeader(code)
}

func (w *WriteCallRecorder) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, fmt.Sprintf("Write(%q)", b))
	return w.ResponseRecorder.Write(b)
}

func (w *WriteCallRecorder) WriteString(s string) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, fmt.Sprintf("WriteString(%q)", s))
	return w.ResponseRecorder.WriteString(s)
}

func (w *WriteCallRecorder) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, "Flush()")
	w.ResponseRecorder.Flush()
}

// requireNothingWritten fails unless the handler wrote nothing to its response
// (no WriteHeader, Write, WriteString or Flush: WriteCallRecorder, read from
// the copy its Writes takes under its mutex) and changed no credential: no
// token made or rotated and no code issued, the rotated token's secret and its
// code from before still working.
func (r *pairingRig) requireNothingWritten(t *testing.T, rec *WriteCallRecorder) {
	t.Helper()
	if writes := rec.Writes(); len(writes) != 0 {
		t.Errorf("the %s wrote %v to a request that had ended, want nothing written", r.route, writes)
	}
	tokens := r.srv.deps.Auth.List()
	switch r.route {
	case "mint":
		if len(tokens) != 0 {
			t.Errorf("a mint whose request had ended made %d token(s), want none", len(tokens))
		}
	case "rotate":
		after, err := r.srv.deps.Auth.Get(r.before.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(tokens) != 1 || after.Hash != r.before.Hash || !after.RotatedAt.IsZero() {
			t.Errorf("a rotation whose request had ended rotated the token (rotated at %v, %d token(s))", after.RotatedAt, len(tokens))
		}
		if _, ok := r.srv.deps.Auth.Validate(r.raw); !ok {
			t.Error("the token's secret from before no longer validates")
		}
	}
	if n := r.codes.issued.Load(); n != 0 {
		t.Errorf("a %s whose request had ended issued %d pairing code(s), want none", r.route, n)
	}
	if r.route == "rotate" {
		if id, hash, ok := r.codes.Store.Take(r.code); !ok || id != r.before.ID || hash != r.before.Hash {
			t.Errorf("the code issued before redeems as (%q, %v), want the token as it was", id, ok)
		}
	}
}

// TestAPairingRequestEndedWhileItWaitsForTheLockWritesNothing: the mint and
// the rotation, their endpoint list read, find the console's lock held by the
// case (testPairingWriteWaits tells it so), and their request ends there; let
// go, each returns having written nothing to its response and changed no
// credential.
func TestAPairingRequestEndedWhileItWaitsForTheLockWritesNothing(t *testing.T) {
	for _, route := range pairingRoutes {
		t.Run(route, func(t *testing.T) {
			r := newPairingRig(t, route)
			r.srv.deps.Endpoints = func() []advertise.Endpoint {
				return []advertise.Endpoint{{URL: pairingDial, Class: advertise.ClassMDNSHost}}
			}
			atLock := make(chan struct{})
			var told sync.Once
			r.srv.testPairingWriteWaits = func() { told.Do(func() { close(atLock) }) }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r.srv.mu.Lock()
			var released sync.Once
			release := func() { released.Do(r.srv.mu.Unlock) }
			rec, done := NewWriteCallRecorder(), make(chan struct{})
			go func() {
				defer close(done)
				r.srv.Handler().ServeHTTP(rec, r.request(ctx))
			}()
			t.Cleanup(func() {
				release()
				cancel()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Errorf("the %s had not returned 10 s after the console's lock was let go", route)
				}
			})
			select {
			case <-atLock:
			case <-time.After(10 * time.Second):
				t.Fatalf("the %s never came to the console's lock with its endpoint list read", route)
			}
			cancel()
			release()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("the %s did not return once the console's lock was let go", route)
			}
			r.requireNothingWritten(t, rec)
		})
	}
}

// TestAnEndedPairingRequestOnAPathThatReadsNoListWritesNothing: a request
// already ended that reaches the console's lock down a path reading no
// endpoint list (a public bridge, a console with no provider, a listen address
// with no port) writes nothing either. Each handler is called directly, past
// the console's middleware, which would refuse the public bridge's request
// before the handler, and the provider wired where the path leaves it unasked
// is never asked.
func TestAnEndedPairingRequestOnAPathThatReadsNoListWritesNothing(t *testing.T) {
	for _, path := range []struct {
		name string
		set  func(*pairingRig)
	}{
		{"public", func(r *pairingRig) { r.cfg.Deployment.Mode = "public" }},
		{"no-provider", func(r *pairingRig) { r.srv.deps.Endpoints = nil }},
		{"no-port", func(r *pairingRig) { r.cfg.ListenAddress = "127.0.0.1" }},
	} {
		for _, route := range pairingRoutes {
			t.Run(path.name+"/"+route, func(t *testing.T) {
				r := newPairingRig(t, route)
				var asked atomic.Int32
				r.srv.deps.Endpoints = func() []advertise.Endpoint {
					asked.Add(1)
					return nil
				}
				path.set(r)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				rec := NewWriteCallRecorder()
				r.handler()(rec, r.request(ctx))
				r.requireNothingWritten(t, rec)
				if n := asked.Load(); n != 0 {
					t.Errorf("the %s asked the endpoint provider %d time(s) on a path that reads no list", route, n)
				}
			})
		}
	}
}

// TestTheWriteCallRecorderNotesCallsMadeOnAnotherGoroutine: a goroutine makes
// each call WriteCallRecorder notes while the test reads Writes, nothing
// ordering a read after any call, as a check would read beside a write a
// handler left to a goroutine of its own. Every copy read is the calls made so
// far, in order, and under -race the recorder's mutex leaves nothing to
// report; once the goroutine has ended, Writes holds the four calls and the
// embedded recorder what they wrote.
func TestTheWriteCallRecorderNotesCallsMadeOnAnotherGoroutine(t *testing.T) {
	want := []string{"WriteHeader(202)", `Write("a")`, `WriteString("b")`, "Flush()"}
	rec := NewWriteCallRecorder()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		rec.WriteHeader(http.StatusAccepted)
		_, _ = rec.Write([]byte("a"))
		_, _ = rec.WriteString("b")
		rec.Flush()
	}()
	var stray []string
	for range 1000 {
		if got := rec.Writes(); stray == nil && (len(got) > len(want) || !slices.Equal(got, want[:len(got)])) {
			stray = got
		}
	}
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the goroutine making the calls had not ended 10 s after the reads")
	}
	if stray != nil {
		t.Errorf("a copy read while the calls were made is %q, want the calls made so far of %q", stray, want)
	}
	if got := rec.Writes(); !slices.Equal(got, want) {
		t.Errorf("Writes holds %q once the goroutine has ended, want %q", got, want)
	}
	if rec.Code != http.StatusAccepted || rec.Body.String() != "ab" || !rec.Flushed {
		t.Errorf("the embedded recorder holds code %d, body %q and flushed %v, want 202, \"ab\" and true", rec.Code, rec.Body.String(), rec.Flushed)
	}
}
