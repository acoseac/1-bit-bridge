package enrich

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// hop is one request a test server saw: the path it asked for and the
// Authorization header it carried.
type hop struct{ path, auth string }

// hopLog keeps every hop one test server saw, in order, and builds the
// handlers the redirect tests wire together.
type hopLog struct {
	mu   sync.Mutex
	seen []hop
}

func (l *hopLog) note(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = append(l.seen, hop{r.URL.Path, r.Header.Get("Authorization")})
}

func (l *hopLog) hops() []hop {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]hop(nil), l.seen...)
}

// answering records a request and answers it with an empty search result.
func (l *hopLog) answering() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l.note(r)
		_, _ = io.WriteString(w, `{"releases":[],"artists":[]}`)
	}
}

// redirecting records a request and sends it, path kept, to target: an
// absolute Location, which carries no user information of its own.
func (l *hopLog) redirecting(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l.note(r)
		http.Redirect(w, r, target+r.URL.Path, http.StatusFound)
	}
}

// bouncing records a request, sends it to /moved<path> on the origin that
// received it (a relative Location, so the scheme stays what it was) and
// answers that one.
func (l *hopLog) bouncing() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l.note(r)
		if strings.HasPrefix(r.URL.Path, "/moved/") {
			_, _ = io.WriteString(w, `{"releases":[],"artists":[]}`)
			return
		}
		http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusFound)
	}
}

