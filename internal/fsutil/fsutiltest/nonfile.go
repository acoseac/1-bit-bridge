//go:build unix

package fsutiltest

import (
	"os"
	"path/filepath"
	"testing"
)

// NonFileCase plants a non-file entry for a bounded refusal test.
type NonFileCase struct {
	Name        string // Short subtest name.
	Description string // Description with articles, for diagnostics or subtest names.
	Kind        string // Kind of the entry after following symlinks.
	// Plant creates the entry at path and returns any FIFOs a blocked read
	// must release with AwaitPastFIFOs before removing the fixture.
	Plant func(t *testing.T, path string) []string
}

// NonFileCases covers direct and linked FIFOs, a linked character device,
// and a directory, without requiring a socket listener or a device node.
func NonFileCases() []NonFileCase {
	return []NonFileCase{
		{"named pipe", "a named pipe", "named pipe", func(t *testing.T, path string) []string {
			t.Helper()
			MakeFIFO(t, path)
			return []string{path}
		}},
		{"link to named pipe", "a link to a named pipe", "named pipe", func(t *testing.T, path string) []string {
			t.Helper()
			pipe := filepath.Join(t.TempDir(), "pipe")
			MakeFIFO(t, pipe)
			if err := os.Symlink(pipe, path); err != nil {
				t.Fatal(err)
			}
			return []string{pipe}
		}},
		{"link to character device", "a link to a character device", "character device", func(t *testing.T, path string) []string {
			t.Helper()
			if err := os.Symlink(os.DevNull, path); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{"directory", "a directory", "directory", func(t *testing.T, path string) []string {
			t.Helper()
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
	}
}
