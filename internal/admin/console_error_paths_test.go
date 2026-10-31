package admin

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The lines the console logs for a backup or a certificate read that failed,
// with the error itself: the log may name the bridge's own files, and keeps
// what the console's answer leaves out.
const (
	backupsNotListedLine = "admin: the backups could not be listed"
	snapshotNotTakenLine = "admin: no snapshot was taken"
	certNotReadLine      = "admin: the TLS certificate could not be read"
)

// requirePathlessAnswer fails the test unless the console answered
// wantStatus and wantCode, with a message that says each of remedies and
// names none of hidden.
func requirePathlessAnswer(t *testing.T, status int, body saveRefusalBody, wantStatus int, wantCode string, hidden []string, remedies ...string) {
	t.Helper()
	if status != wantStatus || body.Error != wantCode {
		t.Errorf("answered %d %q, want %d %q", status, body.Error, wantStatus, wantCode)
	}
	for _, h := range hidden {
		if strings.Contains(body.Message, h) {
			t.Errorf("the message names %s: %q", h, body.Message)
		}
	}
	for _, r := range remedies {
		if !strings.Contains(body.Message, r) {
			t.Errorf("the message %q does not say %q", body.Message, r)
		}
	}
}

// requireTheFaultLogged fails the test unless line was logged at Error with
// the error whole, which names private: the operator still learns which file
// and why.
func requireTheFaultLogged(t *testing.T, logs *loggingtest.Recorder, line, private string) {
	t.Helper()
	for _, l := range logs.Lines(line) {
		if strings.HasPrefix(l, "ERROR ") && strings.Contains(l, private) {
			return
		}
	}
	t.Errorf("no Error line %q carrying the error (%s): %q", line, private, logs.All())
}

// skipWhereModesDoNotRefuse skips a test whose fixture takes a permission
// away with a mode: Windows does not refuse by it, and root is not refused.
func skipWhereModesDoNotRefuse(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a mode does not take a permission away on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by the mode this fixture sets")
	}
}

// withoutPermission takes every permission on path away until the test ends.
func withoutPermission(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, info.Mode().Perm()) })
}

