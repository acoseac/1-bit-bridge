package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"
)

// TestErrorWithoutPathKeepsTheOperationAndTheCause pins the one reading the
// scanner's lines and the disk probes give a file error: an *fs.PathError,
// wrapped or not, by its operation and the error under it and never its
// path; anything else as it says itself; and no panic for a PathError with
// no cause or a nil error, which the walk's callback, under no recover, may
// hand it.
func TestErrorWithoutPathKeepsTheOperationAndTheCause(t *testing.T) {
	const abs = "/Volumes/Private Library/Artist/01.flac"
	pe := &fs.PathError{Op: "open", Path: abs, Err: fs.ErrPermission}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"a PathError", pe, "open: " + fs.ErrPermission.Error()},
		{"a wrapped PathError", fmt.Errorf("walk: %w", pe), "open: " + fs.ErrPermission.Error()},
		{"a PathError with no cause", &fs.PathError{Op: "stat", Path: abs}, "stat: <nil>"},
		{"another error", errors.New("statfs: input/output error"), "statfs: input/output error"},
		{"no error", nil, "<nil>"},
	}
	for _, tc := range cases {
		got := ErrorWithoutPath(tc.err)
		if got != tc.want {
			t.Errorf("%s: ErrorWithoutPath = %q, want %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, abs) {
			t.Errorf("%s: %q names the path", tc.name, got)
		}
	}
}
