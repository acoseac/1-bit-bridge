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
	"strconv"
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
	outside := func(kind config.OutsideKind, settings ...string) *config.ChangedOutsideError {
		return &config.ChangedOutsideError{Path: cfgPath, Kind: kind, Settings: settings}
	}
	cases := []struct {
		name   string
		err    error
		remedy string
		// names are the settings the message names, by their yaml paths.
		names []string
		// never is what the message must not say: a file another writer left
		// that would not load is corrected, never restarted over.
		never string
	}{
		{"another writer holds the lock", &config.BusyError{Path: cfgPath, Wait: config.LockWait}, "try again", nil, ""},
		{"the filesystem gave no lock", &config.NoLockManagerError{Path: cfgPath, Wait: config.LockWait,
			Err: &fs.PathError{Op: "flock", Path: filepath.Join(root, ".bridge.yaml.lock"), Err: errors.New("no locks available")}},
			"lock manager", nil, ""},
		{"another writer changed the file", &config.ChangedOutsideError{Path: cfgPath}, "restart the bridge", nil, ""},
		{"another writer changed a setting the save changes", outside(config.OutsideConflict, "libraryName", "scanIntervalSec"),
			"restart the bridge", []string{"libraryName", "scanIntervalSec"}, ""},
		{"another writer changed a deployment setting", outside(config.OutsideDeployment, "deployment.managedSettings"),
			"restart the bridge", []string{"deployment.managedSettings"}, ""},
		{"the file removed", outside(config.OutsideRemoved), "restart the bridge", nil, ""},
		{"the name opened another file", outside(config.OutsideRepointed), "restart the bridge", nil, ""},
		{"the file does not decode", outside(config.OutsideUndecodable), "restart the bridge", nil, ""},
		{"a file the next start would refuse", &config.ChangedOutsideError{Path: cfgPath, Kind: config.OutsideUnloadable,
			Settings: []string{"libraryRoots"}, Err: fmt.Errorf("read %s: upscale.variantsDir: must not be under a library root, libraryRoots[1]", cfgPath)},
			"restart the bridge", []string{"libraryRoots"}, ""},
		{"a file another writer left that would not load", &config.ChangedOutsideError{Path: cfgPath, Kind: config.OutsideLeftUnloadable,
			Err: fmt.Errorf("read %s: upscale.variantsDir: must not be under a library root, libraryRoots[0]", cfgPath)},
			"correct it", nil, "restart"},
		{"the file changed in the turn", &config.ChangedInTurnError{Path: cfgPath}, "restart the bridge", nil, ""},
		{"the name opens another file", &config.NameMovedError{Path: cfgPath}, "restart the bridge", nil, ""},
		{"the boot read resolved no file", &config.BootUnresolvedError{Path: cfgPath}, "restart the bridge", nil, ""},
		{"the UPnP writer's wrap of a refusal", fmt.Errorf("save bridge.yaml: %w", &config.ChangedOutsideError{Path: cfgPath}), "restart the bridge", nil, ""},
		{"the UPnP writer's wrap of a conflict", fmt.Errorf("save bridge.yaml: %w", outside(config.OutsideConflict, "upnpUpstream.servers")),
			"restart the bridge", []string{"upnpUpstream.servers"}, ""},
		{"a save that failed otherwise", fmt.Errorf("save %s: %w", cfgPath,
			&fs.PathError{Op: "open", Path: filepath.Join(root, ".bridge.yaml.tmp-1"), Err: fs.ErrPermission}), "log", nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := loggingtest.Record(t)
			rec := httptest.NewRecorder()
			writeCfgUpdateErr(rec, tc.err)
			var body saveRefusalBody
			decodeBody(t, rec, &body)
			requireSaveRefusal(t, logs, rec.Code, body, root, tc.remedy)
			for _, setting := range tc.names {
				if !strings.Contains(body.Message, setting) {
					t.Errorf("the refused save's message %q does not name %s", body.Message, setting)
				}
			}
			if tc.never != "" && strings.Contains(body.Message, tc.never) {
				t.Errorf("the refused save's message %q says %q", body.Message, tc.never)
			}
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
	// Each kind of refusal the holder makes once another writer has changed
	// the file serve read (backlog B535), through the real holder: its answer
	// says to restart, or, for a file that writer left that would not load,
	// to correct it and nothing of a restart, names no path, and names the
	// settings it is about by their yaml paths.
	for _, tc := range []struct {
		name string
		// outside is what another writer did once serve read the file.
		outside func(t *testing.T, srv *Server, cfgPath string)
		// method, path and req are the save the console makes.
		method, path string
		req          map[string]any
		names        string
		// remedy is what the answer says to do where it is not to restart,
		// "" for a restart.
		remedy string
	}{
		{"a deployment setting another writer changed", func(t *testing.T, _ *Server, cfgPath string) {
			appendToConfigFile(t, cfgPath, "deployment:\n  managedSettings:\n    - libraryName\n")
		}, http.MethodPatch, "/api/settings", map[string]any{"libraryName": "After"}, "deployment.managedSettings", ""},
		{"the file removed", func(t *testing.T, _ *Server, cfgPath string) {
			if err := os.Remove(cfgPath); err != nil {
				t.Fatal(err)
			}
		}, http.MethodPatch, "/api/settings", map[string]any{"libraryName": "After"}, "", ""},
		{"a file that does not decode", func(t *testing.T, _ *Server, cfgPath string) {
			appendToConfigFile(t, cfgPath, "noSuchSetting: 1\n")
		}, http.MethodPatch, "/api/settings", map[string]any{"libraryName": "After"}, "", ""},
		{"a name that opens another file than serve read", func(t *testing.T, srv *Server, cfgPath string) {
			twin := filepath.Join(filepath.Dir(cfgPath), "twin", "bridge.yaml")
			if err := os.MkdirAll(filepath.Dir(twin), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(twin, readFileOrNil(cfgPath), 0o600); err != nil {
				t.Fatal(err)
			}
			recordBootRead(t, srv, twin)
		}, http.MethodPatch, "/api/settings", map[string]any{"libraryName": "After"}, "", ""},
		{"a file the next start would refuse", func(t *testing.T, _ *Server, cfgPath string) {
			extra := filepath.Join(filepath.Dir(cfgPath), "Extra")
			if err := os.MkdirAll(extra, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := config.Edit(cfgPath, func(c *config.Config) (bool, error) {
				c.LibraryRoots = append(c.LibraryRoots, extra)
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
		}, http.MethodPost, "/api/upscale/variants-dir", nil, "libraryRoots", ""},
		{"another writer's change in the save's own turn", func(t *testing.T, _ *Server, cfgPath string) {
			raw := readFileOrNil(cfgPath)
			stop := config.OnSaveStagedForTest(cfgPath, func() {
				if err := os.WriteFile(cfgPath, append(raw, "# edited in the turn\n"...), 0o600); err != nil {
					t.Error(err)
				}
			})
			t.Cleanup(stop)
		}, http.MethodPatch, "/api/settings", map[string]any{"libraryName": "After"}, "", ""},
		{"a file another writer left that would not load", func(t *testing.T, srv *Server, cfgPath string) {
			root := srv.deps.CfgHolder.Load().LibraryRoots[0]
			appendToConfigFile(t, cfgPath, "upscale:\n  variantsDir: "+strconv.Quote(filepath.Join(root, "variants"))+"\n")
		}, http.MethodPatch, "/api/settings", map[string]any{"libraryName": "After"}, "", "correct it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := loggingtest.Record(t)
			srv, _, cfgPath := newTestServer(t)
			recordBootRead(t, srv, cfgPath)
			tc.outside(t, srv, cfgPath)
			req := tc.req
			if req == nil {
				req = map[string]any{"path": filepath.Join(filepath.Dir(cfgPath), "Extra", "variants")}
			}
			var body saveRefusalBody
			code := doJSON(t, srv.Handler(), tc.method, tc.path, req, &body)
			remedy := "restart the bridge"
			if tc.remedy != "" {
				remedy = tc.remedy
				if strings.Contains(body.Message, "restart") {
					t.Errorf("the refused save's message %q says to restart", body.Message)
				}
			}
			requireSaveRefusal(t, logs, code, body, filepath.Dir(cfgPath), remedy)
			if !strings.Contains(body.Message, tc.names) {
				t.Errorf("the refused save's message %q does not name %s", body.Message, tc.names)
			}
		})
	}
}

// appendToConfigFile appends text to the config file at path, as an edit
// beside the running bridge does.
func appendToConfigFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, append(readFileOrNil(path), text...), 0o600); err != nil {
		t.Fatal(err)
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
