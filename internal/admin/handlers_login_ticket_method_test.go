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
// GET and HEAD requests"). Redemption used to happen on the GET and delete the
// record before judging it, so a HEAD from a mail-security scanner, a chat
// unfurler or a corporate proxy spent the credential. The GET spends nothing
// now, but the HEAD stays refused — it is the honest answer to a prober asking
// about the method — and the assertion that matters is still the SECOND one:
// the human's flow works afterwards.
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

	page := openLink(t, ts.URL, "?t="+ticket)
	assertInterstitial(t, page, ticket)
	page.Body.Close()
	get := redeemTicket(t, ts.URL, "?t="+ticket)
	defer get.Body.Close()
	if get.StatusCode != http.StatusFound {
		t.Fatalf("the human's click after a HEAD: status = %d, want 302 — the HEAD burned the ticket", get.StatusCode)
	}
	if sessionCookie(get) == nil {
		t.Error("the human's click after a HEAD got no session cookie — the HEAD burned the ticket")
	}
}
