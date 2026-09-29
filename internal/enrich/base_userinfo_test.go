package enrich

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// userinfoShapes are the ways an operator can write a credential into an
// enrich base URL. secrets are the fragments no error or log line may carry,
// each searched without regard to case: url.Parse lowercases a scheme, which
// is how a user name written without one got past a case-sensitive search
// (backlog B54). The user name of the second shape is deliberately unlike any
// word an error or a header holds.
var userinfoShapes = []struct {
	name     string
	userinfo string
	secrets  []string
}{
	{"a token written as the user name", "s3cret-Pw", []string{"s3cret-Pw"}},
	{"a user name and a password", "mirror-usr:s3cret-Pw", []string{"mirror-usr", "s3cret-Pw"}},
	{"a user name and an empty password", "s3cret-Pw:", []string{"s3cret-Pw"}},
	{"an empty user name and a password", ":s3cret-Pw", []string{"s3cret-Pw"}},
	{"a percent-encoded user name and password", "s3cret%40Pw:p%3Ass", []string{"s3cret", "p%3ass", "p:ss"}},
	{"an empty user information", "", nil},
}

// withUserinfo writes userinfo before serverURL's host, as an operator writes
// a credential into a base: http://127.0.0.1:9 becomes http://userinfo@127.0.0.1:9.
func withUserinfo(t testing.TB, serverURL, userinfo string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Scheme + "://" + userinfo + "@" + u.Host + u.Path
}

// leakedSecret returns the first of secrets that text carries, compared
// without regard to case, or "" when it carries none.
func leakedSecret(text string, secrets []string) string {
	lower := strings.ToLower(text)
	for _, s := range secrets {
		if strings.Contains(lower, strings.ToLower(s)) {
			return s
		}
	}
	return ""
}

// basicHeader is the Authorization header for a user name with no password:
// what a base written `https://user@mirror` is sent as.
func basicHeader(user string) string {
	r, _ := http.NewRequest(http.MethodGet, "http://x", nil)
	r.SetBasicAuth(user, "")
	return r.Header.Get("Authorization")
}

// authRecorder is a mirror that answers every request with one status and an
// empty search result, and keeps the Authorization header of each in order.
type authRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

