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
	// The spec introduces an endpoint's contract two ways, and both count:
	//
	//   ### `GET /v1/foo?bar=<x>` (additive, since …)      — its own section
	//   **`GET /v1/foo`** — …                              — a lead-in inside
	//                                                        a grouped family
	//
	// A heading may also carry TWO endpoints ("`GET /a` and `GET /b`").
	// A bare mention in running prose deliberately does NOT count: that is
	// exactly the state the upscale-batch endpoints were in — named in a
	// rate-limit list and a demo-mode paragraph, with no contract anywhere.
	res := []*regexp.Regexp{
		regexp.MustCompile("(?m)^#+\\s+.*?`(GET|POST|PUT|DELETE|PATCH) (/v1/[^`?\\s]+)"),
		regexp.MustCompile("(?m)^#+\\s+.*and `(GET|POST|PUT|DELETE|PATCH) (/v1/[^`?\\s]+)"),
		regexp.MustCompile("(?m)^\\*\\*`(GET|POST|PUT|DELETE|PATCH) (/v1/[^`?\\s]+)"),
	}
	seen := map[string]struct{}{}
	var out []string
	var all [][]string
	for _, re := range res {
		all = append(all, re.FindAllStringSubmatch(string(spec), -1)...)
	}
	for _, m := range all {
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

// TestEveryRoutedEndpointIsDocumented is the INVERSE of the guard above,
// and closes the other half of the same class.
//
// The forward guard exists because `/v1/search` was documented and not
// routed. This one exists because six endpoints were the reverse —
// routed, shipped, consumed by iOS, and with no contract in the spec:
// the four `/v1/upscale/batch*` routes appeared only in a rate-limit
// list and a demo-mode paragraph, and `/v1/renderers` and
// `/v1/diagnostics` appeared nowhere at all despite both having a
// `BridgeFeatures` constant on the iOS side.
//
// An undocumented endpoint is not a harmless omission. PROTOCOL.md is
// the file the two repos keep byte-identical precisely so a client
// author can write against it; anything reachable but unwritten is a
// contract only the server knows, and it changes without anyone
// noticing the change was a wire change.
func TestEveryRoutedEndpointIsDocumented(t *testing.T) {
	documented := map[string]struct{}{}
	for _, ep := range documentedEndpoints(t) {
		documented[ep] = struct{}{}
	}

	// Endpoints whose contract is documented INSIDE another section's
	// prose rather than under their own heading or lead-in. Each needs a
	// reason, because "add it to the list" must be a worse option than
	// writing the section.
	exempt := map[string]string{
		"GET /v1/pairing/{}/events": "documented in the SSE section as the pollSecret-gated " +
			"sibling of GET /v1/pairing/{requestId}, whose shape it shares",
		"GET /v1/smart-playlist-image/{}": "documented with its full contract (bearer-authed, " +
			"image/jpeg, 404 when none) in the smart-playlists imageHash paragraph",
		"GET /v1/playlist-image/{}": "documented alongside its smart-mix twin in the same paragraph",
	}

	var missing []string
	for _, rt := range newRouteRegistryTestServer(t).routeRegistry() {
		ep := normalizeEndpoint(rt.pattern)
		if _, ok := documented[ep]; ok {
			continue
		}
		if _, ok := exempt[ep]; ok {
			continue
		}
		missing = append(missing, rt.pattern)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d routed endpoint(s) have no contract in PROTOCOL.md:\n  %s\n\n"+
			"Give each one a `### ` section or a `**`METHOD /path`**` lead-in. A mention in "+
			"running prose does not count — that is the state these were already in.\n"+
			"If it genuinely belongs inside another section, add it to `exempt` WITH a reason.",
			len(missing), strings.Join(missing, "\n  "))
	}

	// An exemption for a route that no longer exists is dead weight that
	// makes the list look more considered than it is.
	registered := map[string]struct{}{}
	for _, rt := range newRouteRegistryTestServer(t).routeRegistry() {
		registered[normalizeEndpoint(rt.pattern)] = struct{}{}
	}
	for ep := range exempt {
		if _, ok := registered[ep]; !ok {
			t.Errorf("exemption for %q no longer matches any route — remove it", ep)
		}
	}
}
