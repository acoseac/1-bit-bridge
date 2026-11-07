package admin

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestTheSettingsSaveStripsControlCharactersFromTheName is B744's item 2 at
// the console's settings save: a name holding control characters (C0, DEL
// and C1, read as runes) is stored without them, live and in the file alike,
// so the name served now is the name served after a restart; a name that is
// nothing but controls is refused as a blank one is (400 `validate`), nothing
// written; and runes whose second byte is a C1 number are kept.
func TestTheSettingsSaveStripsControlCharactersFromTheName(t *testing.T) {
	const nel, csi, bel, del = "\xc2\x85", "\xc2\x9b", "\x07", "\x7f"
	for _, tc := range []struct{ label, typed, stored string }{
		{"a C1 control inside", "Living" + nel + "Room", "LivingRoom"},
		{"C0, DEL and C1 controls around it", bel + "Kitchen" + csi + del, "Kitchen"},
		{"controls between spaces at its ends", " " + nel + " Den " + bel + " ", "Den"},
		{"runes whose second byte is a C1 number", "Ā Šoštarić", "Ā Šoštarić"},
		// The cap counts the name stored: 260 runes sent, 250 once the
		// controls are gone. The save's Normalize strips controls too, so
		// this row and the refusal below are what the typed strip decides.
		{"controls past the cap", strings.Repeat("a", 250) + strings.Repeat(bel, 10), strings.Repeat("a", 250)},
	} {
		t.Run(tc.label, func(t *testing.T) {
			srv, _, cfgPath := newTestServer(t)
			var saved settingsPatchResponse
			if code := doJSON(t, srv.Handler(), "PATCH", "/api/settings", map[string]any{"libraryName": tc.typed}, &saved); code != 200 {
				t.Fatalf("PATCH libraryName %q = %d, want 200", tc.typed, code)
			}
			if got := srv.deps.CfgHolder.Load().LibraryName; got != tc.stored {
				t.Errorf("the live name is %q, want %q", got, tc.stored)
			}
			reloaded, err := config.Load(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			if reloaded.LibraryName != srv.deps.CfgHolder.Load().LibraryName {
				t.Errorf("the saved name loads as %q, served live as %q", reloaded.LibraryName, srv.deps.CfgHolder.Load().LibraryName)
			}
		})
	}
	t.Run("nothing but controls", func(t *testing.T) {
		srv, _, cfgPath := newTestServer(t)
		before, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var refused struct{ Error, Message string }
		code := doJSON(t, srv.Handler(), "PATCH", "/api/settings", map[string]any{"libraryName": bel + nel + del}, &refused)
		if code != 400 || refused.Error != "validate" {
			t.Fatalf("PATCH of a name that is nothing but controls = %d %q, want 400 validate", code, refused.Error)
		}
		after, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("a refused PATCH rewrote the config file")
		}
	})
}
