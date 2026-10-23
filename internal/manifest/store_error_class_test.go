package manifest

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// TestErrorClassNamesAStoreErrorByItsKindAlone: a store error is named by
// its kind and nothing of its text, whatever wraps it (GetUPnPRouting's
// error quotes the stored path): SQLite's primary result code by its name,
// an extended code by its primary (BUSY_SNAPSHOT is busy), a context's end,
// database/sql's closed connection, and other for the rest. The driver's
// errors are real ones (makeDriverErrors).
func TestErrorClassNamesAStoreErrorByItsKindAlone(t *testing.T) {
	e := makeDriverErrors(t)
	const path = "Artist/Album/01 a.flac"
	for _, c := range []struct {
		name string
		err  error
		want string
	}{
		{"SQLITE_BUSY, wrapped with the path", fmt.Errorf("manifest: GetUPnPRouting %q: %w", path, e.busy), "busy"},
		{"SQLITE_BUSY_SNAPSHOT", e.busySnapshot, "busy"},
		{"SQLITE_ERROR", e.notBusy, "error"},
		{"a deadline", fmt.Errorf("read %q: %w", path, context.DeadlineExceeded), "deadline"},
		{"a cancel", context.Canceled, "canceled"},
		{"a closed connection", sql.ErrConnDone, "closed"},
		{"anything else", fmt.Errorf("manifest: GetUPnPRouting %q: sql: database is closed", path), "other"},
		{"no error", nil, ""},
	} {
		got := ErrorClass(c.err)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if strings.Contains(got, "Artist") {
			t.Errorf("%s: the class %q carries the error's text", c.name, got)
		}
	}
}
