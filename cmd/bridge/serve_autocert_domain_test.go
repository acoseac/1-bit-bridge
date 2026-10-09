package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestServePublishesNoAutocertDomainCredential boots the real `serve` in
// public mode over a bridge.yaml whose autocert.domain an operator edited by
// hand to carry a user name and password (backlog B66), and asks every
// surface the secret must never reach: /v1/health with no token, the pairing
// link the console mints (its url=, its urls= and the JSON beside it), the
// startup banner ("Public mode — domain:" and "Admin console:"), and every
// line serve logs. Measured on the binary before B66, health and both QR
// fields carried `https://user:<secret>@bridge.example.test:<port>` and the
// banner printed the secret twice. The config loaded before and must still
// load, and everything is built from the host the domain names. Only this
// test sees the chain whole: the file, Load's Normalize, the live config,
// the three places public mode builds a URL from the domain, and the wire.
func TestServePublishesNoAutocertDomainCredential(t *testing.T) {
	const secret = "s3cret-Pw"
	carries := func(s string) bool {
		return strings.Contains(strings.ToLower(s), strings.ToLower(secret))
	}
	cfgDir := t.TempDir()
	lib := filepath.Join(cfgDir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	ports := pickPublicInitPorts(t)
	code, out := publicInit(t, cfgDir, ports, "Domain Probe", "--library", lib)
	if code != 0 {
		t.Fatalf("bridge init --public = %d:\n%s", code, out)
	}
	password := mintedPassword(t, out)
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "domain: localhost", "domain: user:"+secret+"@bridge.example.test", 1)
	if edited == string(raw) {
		t.Fatalf("the init wrote no `domain: localhost` line to edit:\n%s", raw)
	}
	// UDP is not what this is about, and the port picked free for TCP is not
	// known to be free for UDP.
	if err := os.WriteFile(cfgPath, []byte(edited+"disableHttp3: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Recorded from before the load, so the load's own warning is in it, and
	// registered before the bridge's drain, so it is put back after serve
	// has returned.
	rec := loggingtest.Record(t)
	// Launched here rather than through bootServe, which waits for the TLS
	// fingerprint a public bridge does not print (its phones do not pin the
	// certificate it serves), as the other public-mode serve tests do.
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- run(ctx, []string{"serve", "--config", cfgPath}, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	console := newLiveConsole(ports.admin)
	apiAddr := "127.0.0.1:" + strconv.Itoa(ports.api)
	waitForAdminReady(t, console.addr, done, stderr)
	waitForPortAccepting(t, apiAddr, done, stderr)
	want := "https://bridge.example.test:" + strconv.Itoa(ports.api)

	// What any caller sees, a token or none.
	phone := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	resp, err := phone.Get("https://" + apiAddr + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v; stderr=%s", err, stderr.String())
	}
	health, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/health = %d: %s", resp.StatusCode, health)
	}
	if carries(string(health)) {
		t.Errorf("/v1/health, asked with no token, carries the secret:\n%s", health)
	}
	if !strings.Contains(string(health), `"`+want+`"`) {
		t.Errorf("/v1/health does not advertise %s, the endpoint built from the host the domain names:\n%s", want, health)
	}

	// What the pairing link carries: with no url in the request, its primary
	// is the one public mode builds from the domain.
	session := console.login(t, password, http.StatusOK, stderr)
	mintResp := console.do(t, http.MethodPost, "/api/tokens", `{"name":"boot test"}`, session, stderr)
	body, err := io.ReadAll(mintResp.Body)
	mintResp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if mintResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/tokens = %d: %s", mintResp.StatusCode, body)
	}
	var mint pairResponse
	if err := json.Unmarshal(body, &mint); err != nil {
		t.Fatalf("decode the minted link: %v: %s", err, body)
	}
	for _, s := range append([]string{mint.URL, mint.PairURL}, mint.Alternates...) {
		if carries(s) {
			t.Errorf("the pairing link carries the secret: %s", s)
		}
	}
	if mint.URL != want {
		t.Errorf("the pairing link's primary url= is %q, want %q", mint.URL, want)
	}

	// What serve printed and logged.
	if !strings.Contains(stdout.String(), "Public mode — domain: bridge.example.test\n") {
		t.Errorf("the banner does not name the host the domain names:\n%s", stdout.String())
	}
	for name, s := range map[string]string{"stdout": stdout.String(), "stderr": stderr.String()} {
		if carries(s) {
			t.Errorf("serve's %s carries the secret:\n%s", name, s)
		}
	}
	for _, l := range rec.All() {
		if carries(l) {
			t.Errorf("a log line carries the secret: %s", l)
		}
	}
	warned := 0
	for _, l := range rec.Failures() {
		if strings.Contains(l, "autocert.domain (https://bridge.example.test)") {
			warned++
		}
	}
	if warned != 1 {
		t.Errorf("%d warnings name autocert.domain by its scheme and host, want one (the load's); lines: %q",
			warned, rec.Failures())
	}
}
