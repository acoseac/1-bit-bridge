package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// requireNothingWritten fails unless the handler answered nothing and changed
// no credential: no token made or rotated and no code issued, the rotated
// token's secret and its code from before still working.
func (r *pairingRig) requireNothingWritten(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Body.Len() != 0 {
		t.Errorf("the %s answered %d %q to a request that had ended, want nothing", r.route, rec.Code, rec.Body.String())
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
// go, each returns having answered nothing and changed no credential.
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
			rec, done := httptest.NewRecorder(), make(chan struct{})
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
				rec := httptest.NewRecorder()
				r.handler()(rec, r.request(ctx))
				r.requireNothingWritten(t, rec)
				if n := asked.Load(); n != 0 {
					t.Errorf("the %s asked the endpoint provider %d time(s) on a path that reads no list", route, n)
				}
			})
		}
	}
}
