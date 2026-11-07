package admin

import (
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestTheSettingsPatchConvertsACustomEndpointHostThatIsNotASCII drives the
// console's settings save with a custom endpoint whose host is not ASCII
// (B744). One with an IDNA form is taken and stored in it, live, the form a
// certificate can carry and every mint then takes, and the file holds the
// same list, which loads unchanged; one with no IDNA form is refused whole
// (400 `validate`, naming its place, the file untouched), since the prune
// would drop it and store a list the operator did not type.
func TestTheSettingsPatchConvertsACustomEndpointHostThatIsNotASCII(t *testing.T) {
	for _, text := range []bool{false, true} {
		body := func(entry string) map[string]any {
			list := []string{"https://ok.example:7788", entry}
			if text {
				return map[string]any{"customEndpointsText": strings.Join(list, "\n")}
			}
			return map[string]any{"customEndpoints": list}
		}
		name := "the array"
		if text {
			name = "the textarea"
		}
		t.Run(name, func(t *testing.T) {
			srv, _, cfgPath := newTestServer(t)
			fileBefore, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			var refused struct{ Error, Message string }
			code := doJSON(t, srv.Handler(), "PATCH", "/api/settings", body("https://a_ü.example:7788"), &refused)
			if code != 400 || refused.Error != "validate" {
				t.Fatalf("PATCH with a host that has no IDNA form = %d %q, want 400 validate", code, refused.Error)
			}
			if !strings.Contains(refused.Message, "customEndpoints[1]") {
				t.Errorf("the refusal does not name the entry's place: %s", refused.Message)
			}
			fileAfter, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(fileBefore, fileAfter) {
				t.Errorf("a refused PATCH rewrote the config file:\n%s", fileAfter)
			}

			var saved settingsPatchResponse
			if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings", body("https://münchen.de:7788"), &saved); code != 200 {
				t.Fatalf("PATCH with https://münchen.de:7788 = %d, want 200", code)
			}
			if got := saved.Fields["customEndpoints"].Status; got != applyLive {
				t.Errorf("status = %q, want %q", got, applyLive)
			}
			want := []string{"https://ok.example:7788", "https://xn--mnchen-3ya.de:7788"}
			if got := srv.deps.CfgHolder.Load().CustomEndpoints; !slices.Equal(got, want) {
				t.Errorf("the live endpoints are %q, want %q", got, want)
			}
			reloaded, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(reloaded.CustomEndpoints, want) {
				t.Errorf("the saved endpoints load as %q, want %q", reloaded.CustomEndpoints, want)
			}
		})
	}
}
