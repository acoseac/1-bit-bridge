package admin

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// hrefRe pulls every href out of a template.
//
// Deliberately permissive about what is INSIDE the quotes — the
// classification happens in checkableHrefTarget, where each rejected shape
// is named — because the bug this test exists for was found with a grep
// whose character class silently excluded `{`, `?` and `#`, and so could
// not have seen a templated or query-carrying link at all.
//
// Equally permissive about the quoting: both styles and whitespace around
// `=` are accepted. Every href in the templates is double-quoted with no
// spaces TODAY, so this guards a future blind spot rather than a present
// bug — and that is the point, because a link checker that silently stops
// seeing links keeps passing, which is worse than not having one.
//
// Go's RE2 has no backreferences, so the two quote styles are separate
// alternatives (which also rules out matching a mismatched pair) and the
// caller takes whichever group matched.
var hrefRe = regexp.MustCompile(`(?i)href\s*=\s*(?:"([^"]*)"|'([^']*)')`)

// goTemplateActionRe matches a {{...}} action.
//
// Non-greedy to the closing BRACE PAIR rather than `[^}]*`, because a
// template action may legitimately contain a single `}` — {{if eq .V "}"}}
// — which the naive form truncates, leaving `"}}` behind in the href.
// (?s) so an action broken across lines still matches.
var goTemplateActionRe = regexp.MustCompile(`(?s){{.*?}}`)

// hrefTemplateDummy stands in for a Go template action inside an href.
// A path like /playlists/{{.ID}} has to be resolved against the mux as a
// concrete path, and net/http wildcard patterns ({id}) match any single
// non-empty segment — so any non-empty, slash-free filler works.
const hrefTemplateDummy = "tmpl"

// TestTemplateHrefsResolveToRegisteredRoutes pins that every internal link
// in the admin console actually goes somewhere.
//
// jobs.html linked to /inspector for its entire life while the only route
// was /library/inspector, so the console's own "Start batches from the
// Library Inspector" link 404'd — invisible because a broken href is not a
// compile error, not a vet finding, and not covered by any handler test.
//
// The assertion is deliberately "not 404", not "renders": this is about
// the link resolving to a registered route. A page that 500s because a
// test Server has no UPnP dep wired, or 302s to /login, has still proved
// the route exists.
func TestTemplateHrefsResolveToRegisteredRoutes(t *testing.T) {
	s, _, _ := newTestServer(t)
	h := s.Handler()

	for _, ref := range collectTemplateHrefs(t) {
		t.Run(ref.target, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, ref.target, nil)
			req.RemoteAddr = "127.0.0.1:12345"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Errorf("%s links to %q, which no route serves (404)", ref.source, ref.raw)
			}
		})
	}
}

type hrefRef struct {
	source string // template file the link came from
	raw    string // the href exactly as authored
	target string // the concrete path to request
}

// collectTemplateHrefs returns every href in the embedded templates that
// names an internal route, normalised into something requestable.
//
// The four shapes that are NOT checkable, each of which appears in the
// templates today and each of which a naive checker would report as a
// broken link:
//
//   - "#" and "#anchor"     — in-page anchors, no route involved
//   - "https://..."         — external, not ours to resolve
//   - "{{.Some.URL}}"       — the whole href is runtime data; after dummy
//     substitution it isn't slash-rooted, so it drops
//     out here without a special case
//   - "" (empty)            — nothing to resolve
//
// Query strings ARE stripped rather than dropped: /settings?tab=backups is
// a real link to a real route and must stay covered.
func collectTemplateHrefs(t *testing.T) []hrefRef {
	t.Helper()
	seen := map[string]hrefRef{}
	err := fs.WalkDir(templateFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}
		b, readErr := templateFS.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, m := range hrefRe.FindAllStringSubmatch(string(b), -1) {
			// Group 1 is the double-quoted alternative, group 2 the
			// single-quoted one; exactly one is populated per match.
			raw := m[1]
			if raw == "" {
				raw = m[2]
			}
			target, ok := checkableHrefTarget(raw)
			if !ok {
				continue
			}
			// First template to use a target wins; the report names one
			// source, which is enough to find it.
			if _, dup := seen[target]; !dup {
				seen[target] = hrefRef{source: path, raw: raw, target: target}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk templates: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("no internal hrefs found in the templates — the extractor is broken, " +
			"and a test that checks nothing passes for the wrong reason")
	}
	out := make([]hrefRef, 0, len(seen))
	for _, ref := range seen {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].target < out[j].target })
	return out
}