// mkdirAllT is os.MkdirAll, failing the test on an error.
func mkdirAllT(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestABackupThatFailsAnswersWithoutAPath: GET /api/backups whose listing
// failed, and POST /api/backups whose snapshot did, answered 500 with the
// error's text, which names the backups folder (os.ReadDir's *fs.PathError)
// or a file the snapshot copies, the config file among them (Codex on
// B604's integrated branch). They answer what failed and what to do in
// words, and log the error whole at Error.
func TestABackupThatFailsAnswersWithoutAPath(t *testing.T) {
	t.Run("a listing that fails", func(t *testing.T) {
		logs := loggingtest.Record(t)
		srv, _, _ := newTestServer(t)
		dataDir := filepath.Join(t.TempDir(), "Private Data")
		mkdirAllT(t, dataDir)
		// A file where the backups folder goes: its listing fails, and not as
		// a folder that is not there yet, which lists nothing.
		if err := os.WriteFile(filepath.Join(dataDir, backup.BackupsDirName), []byte("not a folder"), 0o600); err != nil {
			t.Fatal(err)
		}
		srv.deps.BackupSources = backup.Sources{DataDir: dataDir}
		var body saveRefusalBody
		code := doJSON(t, srv.Handler(), http.MethodGet, "/api/backups", nil, &body)
		requirePathlessAnswer(t, code, body, http.StatusInternalServerError, "list-failed", []string{dataDir}, "log")
		requireTheFaultLogged(t, logs, backupsNotListedLine, dataDir)
	})
	t.Run("a listing the bridge may not read", func(t *testing.T) {
		skipWhereModesDoNotRefuse(t)
		logs := loggingtest.Record(t)
		srv, _, _ := newTestServer(t)
		dataDir := filepath.Join(t.TempDir(), "Private Data")
		backups := filepath.Join(dataDir, backup.BackupsDirName)
		mkdirAllT(t, backups)
		withoutPermission(t, backups)
		srv.deps.BackupSources = backup.Sources{DataDir: dataDir}
		var body saveRefusalBody
		code := doJSON(t, srv.Handler(), http.MethodGet, "/api/backups", nil, &body)
		requirePathlessAnswer(t, code, body, http.StatusInternalServerError, "list-failed", []string{dataDir},
			"give the user the bridge runs as")
		requireTheFaultLogged(t, logs, backupsNotListedLine, dataDir)
	})
	t.Run("a snapshot that fails", func(t *testing.T) {
		logs := loggingtest.Record(t)
		srv, cfg, _ := newTestServer(t)
		private := filepath.Join(t.TempDir(), "Private Config")
		// A folder where the config file is: the snapshot's copy of it is
		// refused, naming it.
		yaml := filepath.Join(private, "bridge.yaml")
		mkdirAllT(t, yaml)
		srv.deps.BackupSources = backup.Sources{DataDir: cfg.DataDir, BridgeYAML: yaml}
		var body saveRefusalBody
		code := doJSON(t, srv.Handler(), http.MethodPost, "/api/backups", nil, &body)
		requirePathlessAnswer(t, code, body, http.StatusInternalServerError, "snapshot-failed", []string{private}, "log")
		requireTheFaultLogged(t, logs, snapshotNotTakenLine, private)
	})
	t.Run("a snapshot the bridge may not read a file for", func(t *testing.T) {
		skipWhereModesDoNotRefuse(t)
		logs := loggingtest.Record(t)
		srv, cfg, _ := newTestServer(t)
		private := filepath.Join(t.TempDir(), "Private Data")
		mkdirAllT(t, private)
		tokens := filepath.Join(private, "tokens.json")
		if err := os.WriteFile(tokens, []byte("[]"), 0o600); err != nil {
			t.Fatal(err)
		}
		withoutPermission(t, tokens)
		srv.deps.BackupSources = backup.Sources{DataDir: cfg.DataDir, TokensJSON: tokens}
		var body saveRefusalBody
		code := doJSON(t, srv.Handler(), http.MethodPost, "/api/backups", nil, &body)
		requirePathlessAnswer(t, code, body, http.StatusInternalServerError, "snapshot-failed", []string{private},
			"give the user the bridge runs as", "run the backup again")
		requireTheFaultLogged(t, logs, snapshotNotTakenLine, private)
	})
}

// TestTheCertificateTileAnswersWithoutAPath: GET /api/cert named the
// certificate's path in its 404, and answered a certificate it could not read
// with os.ReadFile's *fs.PathError (Codex on B604's integrated branch). The
// 404 keeps the command and the warning beside it; the 500 says what failed
// in words and logs the error whole at Error.
func TestTheCertificateTileAnswersWithoutAPath(t *testing.T) {
	t.Run("no certificate", func(t *testing.T) {
		srv, cfg, _ := newTestServer(t)
		var body saveRefusalBody
		code := doJSON(t, srv.Handler(), http.MethodGet, "/api/cert", nil, &body)
		requirePathlessAnswer(t, code, body, http.StatusNotFound, "no-certificate", []string{cfg.DataDir},
			"`bridge cert rotate`", "invalidates every existing iOS pairing")
	})
	certAt := func(t *testing.T, srv *Server, cfg *config.Config, private string) string {
		t.Helper()
		cfg.TLSCertPath = filepath.Join(private, "server.crt")
		cfg.TLSKeyPath = filepath.Join(private, "server.key")
		srv.deps.CfgHolder.Store(cfg)
		return cfg.TLSCertPath
	}
	t.Run("a certificate that cannot be read", func(t *testing.T) {
		logs := loggingtest.Record(t)
		srv, cfg, _ := newTestServer(t)
		private := filepath.Join(t.TempDir(), "Private Certs")
		// A folder where the certificate goes: there, and no file to read.
		mkdirAllT(t, certAt(t, srv, cfg, private))
		var body saveRefusalBody
		code := doJSON(t, srv.Handler(), http.MethodGet, "/api/cert", nil, &body)
		requirePathlessAnswer(t, code, body, http.StatusInternalServerError, "inspect-failed", []string{private}, "log")
		requireTheFaultLogged(t, logs, certNotReadLine, private)
	})
	t.Run("a certificate the bridge may not read", func(t *testing.T) {
		skipWhereModesDoNotRefuse(t)
		logs := loggingtest.Record(t)
		srv, cfg, _ := newTestServer(t)
		private := filepath.Join(t.TempDir(), "Private Certs")
		mkdirAllT(t, private)
		crt := certAt(t, srv, cfg, private)
		if err := os.WriteFile(crt, []byte("not read"), 0o600); err != nil {
			t.Fatal(err)
		}
		withoutPermission(t, crt)
		var body saveRefusalBody
		code := doJSON(t, srv.Handler(), http.MethodGet, "/api/cert", nil, &body)
		requirePathlessAnswer(t, code, body, http.StatusInternalServerError, "inspect-failed", []string{private},
			"give the user the bridge runs as read access")
		requireTheFaultLogged(t, logs, certNotReadLine, private)
	})
}

// TestARootTheConsoleRefusesIsRefusedInWords: POST /api/roots answered a path
// it could not stat with the stat's *fs.PathError and a file with the path it
// resolved, and resolved a relative path against the bridge's working
// directory, which both then named (Codex on B604's integrated branch); the
// two basename refusals named the configured roots. Each says what is wrong
// and what to do, and names no path: a folder's own name, the part of a root
// the app keys tracks by, is not one.
func TestARootTheConsoleRefusesIsRefusedInWords(t *testing.T) {
	private := filepath.Join(t.TempDir(), "Private Library")
	mkdirAllT(t, private)
	file := filepath.Join(private, "track.flac")
	if err := os.WriteFile(file, []byte("fLaC"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path, code string
		hidden           []string
		remedies         []string
	}{
		{"a folder that is not there", filepath.Join(private, "Missing"), "not-found", []string{private}, []string{"absolute path"}},
		{"a relative path", "B604 relative folder", "not-found", []string{cwd}, []string{"absolute path"}},
		{"a file", file, "not-a-dir", []string{private}, []string{"not a folder"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			var body saveRefusalBody
			code := doJSON(t, srv.Handler(), http.MethodPost, "/api/roots", map[string]string{"path": tc.path}, &body)
			requirePathlessAnswer(t, code, body, http.StatusBadRequest, tc.code, tc.hidden, tc.remedies...)
		})
	}
	t.Run("a folder the bridge may not reach", func(t *testing.T) {
		skipWhereModesDoNotRefuse(t)
		locked := filepath.Join(t.TempDir(), "Private Locked")
		mkdirAllT(t, filepath.Join(locked, "Music"))
		withoutPermission(t, locked)
		srv, _, _ := newTestServer(t)
		var body saveRefusalBody
		code := doJSON(t, srv.Handler(), http.MethodPost, "/api/roots", map[string]string{"path": filepath.Join(locked, "Music")}, &body)
		requirePathlessAnswer(t, code, body, http.StatusBadRequest, "not-found", []string{locked}, "give the user the bridge runs as")
	})
	t.Run("a folder name another root has", func(t *testing.T) {
		srv, cfg, _ := newTestServer(t)
		twin := filepath.Join(private, "music") // the fixture's root is "Music"
		mkdirAllT(t, twin)
		var body saveRefusalBody
		code := doJSON(t, srv.Handler(), http.MethodPost, "/api/roots", map[string]string{"path": twin}, &body)
		requirePathlessAnswer(t, code, body, http.StatusConflict, "basename-collision",
			[]string{private, filepath.Dir(cfg.LibraryRoots[0])}, `"Music"`, `"music"`, "rename")
	})
	t.Run("a removal its folder's name makes ambiguous", func(t *testing.T) {
		srv, cfg, _ := newTestServer(t)
		// The case-twin pair as a config written before ValidateRoots refused
		// it carries it (TestRootsRemoveRefusesCaseTwinBasename), and a third
		// root so the removal is not the collapse to one root.
		base := filepath.Join(t.TempDir(), "Private Roots")
		twin := filepath.Join(base, "other", "music")
		third := filepath.Join(base, "third", "Spoken")
		mkdirAllT(t, twin)
		mkdirAllT(t, third)
		roots := append(append([]string(nil), srv.deps.Scanner.Roots()...), twin, third)
		srv.deps.Scanner.SetRoots(roots)
		if err := srv.deps.CfgHolder.Update(cfg.DataDir+"/../bridge.yaml", func(next *config.Config) error {
			next.LibraryRoots = roots
			return nil
		}); err != nil {
			t.Fatalf("inject roots: %v", err)
		}
		var body saveRefusalBody
		code := doJSON(t, srv.Handler(), http.MethodDelete, "/api/roots", map[string]string{"path": twin}, &body)
		requirePathlessAnswer(t, code, body, http.StatusConflict, "ambiguous-basename",
			[]string{base, filepath.Dir(cfg.LibraryRoots[0])}, `"music"`, `"Music"`, "rename")
	})
}
