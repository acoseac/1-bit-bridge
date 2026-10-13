package analyze

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsdtone"
)

// TestAMalformedDSFHeaderIsNotTheAnalysisReference: a fmt sample count
// that overstates the data chunk is not the duration analysis compares
// the decode with. The caller keeps ffprobe.
func TestAMalformedDSFHeaderIsNotTheAnalysisReference(t *testing.T) {
	const rate = 2822400
	samples := int64(rate)
	declared := samples + int64(8*(4096*2+1))
	packed := bytes.Repeat([]byte{0x69}, int(samples/8))
	path := filepath.Join(t.TempDir(), "bad.dsf")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = dsdtone.WriteDSF(f, rate, 2, packed, declared)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := dsfContainerDuration(path); ok {
		t.Fatalf("analysis took the header duration %.3fs", d)
	}
}
