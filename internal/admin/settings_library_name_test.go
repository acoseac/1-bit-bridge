package admin

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestSettingsPatchLibraryNameIsWhatARestartServes: the name the running
// bridge serves after a PATCH is the name config.Load serves the file that
// PATCH saved, so the name a device sees does not change when the bridge
// restarts.
//
// Measured on 2026-09-27 with the real binary (main at a2a72f1b): a PATCH of
// `{"libraryName":""}` answered 200 with the field `live`, /v1/health then
// served `"libraryName": ""` (to a caller with no token too), the next
// pairing QR carried `name=` empty, which the app refuses as a pairing code
// "missing the name field", and bridge.yaml was saved with `libraryName: ""`,
// which Load serves as DefaultLibraryName after a restart. `"   "` did the
// same, trimmed to "". The console sends the field on every Save, so
// clearing the box was all it took.
//
// A name that is blank once trimmed is refused with a 400 naming the field,
// and the name stays what it was, live and on disk: a blank Save is a
// mistake, and taking it as DefaultLibraryName would replace the operator's
// name with one nobody chose. Every other name is stored trimmed, the way
// the app's pairing parser requires ("extra spaces in the name field").
//
// A name over config.MaxLibraryNameLength runes once trimmed is refused the
// same way: the app refuses a pairing code whose name is over 256
// Characters ("Pairing code's name field is too long."), and on 2026-09-27
// (main at 3214aa17) a PATCH of 257 characters answered 200 `live` and the
// next QR did not pair. The cap counts runes, as the app counts Characters,
// so 256 two-byte or four-byte runes still pair.
func TestSettingsPatchLibraryNameIsWhatARestartServes(t *testing.T) {
	const fixtureName = "Test Library" // newTestServer's
	a256 := strings.Repeat("a", 256)
	for _, tc := range []struct {
		label      string // the subtest's name; the quoted value when empty
		sent       string
		wantStatus int
		want       string
	}{
		{"", "Renamed", http.StatusOK, "Renamed"},
		{"", "  Padded Name  ", http.StatusOK, "Padded Name"},
		{"", "", http.StatusBadRequest, fixtureName},
		{"", "   ", http.StatusBadRequest, fixtureName},
		{"", "\t\n", http.StatusBadRequest, fixtureName},
		// U+200B: the app's trim removes it, strings.TrimSpace does not.
		{"", "\u200b", http.StatusBadRequest, fixtureName},
		{"", "\u200bZW\u200b", http.StatusOK, "ZW"},
		{"256 runes", a256, http.StatusOK, a256},
		{"257 runes", a256 + "b", http.StatusBadRequest, fixtureName},
		{"256 runes once trimmed", "  " + a256 + "  ", http.StatusOK, a256},
		{"256 two-byte runes", strings.Repeat("é", 256), http.StatusOK, strings.Repeat("é", 256)},
		{"257 two-byte runes", strings.Repeat("é", 257), http.StatusBadRequest, fixtureName},
		{"256 four-byte runes", strings.Repeat("🎧", 256), http.StatusOK, strings.Repeat("🎧", 256)},
	} {
		label := tc.label
		if label == "" {
			label = fmt.Sprintf("%q", tc.sent)
		}
		t.Run(label, func(t *testing.T) {
			srv, cfgPath, fileBefore := libraryNameFixture(t, fixtureName)
			var body map[string]any
			code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
				map[string]any{"libraryName": tc.sent}, &body)
			if code != tc.wantStatus {
				t.Errorf("PATCH libraryName=%q: %d %v, want %d", tc.sent, code, body, tc.wantStatus)
			}
			if code == http.StatusBadRequest {
				assertRefusalWroteNothing(t, body, cfgPath, fileBefore)
			}
			assertNameSurvivesARestart(t, srv, cfgPath, tc.want)
		})
	}
}

