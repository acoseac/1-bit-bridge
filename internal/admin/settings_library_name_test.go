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
func TestSettingsPatchLibraryNameIsWhatARestartServes(t *testing.T) {
	const fixtureName = "Test Library" // newTestServer's
	for _, tc := range []struct {
		sent       string
		wantStatus int
		want       string
	}{
		{"Renamed", http.StatusOK, "Renamed"},
		{"  Padded Name  ", http.StatusOK, "Padded Name"},
		{"", http.StatusBadRequest, fixtureName},
		{"   ", http.StatusBadRequest, fixtureName},
		{"\t\n", http.StatusBadRequest, fixtureName},
		// U+200B: the app's trim removes it, strings.TrimSpace does not.
		{"\u200b", http.StatusBadRequest, fixtureName},
		{"\u200bZW\u200b", http.StatusOK, "ZW"},
	} {
		t.Run(fmt.Sprintf("%q", tc.sent), func(t *testing.T) {
			srv, _, cfgPath := newTestServer(t)
			if got := srv.deps.CfgHolder.Load().LibraryName; got != fixtureName {
				t.Fatalf("premise: the fixture is named %q, want %q", got, fixtureName)
			}
			fileBefore, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}

			var body map[string]any
			code := doJSON(t, srv.Handler(), "PATCH", "/api/settings",
				map[string]any{"libraryName": tc.sent}, &body)
			if code != tc.wantStatus {
				t.Errorf("PATCH libraryName=%q: %d %v, want %d", tc.sent, code, body, tc.wantStatus)
			}
			if code == http.StatusBadRequest {
				if body["error"] != "validate" {
					t.Errorf("error code = %v, want %q", body["error"], "validate")
				}
				if msg, _ := body["message"].(string); !strings.Contains(msg, "libraryName") {
					t.Errorf("the refusal %q does not name the field", msg)
				}
				fileAfter, err := os.ReadFile(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				if string(fileAfter) != string(fileBefore) {
					t.Errorf("a refused PATCH rewrote bridge.yaml:\n%s", fileAfter)
				}
			}

			live := srv.deps.CfgHolder.Load().LibraryName
			if live != tc.want {
				t.Errorf("the running bridge serves %q, want %q", live, tc.want)
			}
			reloaded, err := config.Load(cfgPath)
			if err != nil {
				t.Fatalf("load the saved config: %v", err)
			}
			if reloaded.LibraryName != live {
				t.Errorf("after a restart the bridge would serve %q; it serves %q now",
					reloaded.LibraryName, live)
			}
		})
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