// startPlain starts a plain-http test server for the test's lifetime.
func startPlain(t testing.TB, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// startTLS starts a TLS test server for the test's lifetime. Every httptest
// TLS server presents the same certificate, so the Client of any one of them
// trusts all of them.
func startTLS(t testing.TB, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// credentialedClient is one of the three clients that send a credential to a
// mirror an operator configured, driven through its constructor.
type credentialedClient struct {
	name string
	// want is the Authorization header the client sends to its mirror.
	want string
	// base is what an operator writes for a mirror at u: the credential as
	// user information for MusicBrainz and Cover Art, and nothing for the
	// premium fetch, which always sends a bearer token.
	base func(t testing.TB, u string) string
	// send sends one request through a client the constructor builds over hc
	// for base, and returns the request's error.
	send func(t testing.TB, hc *http.Client, base string) error
}

// sendTo sends one request to the mirror at u, with the credential the
// client is configured with.
func (c credentialedClient) sendTo(t testing.TB, hc *http.Client, u string) error {
	return c.send(t, hc, c.base(t, u))
}

var credentialedClients = []credentialedClient{
	{
		name: "MusicBrainz",
		want: basicHeader("tok"),
		base: func(t testing.TB, u string) string { return withUserinfo(t, u, "tok") },
		send: func(_ testing.TB, hc *http.Client, base string) error {
			_, err := NewMusicBrainzClient(base+"/ws/2", "t", hc).
				SearchRelease(context.Background(), "Artist", "Album")
			return err
		},
	},
	{
		name: "Cover Art",
		want: basicHeader("tok"),
		base: func(t testing.TB, u string) string { return withUserinfo(t, u, "tok") },
		send: func(_ testing.TB, hc *http.Client, base string) error {
			body, err := NewCoverArtClient(base, "t", hc).
				FetchReleaseFrontStream(context.Background(), cancelReleaseMBID, 500)
			if err == nil {
				_ = body.Close()
			}
			return err
		},
	},
	{
		name: "Atlas premium cover",
		want: "Bearer tok123",
		base: func(_ testing.TB, u string) string { return u },
		send: func(t testing.TB, hc *http.Client, base string) error {
			f := NewAtlasPremiumFetcher(fakeCred{token: "tok123", base: base, ok: true}, "ua", hc, "")
			_, err := f.RefetchPremium(context.Background(), filepath.Join(t.TempDir(), "x.jpg"), cancelReleaseMBID, 500)
			return err
		},
	},
}

// TestNoCredentialFollowsARedirectFromHTTPSToACleartextHop pins the rule the
// redirect guard exists for. net/http copies an explicit Authorization header
// onto a redirect to the same host and compares host names only (never the
// scheme, never the port), so an https mirror that answers with a redirect to
// http on its own host was sent the credential in cleartext once the
// credential moved from the URL to a header (backlog B69). Before that an
// absolute Location carried no user information, so the plain hop got none,
// and a relative one stayed on the scheme it came from. All three clients
// that send a credential are driven, each against four redirects: to plain
// http on the same host (followed, without the credential), from that hop on
// to a further hop of its own (still without it, which a guard that strips
// only at the transition would miss, since net/http copies the header from the
// first request again on every hop), to https on the same host and to a path
// on its own https origin (both keep it).
func TestNoCredentialFollowsARedirectFromHTTPSToACleartextHop(t *testing.T) {
	for _, c := range credentialedClients {
		t.Run(c.name, func(t *testing.T) {
			t.Run("a redirect to plain http on the same host is followed without the credential", func(t *testing.T) {
				plain := &hopLog{}
				plainSrv := startPlain(t, plain.answering())
				mirror := &hopLog{}
				mirrorSrv := startTLS(t, mirror.redirecting(plainSrv.URL))

				if err := c.sendTo(t, mirrorSrv.Client(), mirrorSrv.URL); err != nil {
					t.Fatalf("the request failed, and should have been followed without the credential: %v", err)
				}
				if got := mirror.hops(); len(got) != 1 || got[0].auth != c.want {
					t.Errorf("the https mirror saw %v, want one request carrying %q", got, c.want)
				}
				if got := plain.hops(); len(got) != 1 || got[0].auth != "" {
					t.Errorf("the plain http hop saw %v, want one request with no Authorization", got)
				}
			})

			t.Run("nor does a further hop of that cleartext server's own", func(t *testing.T) {
				plain := &hopLog{}
				plainSrv := startPlain(t, plain.bouncing())
				mirror := &hopLog{}
				mirrorSrv := startTLS(t, mirror.redirecting(plainSrv.URL))

				if err := c.sendTo(t, mirrorSrv.Client(), mirrorSrv.URL); err != nil {
					t.Fatalf("the request failed: %v", err)
				}
				got := plain.hops()
				if len(got) != 2 || !strings.HasPrefix(got[1].path, "/moved/") {
					t.Fatalf("the plain http server saw %v, want the request and its own redirect", got)
				}
				for _, h := range got {
					if h.auth != "" {
						t.Errorf("a cleartext hop (%s) carried Authorization %q", h.path, h.auth)
					}
				}
			})

			t.Run("a redirect to https on the same host keeps it", func(t *testing.T) {
				other := &hopLog{}
				otherSrv := startTLS(t, other.answering())
				mirror := &hopLog{}
				mirrorSrv := startTLS(t, mirror.redirecting(otherSrv.URL))

				if err := c.sendTo(t, mirrorSrv.Client(), mirrorSrv.URL); err != nil {
					t.Fatalf("the request failed: %v", err)
				}
				if got := other.hops(); len(got) != 1 || got[0].auth != c.want {
					t.Errorf("the second https server saw %v, want one request carrying %q", got, c.want)
				}
			})

			t.Run("a relative redirect on its own https origin keeps it", func(t *testing.T) {
				mirror := &hopLog{}
				mirrorSrv := startTLS(t, mirror.bouncing())

				if err := c.sendTo(t, mirrorSrv.Client(), mirrorSrv.URL); err != nil {
					t.Fatalf("the request failed: %v", err)
				}
				got := mirror.hops()
				if len(got) != 2 || got[0].auth != c.want || got[1].auth != c.want {
					t.Errorf("the mirror saw %v, want both hops to carry %q", got, c.want)
				}
			})
		})
	}
}

// TestARequestWithNoCredentialStillFollowsARedirectToPlainHTTP pins why the
// guard strips the header and does not refuse the hop. The public MusicBrainz
// and Cover Art hosts, and any mirror written without user information, send
// no credential, so a downgrade redirect leaves nothing to protect and must
// keep working as it always has, where a refusal would end it with an error.
func TestARequestWithNoCredentialStillFollowsARedirectToPlainHTTP(t *testing.T) {
	for _, c := range credentialedClients {
		if c.name == "Atlas premium cover" {
			continue // it never sends a request without its token
		}
		t.Run(c.name, func(t *testing.T) {
			plain := &hopLog{}
			plainSrv := startPlain(t, plain.answering())
			mirrorSrv := startTLS(t, (&hopLog{}).redirecting(plainSrv.URL))

			if err := c.send(t, mirrorSrv.Client(), mirrorSrv.URL); err != nil {
				t.Fatalf("a request with no credential was not followed to plain http: %v", err)
			}
			if got := plain.hops(); len(got) != 1 || got[0].auth != "" {
				t.Errorf("the plain http hop saw %v, want one request with no Authorization", got)
			}
		})
	}
}

// TestTheCredentialGuardKeepsACallersRedirectPolicy pins what installing a
// redirect policy on a client must not take from it. A client that sets a
// CheckRedirect takes over net/http's own rule (stop after ten hops) as well,
// so the guard restates it; a caller's own policy is still asked, after the
// header is dropped, and its refusal still ends the request; and the caller's
// client is never written to, since *http.Client values are shared (the
// trap NewDeezerClient's comment names).
func TestTheCredentialGuardKeepsACallersRedirectPolicy(t *testing.T) {
	errRefused := errors.New("the caller's policy refuses this hop")
	for _, c := range credentialedClients {
		t.Run(c.name, func(t *testing.T) {
			t.Run("the caller's policy still decides, after the header is dropped", func(t *testing.T) {
				plain := &hopLog{}
				plainSrv := startPlain(t, plain.bouncing())
				mirrorSrv := startTLS(t, (&hopLog{}).redirecting(plainSrv.URL))

				var asked []hop
				hc := mirrorSrv.Client()
				hc.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
					asked = append(asked, hop{req.URL.Path, req.Header.Get("Authorization")})
					if strings.HasPrefix(req.URL.Path, "/moved/") {
						return errRefused
					}
					return nil
				}
				err := c.sendTo(t, hc, mirrorSrv.URL)
				if !errors.Is(err, errRefused) {
					t.Fatalf("the caller's refusal did not end the request: %v", err)
				}
				if len(asked) != 2 || asked[0].auth != "" || asked[1].auth != "" {
					t.Errorf("the caller's policy was asked %v, want two hops, each already without Authorization", asked)
				}
				if got := plain.hops(); len(got) != 1 {
					t.Errorf("the plain server saw %v, want the one request before the refused hop", got)
				}
			})

			t.Run("net/http's limit of ten redirects still ends a loop", func(t *testing.T) {
				var hits atomic.Int32
				// A fixed target, where the request's own path would do as well:
				// a redirect taken from the request reads as an open redirect
				// (SonarCloud gosecurity:S5146), which a loop server is not.
				srv := startPlain(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					http.Redirect(w, r, "/loop", http.StatusFound)
				}))
				_, refErr := (&http.Client{}).Get(srv.URL + "/loop")
				refHits := hits.Swap(0)
				if refErr == nil || !strings.Contains(refErr.Error(), "stopped after 10 redirects") {
					t.Fatalf("net/http's own client answered %v, want its ten-redirect refusal", refErr)
				}

				err := c.sendTo(t, nil, srv.URL)
				if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
					t.Fatalf("the client answered %v to a redirect loop, want net/http's refusal after ten", err)
				}
				if got := hits.Load(); got != refHits {
					t.Errorf("the loop reached the server %d times, want the %d net/http's own client makes", got, refHits)
				}
			})

			t.Run("the caller's client is left as it was", func(t *testing.T) {
				plain := &hopLog{}
				plainSrv := startPlain(t, plain.answering())
				shared := &http.Client{}
				if err := c.sendTo(t, shared, plainSrv.URL); err != nil {
					t.Fatalf("the request failed: %v", err)
				}
				if shared.CheckRedirect != nil {
					t.Error("the constructor installed its redirect guard on the caller's client")
				}
			})
		})
	}
}
