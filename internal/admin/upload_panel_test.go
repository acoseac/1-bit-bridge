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
	tmpl := readFile(t, "templates/upload.html")

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
	tmpl := readFile(t, "templates/upload.html")

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
	// CRLF: there is no .gitattributes pinning eol, so a Windows checkout
	// carries "\r\n" and a "\n}\n" literal is not in the bytes at all. The
	// same test shape failed on windows-latest from the day it was added the
	// last time (see CLAUDE.md); normalizing at the read is the fix.
	js := strings.ReplaceAll(readFile(t, "static/app.js"), "\r\n", "\n")
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
	tmpl := readFile(t, "templates/upload.html")
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
	tmpl := readFile(t, "templates/upload.html")
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
	tmpl := readFile(t, "templates/upload.html")
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

// The trash panel has the same silent-drift exposure as the upload one: ids
// resolved at runtime, nothing in Go linking the two halves.
var trashIDRe = regexp.MustCompile(`"(trash-[a-z0-9-]+)"`)
var trashTmplIDRe = regexp.MustCompile(`id="(trash-[a-z0-9-]+)"`)

func TestTrashPanelIDsMatchBothWays(t *testing.T) {
	js := readFile(t, "static/app.js")
	tmpl := readFile(t, "templates/library.html")

	inTmpl := map[string]bool{}
	for _, m := range trashTmplIDRe.FindAllStringSubmatch(tmpl, -1) {
		inTmpl[m[1]] = true
	}
	if len(inTmpl) == 0 {
		t.Fatal("no trash-* ids in library.html — the panel is gone or was renamed wholesale")
	}
	inJS := map[string]bool{}
	for _, m := range trashIDRe.FindAllStringSubmatch(js, -1) {
		inJS[m[1]] = true
	}
	ariaWired := map[string]bool{}
	for _, m := range ariaRefRe.FindAllStringSubmatch(tmpl, -1) {
		for _, id := range strings.Fields(m[2]) {
			ariaWired[id] = true
		}
	}
	for id := range inJS {
		if !inTmpl[id] {
			t.Errorf("app.js reaches for %q but library.html renders no such id", id)
		}
	}
	for id := range inTmpl {
		if !inJS[id] && !ariaWired[id] {
			t.Errorf("library.html renders id=%q but nothing references it", id)
		}
	}
}

// TestTrashPanelIsHiddenUntilDeleteIsAllowed — an operator who has not turned
// deleting on should see no delete chrome, the same rule the upload panel and
// the upscale stats card follow.
func TestTrashPanelIsHiddenUntilDeleteIsAllowed(t *testing.T) {
	tmpl := readFile(t, "templates/library.html")
	i := strings.Index(tmpl, `id="trash-panel"`)
	if i < 0 {
		t.Fatal("trash panel not found")
	}
	open := tmpl[strings.LastIndex(tmpl[:i], "<"):]
	open = open[:strings.Index(open, ">")]
	if !strings.Contains(open, "hidden") {
		t.Errorf("the trash panel renders visible by default: %q", open)
	}
	js := readFile(t, "static/app.js")
	if !strings.Contains(js, "cfg.allowDelete") {
		t.Error("app.js never consults allowDelete, so the panel would either always or never show")
	}
}

// TestExpiryUsesAFutureFormatter — formatTimeAgo clamps at zero, so a trash
// expiry rendered through it reads "0s ago" when it has a week left: a wrong
// answer that looks like a real one. This shipped once.
func TestExpiryUsesAFutureFormatter(t *testing.T) {
	// Normalized for the same reason putUploadChunk's scan is: a Windows
	// checkout carries CRLF and the "\n}\n" delimiter below is not in the
	// bytes at all.
	js := strings.ReplaceAll(readFile(t, "static/app.js"), "\r\n", "\n")
	i := strings.Index(js, "async function refreshTrash")
	if i < 0 {
		t.Fatal("refreshTrash not found — this test no longer covers the expiry cell")
	}
	end := strings.Index(js[i:], "\n}\n")
	if end < 0 {
		t.Fatal("could not delimit refreshTrash")
	}
	body := js[i : i+end]
	if !strings.Contains(body, "formatTimeUntil(new Date(e.expiresAt))") {
		t.Error("the trash expiry is not rendered with formatTimeUntil; formatTimeAgo " +
			"clamps at zero and shows a future timestamp as \"0s ago\"")
	}
}

