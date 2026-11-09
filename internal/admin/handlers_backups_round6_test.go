package admin

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// TestTheConsoleSaysARecordNotReadBackApartFromItsWrite is backlog B653's
// sixth round's red for the console's job (the round's brief: a durable write
// whose read-back fails never reads as a write that failed): a create's
// verification whose write lands and whose record cannot be read back is
// logged as one whose write ended, recorded, its record not read, and why,
// never as a verdict not recorded. Red on the round before, whose job read the
// record it had not read as no verified recovery point with no verdicts.
func TestTheConsoleSaysARecordNotReadBackApartFromItsWrite(t *testing.T) {
	s, _, _ := newTestServer(t)
	s.deps.BackupSources = liveStore(t, t.TempDir())
	t.Cleanup(backup.SetReadBackFailureForTest(func(string) error { return errors.New("injected: the read-back failed") }))
	logs := loggingtest.Record(t)
	if code := doJSON(t, s.Handler(), http.MethodPost, "/api/backups", map[string]any{}, nil); code != http.StatusCreated {
		t.Fatalf("POST /api/backups = %d, want 201", code)
	}
	s.WaitForBackupJobs()
	lines := logs.Lines("admin: a snapshot's verification answered an error once its write had ended")
	if len(lines) != 1 || !strings.Contains(lines[0], "write=recorded") || !strings.Contains(lines[0], "recordRead=false") ||
		!strings.Contains(lines[0], "injected: the read-back failed") {
		t.Errorf("the job logged %q; want its write recorded, its record not read back, and why\nall: %q", lines, logs.All())
	}
	for _, never := range []string{"admin: a snapshot's verdict was not recorded", "admin: a snapshot is not a verified recovery point", "admin: snapshot verified"} {
		if got := logs.Lines(never); len(got) > 0 {
			t.Errorf("the job logged %q of a durable write whose record was not read back", got)
		}
	}
}
