//go:build !(linux && (amd64 || arm64))

package alsapcm

import (
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// TestOpenOffLinuxIsUnsupported pins the stub: off linux/amd64 and
// linux/arm64, Open refuses with ReasonUnsupported and names the platform.
func TestOpenOffLinuxIsUnsupported(t *testing.T) {
	d, err := Open(Endpoint{Card: 0, Device: 0}, Config{RateHz: 48000, Format: format.S16LE})
	if d != nil || ReasonOf(err) != ReasonUnsupported {
		t.Fatalf("Open = %v, %v; want ReasonUnsupported", d, err)
	}
	if !strings.Contains(err.Error(), "linux/amd64 and linux/arm64") {
		t.Errorf("%q does not say where the backend runs", err)
	}
	if Supported() {
		t.Error("Supported() is true on a build that cannot open")
	}
}
