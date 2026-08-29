package admin

import (
	"regexp"
	"strings"
	"testing"
)

// The upload panel is server-rendered markup driven entirely by ids resolved at
// runtime in app.js. Nothing in Go links the two, so a rename on either side
// fails SILENTLY: the JS resolves null, every guard returns early, and the
// panel simply does nothing — no error, no console noise, no failing test.
//
// This is the same guard the transcoded-cache panel already carries, and it
// matches any "upload-…" STRING LITERAL rather than only getElementById
// arguments: most wiring goes through the shared setText(id, …) helper, so an
// anchored-on-getElementById regexp would cover a fraction of the panel while
// appearing to cover all of it.
var uploadIDRe = regexp.MustCompile(`"(upload-[a-z0-9-]+)"`)
var uploadTmplIDRe = regexp.MustCompile(`id="(upload-[a-z0-9-]+)"`)

func TestUploadPanelIDsExistInTheTemplate(t *testing.T) {
	js := readFile(t, "static/app.js")
	tmpl := readFile(t, "templates/library.html")

	present := map[string]bool{}
	for _, m := range uploadTmplIDRe.FindAllStringSubmatch(tmpl, -1) {
		present[m[1]] = true
	}
	if len(present) == 0 {
		t.Fatal("no upload-* ids in library.html — the panel is gone or was renamed wholesale")
	}
	seen := map[string]bool{}
	for _, m := range uploadIDRe.FindAllStringSubmatch(js, -1) {
		id := m[1]
		if seen[id] {
			continue
		}
		seen[id] = true
		if !present[id] {
			t.Errorf("app.js reaches for %q but library.html renders no such id — "+
				"the control resolves null and silently does nothing", id)
		}
	}
}

func TestUploadPanelIDsAreWiredInAppJS(t *testing.T) {
	js := readFile(t, "static/app.js")
	tmpl := readFile(t, "templates/library.html")

	used := map[string]bool{}
	for _, m := range uploadIDRe.FindAllStringSubmatch(js, -1) {
		used[m[1]] = true
	}
	// An id referenced by aria-describedby / aria-labelledby / for /
	// aria-controls is wired BY THE TEMPLATE — it is a static description or
	// label, not a control the JS drives. Recognising that is better than an
	// allowlist, which would also swallow a genuinely dead id.
	ariaWired := map[string]bool{}
	for _, m := range ariaRefRe.FindAllStringSubmatch(tmpl, -1) {
		for _, id := range strings.Fields(m[2]) {
			ariaWired[id] = true
		}
	}
	for _, m := range uploadTmplIDRe.FindAllStringSubmatch(tmpl, -1) {
		if !used[m[1]] && !ariaWired[m[1]] {
			t.Errorf("library.html renders id=%q but nothing references it — "+
				"not app.js, and not an aria/for association either", m[1])
		}
	}
}

var ariaRefRe = regexp.MustCompile(`(aria-describedby|aria-labelledby|aria-controls|for)="([^"]+)"`)

