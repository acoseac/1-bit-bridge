package admin

import (
	"os"
	"strings"
	"testing"
)

// TestStatsScannerComesBeforeLibraryComposition — the Scanner panel
// carries the "Scan now" button and the live scan status, which is what
// someone opens Stats to check. It sat below the composition bars, the
// enrichment breakdown and the harmonic wheel, i.e. off the first screen
// on most windows.
//
// Pinned by ORDER rather than by position index: a panel added between
// them later is fine, a reordering that buries the scanner again is not.
func TestStatsScannerComesBeforeLibraryComposition(t *testing.T) {
	b, err := os.ReadFile("templates/dashboard.html")
	if err != nil {
		t.Fatalf("read dashboard.html: %v", err)
	}
	page := string(b)
	scanner := strings.Index(page, "<h2>Scanner</h2>")
	composition := strings.Index(page, "<h2>Library composition</h2>")
	if scanner < 0 || composition < 0 {
		t.Fatalf("headings not found (scanner=%d composition=%d) — the page was restructured "+
			"and this test is no longer reading it", scanner, composition)
	}
	if scanner > composition {
		t.Error("the Scanner panel is below Library composition again — it carries the " +
			"scan status and the Scan now button, which is what Stats is opened for")
	}
}
