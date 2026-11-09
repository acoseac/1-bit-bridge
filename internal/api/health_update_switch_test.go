package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestHealthAdvertisesNoUpdateWhileUpdateChecksAreOff: while update checks
// are off (update.checkEnabled, backlog B761) /v1/health advertises no
// update to a paired client, whatever the updater last found: updateAvailable
// is an explicit false, latestServerVersion and updateReleaseNotesURL are
// absent, and minClientVersion, the client's floor and no update of the
// bridge's, is still sent. The UpdateInfo here carries the candidate a check
// found before the switch went off, as an adapter that passed the cached
// status would, so the gate is health's own.
func TestHealthAdvertisesNoUpdateWhileUpdateChecksAreOff(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{LibraryRoots: []string{lib}, ListenAddress: ":7788", LibraryName: "T"}
	store, err := auth.OpenStore(filepath.Join(dir, "tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, store, nil, "fp").WithUpdater(fakeUpdater{
		info: UpdateInfo{
			LatestVersion:    "9.9.9",
			UpdateAvailable:  true,
			ReleaseNotesURL:  "https://example.test/release",
			MinClientVersion: "1.2.0",
			ChecksOff:        true,
		},
	})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	raw, _, err := store.Mint("probe")
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/health", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	resp, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if v, ok := got["updateAvailable"]; !ok || v != false {
		t.Errorf("updateAvailable = %v (present %v), want an explicit false while update checks are off", v, ok)
	}
	for _, key := range []string{"latestServerVersion", "updateReleaseNotesURL"} {
		if v, ok := got[key]; ok {
			t.Errorf("%s = %v while update checks are off, want it absent", key, v)
		}
	}
	if v := got["minClientVersion"]; v != "1.2.0" {
		t.Errorf("minClientVersion = %v, want 1.2.0: the client's floor is no update of the bridge's", v)
	}
}
