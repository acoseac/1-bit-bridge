package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// controlImpliedFields is the mapping under test, spelled out here rather
// than read from config so a change to the map has to be made twice, on
// purpose. The point of this table is to be the SECOND opinion: if somebody
// widens managedControlSettings without meaning to, this file disagrees.
var controlImpliedFields = []struct {
	control string
	field   string
	body    string
}{
	{config.ManagedControlUpdates, "updateAutoInstall", `{"updateAutoInstall":true}`},
	{config.ManagedControlUpdates, "updateCheckIntervalHours", `{"updateCheckIntervalHours":1}`},
	{config.ManagedControlUpdates, "updateQuietHours", `{"updateQuietHours":"01:00-02:00"}`},
	{config.ManagedControlRestart, "updateAutoInstall", `{"updateAutoInstall":true}`},
	{config.ManagedControlBackups, "backupIntervalHours", `{"backupIntervalHours":1}`},
	{config.ManagedControlBackups, "backupKeep", `{"backupKeep":10000}`},
}

// TestManagedControlRefusesTheSettingsFieldThatPerformsIt is the hole PR #876
// left open: it gated the ACTIONS at the route table and nothing gated the
// settings FIELDS that reach the same effect.
//
// `updateAutoInstall` is read LIVE by the updater's poll loop, and
// AutoInstallOpts / AutoInstallRestart are wired unconditionally — so one
// authenticated `PATCH /api/settings {"updateAutoInstall":true}` made the
// bridge download a release, swap its own binary and call the graceful
// shutdown closure. That is the version move ManagedControlUpdates exists to
// refuse plus the process restart ManagedControlRestart exists to refuse,
// with no console affordance involved and no route gate in the way.
//
// Each row declares ONLY its own control, which is what makes the assertion
// about that control rather than about "something is managed".
func TestManagedControlRefusesTheSettingsFieldThatPerformsIt(t *testing.T) {
	for _, tc := range controlImpliedFields {
		t.Run(tc.control+"/"+tc.field, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			manageControls(t, srv, tc.control)

			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, loopbackReq("PATCH", "/api/settings", tc.body))

			if w.Code == http.StatusOK {
				t.Fatalf("PATCH %s was ACCEPTED on a bridge declaring managedControls: [%s]",
					tc.field, tc.control)
			}
			var env struct{ Error, Message string }
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not an error envelope: %v (%s)", err, w.Body.String())
			}
			// Asserting on the CODE, not only on non-200: a validation
			// failure would also be non-200 and would prove nothing about
			// the managed posture.
			if env.Error != "managed-setting" {
				t.Errorf("error = %q, want managed-setting (message %q)", env.Error, env.Message)
			}
		})
	}
}

// TestUnmanagedBridgeStillAcceptsTheSameFields is the negative control for
// the test above, and the reason it is not vacuous: on a bridge declaring no
// controls, every one of those requests must still be accepted. Without this,
// a handler that refused every PATCH unconditionally would pass.
func TestUnmanagedBridgeStillAcceptsTheSameFields(t *testing.T) {
	for _, tc := range controlImpliedFields {
		t.Run(tc.field, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			// No manageControls call: the default, self-hosted shape.
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, loopbackReq("PATCH", "/api/settings", tc.body))
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 — an unmanaged bridge owns its own settings (%s)",
					w.Code, w.Body.String())
			}
		})
	}
}

// TestOneControlDoesNotImplyAnothersFields keeps the map honest per control.
// Declaring `backups` must not lock the update fields, and vice versa —
// otherwise the mapping is just "anything managed locks everything", which
// would be indistinguishable from a bug.
func TestOneControlDoesNotImplyAnothersFields(t *testing.T) {
	srv, _, _ := newTestServer(t)
	manageControls(t, srv, config.ManagedControlBackups)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, loopbackReq("PATCH", "/api/settings", `{"updateCheckIntervalHours":6}`))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — `backups` must not own an update field (%s)",
			w.Code, w.Body.String())
	}
}

// TestEffectiveManagedSettingsIsWhatTheConsoleIsTold pins the half that keeps
// a managed bridge SAVEABLE.
//
// The console hides what `managedSettings` names and dropUnofferedFields then
// drops it from the payload. Report the narrow set while refusing the wide
// one and every save on a managed bridge supplies a field the handler
// refuses — which is the nineteen-name wall PR #877 exists to fix, rebuilt
// one layer down. No Go test can drive the JS, so this asserts the contract
// at the wire instead: what we TELL the console must cover what we REFUSE.
func TestEffectiveManagedSettingsIsWhatTheConsoleIsTold(t *testing.T) {
	srv, _, _ := newTestServer(t)
	manageControls(t, srv, config.ManagedControlUpdates, config.ManagedControlBackups)

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, loopbackReq("GET", "/api/settings", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/settings: status = %d", w.Code)
	}
	var got struct {
		ManagedSettings []string `json:"managedSettings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, tc := range controlImpliedFields {
		if tc.control == config.ManagedControlRestart {
			continue // not declared by this fixture
		}
		if !slices.Contains(got.ManagedSettings, tc.field) {
			t.Errorf("managedSettings omits %q, which the PATCH refuses — the console would render it, submit it, and the save would fail whole. got %v",
				tc.field, got.ManagedSettings)
		}
	}
}

// TestUnmanagedBridgeReportsNoManagedSettings pins the omission: the wire
// field is `omitempty`, and a self-hosted install must not suddenly start
// carrying one.
func TestUnmanagedBridgeReportsNoManagedSettings(t *testing.T) {
	srv, _, _ := newTestServer(t)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, loopbackReq("GET", "/api/settings", ""))

	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Key-absence, not a substring probe on the body — the documented way
	// to assert omitempty in this repo.
	if v, ok := raw["managedSettings"]; ok {
		t.Errorf("managedSettings present on an unmanaged bridge: %v", v)
	}
}
