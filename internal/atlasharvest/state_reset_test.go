package atlasharvest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestResetSyncPositionRewindsThePositionAlone pins the database-only
// restore's rewind of the harvest (backlog B653 PR 2b1): the cursor, the last
// submit and the last booklet check go to zero while the credential, its
// expiry, its base and the pending covers stay; a second call changes no
// byte; no file stands, none is made; and a file that did not decode is
// replaced by the clean state, never kept.
func TestResetSyncPositionRewindsThePositionAlone(t *testing.T) {
	t.Run("a held position", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "atlas-harvest.json")
		held := `{"token":"tok","atlasBaseUrl":"https://atlas.example","expiresAt":"2030-01-01T00:00:00Z","resultCursor":42,` +
			`"lastSubmitAt":"2026-01-01T00:00:00Z","lastBookletCheckAt":"2026-01-02T00:00:00Z","pendingCovers":{"m":1}}`
		if err := os.WriteFile(path, []byte(held), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := OpenStateStore(path)
		if err != nil {
			t.Fatal(err)
		}
		present, err := s.ResetSyncPosition()
		if err != nil || !present {
			t.Fatalf("ResetSyncPosition = %v, %v", present, err)
		}
		var st State
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &st); err != nil {
			t.Fatal(err)
		}
		if st.ResultCursor != 0 || !st.LastSubmitAt.IsZero() || !st.LastBookletCheckAt.IsZero() {
			t.Errorf("the position after the rewind: %+v", st)
		}
		if st.Token != "tok" || st.AtlasBaseURL != "https://atlas.example" || !st.ExpiresAt.Equal(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)) || st.PendingCovers["m"] != 1 {
			t.Errorf("the credential or the covers did not stay: %+v", st)
		}
		fi, _ := os.Stat(path)
		if _, err := s.ResetSyncPosition(); err != nil {
			t.Fatal(err)
		}
		again, _ := os.ReadFile(path)
		fi2, _ := os.Stat(path)
		if !bytes.Equal(again, data) || !os.SameFile(fi, fi2) {
			t.Error("a second rewind wrote the file again")
		}
	})
	t.Run("no file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "atlas-harvest.json")
		s, err := OpenStateStore(path)
		if err != nil {
			t.Fatal(err)
		}
		present, err := s.ResetSyncPosition()
		if err != nil || present {
			t.Fatalf("ResetSyncPosition with no file = %v, %v", present, err)
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the rewind made a file: %v", err)
		}
	})
	t.Run("a file that does not decode", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "atlas-harvest.json")
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := OpenStateStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if present, err := s.ResetSyncPosition(); err != nil || !present {
			t.Fatalf("ResetSyncPosition = %v, %v", present, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var st State
		if err := json.Unmarshal(data, &st); err != nil || st.ResultCursor != 0 {
			t.Errorf("the file after the rewind = %q (%v), want the clean state", data, err)
		}
	})
}
