package admin

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestTheSettingsPatchRefusesACustomEndpointCarryingACredential drives the
// console's settings PATCH with a custom endpoint carrying a secret in each
// part of a URL that can carry one, in the array form and the textarea
// form (backlog B54). /v1/health publishes the list to any caller and every
// pairing QR carries it, so a list the operator TYPES with such an entry is
// refused whole (400 `validate`, the config and the file untouched), and the
// refusal names the entry by position, scheme and host, never the secret.
// Storing it without the part would store a URL nobody typed: that repair is
// for a list a config already holds (config.ValidateCustomEndpoints). The
// same lists without the secret save, `live`.
func TestTheSettingsPatchRefusesACustomEndpointCarryingACredential(t *testing.T) {
	const secret = "s3cret-Pw"
	for _, tc := range []struct {
		name  string
		typed string // the entry, second in the list
		clean string // the same endpoint without the secret
		text  bool   // sent as customEndpointsText
	}{
		{"a password", "https://user:" + secret + "@bridge.example:7788", "https://bridge.example:7788", false},
		{"a token as the user name", "https://" + secret + "@bridge.example:7788", "https://bridge.example:7788", false},
		{"a query", "https://bridge.example:7788/?token=" + secret, "https://bridge.example:7788/", false},
		{"a fragment, in the textarea", "https://bridge.example:7788/#" + secret, "https://bridge.example:7788/", true},
		{"a password, in the textarea", "https://user:" + secret + "@bridge.example:7788", "https://bridge.example:7788", true},
	} {
		body := func(entry string) map[string]any {
			list := []string{"https://ok.example:7788", entry}
			if tc.text {
				return map[string]any{"customEndpointsText": strings.Join(list, "\n")}
			}
			return map[string]any{"customEndpoints": list}
		}
		t.Run(tc.name, func(t *testing.T) {
			srv, _, cfgPath := newTestServer(t)
			fileBefore, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			var refused struct{ Error, Message string }
			code := doJSON(t, srv.Handler(), "PATCH", "/api/settings", body(tc.typed), &refused)
			if code != 400 || refused.Error != "validate" {
				t.Fatalf("PATCH with %q = %d %q, want 400 validate", tc.typed, code, refused.Error)
			}
			if strings.Contains(strings.ToLower(refused.Message), strings.ToLower(secret)) {
				t.Errorf("the refusal carries the secret: %s", refused.Message)
			}
			if !strings.Contains(refused.Message, "customEndpoints[1] (https://bridge.example:7788)") {
				t.Errorf("the refusal does not name the entry by position, scheme and host: %s", refused.Message)
			}
			if got := srv.deps.CfgHolder.Load().CustomEndpoints; len(got) != 0 {
				t.Errorf("stored endpoints = %q after a refused PATCH, want none", got)
			}
			fileAfter, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(fileBefore, fileAfter) {
				t.Errorf("a refused PATCH rewrote the config file:\n%s", fileAfter)
			}

			// The positive control: the same list, the secret left out.
			var saved settingsPatchResponse
			if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings", body(tc.clean), &saved); code != 200 {
				t.Fatalf("PATCH with %q = %d, want 200", tc.clean, code)
			}
			if got := saved.Fields["customEndpoints"].Status; got != applyLive {
				t.Errorf("status = %q, want %q", got, applyLive)
			}
			want := "https://ok.example:7788\n" + tc.clean
			if got := strings.Join(srv.deps.CfgHolder.Load().CustomEndpoints, "\n"); got != want {
				t.Errorf("stored endpoints = %q, want %q", got, want)
			}
		})
	}
}
