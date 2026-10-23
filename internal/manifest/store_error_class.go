package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// sqlitePrimaryNames are the names ErrorClass gives SQLite's primary result
// codes, as sqlite3.h names them, lowercased and without the prefix.
var sqlitePrimaryNames = map[int]string{
	sqlite3.SQLITE_ERROR:    "error",
	sqlite3.SQLITE_PERM:     "perm",
	sqlite3.SQLITE_ABORT:    "abort",
	sqlite3.SQLITE_BUSY:     "busy",
	sqlite3.SQLITE_LOCKED:   "locked",
	sqlite3.SQLITE_NOMEM:    "nomem",
	sqlite3.SQLITE_READONLY: "readonly",
	sqlite3.SQLITE_IOERR:    "ioerr",
	sqlite3.SQLITE_CORRUPT:  "corrupt",
	sqlite3.SQLITE_FULL:     "full",
	sqlite3.SQLITE_CANTOPEN: "cantopen",
	sqlite3.SQLITE_PROTOCOL: "protocol",
	sqlite3.SQLITE_SCHEMA:   "schema",
	sqlite3.SQLITE_TOOBIG:   "toobig",
	sqlite3.SQLITE_MISUSE:   "misuse",
	sqlite3.SQLITE_NOTADB:   "notadb",
}

// ErrorClass names the kind of a store read's error, for a log line that
// must not carry the error's text, which can quote a stored path
// (GetUPnPRouting's does): SQLite's primary result code by its name (busy,
// ioerr, corrupt, …; "sqlite" and the number for one it does not name),
// "deadline" and "canceled" for a context's end, "closed" for a connection
// database/sql closed, and "other" for the rest. It reads nothing of the
// error's text.
func ErrorClass(err error) string {
	var se *sqlite.Error
	switch {
	case err == nil:
		return ""
	case errors.As(err, &se):
		code := se.Code() & 0xff
		if name, ok := sqlitePrimaryNames[code]; ok {
			return name
		}
		return fmt.Sprintf("sqlite %d", code)
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, sql.ErrConnDone):
		return "closed"
	}
	return "other"
}
