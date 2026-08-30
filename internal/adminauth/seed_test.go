package adminauth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func emptyStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adminauth.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s, path
}

// TestSeedFromEnvInitialisesAnEmptyStore is the provisioning path: on a
// host nobody has a shell on, `bridge admin reset-password` is not a
// step anyone can take.
func TestSeedFromEnvInitialisesAnEmptyStore(t *testing.T) {
	t.Setenv("BRIDGE_ADMIN_PASSWORD", "a-perfectly-fine-secret")
	s, _ := emptyStore(t)

	seeded, err := s.SeedFromEnv()
	if err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	if !seeded {
		t.Fatal("store was not seeded")
	}
	if s.Username() != "admin" {
		t.Errorf("username = %q, want the default %q", s.Username(), "admin")
	}
	if err := s.Verify("admin", "a-perfectly-fine-secret"); err != nil {
		t.Errorf("the seeded credential does not verify: %v", err)
	}
	if err := s.Verify("admin", "something-else"); err == nil {
		t.Error("Verify accepted a wrong password")
	}
}

// TestSeedFromEnvDoesNotResetAConfiguredStore is the property that keeps
// the environment a SEED rather than the credential itself.
//
// A bridge whose env still carries the variable must not have its
// password reset on every restart: an operator who rotated it would find
// the rotation silently undone by the next bounce.
func TestSeedFromEnvDoesNotResetAConfiguredStore(t *testing.T) {
	s, _ := emptyStore(t)
	if err := s.SetInitialPassword("admin", "the-original-secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetPassword("admin", "the-rotated-secret"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("BRIDGE_ADMIN_PASSWORD", "the-original-secret")
	seeded, err := s.SeedFromEnv()
	if err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	if seeded {
		t.Error("a configured store was re-seeded — a rotated password would be " +
			"silently undone by every restart")
	}
	if err := s.Verify("admin", "the-rotated-secret"); err != nil {
		t.Errorf("the rotated password no longer verifies: %v", err)
	}
}

// TestSeedFromEnvPrefersTheFileForm: a mounted secret file does not
// appear in `ps`, `docker inspect`, or a crash dump of the environment
// the way a variable does, so when both are set the file wins.
func TestSeedFromEnvPrefersTheFileForm(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "admin-password")
	// Trailing newline on purpose: that is what a mounted secret looks
	// like, and the newline is not part of the password.
	if err := os.WriteFile(secret, []byte("from-the-mounted-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_ADMIN_PASSWORD", "from-the-variable")
	t.Setenv("BRIDGE_ADMIN_PASSWORD_FILE", secret)

	s, _ := emptyStore(t)
	if _, err := s.SeedFromEnv(); err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	if err := s.Verify("admin", "from-the-mounted-file"); err != nil {
		t.Errorf("the file's secret does not verify — the inline variable won, or the "+
			"trailing newline was kept: %v", err)
	}
}

// TestSeedFromEnvFailsLoudlyOnAnUnreadableFile: falling back to the
// inline variable here would silently install a DIFFERENT credential
// than the one the operator configured.
func TestSeedFromEnvFailsLoudlyOnAnUnreadableFile(t *testing.T) {
	t.Setenv("BRIDGE_ADMIN_PASSWORD", "the-fallback-secret")
	t.Setenv("BRIDGE_ADMIN_PASSWORD_FILE", filepath.Join(t.TempDir(), "does-not-exist"))

	s, _ := emptyStore(t)
	seeded, err := s.SeedFromEnv()
	if err == nil {
		t.Fatal("an unreadable password file was not an error")
	}
	if seeded {
		t.Error("seeded despite the error")
	}
	if s.IsInitialised() {
		t.Error("the store was initialised from the inline fallback — the operator " +
			"asked for a file, and quietly using a different secret is worse than " +
			"refusing to start")
	}
}

// TestSeedRefusesAWeakPassword. Seeding is unattended: the value arrives
// from automation and nobody reads a warning about it. A weak secret
// installed by a config typo on a public bridge is worth refusing.
func TestSeedRefusesAWeakPassword(t *testing.T) {
	t.Setenv("BRIDGE_ADMIN_PASSWORD", "short")
	s, _ := emptyStore(t)
	if _, err := s.SeedFromEnv(); err == nil {
		t.Fatal("a 5-character seed was accepted")
	}
	if s.IsInitialised() {
		t.Error("the weak credential was installed anyway")
	}
}

// TestNoEnvIsNotAnError: an interactively-provisioned bridge is the
// normal case, and must not be turned into a startup failure.
func TestNoEnvIsNotAnError(t *testing.T) {
	t.Setenv("BRIDGE_ADMIN_PASSWORD", "")
	t.Setenv("BRIDGE_ADMIN_PASSWORD_FILE", "")
	s, _ := emptyStore(t)
	seeded, err := s.SeedFromEnv()
	if err != nil {
		t.Fatalf("absent env became an error: %v", err)
	}
	if seeded {
		t.Error("seeded from nothing")
	}
}

// TestSeededCredentialSurvivesRestart: an in-memory-only seed would
// leave the next boot uninitialised and the operator locked out by a
// bridge that had just accepted them.
func TestSeededCredentialSurvivesRestart(t *testing.T) {
	t.Setenv("BRIDGE_ADMIN_PASSWORD", "a-perfectly-fine-secret")
	s, path := emptyStore(t)
	if _, err := s.SeedFromEnv(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.IsInitialised() {
		t.Fatal("the seeded credential was not persisted")
	}
	if err := s2.Verify("admin", "a-perfectly-fine-secret"); err != nil {
		t.Errorf("verify after reopen: %v", err)
	}
}

func TestSeedHonoursUsernameOverride(t *testing.T) {
	t.Setenv("BRIDGE_ADMIN_USERNAME", "operator")
	t.Setenv("BRIDGE_ADMIN_PASSWORD", "a-perfectly-fine-secret")
	s, _ := emptyStore(t)
	if _, err := s.SeedFromEnv(); err != nil {
		t.Fatal(err)
	}
	if s.Username() != "operator" {
		t.Errorf("username = %q, want operator", s.Username())
	}
	if !strings.Contains(SeedSource(), "PASSWORD") {
		t.Errorf("SeedSource() = %q", SeedSource())
	}
}
