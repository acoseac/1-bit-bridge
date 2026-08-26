package admin

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Feature trays are the gear beside a heading that opens that feature's
// switches in place. Each row names a settings field as a STRING, and
// that string has to be right on both legs of a round trip:
//
//   - reading, from GET /api/settings, or the control paints the wrong
//     state (a switch showing "off" for something that is on, which a
//     reader then "enables" by turning it off);
//   - writing, into PATCH /api/settings, whose body decodes with
//     encoding/json's default behaviour — an UNKNOWN field is silently
//     dropped. So a typo saves nothing, the handler answers 200, and the
//     tray reports "Saved." That is the same failure the settings form's
//     allowlist test exists for, and it is worse here because there is no
//     Save button to leave un-pressed: the operator watches a switch move
//     and believes it.
//
// Nothing else connects a tray row to the Go structs, so this is the only
// thing that would notice a field being renamed on one side.

// trayFieldRe pulls `field: "name"` out of a tray spec. Both quote styles
// (the codebase uses double), and tolerant of whitespace around the colon.
var trayFieldRe = regexp.MustCompile(`\bfield:\s*["']([A-Za-z][A-Za-z0-9]*)["']`)

// trayCommentRe strips `//` comments before the scan, for the reason
// player_css_parity_test.go strips CSS comments: this repo's commentary
// quotes the identifiers it discusses, and a comment naming a field that
// no longer exists would fail a test whose subject is live code.
//
// Anchored to a whole comment LINE, unlike js_reference_parity_test.go's
// jsLineCommentRe, which strips from `//` to end-of-line anywhere. That
// one is looking at call syntax and a trailing comment is noise; here a
// `//` inside a string literal (a URL in a tray's link) would take the
// rest of the line with it.
var trayCommentRe = regexp.MustCompile(`(?m)^\s*//.*$`)

// trayScriptBodies reads every script under static/, comments stripped.
//
// Walked rather than hand-listed. Trays live in three files today (the
// operator pages' in app.js, the player's Smart-mixes tray in views.js,
// the variant-generation one in variants.js) and a fourth added to a new
// file would otherwise be silently unchecked — which is the same
// forgot-to-add-it-to-the-list failure the test itself is about.
func trayScriptBodies(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir("static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".js" {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		out[path] = trayCommentRe.ReplaceAllString(string(b), "")
		return nil
	})
	if err != nil {
		t.Fatalf("walk static: %v", err)
	}
	if len(out) < 5 {
		t.Fatalf("only %d scripts found under static/ — the walk has stopped working", len(out))
	}
	return out
}

func TestEveryFeatureTrayFieldExistsOnBothSettingsStructs(t *testing.T) {
	fields := map[string][]string{} // field -> files that name it
	for path, body := range trayScriptBodies(t) {
		for _, m := range trayFieldRe.FindAllStringSubmatch(body, -1) {
			fields[m[1]] = append(fields[m[1]], path)
		}
	}
	// A regex that has stopped matching passes vacuously, which is the one
	// outcome that leaves the drift it guards against invisible.
	if len(fields) < 8 {
		t.Fatalf("only %d tray fields scraped (%v) — the regex has stopped "+
			"matching, so this test is checking nothing", len(fields), keysOf(fields))
	}

	patch := jsonTagsOf(reflect.TypeOf(settingsPatch{}))
	read := jsonTagsOf(reflect.TypeOf(settingsResponse{}))

	for _, name := range keysOf(fields) {
		where := strings.Join(fields[name], ", ")
		if !patch[name] {
			t.Errorf("tray field %q (%s) is not a json tag on settingsPatch — "+
				"encoding/json drops unknown fields, so saving it would report "+
				"success and change nothing", name, where)
		}
		if !read[name] {
			t.Errorf("tray field %q (%s) is not a json tag on settingsResponse — "+
				"the control has nothing to load its current state from and would "+
				"paint the zero value", name, where)
		}
	}
}

