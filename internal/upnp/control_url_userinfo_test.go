package upnp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// TestAControlURLsUserInformationTravelsAsBasicAuth: a manual upstream's
// description URL can carry the operator's credential
// (`http://user:password@nas:8200/rootDesc.xml`), and a ContentDirectory
// control URL the description names relative to it inherits that user
// information through url.ResolveReference (backlog B66). The SOAP client
// built its request from that URL, so net/http sent the credential as Basic
// auth, which is the operator's intent, and the client's error named the
// URL whole: `upnp: POST http://user:<password>@nas:8200/ctl: status 401`,
// which the ingest logs at Warn on every failed walk ("UPnP upstream:
// per-server error", measured with the real binary) and the console shows
// as the server's last walk error. net/http's own *url.Error masks a
// password and nothing else, so a token written as the user name reached it
// whole as well. The user information now travels as the Authorization
// header alone, the same header net/http built from the URL (taken from
// net/http on every run, never restated), and no error names it, for a
// status the client refuses and for a connection that fails.
func TestAControlURLsUserInformationTravelsAsBasicAuth(t *testing.T) {
	const secret = "s3cret-Pw"
	var (
		mu   sync.Mutex
		auth []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	host := strings.TrimPrefix(srv.URL, "http://")
	lastAuth := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(auth) == 0 {
			return ""
		}
		return auth[len(auth)-1]
	}
	carries := func(s string) bool { return strings.Contains(strings.ToLower(s), strings.ToLower(secret)) }

	// The production dispatcher, under the approval the manual poller
	// records for a URL the operator chose.
	client := NewContentDirectoryClient(&discovery.HTTPClientDispatcher{Client: discovery.NewDeviceFetchClient(5 * time.Second)})
	for _, userinfo := range []string{"user:" + secret + "@", secret + "@", "user:@"} {
		ctrl := "http://" + userinfo + host + "/ctl"
		t.Run(userinfo, func(t *testing.T) {
			// net/http's own header for a request to this URL: the reference.
			resp, err := http.Post(ctrl, "text/xml", strings.NewReader("<x/>"))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			want := lastAuth()
			if !strings.HasPrefix(want, "Basic ") {
				t.Fatalf("net/http sent no Basic header for %s; the reference is broken: %q", ctrl, want)
			}

			ctx := discovery.WithDialApproval(context.Background(), discovery.OperatorChose(ctrl))
			_, err = client.GetSystemUpdateID(ctx, ctrl)
			if err == nil {
				t.Fatal("GetSystemUpdateID answered no error for a 401")
			}
			if got := lastAuth(); got != want {
				t.Errorf("the control URL's user information reached the server as %q, want net/http's own %q", got, want)
			}
			if carries(err.Error()) || strings.Contains(err.Error(), "@") {
				t.Errorf("the error names the control URL's user information: %v", err)
			}
			if !strings.Contains(err.Error(), host) {
				t.Errorf("the error does not name the server it failed on (%s): %v", host, err)
			}
		})
	}

	// A connection that fails: net/http's own *url.Error names the request
	// URL, so the user information must not be in it either.
	srv.Close()
	for _, userinfo := range []string{"user:" + secret + "@", secret + "@"} {
		ctrl := "http://" + userinfo + host + "/ctl"
		ctx := discovery.WithDialApproval(context.Background(), discovery.OperatorChose(ctrl))
		_, err := client.GetSystemUpdateID(ctx, ctrl)
		if err == nil {
			t.Fatalf("GetSystemUpdateID answered no error with the server gone (%s)", userinfo)
		}
		if carries(err.Error()) || strings.Contains(err.Error(), "@") {
			t.Errorf("a failed connection's error names the control URL's user information: %v", err)
		}
	}
}
