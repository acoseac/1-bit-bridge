package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPageDevicesRendersRealExpiry drives the PAGE render, not the API
// handler — apiTokensList already populated ExpiresAt correctly, and that is
// precisely why the bug survived.
//
// `#tokens-body` is server-rendered once and never repainted from
// /api/tokens; the only client-side write to `.expires-cell` is the
// in-session echo of the PATCH response. So with pageDevices omitting
// ExpiresAt, setting an expiry updated the cell, and reloading /devices
// reverted it to "never" — while auth.Store went on enforcing the expiry and
// returning ErrExpired. The console asserted the opposite of what the token
// store was doing.
func TestPageDevicesRendersRealExpiry(t *testing.T) {
	srv, _, _ := newTestServer(t)

	_, tok, err := srv.deps.Auth.Mint("iPhone")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	exp := time.Now().Add(72 * time.Hour)
	if _, err := srv.deps.Auth.SetExpiry(tok.ID, &exp); err != nil {
		t.Fatalf("SetExpiry: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}

	body := w.Body.String()
	row, ok := expiresCellFor(body, tok.ID)
	if !ok {
		t.Fatalf("no expires-cell found for token %s in rendered page", tok.ID)
	}
	if strings.Contains(row, "never") {
		t.Errorf("expiry cell renders %q — the page reports a token that DOES "+
			"expire as never-expiring, and nothing repaints it from /api/tokens",
			strings.TrimSpace(row))
	}
}

// TestPageDevicesStillRendersNeverWhenNoExpiry is the other half: the
// never-expiring case must keep saying so.
func TestPageDevicesStillRendersNeverWhenNoExpiry(t *testing.T) {
	srv, _, _ := newTestServer(t)
	_, tok, err := srv.deps.Auth.Mint("iPad")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/devices", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	row, ok := expiresCellFor(w.Body.String(), tok.ID)
	if !ok {
		t.Fatalf("no expires-cell found for token %s", tok.ID)
	}
	if !strings.Contains(row, "never") {
		t.Errorf("expiry cell renders %q; want never for a token with no expiry",
			strings.TrimSpace(row))
	}
}

// expiresCellFor pulls the `.expires-cell` contents out of the rendered
// token row for the given id.
func expiresCellFor(body, id string) (string, bool) {
	rowStart := strings.Index(body, `data-id="`+id+`"`)
	if rowStart < 0 {
		return "", false
	}
	rest := body[rowStart:]
	end := strings.Index(rest, "</tr>")
	if end < 0 {
		return "", false
	}
	row := rest[:end]
	cellStart := strings.Index(row, `class="expires-cell"`)
	if cellStart < 0 {
		return "", false
	}
	cell := row[cellStart:]
	cellEnd := strings.Index(cell, "</td>")
	if cellEnd < 0 {
		return "", false
	}
	return cell[:cellEnd], true
}
