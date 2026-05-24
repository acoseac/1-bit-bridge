package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

func newCookieJar() (*cookiejar.Jar, error) {
	return cookiejar.New(nil)
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func readResponseSnippet(resp *http.Response) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return buf, err
}

func loginAndExpectOK(t *testing.T, client *http.Client, base, username, password string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req, _ := http.NewRequest("POST", base+"/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://bridge.example.com")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := readResponseSnippet(resp)
		t.Fatalf("login: status %d, want 200; body: %s", resp.StatusCode, buf)
	}
}
