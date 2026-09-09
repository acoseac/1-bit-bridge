package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHeadDoesNotConsumeTheLoginTicket.
//
// mux.HandleFunc("GET /login/ticket", …) also matches HEAD — Go's ServeMux
// documents it in as many words ("a pattern with the method GET matches both
// GET and HEAD requests") — and redemption DELETES the record before judging
// it. So anything that probes the link before the human clicks spends the
// credential: a mail-security scanner, a chat unfurler, a corporate proxy, a
// prefetcher. The human's real GET then gets a bare redirect to /login and,
// by deliberate design, no explanation of why.
//
// The assertion that matters is the SECOND one: the same ticket must still
// work afterwards. A 405 alone would pass against a handler that refused the
// HEAD after already redeeming.
func TestHeadDoesNotConsumeTheLoginTicket(t *testing.T) {
	srv, store, _ := newPublicTestServer(t, "correct horse battery staple")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ticket, err := store.MintLoginTicket("admin")
	if err != nil {
		t.Fatal(err)
	}
	url := ts.URL + "/login/ticket?t=" + ticket

	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	head, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if head.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("HEAD status = %d, want 405", head.StatusCode)
	}
	if sessionCookie(head) != nil {
		t.Error("HEAD handed out a session cookie")
	}

	get, err := noRedirectClient().Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer get.Body.Close()
	if get.StatusCode != http.StatusFound {
		t.Fatalf("the human's GET after a HEAD: status = %d, want 302 — the HEAD burned the ticket", get.StatusCode)
	}
	if sessionCookie(get) == nil {
		t.Error("the human's GET after a HEAD got no session cookie — the HEAD burned the ticket")
	}
}
