package adminauth

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSignedOutFileKeepsTheAccountAndDropsSessions(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	raw, err := json.Marshal(storeFile{
		User: &userRecord{
			Username:          "operator",
			PasswordHash:      "hash",
			CreatedAt:         created,
			PasswordChangedAt: created,
		},
		Sessions: map[string]*Session{
			"abc": {Username: "operator", IssuedAt: created, LastUsedAt: created},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 1, 2, 3, 0, time.UTC)
	out, changed, err := SignedOutFile(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("an account with a session was left unchanged")
	}
	if !strings.HasSuffix(string(out), "\n") {
		t.Fatal("signed-out file has no trailing newline")
	}
	var got storeFile
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.User == nil || got.User.Username != "operator" || got.User.PasswordHash != "hash" {
		t.Fatalf("account = %+v", got.User)
	}
	if !got.User.CreatedAt.Equal(created) || !got.User.PasswordChangedAt.Equal(created) {
		t.Fatalf("timestamps moved: %+v", got.User)
	}
	if len(got.Sessions) != 0 {
		t.Fatalf("sessions = %d, want none", len(got.Sessions))
	}
	if got.SessionsRevokedAt == nil || !got.SessionsRevokedAt.Equal(now) {
		t.Fatalf("marker = %v, want %v", got.SessionsRevokedAt, now)
	}

	again, changed, err := SignedOutFile(out, now)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a second sign-out of an account was left unchanged")
	}
	var second storeFile
	if err := json.Unmarshal(again, &second); err != nil {
		t.Fatal(err)
	}
	if second.SessionsRevokedAt == nil || !second.SessionsRevokedAt.Equal(now.Add(time.Nanosecond)) {
		t.Fatalf("second marker = %v, want one nanosecond past the first", second.SessionsRevokedAt)
	}
}

func TestSignedOutFileCopiesAFileWithNoAccount(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte{}, []byte("{}")} {
		out, changed, err := SignedOutFile(raw, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Fatalf("changed a file with no account: %q", raw)
		}
		if string(out) != string(raw) {
			t.Fatalf("out %q, raw %q", out, raw)
		}
	}
}

func TestSignedOutFileRefusesAFileThatDoesNotParse(t *testing.T) {
	if _, _, err := SignedOutFile([]byte(`{`), time.Now()); err == nil {
		t.Fatal("a truncated file parsed")
	}
}

func TestSignedOutFileUpgradesALegacyAccount(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	raw, err := json.Marshal(userRecord{
		Username:          "operator",
		PasswordHash:      "hash",
		CreatedAt:         created,
		PasswordChangedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 13, 1, 2, 3, 0, time.UTC)
	out, changed, err := SignedOutFile(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a legacy account was left unchanged")
	}
	var got storeFile
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.User == nil || got.User.PasswordHash != "hash" || len(got.Sessions) != 0 {
		t.Fatalf("upgraded = %+v sessions %d", got.User, len(got.Sessions))
	}
	if got.SessionsRevokedAt == nil || !got.SessionsRevokedAt.Equal(now) {
		t.Fatalf("marker = %v", got.SessionsRevokedAt)
	}
}
