package admin

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dupes"
)

// An album's editions (B1023): the album page draws an album whose folders
// hold the same songs one edition at a time, each a folder by the catalog's
// own disc-folder fold. The bar's harness runs the page under node
// (TestAnAlbumsEditionsAreItsFoldersOneShownAtATime and the cases beside
// it); these two hold the console's fold to the catalog's and the picker to
// a finger, on every host.

// discFolderCase is one row of testdata/disc-folder-fold-cases.json: a
// library path, the name of the folder its album is by the catalog's fold,
// and the disc it resolves to untagged. The bar's harness reads the same
// table through the console's fold (TestADiscFolderBelongsToItsEdition).
type discFolderCase struct {
	Path  string `json:"path"`
	Album string `json:"album"`
	Disc  int    `json:"disc"`
}

// TestTheConsoleFoldsDiscFoldersAsTheCatalogDoes holds the table the
// console's fold is checked by to the catalog's own answer: each path,
// untagged, resolves to that disc and to an album named for that folder, so
// the console's port and dupes' mirror of the client read every case alike.
func TestTheConsoleFoldsDiscFoldersAsTheCatalogDoes(t *testing.T) {
	var cases []discFolderCase
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join("testdata", "disc-folder-fold-cases.json"))), &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 10 {
		t.Fatalf("only %d cases read: the table has stopped checking anything", len(cases))
	}
	folds := 0
	for _, c := range cases {
		res := dupes.Resolve(dupes.Row{Path: c.Path})
		if res.Disc != c.Disc || res.Album != c.Album {
			t.Errorf("%s: the catalog resolves disc %d of %q, the table says disc %d of %q", c.Path, res.Disc, res.Album, c.Disc, c.Album)
		}
		if parts := strings.Split(c.Path, "/"); len(parts) > 1 && c.Album != parts[len(parts)-2] {
			folds++
		}
	}
	if folds == 0 || folds == len(cases) {
		t.Errorf("%d of %d cases fold: the table must hold folders that fold and folders that do not", folds, len(cases))
	}
}

// TestTheEditionPickerTakesAFinger pins at the source that each option of an
// album's edition picker is at least 44px square where the pointer is a
// finger, as the album's own controls are
// (TestAnAlbumsControlsTakeAFingerOnATouchScreen).
func TestTheEditionPickerTakesAFinger(t *testing.T) {
	touch := playerCSSBlocks(t, "(pointer: coarse)")
	if !anyRule(touch, ".edition-btn", "min-width", "44px") || !anyRule(touch, ".edition-btn", "min-height", "44px") {
		t.Error("on a touch screen the edition picker's options are not at least 44px square")
	}
}