// TestErrorClassIsStyled — `.error` is used by four elements across the
// templates and had NO rule in either stylesheet, so every one of them rendered
// as ordinary body text. An upload rejection appeared as an unremarkable line
// under a long file list and was missed in the field.
//
// Same shape as the warn-hint guard: comments stripped, selector anchored to a
// boundary, because a plain substring passes against `.error-details` and
// against the class merely being named in a comment.
func TestErrorClassIsStyled(t *testing.T) {
	tmpl := ""
	for _, f := range []string{"templates/library.html", "templates/settings.html", "templates/devices.html"} {
		tmpl += readFile(t, f)
	}
	if !strings.Contains(tmpl, `class="error"`) {
		t.Skip("no bare .error elements any more")
	}
	css := uploadCSSCommentRe.ReplaceAllString(readFile(t, "static/app.css"), "")
	if !errorRuleRe.MatchString(css) {
		t.Error(`templates render class="error" but app.css has no .error rule — ` +
			"every error message renders as ordinary body text")
	}
}

var errorRuleRe = regexp.MustCompile(`\.error\s*[,{]`)

// TestUploadFiltersOSJunkBeforeDeclaring — a folder picked on a Mac contains a
// .DS_Store nobody put there. Declaring it means the operator is told about a
// rejection for a file they did not choose and cannot avoid.
func TestUploadFiltersOSJunkBeforeDeclaring(t *testing.T) {
	js := readFile(t, "static/app.js")
	if !strings.Contains(js, "function isOSJunkPath") {
		t.Fatal("no OS-junk filter in the upload client")
	}
	for _, name := range []string{".ds_store", "thumbs.db", "desktop.ini", "@eadir"} {
		if !strings.Contains(js, name) {
			t.Errorf("the junk list does not cover %q", name)
		}
	}
	// The AppleDouble sidecar rule is a prefix test, not a name.
	if !strings.Contains(js, `startsWith("._")`) {
		t.Error("AppleDouble sidecars (._name) are not filtered")
	}
	if !strings.Contains(js, "isOSJunkPath(p.path)") {
		t.Error("the filter is defined but never applied to the picked files")
	}
}

// TestAddMusicHasItsOwnPageAndRailEntry — uploading used to be a panel on the
// Roots page, below the roots table and the transcoded-cache card. On a host
// with no shell it is the ONLY way to get audio in, so it is a destination
// rather than something you scroll past.
func TestAddMusicHasItsOwnPageAndRailEntry(t *testing.T) {
	if pages["upload"] != "upload.html" {
		t.Fatalf(`pages["upload"] = %q, want upload.html`, pages["upload"])
	}
	layout := readFile(t, "templates/layout.html")
	if !strings.Contains(layout, `href="/upload" data-tab="upload"`) {
		t.Error("no Add music entry in the sidebar")
	}
	// The icon must exist in the sprite, or the entry renders a blank box.
	if !strings.Contains(layout, `<g id="i-add">`) {
		t.Error(`the rail entry references #i-add but the sprite has no such symbol`)
	}
	// The panel moved: it must NOT still be on the Roots page as well.
	if strings.Contains(readFile(t, "templates/library.html"), `id="upload-panel"`) {
		t.Error("the upload panel is on BOTH the Roots page and its own page")
	}
	if !strings.Contains(readFile(t, "templates/upload.html"), `id="upload-panel"`) {
		t.Error("upload.html does not contain the upload panel")
	}
}

// TestAddMusicPageExplainsItselfWhenDisabled — the entry is unconditional, so a
// visitor with uploads off must land on something better than an empty page.
func TestAddMusicPageExplainsItselfWhenDisabled(t *testing.T) {
	tmpl := readFile(t, "templates/upload.html")
	if !strings.Contains(tmpl, `id="upload-disabled-panel"`) {
		t.Fatal("no off-state panel on the Add music page")
	}
	js := readFile(t, "static/app.js")
	if !strings.Contains(js, `getElementById("upload-disabled-panel")`) {
		t.Error("the off-state panel is never revealed")
	}
	if !strings.Contains(js, "attachFeatureTray(off.querySelector") {
		t.Error("the off-state offers no way to turn uploads on from this page")
	}
	if !strings.Contains(readFile(t, "templates/upload.html"), `class="panel-actions"`) {
		t.Error("the off-state panel has no action group for the tray gear to land in")
	}
}
