package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// saveRefusalBody is a console error body.
type saveRefusalBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// requireSaveRefusal fails the test unless the response is the console's 500
// save-config, its message saying remedy and naming nothing below root (the
// config file's directory, which holds the lock file and the staged save
// too), and the log holds the refusal itself, at Error, so the operator
// still learns which file and why.
func requireSaveRefusal(t *testing.T, logs *loggingtest.Recorder, status int, body saveRefusalBody, root, remedy string) {
	t.Helper()
	if status != http.StatusInternalServerError || body.Error != errCodeSaveConfig {
		t.Errorf("the refused save answered %d %q, want 500 %q", status, body.Error, errCodeSaveConfig)
	}
	if strings.Contains(body.Message, root) {
		t.Errorf("the refused save's message names the config file's directory: %q", body.Message)
	}
	if !strings.Contains(body.Message, remedy) {
		t.Errorf("the refused save's message %q does not say what to do (%q)", body.Message, remedy)
	}
	if lines := logs.Lines(configNotSavedLine); len(lines) == 0 || !strings.HasPrefix(lines[len(lines)-1], "ERROR ") {
		t.Errorf("the refused save left no Error line (%q) for the operator: %q", configNotSavedLine, logs.All())
	}
}

// configNotSavedLine is the line the console logs for a save of the config
// file that was refused or failed, with the error itself.
const configNotSavedLine = "admin: the config file was not saved"

// TestAConfigSaveRefusalSaysWhatToDoAndNamesNoPath: a console save of
// bridge.yaml that the config's lock, its turn or the holder refuses answers
// 500 save-config with the refusal's remedy in words (try again, repair the
// lock manager, restart, fix the link) and never the error's text, which
// names the config file, its lock file or a staged save: a console response
// may name no path (#1055's rule; the log may name the bridge's own files,
// and has the error whole). A refusal fn makes (*cfgAbort) answers as it
// always did.
func TestAConfigSaveRefusalSaysWhatToDoAndNamesNoPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Private Config")
	cfgPath := filepath.Join(root, "bridge.yaml")
	cases := []struct {
		name   string
		err    error
		remedy string
	}{
		{"another writer holds the lock", &config.BusyError{Path: cfgPath, Wait: config.LockWait}, "try again"},
		{"the filesystem gave no lock", &config.NoLockManagerError{Path: cfgPath, Wait: config.LockWait,
			Err: &fs.PathError{Op: "flock", Path: filepath.Join(root, ".bridge.yaml.lock"), Err: errors.New("no locks available")}},
			"lock manager"},
		{"another writer changed the file", &config.ChangedOutsideError{Path: cfgPath}, "restart the bridge"},
		{"the file changed in the turn", &config.ChangedInTurnError{Path: cfgPath}, "restart the bridge"},
		{"the name opens another file", &config.NameMovedError{Path: cfgPath}, "restart the bridge"},
		{"the boot read resolved no file", &config.BootUnresolvedError{Path: cfgPath}, "restart the bridge"},
		{"the UPnP writer's wrap of a refusal", fmt.Errorf("save bridge.yaml: %w", &config.ChangedOutsideError{Path: cfgPath}), "restart the bridge"},
		{"a save that failed otherwise", fmt.Errorf("save %s: %w", cfgPath,
			&fs.PathError{Op: "open", Path: filepath.Join(root, ".bridge.yaml.tmp-1"), Err: fs.ErrPermission}), "log"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := loggingtest.Record(t)
			rec := httptest.NewRecorder()
			writeCfgUpdateErr(rec, tc.err)
			var body saveRefusalBody
			decodeBody(t, rec, &body)
			requireSaveRefusal(t, logs, rec.Code, body, root, tc.remedy)
		})
	}
	t.Run("fn's own refusal", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeCfgUpdateErr(rec, &cfgAbort{status: http.StatusBadRequest, code: "validate", msg: "libraryName: must not be empty"})
		var body saveRefusalBody
		decodeBody(t, rec, &body)
		if rec.Code != http.StatusBadRequest || body.Error != "validate" || body.Message != "libraryName: must not be empty" {
			t.Errorf("fn's refusal answered %d %+v, want it as fn made it", rec.Code, body)
		}
	})
}

