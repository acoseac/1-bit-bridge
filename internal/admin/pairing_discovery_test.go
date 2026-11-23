package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/pairingcode"
)

// A pairing request reads the endpoint list its QR carries before it takes the
// console's lock and writes a token (backlog B997). The list can wait behind
// another caller's Tailscale status read, up to that read's own bound, and a
// request that ends while it waits changes no credential: no secret is made
// or replaced that nobody was shown, and the pairing requests behind it do not
// wait on the console's lock for it.

// countedCodes is the pairing links' code store, counting the codes the
// console asks it for.
type countedCodes struct {
	*pairingcode.Store
	issued atomic.Int32
}

func (c *countedCodes) Issue(tokenID, tokenHash string) (string, error) {
	c.issued.Add(1)
	return c.Store.Issue(tokenID, tokenHash)
}

// askPairing serves a POST of body to path on ctx, on a goroutine of its own:
// done is closed once the handler has returned, its answer then in rec, and
// every call that wrote to it in the copy rec.Writes() takes.
func askPairing(srv *admin.Server, ctx context.Context, path, body string) (rec *admin.WriteCallRecorder, done <-chan struct{}) {
	rec = admin.NewWriteCallRecorder()
	finished := make(chan struct{})
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
	req.RemoteAddr, req.Host = "127.0.0.1:51000", "127.0.0.1:7789"
	req.Header.Set("Content-Type", "application/json")
	go func() {
		defer close(finished)
		srv.Handler().ServeHTTP(rec, req)
	}()
	return rec, finished
}

// TestAPairingRequestThatEndsWhileItsListWaitsChangesNoCredential: the mint
// and the rotation, each with the API's shared Tailscale status read held
// under its endpoint list, return as soon as their request ends, writing
// nothing to their response (no WriteHeader, Write, WriteString or Flush:
// admin.WriteCallRecorder): no token is minted or rotated and no code issued,
// the rotated token's secret and its code from before still work, and the
// status read they left stays held, then runs to its end once let go and is
// cached whole, /v1/health's list carrying the tailnet's name it found, asked
// once.
func TestAPairingRequestThatEndsWhileItsListWaitsChangesNoCredential(t *testing.T) {
	const dial = "https://box.local:7788"
	for _, route := range []string{"mint", "rotate"} {
		t.Run(route, func(t *testing.T) {
			held := newHeldTailnet()
			var releaseOnce sync.Once
			letGo := func() { releaseOnce.Do(func() { close(held.release) }) }
			t.Cleanup(letGo)
			codes := &countedCodes{Store: pairingcode.New()}
			srv, apiSrv, tokens := consoleBesideTheAPIIssuing(t, held, codes)

			path, body := "/api/tokens", `{"name":"Phone","url":"`+dial+`"}`
			var raw, code string
			var before auth.Token
			if route == "rotate" {
				var err error
				if raw, before, err = tokens.Mint("Phone"); err != nil {
					t.Fatal(err)
				}
				if code, err = codes.Store.Issue(before.ID, before.Hash); err != nil {
					t.Fatal(err)
				}
				path, body = "/api/tokens/"+before.ID+"/rotate", `{"url":"`+dial+`"}`
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rec, done := askPairing(srv, ctx, path, body)
			t.Cleanup(func() {
				letGo()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Errorf("the %s had not returned 10 s after its status read was let go", route)
				}
			})
			select {
			case <-held.asked:
			case <-time.After(10 * time.Second):
				t.Fatalf("the %s never asked the Tailscale status for its endpoint list", route)
			}
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("the %s did not return when its request ended: its endpoint list was not waited for on the request's context", route)
			}
			if writes := rec.Writes(); len(writes) != 0 {
				t.Errorf("the %s wrote %v to a request that had ended, want nothing written", route, writes)
			}
			switch route {
			case "mint":
				if list := tokens.List(); len(list) != 0 {
					t.Errorf("a mint whose request ended while its endpoint list waited made %d token(s), want none", len(list))
				}
			case "rotate":
				after, err := tokens.Get(before.ID)
				if err != nil {
					t.Fatal(err)
				}
				if after.Hash != before.Hash || !after.RotatedAt.IsZero() {
					t.Errorf("a rotation whose request ended while its endpoint list waited rotated the token (rotated at %v)", after.RotatedAt)
				}
				if _, ok := tokens.Validate(raw); !ok {
					t.Error("the token's secret from before the ended rotation no longer validates")
				}
			}
			if n := codes.issued.Load(); n != 0 {
				t.Errorf("a %s whose request ended while its endpoint list waited issued %d pairing code(s), want none", route, n)
			}
			if calls, returned := held.counts(); calls != 1 || returned != 0 {
				t.Fatalf("with the %s ended the status was asked %d times and returned %d, want one read still held", route, calls, returned)
			}
			letGo()
			select {
			case <-held.back:
			case <-time.After(10 * time.Second):
				t.Fatal("the shared status read never returned once let go")
			}
			want := "https://" + tailnetHeldTestName + ":7788"
			if eps := apiSrv.HealthEndpoints(); !slices.Contains(eps, want) {
				t.Fatalf("/v1/health's endpoints %v do not carry %s: the read the ended %s left was not cached whole", eps, want, route)
			}
			if calls, _ := held.counts(); calls != 1 {
				t.Fatalf("the status was asked %d times, want the one shared read", calls)
			}
			if route == "rotate" {
				// A rotation issues the token a fresh code, which drops the one
				// before (pairingcode.Store keeps one live code a token).
				id, hash, ok := codes.Store.Take(code)
				if !ok || id != before.ID || hash != before.Hash {
					t.Errorf("the code issued before the ended rotation redeems as (%q, %v), want the token as it was", id, ok)
				}
			}
		})
	}
}