// TestUploadClientEncodesPathSegmentsNotFormEncoded — URLSearchParams
// form-encodes a space as "+", and url.Values decodes "+" back to a space, so a
// session or file id containing one would resolve to a different value
// server-side. That is the documented /v1 variant-delete trap; the ids here are
// hex so it cannot bite today, but the habit is what stops it biting when
// something user-derived enters a URL later.
func TestUploadClientEncodesPathSegmentsNotFormEncoded(t *testing.T) {
	js := readFile(t, "static/app.js")
	start := strings.Index(js, "async function putUploadChunk")
	if start < 0 {
		t.Fatal("putUploadChunk not found — this test no longer covers the chunk URL")
	}
	end := strings.Index(js[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit putUploadChunk")
	}
	body := js[start : start+end]
	if strings.Contains(body, "URLSearchParams") {
		t.Error("putUploadChunk builds its URL with URLSearchParams, which form-encodes a space as '+'")
	}
	if !strings.Contains(body, "encodeURIComponent") {
		t.Error("putUploadChunk does not encodeURIComponent its path segments")
	}
}

// TestUploadPanelIsHiddenUntilEnabled — an operator who has not opted in should
// see no upload chrome at all, the same rule the upscale stats card follows.
func TestUploadPanelIsHiddenUntilEnabled(t *testing.T) {
	tmpl := readFile(t, "templates/library.html")
	i := strings.Index(tmpl, `id="upload-panel"`)
	if i < 0 {
		t.Fatal("upload panel not found in library.html")
	}
	// The hidden attribute must be on the panel element itself.
	openTag := tmpl[strings.LastIndex(tmpl[:i], "<"):]
	openTag = openTag[:strings.Index(openTag, ">")]
	if !strings.Contains(openTag, "hidden") {
		t.Errorf("the upload panel renders visible by default: %q", openTag)
	}
	js := readFile(t, "static/app.js")
	if !strings.Contains(js, "cfg.uploadEnabled") {
		t.Error("app.js never consults uploadEnabled, so the panel would either always or never show")
	}
}

// TestUploadDupeNoteOutlivesTheReviewBlock — the duplicate note explains the
// count in #upload-result, which is rendered AFTER the review block collapses.
// Nested inside #upload-review it is hidden with it, and the operator reads
// "1 added" with no account of where the others went.
//
// This shipped once and a JS assertion missed it: checking `.hidden` on the
// note itself says nothing about an ancestor being hidden. Only a screenshot
// caught it, so the pin has to be structural.
func TestUploadDupeNoteOutlivesTheReviewBlock(t *testing.T) {
	tmpl := readFile(t, "templates/library.html")
	review := strings.Index(tmpl, `id="upload-review"`)
	note := strings.Index(tmpl, `id="upload-dupe-note"`)
	result := strings.Index(tmpl, `id="upload-result"`)
	if review < 0 || note < 0 || result < 0 {
		t.Fatal("upload panel ids missing — this test no longer covers anything")
	}
	// The review block ends before the note begins.
	reviewEnd := strings.Index(tmpl[review:], `id="upload-progress"`)
	if reviewEnd < 0 {
		t.Fatal("could not delimit the review block")
	}
	if note < review+reviewEnd {
		t.Error("#upload-dupe-note is inside #upload-review, so it is hidden the " +
			"moment the upload starts — exactly when it is needed")
	}
	if note > result {
		t.Error("#upload-dupe-note renders after #upload-result; the explanation " +
			"should precede the count it explains")
	}
}

// TestTemplateWarnHintIsStyled — `.hint warn` was an inert class combination for
// its first outing, so the note rendered as ordinary hint text, visually
// indistinguishable from the two plain hints directly above it.
func TestTemplateWarnHintIsStyled(t *testing.T) {
	tmpl := readFile(t, "templates/library.html")
	if !strings.Contains(tmpl, `class="hint warn"`) {
		t.Skip("no warn hint in this template any more")
	}
	// Comments are stripped first and the selector is matched to a BOUNDARY.
	// A plain substring check passes against `.hint.warn-anything`, against
	// `.hint.warning`, and against the class merely being named in a comment —
	// and this repo's CSS comments name the classes they discuss. A guard that
	// cannot fail is worse than none: verified by mutation, where renaming the
	// rule to `.hint.warn-DISABLED` left the substring form green.
	css := uploadCSSCommentRe.ReplaceAllString(readFile(t, "static/app.css"), "")
	if !hintWarnRuleRe.MatchString(css) {
		t.Error("library.html renders class=\"hint warn\" but app.css has no .hint.warn rule — " +
			"the class is inert and the note reads as ordinary hint text")
	}
}

var (
	uploadCSSCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	hintWarnRuleRe     = regexp.MustCompile(`\.hint\.warn\s*[,{]`)
)
