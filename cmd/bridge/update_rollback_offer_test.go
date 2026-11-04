package main

import (
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/updater"
	"github.com/acoseac/1-bit-bridge/internal/version"
)

// Backlog B707: the console's Updates panel offered Roll back wherever the
// previous binary's backup stood beside the binary (canRollback), while since
// B692 and B705 the rollback refuses, 409 rollback-refused with its reason,
// once the version installed has opened the data dir: the button showed, and
// then failed. The panel asks the rollback's own check of the install marker
// it reads.

// layOutRollback puts a binary in dir, the previous binary's backup beside it
// where backup says, and a data dir with no install marker in it, and returns
// the binary's path and the data dir.
func layOutRollback(t *testing.T, dir string, backup bool) (bin, dataDir string) {
	t.Helper()
	name := "bridge"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin, dataDir = filepath.Join(dir, "bin", name), filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("the new bridge binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if backup {
		if err := os.WriteFile(bin+".bak", []byte("the previous bridge binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin, dataDir
}

// markerLayout leaves an install marker in a data dir, or none.
type markerLayout func(t *testing.T, dataDir string)

// theMarker lays st as the install marker.
func theMarker(st updater.State) markerLayout {
	return func(t *testing.T, dataDir string) {
		t.Helper()
		if err := updater.SaveState(dataDir, st); err != nil {
			t.Fatal(err)
		}
	}
}

// anUnreadableMarker puts a directory where the install marker goes, which
// LoadState answers with an error on every platform and as root.
func anUnreadableMarker(t *testing.T, dataDir string) {
	t.Helper()
	if err := os.Mkdir(updater.StatePath(dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := updater.LoadState(dataDir); err == nil {
		t.Fatal("a directory where the install marker goes reads as a marker; the case needs one that cannot be read")
	}
}

// noMarker leaves the data dir with no install marker.
func noMarker(*testing.T, string) {}

// TestTheUpdatesPanelOffersRollBackOnlyWhereTheRollbackWouldRun: the status
// the console's Updates panel renders (the adapter's Status, and an install's
// answer, which is a status too) offers Roll back exactly where the console's
// rollback then puts the backup back; where the backup is there and the
// rollback refuses, it says why in the rollback's own words, naming no path;
// with no backup it offers nothing and says nothing.
func TestTheUpdatesPanelOffersRollBackOnlyWhereTheRollbackWouldRun(t *testing.T) {
	now := time.Now().UTC()
	installing := updater.State{Status: "installing", TargetVersion: version.ServerVersion, PreviousVersion: previousVersion,
		AttemptedAt: now.Add(-time.Minute), SwapStarted: true}
	confirmed := installing
	confirmed.Status, confirmed.InstalledAt, confirmed.DataDirOpenedBy = "installed", now, version.ServerVersion
	openedByTheBackupsVersion := installing
	openedByTheBackupsVersion.DataDirOpenedBy = previousVersion
	keptByAClear := updater.State{DataDirOpenedBy: version.ServerVersion, PreviousVersion: previousVersion}
	aHold := updater.State{RejectedVersion: "9.9.9",
		RejectedReason: "9.9.9 refuses this bridge's settings: line 1: a whole number is written with a leading zero"}
	for _, tc := range []struct {
		name    string
		backup  bool
		marker  markerLayout
		offered bool
		reason  bool
	}{
		{"the version installed opened the data dir", true, theMarker(confirmed), false, true},
		{"the record a clear at boot kept beside the backup", true, theMarker(keptByAClear), false, true},
		{"an install marker that cannot be read", true, anUnreadableMarker, false, true},
		{"an install whose version has not opened the data dir", true, theMarker(installing), true, false},
		{"the data dir opened by the backup's own version", true, theMarker(openedByTheBackupsVersion), true, false},
		{"a hold of another version alone", true, theMarker(aHold), true, false},
		{"no install marker", true, noMarker, true, false},
		{"no backup, the version installed opened the data dir", false, theMarker(confirmed), false, false},
		{"no backup and no install marker", false, noMarker, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin, dataDir := layOutRollback(t, dir, tc.backup)
			tc.marker(t, dataDir)
			a := updateInfoAdapter{u: updater.New(updater.Options{}), dataDir: dataDir, binaryPath: bin}
			st := a.Status()
			if st.CanRollback != tc.offered || (st.RollbackRefusal != "") != tc.reason {
				t.Errorf("the panel offers Roll back: %v, saying %q; want offered %v, a reason %v",
					st.CanRollback, st.RollbackRefusal, tc.offered, tc.reason)
			}
			if strings.Contains(st.RollbackRefusal, dir) {
				t.Errorf("the reason %q names a path", st.RollbackRefusal)
			}
			// There is no update, so the install installs nothing and answers
			// the status as it stands.
			answer, err := a.Install(t.Context(), false)
			if !errors.Is(err, admin.ErrUpdateNoUpdate) {
				t.Fatalf("an install with no update = %v, want admin.ErrUpdateNoUpdate", err)
			}
			if answer.CanRollback != st.CanRollback || answer.RollbackRefusal != st.RollbackRefusal {
				t.Errorf("an install's answer offers Roll back: %v, saying %q; the status offers %v, saying %q",
					answer.CanRollback, answer.RollbackRefusal, st.CanRollback, st.RollbackRefusal)
			}
			// What the button sends.
			err = a.Rollback(t.Context(), true)
			if (err == nil) != st.CanRollback {
				t.Errorf("the panel offers Roll back: %v, and the console's rollback answers %v", st.CanRollback, err)
			}
			if st.RollbackRefusal != "" && (!errors.Is(err, admin.ErrUpdateRollbackRefused) || !strings.Contains(err.Error(), st.RollbackRefusal)) {
				t.Errorf("the panel says %q, and the console's rollback answers %v: want its refusal, saying the same", st.RollbackRefusal, err)
			}
		})
	}
}

// rollbackPanel is what the console's Updates panel shows of the rollback, as
// GET /api/updates, and the updates frame of the console's event stream, carry
// it.
type rollbackPanel struct {
	CanRollback     bool   `json:"canRollback"`
	RollbackRefusal string `json:"rollbackRefusal"`
}

func readRollbackPanel(t *testing.T, b *consoleBridge) rollbackPanel {
	t.Helper()
	var p rollbackPanel
	getJSON(t, b.console, b.adminBase+"/api/updates", "", &p)
	return p
}

// startConsoleOverABackup boots this build's serve with its console over a
// binary with the previous binary's backup beside it, and marker as the
// install marker in its data dir (the zero State for none), and returns the
// bridge and the binary's path.
func startConsoleOverABackup(t *testing.T, marker updater.State) (*consoleBridge, string) {
	t.Helper()
	var bin string
	b := startConsoleBridge(t, "", nil, func(o *serveOpts) {
		var dataDir string
		bin, dataDir = layOutRollback(t, filepath.Dir(o.configPath), true)
		o.binaryPath = bin
		if marker != (updater.State{}) {
			theMarker(marker)(t, dataDir)
		}
	})
	return b, bin
}

// settingsRollbackRowRe is the Updates panel's rollback row on the Settings
// page: the label's opening tag, the value's attributes, and the value.
var settingsRollbackRowRe = regexp.MustCompile(`(?s)(<dt[^>]*>)[^<]*</dt>\s*<dd id="update-rollback-refused"([^>]*)>(.*?)</dd>`)

// settingsRollbackRow reads the Settings page's first paint: whether it has
// the rollback row, whether the row shows (its label and its value not
// hidden), the value as text, and whether Roll back is hidden.
func settingsRollbackRow(t *testing.T, b *consoleBridge) (found, shown bool, text string, buttonHidden bool) {
	t.Helper()
	resp, err := b.console.Get(b.adminBase + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings = %d", resp.StatusCode)
	}
	page := string(raw)
	button := regexp.MustCompile(`<button [^>]*id="update-rollback"[^>]*>`).FindString(page)
	if button == "" {
		t.Fatal("the Settings page has no Roll back button")
	}
	buttonHidden = strings.Contains(button, " hidden")
	m := settingsRollbackRowRe.FindStringSubmatch(page)
	if m == nil {
		return false, false, "", buttonHidden
	}
	shown = !strings.Contains(m[1], "hidden") && !strings.Contains(m[2], "hidden")
	text = strings.Join(strings.Fields(html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(m[3], ""))), " ")
	return true, shown, text, buttonHidden
}

// postRollback sends what the console's Roll back sends, and returns the
// answer's status and, for a refusal, its code and message.
func postRollback(t *testing.T, b *consoleBridge) (status int, code, message string) {
	t.Helper()
	resp, err := b.console.Post(b.adminBase+"/api/updates/rollback", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		var body struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("POST /api/updates/rollback = %d: %s", resp.StatusCode, raw)
		}
		return resp.StatusCode, body.Error, body.Message
	}
	return resp.StatusCode, "", ""
}

// TestTheConsoleOffersRollBackOnlyWhereTheRollbackWouldRun boots this build's
// real serve and console. Over its own install, which its start confirms once
// its admission has recorded that it opened the data dir, the console's
// rollback refuses (409 rollback-refused), and the Updates panel, read as the
// page reads it (GET /api/updates, and the Settings page's first paint),
// offers no Roll back and says why in the rollback's words, naming no path.
// Over a backup and no install marker it offers Roll back, and the console's
// rollback puts the backup back.
func TestTheConsoleOffersRollBackOnlyWhereTheRollbackWouldRun(t *testing.T) {
	t.Run("the version installed opened the data dir", func(t *testing.T) {
		b, bin := startConsoleOverABackup(t, updater.State{Status: "installing", TargetVersion: version.ServerVersion,
			PreviousVersion: previousVersion, AttemptedAt: time.Now().UTC().Add(-time.Minute), SwapStarted: true})
		if st, err := updater.LoadState(b.dataDir); err != nil || st.Status != "installed" || st.DataDirOpenedBy != version.ServerVersion {
			t.Fatalf("the marker after the start = %+v (%v), want the install confirmed with the record that %s opened the data dir; stderr=%s",
				st, err, version.ServerVersion, b.stderr.String())
		}
		panel := readRollbackPanel(t, b)
		if panel.CanRollback || panel.RollbackRefusal == "" {
			t.Errorf("GET /api/updates offers Roll back: %v, saying %q; want it not offered, and why", panel.CanRollback, panel.RollbackRefusal)
		}
		if strings.Contains(panel.RollbackRefusal, filepath.Dir(b.dataDir)) {
			t.Errorf("the reason %q names a path", panel.RollbackRefusal)
		}
		found, shown, text, buttonHidden := settingsRollbackRow(t, b)
		if !found || !shown || !strings.HasSuffix(text, panel.RollbackRefusal) || !buttonHidden {
			t.Errorf("the Settings page's rollback row: there %v, shown %v, saying %q; Roll back hidden %v; want the row shown with %q, Roll back hidden",
				found, shown, text, buttonHidden, panel.RollbackRefusal)
		}
		files := watch(t, bin, bin+".bak", updater.StatePath(b.dataDir))
		status, code, message := postRollback(t, b)
		if status != http.StatusConflict || code != "rollback-refused" {
			t.Errorf("POST /api/updates/rollback = %d %s %q, want 409 rollback-refused", status, code, message)
		}
		if panel.RollbackRefusal != "" && !strings.Contains(message, panel.RollbackRefusal) {
			t.Errorf("the panel says %q, and the rollback's refusal %q", panel.RollbackRefusal, message)
		}
		files.requireUnchanged(t)
	})

	t.Run("a backup and no install marker", func(t *testing.T) {
		b, bin := startConsoleOverABackup(t, updater.State{})
		if panel := readRollbackPanel(t, b); !panel.CanRollback || panel.RollbackRefusal != "" {
			t.Errorf("GET /api/updates offers Roll back: %v, saying %q; want it offered, saying nothing", panel.CanRollback, panel.RollbackRefusal)
		}
		if found, shown, text, _ := settingsRollbackRow(t, b); found && (shown || text != "") {
			t.Errorf("the Settings page's rollback row shows %v, saying %q, beside a rollback that would run", shown, text)
		}
		if status, code, message := postRollback(t, b); status != http.StatusNoContent {
			t.Fatalf("POST /api/updates/rollback = %d %s %q, want 204", status, code, message)
		}
		if got, err := os.ReadFile(bin); err != nil || string(got) != "the previous bridge binary" {
			t.Errorf("the binary after the rollback = %q (%v), want the backup put back", got, err)
		}
		if panel := readRollbackPanel(t, b); panel.CanRollback || panel.RollbackRefusal != "" {
			t.Errorf("GET /api/updates after the rollback offers Roll back: %v, saying %q; want nothing, with no backup left",
				panel.CanRollback, panel.RollbackRefusal)
		}
	})
}