// TestAConfigSaveRefusalOfALinkToNothingSaysToFixTheLink: a config whose
// name is a symbolic link to no file is refused at the turn with what to
// do, which the console says in words without the link's path.
func TestAConfigSaveRefusalOfALinkToNothingSaysToFixTheLink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Private Config")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bridge.yaml")
	if err := os.Symlink(filepath.Join(root, "gone.yaml"), link); err != nil {
		t.Skipf("this host makes no symbolic link: %v", err)
	}
	refusal := config.CheckName(link)
	if refusal == nil {
		t.Fatal("CheckName took a link to nothing")
	}
	logs := loggingtest.Record(t)
	rec := httptest.NewRecorder()
	writeCfgUpdateErr(rec, refusal)
	var body saveRefusalBody
	decodeBody(t, rec, &body)
	requireSaveRefusal(t, logs, rec.Code, body, root, "point the link")
}

// TestTheConsolesSavesAnswerARefusalWithoutThePath drives the handlers that
// save the config, through the real holder: the settings PATCH over a file
// another writer changed (writeCfgUpdateErr), and a root added or removed,
// whose own 500s wrote the save's error as their message, by a holder whose
// boot read resolved no file.
func TestTheConsolesSavesAnswerARefusalWithoutThePath(t *testing.T) {
	t.Run("the settings PATCH", func(t *testing.T) {
		logs := loggingtest.Record(t)
		srv, _, cfgPath := newTestServer(t)
		editConfigBesideTheConsole(t, srv, cfgPath)
		var body saveRefusalBody
		code := doJSON(t, srv.Handler(), http.MethodPatch, "/api/settings", map[string]any{"libraryName": "After"}, &body)
		requireSaveRefusal(t, logs, code, body, filepath.Dir(cfgPath), "restart the bridge")
	})
	for _, tc := range rootChangeCases() {
		t.Run(tc.name, func(t *testing.T) {
			logs := loggingtest.Record(t)
			srv, dataDir, cfgPath, _, _, _ := seedSingleRootTrack(t)
			method, req, _ := tc.arrange(t, srv, dataDir)
			_, raw, _, err := config.LoadFileAt(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			srv.deps.CfgHolder.RecordFile(config.BootFile{}, raw)
			var body saveRefusalBody
			code := doJSON(t, srv.Handler(), method, "/api/roots", req, &body)
			srvBgScansWait(t, srv)
			requireSaveRefusal(t, logs, code, body, filepath.Dir(cfgPath), "restart the bridge")
		})
	}
}

// TestTheUPnPServerSavesAnswerARefusalWithoutThePath: the UPnP server
// writes' 500s wrote the save's error, which the upstream adapter wraps
// ("save bridge.yaml: …"), as their message.
func TestTheUPnPServerSavesAnswerARefusalWithoutThePath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Private Config")
	refusal := fmt.Errorf("save bridge.yaml: %w", &config.BusyError{Path: filepath.Join(root, "bridge.yaml"), Wait: config.LockWait})
	const udn = "uuid:4d696e69-444c-164e-9d41-00b78f5ae46a"
	for _, tc := range []struct {
		name string
		call func(s *Server, w http.ResponseWriter)
	}{
		{"add", func(s *Server, w http.ResponseWriter) {
			s.apiUPnPServerAdd(w, jsonRequest(http.MethodPost, "/api/upnp/servers", `{"udn":"`+udn+`","name":"Chord"}`))
		}},
		{"remove", func(s *Server, w http.ResponseWriter) {
			s.apiUPnPServerRemove(w, httptest.NewRequest(http.MethodDelete, "/api/upnp/servers?udn="+udn, nil))
		}},
		{"update", func(s *Server, w http.ResponseWriter) {
			s.apiUPnPServerUpdate(w, jsonRequest(http.MethodPatch, "/api/upnp/servers?udn="+udn, `{"name":"Chord"}`))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := loggingtest.Record(t)
			s := newTestUPnPHandler(t, &stubUPnPProvider{addErr: refusal, removeErr: refusal, updateErr: refusal})
			rec := httptest.NewRecorder()
			tc.call(s, rec)
			var body saveRefusalBody
			decodeBody(t, rec, &body)
			requireSaveRefusal(t, logs, rec.Code, body, root, "try again")
		})
	}
}

// jsonRequest is a request carrying body as JSON.
func jsonRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// decodeBody decodes rec's JSON body into out.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("the response is not JSON: %v: %s", err, rec.Body.String())
	}
}