// jsonTagsOf returns the set of json field names a struct puts on the
// wire, ignoring the ",omitempty" half and skipping `json:"-"`.
func jsonTagsOf(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name != "" {
			out[name] = true
		}
	}
	return out
}

func keysOf(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestFeatureTrayRestartBadgesAgreeWithSettings pins the tray's restart
// badges against the Settings page's, field by field.
//
// The badge is a PREDICTION — the authoritative answer is the
// restartRequired flag on the PATCH response, which the tray reports
// after every save regardless. So the badge's only job is to set the
// right expectation BEFORE the click, and two surfaces predicting
// differently for one field is worse than either being wrong: the
// operator has no way to tell which one to believe.
//
// autoOptimizeEnabled is the field this catches. It hot-applies whenever
// a sweeper is wired, carries no badge on the Settings page, and is the
// one switch on the Jobs CarPlay tray that takes effect immediately.
func TestFeatureTrayRestartBadgesAgreeWithSettings(t *testing.T) {
	settings, err := os.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	var sb strings.Builder
	for _, body := range trayScriptBodies(t) {
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	js := sb.String()

	// A tray row is one object literal; `restart: true` sits inside the
	// same braces as its `field`. Matched together rather than looked up
	// separately so a row cannot borrow its neighbour's badge.
	rowRe := regexp.MustCompile(`\{[^{}]*\bfield:\s*"([A-Za-z0-9]+)"[^{}]*\}`)
	seen := map[string]bool{}
	for _, m := range rowRe.FindAllStringSubmatch(js, -1) {
		field, row := m[1], m[0]
		if seen[field] {
			continue // the same field can appear on two trays; one check is enough
		}
		seen[field] = true
		trayBadge := strings.Contains(row, "restart: true")
		wantBadge, ok := settingsBadgeFor(string(settings), field)
		if !ok {
			// A field the Settings page doesn't render as a named input:
			// nothing to agree with, and the parity test above already
			// pins that it is a real settings field.
			continue
		}
		if trayBadge != wantBadge {
			t.Errorf("field %q: tray restart badge = %v, Settings page = %v — "+
				"one of the two is telling the operator the wrong thing about "+
				"whether the change takes effect now", field, trayBadge, wantBadge)
		}
	}
	if len(seen) < 8 {
		t.Fatalf("only %d tray rows scraped (%v) — the regex has stopped matching",
			len(seen), seen)
	}
}

// settingsBadgeFor reports whether the Settings page marks a field
// restart-required, and whether it renders the field at all.
//
// Two shapes have to be read, and they put the badge on opposite sides
// of the input:
//
//	<label class="checkbox"><input name=x> Text <badge></label>   (after)
//	<div class="field"><label for=x>Text <badge></label><input name=x>  (before)
//
// So the window opens at the enclosing `<div class="field"`, which both
// shapes have, and closes at the input UNLESS the input sits inside a
// label — in which case it extends to that label's close. Without the
// inside-a-label test, a sibling-label field would run its window on to
// the NEXT field's label and report that field's badge as its own.
//
// The class is matched loosely (`badge<anything>`) because the page uses
// BOTH `badge warn` and `badge restart` for this marker. An exact-class
// matcher found four disagreements that were all its own.
func settingsBadgeFor(html, field string) (badge, found bool) {
	i := strings.Index(html, `name="`+field+`"`)
	if i < 0 {
		return false, false
	}
	start := strings.LastIndex(html[:i], `<div class="field`)
	if start < 0 {
		return false, false
	}
	end := i
	if closed := strings.Index(html[i:], "</label>"); closed >= 0 {
		if opened := strings.Index(html[i:i+closed], "<label"); opened < 0 {
			end = i + closed
		}
	}
	return settingsRestartBadgeRe.MatchString(html[start:end]), true
}

var settingsRestartBadgeRe = regexp.MustCompile(`class="badge[^"]*">restart<`)