// libraryNameFixture is newTestServer, checked to be named name, with its
// bridge.yaml as it was before the test sent anything.
func libraryNameFixture(t *testing.T, name string) (*Server, string, []byte) {
	t.Helper()
	srv, _, cfgPath := newTestServer(t)
	if got := srv.deps.CfgHolder.Load().LibraryName; got != name {
		t.Fatalf("premise: the fixture is named %q, want %q", got, name)
	}
	before, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return srv, cfgPath, before
}

// assertRefusalWroteNothing fails unless body is the handler's `validate`
// refusal, naming libraryName, and bridge.yaml still holds exactly before.
func assertRefusalWroteNothing(t *testing.T, body map[string]any, cfgPath string, before []byte) {
	t.Helper()
	if body["error"] != "validate" {
		t.Errorf("error code = %v, want %q", body["error"], "validate")
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "libraryName") {
		t.Errorf("the refusal %q does not name the field", msg)
	}
	after, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Errorf("a refused PATCH rewrote bridge.yaml:\n%s", after)
	}
}

// assertNameSurvivesARestart fails unless the running bridge serves want and
// config.Load serves the saved bridge.yaml under the same name, which is
// what a restart would serve.
func assertNameSurvivesARestart(t *testing.T, srv *Server, cfgPath, want string) {
	t.Helper()
	live := srv.deps.CfgHolder.Load().LibraryName
	if live != want {
		t.Errorf("the running bridge serves %q, want %q", live, want)
	}
	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load the saved config: %v", err)
	}
	if reloaded.LibraryName != live {
		t.Errorf("after a restart the bridge would serve %q; it serves %q now",
			reloaded.LibraryName, live)
	}
}

// libraryNameInputRe finds the settings form's library-name control.
var libraryNameInputRe = regexp.MustCompile(`<input[^>]*\bname="libraryName"[^>]*>`)

// TestSettingsFormRequiresALibraryName: the console does not send a blank
// name for the handler to refuse. The Save is a real submit button, so the
// browser runs the form's constraint validation before app.js sees the
// submit: `required` stops an empty box, and the pattern one of spaces
// alone, which `required` lets through and the handler would refuse once
// trimmed. The pattern is a JavaScript regular expression the browser
// anchors at both ends; Go's reads these ASCII cases the same way.
func TestSettingsFormRequiresALibraryName(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings: %d", resp.StatusCode)
	}

	input := libraryNameInputRe.FindString(string(page))
	if input == "" {
		t.Fatal("the settings page renders no libraryName input")
	}
	if !regexp.MustCompile(`\srequired[\s>]`).MatchString(input) {
		t.Errorf("the libraryName input is not required: %s", input)
	}
	// maxlength counts UTF-16 code units, never fewer than the runes the
	// handler counts, so the box never lets through a name the handler
	// refuses; it stops a name of astral characters (emoji) short of the
	// cap, where the handler's 400 would not come into it.
	if want := fmt.Sprintf(` maxlength="%d"`, config.MaxLibraryNameLength); !strings.Contains(input, want) {
		t.Errorf("the libraryName input lacks%s, so a name the handler refuses is sent: %s", want, input)
	}
	m := regexp.MustCompile(`\spattern="([^"]*)"`).FindStringSubmatch(input)
	if m == nil {
		t.Fatalf("the libraryName input has no pattern, so a name of spaces alone is sent: %s", input)
	}
	anchored, err := regexp.Compile(`^(?:` + m[1] + `)$`)
	if err != nil {
		t.Fatalf("the pattern %q does not compile: %v", m[1], err)
	}
	for _, v := range []string{"My Library", "x", "  Padded  "} {
		if !anchored.MatchString(v) {
			t.Errorf("the pattern %q refuses %q, a name the handler accepts", m[1], v)
		}
	}
	for _, v := range []string{" ", "   ", "\t"} {
		if anchored.MatchString(v) {
			t.Errorf("the pattern %q accepts %q, which the handler refuses", m[1], v)
		}
	}
}
