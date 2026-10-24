package admin

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// seedRendition records a rendition of the track at source, with its sidecar
// file in the data dir, and returns the sidecar's path: what a wipe of the
// library index or a prefix delete unlinks from disk once it commits, which
// no scan puts back.
func seedRendition(t *testing.T, srv *Server, dataDir, source string) string {
	t.Helper()
	sidecar := filepath.Join(dataDir, "variants", filepath.Base(filepath.Dir(source))+"-"+filepath.Base(source))
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sidecar, []byte("a rendition hours of work went into"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := srv.deps.Manifest.UpsertVariant(t.Context(), manifest.VariantRow{
		SourcePath: source, VariantID: "upscaled-v2-176400-24", SidecarPath: sidecar,
		Format: "flac", SampleRate: 176400, BitsPerSample: 24, SizeBytes: 36,
		SourceMTimeNS: 1, SourceSize: 1, SoxSettings: "{}", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return sidecar
}

// editConfigBesideTheConsole tells the console's holder the config file as
// serve read it at boot (RecordFile), then changes the file as an edit, a
// library add beside a serve that had not started yet, or an init does, and
// returns what that writer left.
func editConfigBesideTheConsole(t *testing.T, srv *Server, cfgPath string) []byte {
	t.Helper()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv.deps.CfgHolder.RecordFile(raw)
	edited := bytes.Replace(raw, []byte("Test Library"), []byte("Edited Library"), 1)
	if bytes.Equal(edited, raw) {
		t.Fatal("the fixture's config holds no library name to edit")
	}
	if err := os.WriteFile(cfgPath, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	return edited
}

// TestARootChangeTheSaveRefusesLeavesTheLibraryAlone: a root added or
// removed from the console whose save is refused, because another writer
// changed bridge.yaml since serve read it, leaves the library index as it
// was: the change the root list needs (the wipe a library going from one
// root to more takes, a removed root's prefix delete, the wipe of a
// collapse to one root) runs in the save's turn, after the comparison that
// refuses it. Until B506's second round it ran first, and a refused save
// had already unlinked the renditions it covered, and did it again on every
// retry while the refusal lasted, which is until a restart.
func TestARootChangeTheSaveRefusesLeavesTheLibraryAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arrange lays out the library and returns the request to send and
		// the track whose rendition the change would unlink.
		arrange func(t *testing.T, srv *Server, dataDir string) (method string, body map[string]string, source string)
	}{
		{"an add that takes the library from one root to two",
			func(t *testing.T, srv *Server, dataDir string) (string, map[string]string, string) {
				return http.MethodPost, map[string]string{"path": emptyExtraRoot(t, dataDir)}, singleRootSong
			}},
		{"a remove that leaves two roots",
			func(t *testing.T, srv *Server, dataDir string) (string, map[string]string, string) {
				music := srv.deps.Scanner.Roots()[0]
				parent := filepath.Dir(dataDir)
				jazz, spoken := filepath.Join(parent, "Jazz"), filepath.Join(parent, "Spoken")
				for _, root := range []string{jazz, spoken} {
					writeStubTrack(t, filepath.Join(root, "Artist", "Album"))
				}
				srv.deps.Scanner.SetRoots([]string{music, jazz, spoken})
				const source = "Spoken/Artist/Album/song.flac"
				if err := srv.deps.Manifest.UpsertTrack(t.Context(), &manifest.Track{Path: source, Size: 14, ModTime: time.Unix(1, 0)}); err != nil {
					t.Fatal(err)
				}
				return http.MethodDelete, map[string]string{"path": spoken}, source
			}},
		{"a remove that collapses the library to one root",
			func(t *testing.T, srv *Server, dataDir string) (string, map[string]string, string) {
				music := srv.deps.Scanner.Roots()[0]
				extra := filepath.Join(filepath.Dir(dataDir), "Extra")
				writeStubTrack(t, filepath.Join(extra, "Artist", "Album"))
				srv.deps.Scanner.SetRoots([]string{music, extra})
				const source = "Music/Artist/Album/song.flac"
				if err := srv.deps.Manifest.UpsertTrack(t.Context(), &manifest.Track{Path: source, Size: 14, ModTime: time.Unix(1, 0)}); err != nil {
					t.Fatal(err)
				}
				return http.MethodDelete, map[string]string{"path": extra}, source
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, dataDir, cfgPath, _, _, _ := seedSingleRootTrack(t)
			method, body, source := tc.arrange(t, srv, dataDir)
			sidecar := seedRendition(t, srv, dataDir, source)
			roots := append([]string(nil), srv.deps.Scanner.Roots()...)
			edited := editConfigBesideTheConsole(t, srv, cfgPath)

			if code := doJSON(t, srv.Handler(), method, "/api/roots", body, nil); code != http.StatusInternalServerError {
				t.Errorf("the root change over a config another writer changed answered %d, want 500", code)
			}
			// The failed change's scan, which redoes the walk the change
			// cancelled, may still be reading the library.
			srvBgScansWait(t, srv)
			if _, err := os.Stat(sidecar); err != nil {
				t.Errorf("the refused root change unlinked the rendition %s: %v", sidecar, err)
			}
			if n := carryCount(t, dataDir); n != 0 {
				t.Errorf("the refused root change recorded %d dates for a flip", n)
			}
			if got, _ := os.ReadFile(cfgPath); !bytes.Equal(got, edited) {
				t.Errorf("the refused root change wrote the config:\n%s", got)
			}
			if got := srv.deps.Scanner.Roots(); !slices.Equal(got, roots) {
				t.Errorf("the refused root change moved the scanner's roots to %q, want %q", got, roots)
			}
		})
	}
}
