package backup

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func TestTheStagedManifestChmodsItsOpenFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot replace this open temporary file; the pathname chmod race is Unix's")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	var hookErr error
	err = stageAndRenameWithHook(context.Background(), filepath.Join(dir, ManifestFile), []byte("{}"), func(staged string) {
		if hookErr = os.Remove(staged); hookErr == nil {
			hookErr = os.Symlink(target, staged)
		}
	})
	if hookErr != nil || err != nil {
		t.Fatalf("replacement %v, staging %v", hookErr, err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode() != before.Mode() {
		t.Fatalf("staging changed the symlink target's mode: %v -> %v", before.Mode(), after.Mode())
	}
}

func TestTheStuckMarkerNeverFollowsAReplacement(t *testing.T) {
	for _, racing := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing link", true: "replacement after removal"}[racing], func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, stuckMarkerName)
			if err := os.Symlink(target, marker); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("this Windows account cannot create a symlink: %v", err)
				}
				t.Fatal(err)
			}
			mark := &stuckMark{marker: marker, ref: dir, check: StuckCheck{Snapshot: "snapshot"}}
			var hookErr error
			if racing {
				mark.beforeCreate = func() { hookErr = os.Symlink(target, marker) }
			}
			mark.writeMarker()
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != "untouched" {
				t.Fatalf("marker changed the symlink target: %q, %v", got, err)
			}
			info, err := os.Lstat(marker)
			if err != nil {
				t.Fatal(err)
			}
			if (info.Mode()&os.ModeSymlink != 0) != racing {
				t.Errorf("marker kind %v; racing replacement %v", info.Mode(), racing)
			}
		})
	}
}

// schemaFaultConnector drives real database/sql query, Scan and Rows.Err paths
// separately. The iteration fault arrives after every required name was read.
type schemaFaultConnector struct {
	collection string
	fault      string
	cancel     context.CancelFunc
}

func (c schemaFaultConnector) Connect(context.Context) (driver.Conn, error) { return c, nil }
func (c schemaFaultConnector) Driver() driver.Driver                        { return schemaFaultDriver{} }

type schemaFaultDriver struct{}

func (schemaFaultDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }
func (c schemaFaultConnector) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (c schemaFaultConnector) Close() error { return nil }
func (c schemaFaultConnector) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}
func (c schemaFaultConnector) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	r := &schemaFaultRows{}
	required, _ := manifest.RequiredSchema(1)
	collection := ""
	switch {
	case query == "PRAGMA quick_check":
		r.values = []driver.Value{"ok"}
	case query == "PRAGMA user_version":
		r.values = []driver.Value{int64(1)}
	case strings.Contains(query, "sqlite_master"):
		collection = "tables"
		for table := range required {
			r.values = append(r.values, table)
		}
	case strings.Contains(query, "pragma_table_info"):
		collection = "columns"
		for _, column := range required[args[0].Value.(string)] {
			r.values = append(r.values, column)
		}
	case strings.HasPrefix(query, "SELECT COUNT"):
		r.values = []driver.Value{int64(0)}
	default:
		return nil, errors.New("unexpected query: " + query)
	}
	if collection == c.collection {
		switch c.fault {
		case "query":
			return nil, errors.New("injected query read error")
		case "scan":
			// NULL cannot scan into a string, unlike ordinary SQLite names.
			r.values = []driver.Value{nil}
		case "iteration":
			r.endErr = errors.New("injected iteration read error after all names")
		case "cancel":
			c.cancel()
			return nil, context.Canceled
		}
	}
	return r, nil
}

type schemaFaultRows struct {
	values []driver.Value
	endErr error
}

func (*schemaFaultRows) Columns() []string { return []string{"name"} }
func (*schemaFaultRows) Close() error      { return nil }
func (r *schemaFaultRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		if r.endErr != nil {
			return r.endErr
		}
		return io.EOF
	}
	dest[0], r.values = r.values[0], r.values[1:]
	return nil
}

func TestSchemaReadErrorsAreNotSchemaVerdicts(t *testing.T) {
	for _, collection := range []string{"tables", "columns"} {
		for _, fault := range []string{"query", "scan", "iteration", "cancel", "none"} {
			t.Run(collection+"/"+fault, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				db := sql.OpenDB(schemaFaultConnector{collection: collection, fault: fault, cancel: cancel})
				defer db.Close()
				got := runChecks(ctx, db, CheckQuick)
				want, reason := StateFailed, ReasonOpenFailed
				if fault == "cancel" {
					want, reason = StateIncomplete, ReasonCancelled
				} else if fault == "none" {
					want, reason = StateVerified, ""
				}
				if got.state != want || reason != "" && !slices.Contains(got.reasons, reason) {
					t.Fatalf("%s/%s: got %s %v, want %s %s", collection, fault, got.state, got.reasons, want, reason)
				}
				if slices.Contains(got.reasons, ReasonSchemaColumn) || slices.Contains(got.reasons, ReasonSchemaTable) {
					t.Fatalf("read error became a schema verdict: %+v", got)
				}
			})
		}
	}
}

func TestOnlyACompletedFailureDisprovesAVerifiedComponent(t *testing.T) {
	for _, tc := range []struct {
		prior, found Verification
		want         bool
	}{
		{Verification{DatabaseState: StateVerified}, Verification{DatabaseState: StateFailed, FilesState: StateVerified}, true},
		{Verification{FilesState: StateVerified}, Verification{DatabaseState: StateUnverified, FilesState: StateFailed}, true},
		{Verification{FilesState: StateVerified}, Verification{DatabaseState: StateFailed, FilesState: StateVerified}, false},
		{Verification{DatabaseState: StateVerified}, Verification{DatabaseState: StateFailed, FilesState: StateIncomplete}, true},
		{Verification{FilesState: StateVerified}, Verification{DatabaseState: StateIncomplete, FilesState: StateFailed}, true},
	} {
		if got := disproves(&tc.prior, tc.found); got != tc.want {
			t.Errorf("prior %+v, found %+v: disproves %v, want %v", tc.prior, tc.found, got, tc.want)
		}
	}
}