// newAuthRecorder starts an authRecorder answering status for the test's
// lifetime.
func newAuthRecorder(t testing.TB, status int) *authRecorder {
	t.Helper()
	r := &authRecorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.seen = append(r.seen, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"releases":[],"artists":[]}`)
	}))
	t.Cleanup(r.Close)
	return r
}

// headers returns the Authorization header of every request so far.
func (r *authRecorder) headers() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// last returns the Authorization header of the latest request, or "".
func (r *authRecorder) last() string {
	h := r.headers()
	if len(h) == 0 {
		return ""
	}
	return h[len(h)-1]
}

// TestABaseURLsCredentialReachesTheMirrorAsBasicAuth pins the half of backlog
// B69 that must not move: a mirror behind an auth proxy keeps authenticating,
// with exactly the header net/http built from the same URL before the
// credential left the request URL. The reference is taken from net/http on
// every run rather than restated, so a shape the two spell differently (an
// empty password, an empty user name, an encoded one) is caught. Both clients
// are driven, a live base included.
func TestABaseURLsCredentialReachesTheMirrorAsBasicAuth(t *testing.T) {
	ctx := context.Background()
	for _, shape := range userinfoShapes {
		t.Run(shape.name, func(t *testing.T) {
			mirror := newAuthRecorder(t, http.StatusOK)
			base := withUserinfo(t, mirror.URL, shape.userinfo)

			resp, err := http.Get(base + "/reference")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			want := mirror.last()
			if !strings.HasPrefix(want, "Basic ") {
				t.Fatalf("reference request carried Authorization %q, want a Basic header", want)
			}

			if _, err := NewMusicBrainzClient(base+"/ws/2", "t", nil).SearchRelease(ctx, "Artist", "Album"); err != nil {
				t.Fatalf("MusicBrainz search: %v", err)
			}
			if got := mirror.last(); got != want {
				t.Errorf("MusicBrainz request: Authorization = %q, want %q (what net/http sends for the URL)", got, want)
			}

			live := NewMusicBrainzClient("", "t", nil).WithLiveBase(func() string { return base + "/ws/2" })
			if _, err := live.SearchArtist(ctx, "Artist"); err != nil {
				t.Fatalf("MusicBrainz search on a live base: %v", err)
			}
			if got := mirror.last(); got != want {
				t.Errorf("live MusicBrainz request: Authorization = %q, want %q", got, want)
			}

			cover := NewCoverArtClient(base, "t", nil)
			for name, fetch := range map[string]func() (io.ReadCloser, error){
				"release": func() (io.ReadCloser, error) {
					return cover.FetchReleaseFrontStream(ctx, cancelReleaseMBID, 500)
				},
				"release-group": func() (io.ReadCloser, error) {
					return cover.FetchReleaseGroupFrontStream(ctx, cancelReleaseMBID, 500)
				},
			} {
				body, err := fetch()
				if err != nil {
					t.Fatalf("Cover Art %s fetch: %v", name, err)
				}
				_ = body.Close()
				if got := mirror.last(); got != want {
					t.Errorf("Cover Art %s request: Authorization = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// newUntrustedTLSServer starts a TLS server whose certificate this process
// does not trust, and whose own log of the handshakes it was refused goes
// nowhere.
func newUntrustedTLSServer(t testing.TB) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// failingMirror is a way a mirror can fail every request at the transport,
// and starts one. Each ends in a *url.Error that names the request URL,
// which is what carried the credential: a redirect that never ends does not
// (net/http names the Location it refused, not the request), so it is not one
// of them.
type failingMirror struct {
	name  string
	start func(t testing.TB) string
}

var failingMirrors = []failingMirror{
	{"a refused connection", func(t testing.TB) string {
		srv := httptest.NewServer(http.NotFoundHandler())
		u := srv.URL
		srv.Close()
		return u
	}},
	{"a certificate this process does not trust", func(t testing.TB) string {
		return newUntrustedTLSServer(t).URL
	}},
	{"a server that hangs up", func(t testing.TB) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}},
}

// TestNoRequestErrorNamesABaseURLsCredential drives every request either
// client makes against every way a mirror can fail and every way a credential
// can be written, and requires that the error names the mirror (the check
// that it is the request's error and not another) and none of the
// credential. net/http masks a URL's password in that error and keeps a user
// name whole, so a token written as the user name reached every log line the
// enricher writes with it (backlog B69).
func TestNoRequestErrorNamesABaseURLsCredential(t *testing.T) {
	const mbid = cancelReleaseMBID
	type call func(ctx context.Context, base string) error
	mb := func(f func(ctx context.Context, c *MusicBrainzClient) error) call {
		return func(ctx context.Context, base string) error {
			return f(ctx, NewMusicBrainzClient(base+"/ws/2", "t", nil))
		}
	}
	cover := func(f func(ctx context.Context, c *CoverArtClient) error) call {
		return func(ctx context.Context, base string) error {
			return f(ctx, NewCoverArtClient(base, "t", nil))
		}
	}
	calls := []struct {
		name string
		do   call
	}{
		{"MusicBrainz release search", mb(func(ctx context.Context, c *MusicBrainzClient) error {
			_, err := c.SearchRelease(ctx, "Artist", "Album")
			return err
		})},
		{"MusicBrainz release-group lookup", mb(func(ctx context.Context, c *MusicBrainzClient) error {
			_, err := c.ReleaseGroupMBID(ctx, mbid)
			return err
		})},
		{"MusicBrainz artist search", mb(func(ctx context.Context, c *MusicBrainzClient) error {
			_, err := c.SearchArtist(ctx, "Artist")
			return err
		})},
		{"MusicBrainz search on a live base", func(ctx context.Context, base string) error {
			c := NewMusicBrainzClient("", "t", nil).WithLiveBase(func() string { return base + "/ws/2" })
			_, err := c.SearchRelease(ctx, "Artist", "Album")
			return err
		}},
		{"Cover Art release fetch", cover(func(ctx context.Context, c *CoverArtClient) error {
			_, err := c.FetchReleaseFront(ctx, mbid, 500)
			return err
		})},
		{"Cover Art release-group fetch", cover(func(ctx context.Context, c *CoverArtClient) error {
			_, err := c.FetchReleaseGroupFront(ctx, mbid, 500)
			return err
		})},
		{"Cover Art release stream", cover(func(ctx context.Context, c *CoverArtClient) error {
			_, err := c.FetchReleaseFrontStream(ctx, mbid, 500)
			return err
		})},
		{"Cover Art release-group stream", cover(func(ctx context.Context, c *CoverArtClient) error {
			_, err := c.FetchReleaseGroupFrontStream(ctx, mbid, 500)
			return err
		})},
		{"Cover Art fetch on a live base", func(ctx context.Context, base string) error {
			c := NewCoverArtClient("", "t", nil).WithLiveBase(func() string { return base })
			_, err := c.FetchReleaseFront(ctx, mbid, 500)
			return err
		}},
	}

	for _, fm := range failingMirrors {
		root := fm.start(t)
		host := strings.TrimPrefix(strings.TrimPrefix(root, "https://"), "http://")
		for _, shape := range userinfoShapes {
			if shape.secrets == nil {
				continue
			}
			base := withUserinfo(t, root, shape.userinfo)
			for _, c := range calls {
				err := c.do(context.Background(), base)
				if err == nil {
					t.Errorf("%s / %s / %s: no error from a mirror that fails every request", fm.name, shape.name, c.name)
					continue
				}
				if !strings.Contains(err.Error(), host) {
					t.Errorf("%s / %s / %s: error %q does not name the mirror %s, so it is not the request's error",
						fm.name, shape.name, c.name, err, host)
				}
				if s := leakedSecret(err.Error(), shape.secrets); s != "" {
					t.Errorf("%s / %s / %s: error carries the credential %q: %v", fm.name, shape.name, c.name, s, err)
				}
			}
		}
	}
}

// enricherOverBases builds an Enricher whose clients are pointed at the two
// bases, with no politeness gap between requests.
func enricherOverBases(t *testing.T, mbBase, caaBase string) (*Enricher, *manifest.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	e := NewEnricher(store, NewMusicBrainzClient(mbBase, "t", nil),
		NewCoverArtClient(caaBase, "t", nil), nil, filepath.Join(dir, "artwork"))
	e.MBMinInterval, e.CAAMinInterval = 0, 0
	return e, store
}

// TestNoLogLineOrSkipDetailCarriesABaseURLsCredential runs the real enricher
// over a mirror it cannot verify and requires that no line it logs, at any
// level, carries the credential written into either base. The failed MB
// search, the artist search, the release-group lookup and the artwork fetch
// each log their error, and markSkipped puts the search's in the
// `enrichment skipped` line's detail: before backlog B69 all of them named
// the request URL with a user-name token whole.
func TestNoLogLineOrSkipDetailCarriesABaseURLsCredential(t *testing.T) {
	const secret = "s3cret-Pw"
	ctx := context.Background()

	t.Run("MusicBrainz base", func(t *testing.T) {
		mbSrv := newUntrustedTLSServer(t)
		// The cover mirror has no cover for the release, so the release
		// group is looked up at MusicBrainz, which fails.
		cover := newAuthRecorder(t, http.StatusNotFound)
		e, store := enricherOverBases(t, withUserinfo(t, mbSrv.URL, secret)+"/ws/2", withUserinfo(t, cover.URL, secret))

		// Track A has to be searched for; track B carries its album MBID.
		a := storeCancelTrack(t, store, manifest.Track{Path: "Alpha/Album/01.flac", Size: 1, ModTime: time.Now(),
			Artist: "Alpha", Album: "Album"})
		b := storeCancelTrack(t, store, manifest.Track{Path: "Beta/Album/01.flac", Size: 1, ModTime: time.Now(),
			Artist: "Beta", Album: "Album", MusicBrainzAlbumID: cancelReleaseMBID})

		rec := loggingtest.Record(t)
		e.enrichOne(ctx, &a)
		e.enrichOne(ctx, &b)

		for _, msg := range []string{"MB search", "enrichment skipped", "release-group lookup", "MB artist search"} {
			if len(rec.Lines(msg)) == 0 {
				t.Errorf("no %q line was logged, so the run did not reach that site: %v", msg, rec.All())
			}
		}
		for _, line := range rec.All() {
			if leakedSecret(line, []string{secret}) != "" {
				t.Errorf("a log line carries the credential: %s", line)
			}
		}
		if got := e.SkipReasons()[skipReasonMBError]; got != 1 {
			t.Errorf("skip reason %s = %d, want 1 (track A)", skipReasonMBError, got)
		}
		if got := cover.last(); got != basicHeader(secret) {
			t.Errorf("the cover mirror saw Authorization %q, want its credential %q", got, basicHeader(secret))
		}
	})

	t.Run("Cover Art base", func(t *testing.T) {
		mb := newAuthRecorder(t, http.StatusOK)
		caaSrv := newUntrustedTLSServer(t)
		e, store := enricherOverBases(t, withUserinfo(t, mb.URL, secret)+"/ws/2", withUserinfo(t, caaSrv.URL, secret))
		b := storeCancelTrack(t, store, manifest.Track{Path: "Beta/Album/01.flac", Size: 1, ModTime: time.Now(),
			Artist: "Beta", Album: "Album", MusicBrainzAlbumID: cancelReleaseMBID})

		rec := loggingtest.Record(t)
		e.enrichOne(ctx, &b)

		if len(rec.Lines("artwork")) == 0 {
			t.Errorf("no %q line was logged, so the run did not reach the cover fetch: %v", "artwork", rec.All())
		}
		for _, line := range rec.All() {
			if leakedSecret(line, []string{secret}) != "" {
				t.Errorf("a log line carries the credential: %s", line)
			}
		}
		if got := mb.last(); got != basicHeader(secret) {
			t.Errorf("the MusicBrainz mirror saw Authorization %q, want its credential %q", got, basicHeader(secret))
		}
	})
}

// TestALiveBasesCredentialFollowsTheBaseItWasWrittenOn pins that a request
// takes its credential from the same resolution as its host. A live base can
// change between two reads, so a credential read apart from the URL could
// send one mirror's to another's host; here the base changes between calls,
// and each mirror sees its own credential and never the other's.
func TestALiveBasesCredentialFollowsTheBaseItWasWrittenOn(t *testing.T) {
	first, second := newAuthRecorder(t, http.StatusOK), newAuthRecorder(t, http.StatusOK)
	var mu sync.Mutex
	live := withUserinfo(t, first.URL, "first-token")
	current := func() string {
		mu.Lock()
		defer mu.Unlock()
		return live
	}
	c := NewMusicBrainzClient("", "t", nil).WithLiveBase(func() string { return current() + "/ws/2" })
	cover := NewCoverArtClient("", "t", nil).WithLiveBase(current)
	ctx := context.Background()

	if _, err := c.SearchRelease(ctx, "Artist", "Album"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	live = withUserinfo(t, second.URL, "second-token")
	mu.Unlock()
	if _, err := c.SearchArtist(ctx, "Artist"); err != nil {
		t.Fatal(err)
	}
	body, err := cover.FetchReleaseFrontStream(ctx, cancelReleaseMBID, 500)
	if err != nil {
		t.Fatal(err)
	}
	_ = body.Close()

	if got := strings.Join(first.headers(), "|"); got != basicHeader("first-token") {
		t.Errorf("first mirror saw %q, want its own credential once", got)
	}
	want := basicHeader("second-token") + "|" + basicHeader("second-token")
	if got := strings.Join(second.headers(), "|"); got != want {
		t.Errorf("second mirror saw %q, want its own credential twice", got)
	}
}

// TestALiveBaseThatChangesBetweenReadsNeverPairsACredentialWithAnotherHost
// pins that a request resolves its base ONCE. The provider answers a
// different mirror, with a different credential, on every call, so a request
// that read it twice (the root, then the credential) would send one mirror's
// credential to the other's host on every request. Each mirror must see only
// its own.
func TestALiveBaseThatChangesBetweenReadsNeverPairsACredentialWithAnotherHost(t *testing.T) {
	first, second := newAuthRecorder(t, http.StatusOK), newAuthRecorder(t, http.StatusOK)
	bases := []string{withUserinfo(t, first.URL, "first-token"), withUserinfo(t, second.URL, "second-token")}
	var mu sync.Mutex
	calls := 0
	alternating := func() string {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return bases[calls%2]
	}
	c := NewMusicBrainzClient("", "t", nil).WithLiveBase(func() string { return alternating() + "/ws/2" })
	cover := NewCoverArtClient("", "t", nil).WithLiveBase(alternating)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if _, err := c.SearchArtist(ctx, "Artist"); err != nil {
			t.Fatal(err)
		}
		body, err := cover.FetchReleaseFrontStream(ctx, cancelReleaseMBID, 500)
		if err != nil {
			t.Fatal(err)
		}
		_ = body.Close()
	}

	for _, m := range []struct {
		name string
		got  []string
		want string
	}{
		{"first", first.headers(), basicHeader("first-token")},
		{"second", second.headers(), basicHeader("second-token")},
	} {
		if len(m.got) != 4 {
			t.Errorf("%s mirror saw %d requests, want 4 (every other read of the provider)", m.name, len(m.got))
		}
		for _, h := range m.got {
			if h != m.want {
				t.Errorf("%s mirror saw Authorization %q, want only its own %q", m.name, h, m.want)
			}
		}
	}
}

// TestABaseURLsCredentialFollowsARedirectOnlyWhereNetHTTPSendsAnAuthorizationHeader
// pins the two redirect cases the header form has to get right. A mirror that
// redirects to another path on its own host (a proxy adding a slash or moving
// a path) keeps authenticating, as it did when the credential rode the URL of
// a relative Location. And one that redirects to another domain, as Cover Art
// Archive does to Internet Archive, is not sent the credential: net/http drops
// an explicit Authorization header there, and never sent a URL's user
// information to another host either.
func TestABaseURLsCredentialFollowsARedirectOnlyWhereNetHTTPSendsAnAuthorizationHeader(t *testing.T) {
	ctx := context.Background()
	want := basicHeader("tok")

	t.Run("a relative redirect on the mirror's own host", func(t *testing.T) {
		var mu sync.Mutex
		var seen []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization"))
			mu.Unlock()
			if strings.HasPrefix(r.URL.Path, "/moved/") {
				_, _ = io.WriteString(w, `{"releases":[],"artists":[]}`)
				return
			}
			http.Redirect(w, r, "/moved"+r.URL.Path, http.StatusFound)
		}))
		defer srv.Close()
		c := NewMusicBrainzClient(withUserinfo(t, srv.URL, "tok"), "t", nil)
		if _, err := c.SearchRelease(ctx, "Artist", "Album"); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(seen) != 2 || !strings.HasSuffix(seen[0], " "+want) || !strings.HasPrefix(seen[1], "/moved/") || !strings.HasSuffix(seen[1], " "+want) {
			t.Errorf("both hops should carry %q, saw %q", want, seen)
		}
	})

	t.Run("a redirect to another domain", func(t *testing.T) {
		other := newAuthRecorder(t, http.StatusOK)
		// localhost and 127.0.0.1 are two names, so two domains to net/http.
		otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
		mirror := newRedirector(t, otherURL)
		c := NewCoverArtClient(withUserinfo(t, mirror.URL, "tok"), "t", nil)
		body, err := c.FetchReleaseFrontStream(ctx, cancelReleaseMBID, 500)
		if err != nil {
			t.Fatal(err)
		}
		_ = body.Close()
		if got := mirror.last(); got != want {
			t.Errorf("the mirror saw Authorization %q, want its credential %q", got, want)
		}
		if got := other.headers(); len(got) != 1 || got[0] != "" {
			t.Errorf("the other domain saw Authorization %q, want no header", got)
		}
	})
}

// newRedirector starts a mirror that records each request's Authorization
// header and redirects it, path kept, to target.
func newRedirector(t testing.TB, target string) *authRecorder {
	t.Helper()
	r := &authRecorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.seen = append(r.seen, req.Header.Get("Authorization"))
		r.mu.Unlock()
		http.Redirect(w, req, target+req.URL.Path, http.StatusFound)
	}))
	t.Cleanup(r.Close)
	return r
}

// TestTheBaseParserCutsUserInformationOutOfEveryUsableBase table-tests
// parseBaseEndpoint: the root a request is built from never carries user
// information, a base with none is kept as written, and what is not a usable
// base is refused by an error that names none of it.
func TestTheBaseParserCutsUserInformationOutOfEveryUsableBase(t *testing.T) {
	usable := []struct {
		raw, root string
		hasUser   bool
		user      string
	}{
		{"https://musicbrainz.org/ws/2", "https://musicbrainz.org/ws/2", false, ""},
		{"  http://127.0.0.1:8080/ws/2 ", "http://127.0.0.1:8080/ws/2", false, ""},
		{"HTTP://Mirror.example/ws/2", "HTTP://Mirror.example/ws/2", false, ""},
		{"https://mirror.example/ws/2/@x", "https://mirror.example/ws/2/@x", false, ""},
		{"https://tok@mirror.example/ws/2", "https://mirror.example/ws/2", true, "tok"},
		{"https://user:pw@mirror.example:8443/ws/2", "https://mirror.example:8443/ws/2", true, "user"},
		{"http://tok@[::1]:8080", "http://[::1]:8080", true, "tok"},
		{"https://tok@mirror.example/ws/2?x=1#frag", "https://mirror.example/ws/2?x=1#frag", true, "tok"},
		{"https://s3cret%40Pw:p%3Ass@mirror.example/a%20b", "https://mirror.example/a%20b", true, "s3cret@Pw"},
		{"https://@mirror.example", "https://mirror.example", true, ""},
		{"https://mirror.example/ws/2/", "https://mirror.example/ws/2", false, ""},
		{" https://mirror.example/ws/2/// ", "https://mirror.example/ws/2", false, ""},
		{"https://tok@mirror.example/ws/2/", "https://mirror.example/ws/2", true, "tok"},
		{"https://mirror.example/", "https://mirror.example", false, ""},
	}
	for _, tc := range usable {
		ep := parseBaseEndpoint(tc.raw)
		if ep.err != nil || ep.root != tc.root {
			t.Errorf("parseBaseEndpoint(%q) = root %q, err %v; want root %q", tc.raw, ep.root, ep.err, tc.root)
			continue
		}
		if u, err := url.Parse(ep.root); err != nil || u.User != nil {
			t.Errorf("parseBaseEndpoint(%q): root %q still carries user information", tc.raw, ep.root)
		}
		if (ep.user != nil) != tc.hasUser || (tc.hasUser && ep.user.Username() != tc.user) {
			t.Errorf("parseBaseEndpoint(%q): user information = %v, want present=%v user %q", tc.raw, ep.user, tc.hasUser, tc.user)
		}
	}

	unusable := []string{
		"", "   ", "mirror.example/ws/2", "s3cret-Pw@mirror.example/ws/2",
		"s3cret-Pw:x@mirror.example/ws/2", "ftp://s3cret-Pw@mirror.example/ws/2",
		"file:///s3cret-Pw", "http://", "https://s3cret-Pw@", "http:s3cret-Pw@mirror.example",
		"https://s3cret-Pw@mirror.example/\x7f", "https://s3cret%zz@mirror.example/ws/2",
		"https://s3cret Pw@mirror.example/ws/2", "://s3cret-Pw@mirror",
	}
	for _, raw := range unusable {
		ep := parseBaseEndpoint(raw)
		if ep.err == nil {
			t.Errorf("parseBaseEndpoint(%q) accepted an unusable base as %q", raw, ep.root)
			continue
		}
		if ep.root != "" || ep.user != nil {
			t.Errorf("parseBaseEndpoint(%q) kept root %q, user %v beside its error", raw, ep.root, ep.user)
		}
		if _, err := ep.newRequest(context.Background(), "/x"); err == nil || leakedSecret(err.Error(), []string{"s3cret", "mirror"}) != "" {
			t.Errorf("a request from the unusable base %q answered %v, want an error naming none of it", raw, err)
		}
	}
}

// TestABaseWithTrailingSlashesRequestsNoDoubleSlashPath pins the root's
// trailing slashes being trimmed for a base handed straight to a constructor,
// with user information or without: newRequest joins a path that begins with
// a slash, so a slash left on the root requested `/ws/2//release/…`, which a
// strict mirror answers 404. (Config trims it, and a live value is trimmed by
// the client; nothing trimmed a constructed base until the parser did.)
func TestABaseWithTrailingSlashesRequestsNoDoubleSlashPath(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"releases":[],"artists":[]}`)
	}))
	defer srv.Close()
	ctx := context.Background()

	requests := 0
	for _, base := range []string{srv.URL, withUserinfo(t, srv.URL, "tok")} {
		for _, slashes := range []string{"", "/", "//"} {
			if _, err := NewMusicBrainzClient(base+"/ws/2"+slashes, "t", nil).SearchRelease(ctx, "Artist", "Album"); err != nil {
				t.Fatal(err)
			}
			body, err := NewCoverArtClient(base+slashes, "t", nil).FetchReleaseFrontStream(ctx, cancelReleaseMBID, 500)
			if err != nil {
				t.Fatal(err)
			}
			_ = body.Close()
			requests += 2
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(paths) != requests {
		t.Fatalf("the mirror saw %d requests, want %d", len(paths), requests)
	}
	for _, p := range paths {
		if strings.Contains(p, "//") {
			t.Errorf("a request path holds a double slash: %q", p)
		}
	}
}

// TestAStoredPremiumBaseWithUserInformationLeavesItOutOfTheRequestURL guards
// the third client built from a configured URL. The Atlas credential's base
// is reduced to scheme://host before it is stored (config.CanonicalHTTPSBase
// refuses user information), so none can arrive that way; a state file edited
// by hand can still hold one, and the bearer token is the only credential the
// request sends, so its URL and its errors name no user information.
func TestAStoredPremiumBaseWithUserInformationLeavesItOutOfTheRequestURL(t *testing.T) {
	const secret = "s3cret-Pw"
	const mbid = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	ctx := context.Background()

	t.Run("the request", func(t *testing.T) {
		mirror := newAuthRecorder(t, http.StatusOK)
		f := NewAtlasPremiumFetcher(fakeCred{token: "tok123", base: withUserinfo(t, mirror.URL, secret), ok: true}, "ua", nil, "")
		if _, err := f.RefetchPremium(ctx, filepath.Join(t.TempDir(), "x.jpg"), mbid, 500); err != nil {
			t.Fatal(err)
		}
		if got := mirror.headers(); len(got) != 1 || got[0] != "Bearer tok123" {
			t.Errorf("Authorization = %q, want the bearer token alone", got)
		}
	})

	t.Run("its errors", func(t *testing.T) {
		u := failingMirrors[1].start(t) // a certificate this process does not trust
		f := NewAtlasPremiumFetcher(fakeCred{token: "tok123", base: withUserinfo(t, u, secret), ok: true}, "ua", nil, "")
		rec := loggingtest.Record(t)
		_, err := f.RefetchPremium(ctx, filepath.Join(t.TempDir(), "x.jpg"), mbid, 500)
		if err == nil || !strings.Contains(err.Error(), strings.TrimPrefix(u, "https://")) {
			t.Fatalf("RefetchPremium answered %v, want the request's error", err)
		}
		if leakedSecret(err.Error(), []string{secret}) != "" {
			t.Errorf("the error carries the user information: %v", err)
		}
		if f.TryCache(ctx, filepath.Join(t.TempDir(), "y.jpg"), mbid, 500) {
			t.Fatal("TryCache cached from a mirror it cannot verify")
		}
		if len(rec.Lines("atlas premium cover fetch")) == 0 {
			t.Errorf("TryCache logged nothing, so the run did not reach its error line: %v", rec.All())
		}
		for _, line := range rec.All() {
			if leakedSecret(line, []string{secret}) != "" {
				t.Errorf("a log line carries the user information: %s", line)
			}
		}
	})
}
