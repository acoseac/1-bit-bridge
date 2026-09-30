package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestAStartupRefusalNamesAURLWithoutItsCredential runs `bridge serve` and
// `bridge doctor --config` over a bridge.yaml whose base URL the load
// refuses, the value carrying a secret: a password, a token written as the
// user name, a query (backlog B54). The refusal stops the bridge from
// starting, which is where an operator reads it (the terminal, the journal,
// doctor's config-file line), and until B54 both refusals quoted the value
// whole. Each must still refuse, still name the field, and carry no secret.
func TestAStartupRefusalNamesAURLWithoutItsCredential(t *testing.T) {
	const secret = "s3cret-Pw"
	for _, tc := range []struct{ name, field, yaml string }{
		{"an enrich base with a password", "enrich.musicbrainzBaseURL",
			"enrich:\n  musicbrainzBaseURL: 'ftp://user:" + secret + "@mirror.example/ws/2'\n"},
		{"an enrich base with a token as the user name, no scheme", "enrich.coverArtBaseURL",
			"enrich:\n  coverArtBaseURL: '" + secret + ":x@mirror.example'\n"},
		{"a harvest pin with a token as the user name", "atlas.harvestBaseUrl",
			"atlas:\n  harvestBaseUrl: 'https://" + secret + "@atlas.example'\n"},
		{"a harvest pin with a query", "atlas.harvestBaseUrl",
			"atlas:\n  harvestBaseUrl: 'https://atlas.example/?token=" + secret + "'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateConfigEnv(t)
			cfgPath := writeValidConfig(t)
			f, err := os.OpenFile(cfgPath, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(tc.yaml); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			check := func(what, out string) {
				t.Helper()
				if strings.Contains(strings.ToLower(out), strings.ToLower(secret)) {
					t.Errorf("%s carries the secret:\n%s", what, out)
				}
				if !strings.Contains(out, tc.field) {
					t.Errorf("%s does not name %s:\n%s", what, tc.field, out)
				}
			}

			stdout, stderr, code := runCapture(t, "serve", "--config", cfgPath, "--addr", "127.0.0.1:0")
			if code != 2 {
				t.Errorf("serve exited %d over a refused %s, want 2; stderr:\n%s", code, tc.field, stderr)
			}
			check("serve's output", stdout+stderr)

			var so, se bytes.Buffer
			if code := doctorCmd([]string{"--json", "--config", cfgPath}, &so, &se); code != 1 {
				t.Errorf("doctor exited %d over a config that does not load, want 1; stderr:\n%s", code, se.String())
			}
			var rep jsonDoctorReport
			if err := json.Unmarshal(so.Bytes(), &rep); err != nil {
				t.Fatalf("decode the JSON report: %v\n%s", err, so.String())
			}
			for _, c := range rep.Checks {
				if c.Name == "config-file" {
					check("doctor's config-file line", c.Summary)
				}
			}
			if strings.Contains(strings.ToLower(so.String()+se.String()), strings.ToLower(secret)) {
				t.Errorf("doctor's report carries the secret:\n%s%s", so.String(), se.String())
			}
			// The file itself still holds the value: nothing rewrote it.
			if raw, err := os.ReadFile(filepath.Clean(cfgPath)); err != nil || !strings.Contains(string(raw), secret) {
				t.Errorf("the config file lost the value it was refused for (err %v)", err)
			}
		})
	}
}

// TestServePublishesNoCustomEndpointCredential boots the real `serve` on a
// bridge.yaml whose customEndpoints carry a secret in each part of a URL
// that can carry one (backlog B54; the path since B66), and asks every surface the secret must
// never reach: /v1/health with no token, which published every entry as
// written until B54; the pairing link the console mints (POST /api/tokens:
// its url=, its urls= and the JSON beside it); and every line serve logs
// or prints. The config LOADED before B54 and must still load: each
// endpoint is still advertised, to the host and port it names, without the
// part, and the load warns once per entry. Only this test sees the whole
// chain: the file, Load's Normalize, the live config, the one enumeration
// both health and the QR read, and the wire.
func TestServePublishesNoCustomEndpointCredential(t *testing.T) {
	const secret = "s3cret-Pw"
	declared := []string{
		"https://user:" + secret + "@a.example.test:7788",
		"https://" + secret + "@b.example.test:7788",
		"https://c.example.test:7788/?token=" + secret,
		"https://d.example.test:7788/#" + secret,
		"https://e.example.test:7788/" + secret + "/",
	}
	published := []string{
		"https://a.example.test:7788",
		"https://b.example.test:7788",
		"https://c.example.test:7788/",
		"https://d.example.test:7788/",
		"https://e.example.test:7788",
	}
	carries := func(s string) bool {
		return strings.Contains(strings.ToLower(s), strings.ToLower(secret))
	}
	// Recorded from before the load, so the load's own warnings are in it.
	// Registered before the bridge's drain, so it is put back after serve
	// has returned.
	rec := loggingtest.Record(t)
	var tail strings.Builder
	tail.WriteString("customEndpoints:\n")
	for _, d := range declared {
		tail.WriteString("  - '" + d + "'\n")
	}
	b := startConsoleBridge(t, tail.String(), nil)

	// What any caller sees, a token or none.
	resp, err := b.phone.Get(b.apiBase + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v; stderr=%s", err, b.stderr.String())
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
	for _, p := range published {
		if !strings.Contains(string(health), `"`+p+`"`) {
			t.Errorf("/v1/health does not advertise %s, the endpoint without its credential:\n%s", p, health)
		}
	}

	// What the pairing link carries.
	const primary = "https://primary.example.test:7788"
	mint := pairViaAdmin(t, t.Context(), b.console, b.adminBase+"/api/tokens",
		`{"name":"boot test","url":"`+primary+`"}`, http.StatusCreated, b.stderr)
	for _, s := range append([]string{mint.URL, mint.PairURL}, mint.Alternates...) {
		if carries(s) {
			t.Errorf("the pairing link carries the secret: %s", s)
		}
	}
	for _, p := range published {
		if !containsString(mint.Alternates, p) {
			t.Errorf("the pairing link does not carry %s among its urls=: %q", p, mint.Alternates)
		}
	}

	// What serve logged and printed.
	for _, l := range rec.All() {
		if carries(l) {
			t.Errorf("a log line carries the secret: %s", l)
		}
	}
	for name, out := range map[string]string{"stdout": b.stdout.String(), "stderr": b.stderr.String()} {
		if carries(out) {
			t.Errorf("serve's %s carries the secret:\n%s", name, out)
		}
	}
	if got := len(rec.Failures("custom endpoint published without its user name, password, path, query or fragment; " +
		"a save stores it that way")); got != len(declared) {
		t.Errorf("%d warnings that an endpoint was published without its credential, want one per entry (%d); lines: %q",
			got, len(declared), rec.Failures())
	}
}
