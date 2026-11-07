//go:build linux

package updater

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// standInCapabilities makes the swap read dst's capabilities as carried and
// records each one it sets, failing every set with setErr.
func standInCapabilities(t *testing.T, dst string, carried []byte, setErr error) *[]string {
	t.Helper()
	var sets []string
	prev := capabilityXattr
	capabilityXattr.get = func(path string) ([]byte, error) {
		if path == dst {
			return carried, nil
		}
		return nil, nil
	}
	capabilityXattr.set = func(path string, value []byte) error {
		sets = append(sets, path+"="+string(value))
		return setErr
	}
	t.Cleanup(func() { capabilityXattr = prev })
	return &sets
}

// swapLayout is a binary at dst and a new one to swap in.
func swapLayout(t *testing.T) (dst, newBinary string) {
	t.Helper()
	dir := t.TempDir()
	dst = filepath.Join(dir, "bridge")
	if err := os.WriteFile(dst, []byte("bridge-binary-old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBinary = filepath.Join(t.TempDir(), "bridge-new")
	if err := os.WriteFile(newBinary, []byte("bridge-binary-new"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dst, newBinary
}

// TestTheSwapCarriesTheBinarysFileCapabilities: a binary the operator gave
// capabilities (setcap cap_net_bind_service, for a port below 1024) keeps
// them across an update: the staged binary is given dst's before it is
// renamed into place (backlog B660's second round). Until then the swap
// carried the mode alone, and the new binary's bind was refused.
func TestTheSwapCarriesTheBinarysFileCapabilities(t *testing.T) {
	dst, newBinary := swapLayout(t)
	sets := standInCapabilities(t, dst, []byte("cap_net_bind_service"), nil)
	if err := swapBinary(dst, newBinary, ".bak", nil); err != nil {
		t.Fatal(err)
	}
	if len(*sets) != 1 {
		t.Fatalf("the capabilities set = %q, want dst's set once on the staged binary", *sets)
	}
	path, value, _ := strings.Cut((*sets)[0], "=")
	if filepath.Dir(path) != filepath.Dir(dst) || !strings.HasPrefix(filepath.Base(path), ".bridge-swap-") || value != "cap_net_bind_service" {
		t.Errorf("the capabilities were set as %q, want dst's on the binary staged beside it", (*sets)[0])
	}
	if b, _ := os.ReadFile(dst); string(b) != "bridge-binary-new" {
		t.Errorf("dst = %q after the swap, want the new binary", b)
	}
}

// TestASwapThatCannotSetTheCapabilitiesStillSwaps: only a process with
// CAP_SETFCAP may set a file's capabilities, which the service user's
// auto-installer has not: the swap goes on without them, and serve's bind
// check reads the difference from the backup (cmd/bridge).
func TestASwapThatCannotSetTheCapabilitiesStillSwaps(t *testing.T) {
	dst, newBinary := swapLayout(t)
	sets := standInCapabilities(t, dst, []byte("cap_net_bind_service"), syscall.EPERM)
	if err := swapBinary(dst, newBinary, ".bak", nil); err != nil {
		t.Fatalf("a swap whose capabilities could not be set = %v, want it done", err)
	}
	if len(*sets) != 1 {
		t.Errorf("the capabilities set = %q, want the one try", *sets)
	}
	if b, _ := os.ReadFile(dst); string(b) != "bridge-binary-new" {
		t.Errorf("dst = %q after the swap, want the new binary", b)
	}
}

// TestASwapOfABinaryWithNoCapabilitiesSetsNone: a binary that carries none
// gives the new one none.
func TestASwapOfABinaryWithNoCapabilitiesSetsNone(t *testing.T) {
	dst, newBinary := swapLayout(t)
	sets := standInCapabilities(t, dst, nil, errors.New("no set was asked for"))
	if err := swapBinary(dst, newBinary, ".bak", nil); err != nil {
		t.Fatal(err)
	}
	if len(*sets) != 0 {
		t.Errorf("the capabilities set = %q, want none", *sets)
	}
}
