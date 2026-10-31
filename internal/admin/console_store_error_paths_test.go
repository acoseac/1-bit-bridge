package admin

import (
	"net/http"
	"os"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The lines the console logs for a token write and a compaction that
// failed, with the error itself.
const (
	tokensNotSavedLine       = "admin: the device tokens were not saved"
	databaseNotCompactedLine = "admin: the database was not compacted"
	pairingURLForTheTests    = "https://127.0.0.1:7788"
)

// TestATokenWriteThatFailsAnswersWithoutAPath: the four console routes that
// write the device tokens answered a write that failed with its error,
// whose *fs.PathError names tokens.json or its staging file in the data
// directory: a read-only data directory, or a tokens.json a root CLI left
// root's (#1048), is enough. Found beside Codex's findings on B604's
// integrated branch, by the same sweep. Each says what did not happen and
// what to do in words, and logs the error whole at Error.
func TestATokenWriteThatFailsAnswersWithoutAPath(t *testing.T) {
	skipWhereModesDoNotRefuse(t)
	logs := loggingtest.Record(t)
	srv, cfg, _ := newTestServer(t)
	h := srv.Handler()
	var minted pairResult
	if code := doJSON(t, h, http.MethodPost, "/api/tokens",
		map[string]string{"name": "Kept", "url": pairingURLForTheTests}, &minted); code != http.StatusCreated {
		t.Fatalf("mint before the refusal: %d", code)
	}
	// A data directory the bridge may not create its staging file in.
	info, err := os.Stat(cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfg.DataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.DataDir, info.Mode().Perm()) })

	for _, tc := range []struct {
		name, method, path, code, consequence string
		body                                  any
	}{
		{"a mint", http.MethodPost, "/api/tokens", "mint", "no device token was made",
			map[string]string{"name": "Refused", "url": pairingURLForTheTests}},
		{"a revoke", http.MethodDelete, "/api/tokens/" + minted.ID, "revoke", "still works", nil},
		{"a rotate", http.MethodPost, "/api/tokens/" + minted.ID + "/rotate", "rotate", "still works",
			map[string]string{"url": pairingURLForTheTests}},
		{"an expiry", http.MethodPatch, "/api/tokens/" + minted.ID, "set-expiry", "not changed",
			map[string]any{"expiresAt": "2030-01-01T00:00:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body saveRefusalBody
			code := doJSON(t, h, tc.method, tc.path, tc.body, &body)
			requirePathlessAnswer(t, code, body, http.StatusInternalServerError, tc.code, []string{cfg.DataDir},
				tc.consequence, "give the user the bridge runs as")
		})
	}
	requireTheFaultLogged(t, logs, tokensNotSavedLine, cfg.DataDir)
}

// TestACompactionThatFailsAnswersWithoutAPath: POST /api/database/compact
// answered a compaction that failed with its error, which names the
// database file when it cannot be measured (os.Stat's *fs.PathError, here a
// data directory the bridge may not search). Found beside Codex's findings
// on B604's integrated branch, by the same sweep. It says what failed in
// words, and logs the error whole at Error.
func TestACompactionThatFailsAnswersWithoutAPath(t *testing.T) {
	skipWhereModesDoNotRefuse(t)
	logs := loggingtest.Record(t)
	srv, cfg, _ := newTestServer(t)
	withoutPermission(t, cfg.DataDir)
	var body saveRefusalBody
	code := doJSON(t, srv.Handler(), http.MethodPost, "/api/database/compact", nil, &body)
	requirePathlessAnswer(t, code, body, http.StatusInternalServerError, "compact-failed", []string{cfg.DataDir}, "log")
	requireTheFaultLogged(t, logs, databaseNotCompactedLine, cfg.DataDir)
}