// TestCheckableHrefTarget pins the classification, which is where all the
// judgement in this check lives. Every "skip" row below is a shape that
// really appears in the templates today, so a checker that got any of them
// wrong would either report a false broken link or quietly stop covering a
// real one.
func TestCheckableHrefTarget(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string // "" means: not checkable
	}{
		{"plain internal path", "/jobs", "/jobs"},
		{"root", "/", "/"},
		{"query string is stripped, not skipped", "/settings?tab=backups", "/settings"},
		{"fragment is stripped", "/library#roots", "/library"},
		{"bare anchor", "#", ""},
		{"external https", "https://github.com/acoseac/1-bit-atlas", ""},
		{"protocol-relative", "//evil.example/x", ""},
		{"whole href is a template action", "{{.Data.Update.ReleaseNotesURL}}", ""},
		{"empty", "", ""},
		{"relative path", "app.css", ""},
		// A partially-templated path must survive as a CONCRETE path so a
		// wildcard route can match it — the interesting case, and the one
		// that separates "drop anything containing {{" from doing this
		// properly.
		{"templated segment survives", "/playlists/{{.ID}}/cover", "/playlists/" + hrefTemplateDummy + "/cover"},
		{"templated segment plus query", "/library/inspector?path={{.Path}}", "/library/inspector"},
		// A template action may legitimately contain a single `}`. A
		// `{{[^}]*}}` pattern stops at that brace and leaves `"}}` behind,
		// producing a target that no route serves — a false broken-link
		// report, which is how a guard test loses its credibility.
		{"action containing a brace", `/x/{{if eq .V "}"}}/y`, "/x/" + hrefTemplateDummy + "/y"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := checkableHrefTarget(tc.raw)
			if tc.want == "" {
				if ok {
					t.Errorf("checkableHrefTarget(%q) = %q, true; want not-checkable", tc.raw, got)
				}
				return
			}
			if !ok {
				t.Fatalf("checkableHrefTarget(%q) = not-checkable; want %q", tc.raw, tc.want)
			}
			if got != tc.want {
				t.Errorf("checkableHrefTarget(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// checkableHrefTarget normalises an authored href into a requestable path,
// reporting false for the shapes that name no internal route.
func checkableHrefTarget(raw string) (string, bool) {
	// An href that is ENTIRELY a template action is runtime data, and drops
	// out at the slash-rooted test below — substituted or not, it is never
	// slash-rooted. (Order is not load-bearing here; a negative control
	// moving this after the slash test changed no verdict. It reads better
	// as plain normalise-then-classify.)
	//
	// A PARTIALLY templated path is the case that needs the substitution:
	// /playlists/{{.ID}} has to reach the mux as a concrete path for a
	// wildcard route to match it.
	target := goTemplateActionRe.ReplaceAllString(raw, hrefTemplateDummy)
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	target = strings.TrimSpace(target)
	// Internal links only: this also excludes "https://…" and "//host/…",
	// neither of which is ours to resolve.
	if !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return "", false
	}
	return target, true
}

// TestHrefRegexAcceptsBothQuotingStyles pins the extractor's reach.
//
// Every href in the templates today is double-quoted with no spaces
// around the `=`, so this is guarding a FUTURE blind spot rather than a
// present bug — and that is exactly the point: a link checker that
// silently stops seeing links keeps passing, which is worse than not
// having one. HTML permits single quotes and whitespace around `=`.
func TestHrefRegexAcceptsBothQuotingStyles(t *testing.T) {
	const doc = `
	  <a href="/double">d</a>
	  <a href='/single'>s</a>
	  <a href = "/spaced">sp</a>
	  <a HREF="/uppercase">u</a>
	`
	want := map[string]bool{"/double": true, "/single": true, "/spaced": true, "/uppercase": true}
	got := map[string]bool{}
	for _, m := range hrefRe.FindAllStringSubmatch(doc, -1) {
		raw := m[1]
		if raw == "" {
			raw = m[2]
		}
		got[raw] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("extractor missed %q; it would silently stop checking links written that way", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("extracted %v, want exactly %v", got, want)
	}
}
