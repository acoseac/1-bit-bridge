//go:build !windows

package admin

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// A console root change takes bridge.yaml's turn, asks the turn's check just
// before its library index change, and saves after it. A link at the
// config's name repointed meanwhile (a dotfiles restow, a home-manager
// switch) names a file serve never read: until B506's fourth round the check
// and the save compared the bytes of the file the turn had locked and never
// the name, so the change ran and saved a file the name no longer opened.
// Each test repoints the link to a twin holding the same bytes, so that only
// the name's resolution tells the two apart.

// linkTheConsolesConfig moves the console's config to a file of its own, A,
// makes the console's config name a link to it, puts a twin holding A's
// bytes beside it, B, and tells the holder the boot read of A. It returns the
// link, A, B and those bytes.
func linkTheConsolesConfig(t *testing.T, srv *Server, cfgPath string) (link, a, b string, raw []byte) {
	t.Helper()
	store := filepath.Join(filepath.Dir(cfgPath), "store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	a, b = filepath.Join(store, "actual.yaml"), filepath.Join(store, "switched.yaml")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link = filepath.Join(filepath.Dir(cfgPath), "alias.yaml")
	if err := os.Symlink(a, link); err != nil {
		t.Fatal(err)
	}
	srv.deps.CfgPath = link
	recordBootRead(t, srv, link)
	return link, a, b, raw
}

// repointLink makes link lead to to, in one rename, as a restow replaces it.
func repointLink(t *testing.T, link, to string) {
	t.Helper()
	tmp := link + ".repoint"
	if err := os.Symlink(to, tmp); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Error(err)
	}
}

// requireBothAsTheyWere fails the test unless a and b still hold raw and link
// still leads to b.
func requireBothAsTheyWere(t *testing.T, raw []byte, link, a, b string) {
	t.Helper()
	for _, p := range []string{a, b} {
		if got := readFileOrNil(p); !bytes.Equal(got, raw) {
			t.Errorf("the root change wrote %s:\n%s", filepath.Base(p), got)
		}
	}
	if to, err := os.Readlink(link); err != nil || to != b {
		t.Errorf("%s leads to %q (%v), want %s", link, to, err, b)
	}
}

// TestARootChangeRefusesANameRepointedInItsTurn: a link at the config's name
// repointed once the root change's turn has read the file, before its check,
// refuses the change at that check: the library index is left as it was (the
// rendition on disk, no dates saved for a flip), and neither file is written.
func TestARootChangeRefusesANameRepointedInItsTurn(t *testing.T) {
	for _, tc := range rootChangeCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, dataDir, cfgPath, _, _, _ := seedSingleRootTrack(t)
			method, body, source := tc.arrange(t, srv, dataDir)
			sidecar := seedRendition(t, srv, dataDir, source)
			roots := append([]string(nil), srv.deps.Scanner.Roots()...)
			link, a, b, raw := linkTheConsolesConfig(t, srv, cfgPath)
			stop := config.OnTurnTakenForTest(link, func() { repointLink(t, link, b) })
			defer stop()
			requireTheRootChangeRefused(t, srv, dataDir, link, method, body, sidecar, roots, func() []byte { return raw })
			requireBothAsTheyWere(t, raw, link, a, b)
		})
	}
}

// TestARootAddWhoseNameMovesAfterItsWipeIsCompensated: a link repointed after
// the root change's check, during its library index change or while its save
// stages (the window no check before the change can close), is refused at the
// save's rename, after the wipe: the compensating scan brings the wiped rows
// back with the dates the flip saved, and drops those dates, and neither file
// is written.
func TestARootAddWhoseNameMovesAfterItsWipeIsCompensated(t *testing.T) {
	srv, dataDir, cfgPath, _, _, kept := seedSingleRootTrack(t)
	extra := emptyExtraRoot(t, dataDir)
	roots := append([]string(nil), srv.deps.Scanner.Roots()...)
	link, a, b, raw := linkTheConsolesConfig(t, srv, cfgPath)
	stop := config.OnSaveStagedForTest(link, func() { repointLink(t, link, b) })
	defer stop()
	if code := doJSON(t, srv.Handler(), http.MethodPost, "/api/roots", map[string]string{"path": extra}, nil); code != http.StatusInternalServerError {
		t.Errorf("a root add whose name moved while it saved answered %d, want 500", code)
	}
	srvBgScansWait(t, srv)
	requireKeptDate(t, srv, singleRootSong, kept, "the row the wipe took, after the compensating scan")
	requireNoCarry(t, dataDir, "a save refused after its wipe")
	if got := srv.deps.Scanner.Roots(); !slices.Equal(got, roots) {
		t.Errorf("the refused root add moved the scanner's roots to %q, want %q", got, roots)
	}
	requireBothAsTheyWere(t, raw, link, a, b)
}
