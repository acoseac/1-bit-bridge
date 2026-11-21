package admin

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
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

// recordBootRead tells the console's holder the config file at cfgPath as
// serve's boot read leaves it (config.LoadFileAt, RecordFile: the file the
// name opens, and the bytes it holds), and returns those bytes.
func recordBootRead(t *testing.T, srv *Server, cfgPath string) []byte {
	t.Helper()
	_, raw, file, err := config.LoadFileAt(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv.deps.CfgHolder.RecordFile(file, raw)
	return raw
}

// editConfigBesideTheConsole tells the console's holder the config file as
// serve read it at boot (RecordFile), then changes the file as an edit, a
// library add beside a serve that had not started yet, or an init does, and
// returns what that writer left.
func editConfigBesideTheConsole(t *testing.T, srv *Server, cfgPath string) []byte {
	t.Helper()
	raw := recordBootRead(t, srv, cfgPath)
	edited := bytes.Replace(raw, []byte("Test Library"), []byte("Edited Library"), 1)
	if bytes.Equal(edited, raw) {
		t.Fatal("the fixture's config holds no library name to edit")
	}
	if err := os.WriteFile(cfgPath, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	return edited
}

// rootChangeCase is one root change from the console that changes the
// library index before its save: arrange lays out the library and returns the
// request to send and the track whose rendition the change would unlink.
type rootChangeCase struct {
	name    string
	arrange func(t *testing.T, srv *Server, dataDir string) (method string, body map[string]string, source string)
}

// requireTheRootChangeRefused sends the root change tc arranges, which
// another writer's change to the config refuses, and fails the test unless it
// is refused with the library left as it was: the rendition on disk, no dates
// saved for a flip, the config as the other writer left it (wantConfig) and
// the scanner's roots as they were.
func requireTheRootChangeRefused(t *testing.T, srv *Server, dataDir, cfgPath, method string, body map[string]string,
	sidecar string, roots []string, wantConfig func() []byte) {
	t.Helper()
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
	if got, want := readFileOrNil(cfgPath), wantConfig(); !bytes.Equal(got, want) {
		t.Errorf("the refused root change wrote the config:\n%s", got)
	}
	if got := srv.deps.Scanner.Roots(); !slices.Equal(got, roots) {
		t.Errorf("the refused root change moved the scanner's roots to %q, want %q", got, roots)
	}
}

// readFileOrNil is the file at path, or nil when it cannot be read.
func readFileOrNil(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

// editRootsBesideTheConsole tells the console's holder the config file as
// serve's boot read leaves it, then adds a root to the file through
// config.Edit, as `bridge library add` beside a serve that had not started yet
// does, and returns what that writer left.
func editRootsBesideTheConsole(t *testing.T, srv *Server, cfgPath, dataDir string) []byte {
	t.Helper()
	recordBootRead(t, srv, cfgPath)
	other := filepath.Join(filepath.Dir(dataDir), "Other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.Edit(cfgPath, func(c *config.Config) (bool, error) {
		c.LibraryRoots = append(c.LibraryRoots, other)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	return readFileOrNil(cfgPath)
}

// holdRootsInTheConsole points the scanner, the console's holder and its
// config file at roots, the file through the holder's own save, as a serve
// that booted on a config listing them is.
func holdRootsInTheConsole(t *testing.T, srv *Server, roots ...string) {
	t.Helper()
	srv.deps.Scanner.SetRoots(roots)
	if err := srv.deps.CfgHolder.Update(srv.deps.CfgPath, func(c *config.Config) error {
		c.LibraryRoots = append([]string(nil), roots...)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestARootChangeTheSaveRefusesLeavesTheLibraryAlone: a root added or
// removed from the console whose save is refused, because another writer
// changed the root list in bridge.yaml since serve read it, leaves the library
// index as it was: the change sets its root list on the clone before it asks
// the turn's check, which refuses a change of a setting another writer
// changed, before the library index change the root list needs (the wipe a
// library going from one root to more takes, a removed root's prefix delete,
// the wipe of a collapse to one root). Until B506's second round that change
// ran first, and a refused save had already unlinked the renditions it
// covered, and did it again on every retry while the refusal lasted, which is
// until a restart.
func TestARootChangeTheSaveRefusesLeavesTheLibraryAlone(t *testing.T) {
	for _, tc := range rootChangeCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, dataDir, cfgPath, _, _, _ := seedSingleRootTrack(t)
			method, body, source := tc.arrange(t, srv, dataDir)
			sidecar := seedRendition(t, srv, dataDir, source)
			roots := append([]string(nil), srv.deps.Scanner.Roots()...)
			edited := editRootsBesideTheConsole(t, srv, cfgPath, dataDir)
			requireTheRootChangeRefused(t, srv, dataDir, cfgPath, method, body, sidecar, roots, func() []byte { return edited })
		})
	}
}

// TestARootAddedBesideAnotherWritersChangeKeepsIt: a root added from the
// console over a config file another writer changed since serve read it
// (the library name) lands, its library index change made once, and the file
// holds the new root list and that writer's name, which the bridge loads at
// its next start. Until B535 the save was refused, as every save over a
// changed file was, until a restart.
func TestARootAddedBesideAnotherWritersChangeKeepsIt(t *testing.T) {
	srv, dataDir, cfgPath, _, _, _ := seedSingleRootTrack(t)
	music := srv.deps.Scanner.Roots()[0]
	extra := emptyExtraRoot(t, dataDir)
	editConfigBesideTheConsole(t, srv, cfgPath)
	var wipes atomic.Int32
	manifest.SetRootFlipStageHookForTest(func(stage string) {
		if stage == "wipe" {
			wipes.Add(1)
		}
	})
	t.Cleanup(func() { manifest.SetRootFlipStageHookForTest(nil) })
	if code := doJSON(t, srv.Handler(), http.MethodPost, "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusCreated {
		t.Errorf("a root add beside another writer's change answered %d, want 201", code)
	}
	srvBgScansWait(t, srv)
	if n := wipes.Load(); n != 1 {
		t.Errorf("the add's library index change ran %d times, want once", n)
	}
	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(after.LibraryRoots, []string{music, extra}) || after.LibraryName != "Edited Library" {
		t.Errorf("the file loads roots %q and name %q, want the added root and the other writer's name", after.LibraryRoots, after.LibraryName)
	}
}

// TestARootAddWhoseFileTheNextStartWouldRefuseWipesNothing: another writer
// sets upscale.variantsDir below a folder the console then adds as a root.
// Neither changes a setting the other changed, and together they make a file
// the next start refuses (a variants folder under a library root), so the add
// is refused before its wipe, at the check it asks just before it, which
// loads the file the save would write: the rendition the wipe would unlink is
// kept, and the roots and the file are as they were.
func TestARootAddWhoseFileTheNextStartWouldRefuseWipesNothing(t *testing.T) {
	srv, dataDir, cfgPath, _, _, _ := seedSingleRootTrack(t)
	sidecar := seedRendition(t, srv, dataDir, singleRootSong)
	roots := append([]string(nil), srv.deps.Scanner.Roots()...)
	extra := emptyExtraRoot(t, dataDir)
	recordBootRead(t, srv, cfgPath)
	if err := config.Edit(cfgPath, func(c *config.Config) (bool, error) {
		c.Upscale.VariantsDir = filepath.Join(extra, "variants")
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	edited := readFileOrNil(cfgPath)
	requireTheRootChangeRefused(t, srv, dataDir, cfgPath, http.MethodPost, map[string]string{"path": extra}, sidecar, roots,
		func() []byte { return edited })
}

// TestAThirdRootWhoseFileTheNextStartWouldRefuseIsRefused: a root added to a
// library of two runs no library index change, so it asks no check before
// one; the file its save would write is loaded as the next start loads it
// once its change is made, before the save, and the add is refused there with
// nothing written: another writer's variants folder below the folder it adds
// would have made a file the next start refuses.
func TestAThirdRootWhoseFileTheNextStartWouldRefuseIsRefused(t *testing.T) {
	srv, cfg, cfgPath := newTestServer(t)
	parent := filepath.Dir(cfg.DataDir)
	jazz, third := filepath.Join(parent, "Jazz"), filepath.Join(parent, "Third")
	for _, dir := range []string{jazz, third} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	roots := []string{cfg.LibraryRoots[0], jazz}
	holdRootsInTheConsole(t, srv, roots...)
	recordBootRead(t, srv, cfgPath)
	if err := config.Edit(cfgPath, func(c *config.Config) (bool, error) {
		c.Upscale.VariantsDir = filepath.Join(third, "variants")
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	edited := readFileOrNil(cfgPath)
	if code := doJSON(t, srv.Handler(), http.MethodPost, "/api/roots", map[string]string{"path": third}, nil); code != http.StatusInternalServerError {
		t.Errorf("a third root whose file the next start would refuse answered %d, want 500", code)
	}
	srvBgScansWait(t, srv)
	if got := readFileOrNil(cfgPath); !bytes.Equal(got, edited) {
		t.Errorf("the refused add wrote the config:\n%s", got)
	}
	if got := srv.deps.Scanner.Roots(); !slices.Equal(got, roots) {
		t.Errorf("the refused add moved the scanner's roots to %q, want %q", got, roots)
	}
	if got := srv.deps.CfgHolder.Load().LibraryRoots; !slices.Equal(got, roots) {
		t.Errorf("the refused add stored the roots %q", got)
	}
}

// TestARootChangeRefusedJustBeforeItsIndexChangeLeavesTheLibraryAlone: the
// console's root change compares the config file once more, just before its
// library index change starts, with the file its save's turn found, so a
// config an edit (or a bridge older than the lock, which no lock keeps out)
// changed after the turn read it refuses the change before anything is
// unlinked. Until B506's third round nothing compared between the turn's read
// and the save's staging, so such an edit was refused only once the wipe or
// the prune had unlinked the renditions; that is left to an edit landing
// during the index change itself or the staging.
func TestARootChangeRefusedJustBeforeItsIndexChangeLeavesTheLibraryAlone(t *testing.T) {
	for _, tc := range rootChangeCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, dataDir, cfgPath, _, _, _ := seedSingleRootTrack(t)
			method, body, source := tc.arrange(t, srv, dataDir)
			sidecar := seedRendition(t, srv, dataDir, source)
			roots := append([]string(nil), srv.deps.Scanner.Roots()...)
			raw := recordBootRead(t, srv, cfgPath)
			edited := bytes.Replace(raw, []byte("Test Library"), []byte("Edited Library"), 1)
			stop := config.OnTurnTakenForTest(cfgPath, func() {
				if err := os.WriteFile(cfgPath, edited, 0o600); err != nil {
					t.Error(err)
				}
			})
			defer stop()
			requireTheRootChangeRefused(t, srv, dataDir, cfgPath, method, body, sidecar, roots, func() []byte { return edited })
		})
	}
}

// rootChangeCases are the root changes that change the library index before
// their save: an add that takes the library from one root to two, a remove
// that leaves two, and a remove that collapses it to one. The holder and the
// file hold the roots the scanner is at (holdRootsInTheConsole), so each
// change changes the root list the holder compares it with.
func rootChangeCases() []rootChangeCase {
	return []rootChangeCase{
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
				holdRootsInTheConsole(t, srv, music, jazz, spoken)
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
				holdRootsInTheConsole(t, srv, music, extra)
				const source = "Music/Artist/Album/song.flac"
				if err := srv.deps.Manifest.UpsertTrack(t.Context(), &manifest.Track{Path: source, Size: 14, ModTime: time.Unix(1, 0)}); err != nil {
					t.Fatal(err)
				}
				return http.MethodDelete, map[string]string{"path": extra}, source
			}},
	}
}
