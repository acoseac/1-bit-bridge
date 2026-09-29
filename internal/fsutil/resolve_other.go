//go:build !windows

package fsutil

import "path/filepath"

// resolveLinks is ResolveLinks on every platform but Windows, where nothing
// but a symbolic link is a link: filepath.EvalSymlinks.
func resolveLinks(p string) (string, error) {
	return filepath.EvalSymlinks(p)
}
