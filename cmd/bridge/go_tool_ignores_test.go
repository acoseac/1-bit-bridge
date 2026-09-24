package main

import "strings"

// goToolIgnores reports whether the go tool ignores a file of this name: one
// beginning with "." or "_" (`go help packages`). A test that sweeps this
// package's source skips such a file BEFORE opening it, for two reasons.
//
// It is not the package. The compiler never reads it, so a test defined in
// `_x_test.go` never runs and code in `.x.go` builds into nothing; a sweep
// that counted either would answer about files no build contains.
//
// And an editor creates such files in place. Emacs locks a file it is
// editing with `.#<name>` beside it: a DANGLING symlink holding
// `user@host.pid:boot` where it can, and a REGULAR file holding the same
// string where it cannot, which is always on Windows (emacs's filelock.c).
// The symlink fails os.ReadFile and the regular file fails
// parser.ParseFile, so neither a file-type check nor tolerating ENOENT
// covers both. Deciding from the name does.
func goToolIgnores(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}
