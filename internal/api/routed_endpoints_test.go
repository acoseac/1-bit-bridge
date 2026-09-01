package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryDocumentedEndpointIsRouted is the guard whose absence let
// GET /v1/search ship UNREACHABLE.
//
// The endpoint had a handler, a store method, a rate limiter, config, a
// health flag and a PROTOCOL.md section — and no entry in routeRegistry,
// because an edit to that file silently failed. Every one of its tests
// called the handler FUNCTION directly, so the mux was never exercised
// and nothing noticed. It answered 404 in production while its unit
// tests were green.
//
// This closes the class rather than the instance: PROTOCOL.md is the
// wire contract, so anything it documents as an endpoint must appear in
// the registry Handler() builds from. A documented-but-unrouted endpoint
// is a lie to every client author who reads the spec.
//
// It compares against the REGISTRY rather than probing responses,
// deliberately: several routed endpoints answer 404 BY DESIGN
// (`pairing_not_supported` when the pairing store is unwired), so a
// response probe cannot tell "no route" from "routed and refusing".
func TestEveryDocumentedEndpointIsRouted(t *testing.T) {
	documented := documentedEndpoints(t)
	if len(documented) < 15 {
		t.Fatalf("only parsed %d documented endpoints from PROTOCOL.md; the scan is broken "+
			"and this test would pass vacuously", len(documented))
	}

	registered := map[string]struct{}{}
	for _, rt := range newRouteRegistryTestServer(t).routeRegistry() {
		registered[normalizeEndpoint(rt.pattern)] = struct{}{}
	}

	var missing []string
	for _, ep := range documented {
		if _, ok := registered[ep]; !ok {
			missing = append(missing, ep)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("PROTOCOL.md documents %d endpoint(s) absent from routeRegistry:\n  %s\n\n"+
			"A documented-but-unrouted endpoint answers 404 in production while its handler's "+
			"unit tests stay green — that is exactly how GET /v1/search shipped unreachable.",
			len(missing), strings.Join(missing, "\n  "))
	}
}

// documentedEndpoints extracts "METHOD /path" pairs from PROTOCOL.md's
// endpoint section headings, normalised for comparison.
func documentedEndpoints(t *testing.T) []string {
	t.Helper()
	spec, err := os.ReadFile("../../PROTOCOL.md")
	if err != nil {
		t.Fatalf("read PROTOCOL.md: %v", err)
	}
	// Headings look like:  ### `GET /v1/foo?bar=<x>` (additive, since …)
	re := regexp.MustCompile("(?m)^#+\\s+`(GET|POST|PUT|DELETE|PATCH) (/v1/[^`?\\s]+)")
	seen := map[string]struct{}{}
	var out []string
	for _, m := range re.FindAllStringSubmatch(string(spec), -1) {
		// The docs use an alternation shorthand for sibling routes:
		//   /v1/atlas-meta/{release|artist}/{mbid}
		// Expand it so each real route is checked.
		for _, path := range expandAlternation(m[2]) {
			ep := normalizeEndpoint(m[1] + " " + path)
			if _, dup := seen[ep]; dup {
				continue
			}
			seen[ep] = struct{}{}
			out = append(out, ep)
		}
	}
	sort.Strings(out)
	return out
}

// expandAlternation turns `/a/{x|y}/b` into `/a/x/b` and `/a/y/b`. Only
// the documentation uses this shorthand; a real pattern never does.
func expandAlternation(path string) []string {
	open := strings.Index(path, "{")
	if open < 0 {
		return []string{path}
	}
	close := strings.Index(path[open:], "}")
	if close < 0 {
		return []string{path}
	}
	close += open
	inner := path[open+1 : close]
	if !strings.Contains(inner, "|") {
		// An ordinary wildcard — keep scanning the rest of the path.
		rest := expandAlternation(path[close+1:])
		out := make([]string, 0, len(rest))
		for _, r := range rest {
			out = append(out, path[:close+1]+r)
		}
		return out
	}
	var out []string
	for _, alt := range strings.Split(inner, "|") {
		for _, r := range expandAlternation(path[close+1:]) {
			out = append(out, path[:open]+alt+r)
		}
	}
	return out
}

// normalizeEndpoint collapses wildcard NAMES so `{requestId}` in the
// prose and `{requestID}` in the pattern compare equal. The name is
// documentation; the position is the contract.
func normalizeEndpoint(s string) string {
	return regexp.MustCompile(`\{[^}]*\}`).ReplaceAllString(strings.TrimSpace(s), "{}")
}

// TestSearchIsReachableThroughTheMux is the instance-level pin. Every
// other search test calls s.search directly; this one goes through
// Handler(), which is the layer that was broken.
func TestSearchIsReachableThroughTheMux(t *testing.T) {
	s := newRouteRegistryTestServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/search?q=abc", nil))

	if rr.Code == http.StatusNotFound {
		t.Fatal("GET /v1/search is not routed — the handler exists but nothing dispatches to it")
	}
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated /v1/search = %d, want 401 (it is an authed route)", rr.Code)
	}
}
