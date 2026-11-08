package manifest

import (
	"bytes"
	"database/sql"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

// ladderSchemaUpdateEnv, set to 1, makes TestTheLadderSchemaIsTheLadders
// write ladder_schema_gen.go from the steps rather than compare with it.
const ladderSchemaUpdateEnv = "BRIDGE_UPDATE_LADDER_SCHEMA"

// stepTheLadder runs the migration ladder's steps one at a time over an
// empty database, as migrate runs them, and reads after each the tables and
// their columns the store then holds, by the version the step stamps.
func stepTheLadder(t *testing.T) map[int]map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", dsn.File(filepath.Join(t.TempDir(), "ladder.db"),
		"_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	at := make(map[int]map[string][]string, len(migrations))
	for _, m := range migrations {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatalf("migration %d (%s): %v", m.version, m.name, err)
		}
		if m.post != nil {
			if err := m.post(db); err != nil {
				t.Fatalf("migration %d (%s) post: %v", m.version, m.name, err)
			}
		}
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, m.version)); err != nil {
			t.Fatal(err)
		}
		at[m.version] = schemaOf(t, db)
	}
	return at
}

// schemaOf is every table db holds but SQLite's own, with its columns by
// pragma_table_info, the read a backup's verification makes of a store.
func schemaOf(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]string, len(names))
	for _, n := range names {
		cols, err := db.Query(`SELECT name FROM pragma_table_info(?)`, n)
		if err != nil {
			t.Fatalf("columns of %s: %v", n, err)
		}
		for cols.Next() {
			var c string
			if err := cols.Scan(&c); err != nil {
				t.Fatal(err)
			}
			out[n] = append(out[n], c)
		}
		if err := cols.Close(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(out[n])
	}
	return out
}

// ladderColumnsOf folds the schema at each version into the list
// ladder_schema_gen.go holds: each table's column with the first version
// that has it and the last, 0 where the top still has it. A column that goes
// and comes back would need two entries, which the list cannot say; the
// test refuses one.
func ladderColumnsOf(t *testing.T, at map[int]map[string][]string) []ladderColumn {
	t.Helper()
	versions := make([]int, 0, len(at))
	for v := range at {
		versions = append(versions, v)
	}
	sort.Ints(versions)
	type key struct{ table, column string }
	seen := map[key]*ladderColumn{}
	var order []key
	for i, v := range versions {
		for table, cols := range at[v] {
			for _, c := range cols {
				k := key{table, c}
				lc, ok := seen[k]
				switch {
				case !ok:
					seen[k] = &ladderColumn{table: table, column: c, since: v}
					order = append(order, k)
				case lc.until != 0:
					t.Fatalf("%s.%s is gone after version %d and back at %d: the list cannot say that", table, c, lc.until, v)
				}
			}
		}
		// What the previous version had and this one has not ends there.
		if i > 0 {
			for k, lc := range seen {
				if lc.until == 0 && !has(at[v], k.table, k.column) && has(at[versions[i-1]], k.table, k.column) {
					lc.until = versions[i-1]
				}
			}
		}
	}
	out := make([]ladderColumn, 0, len(order))
	for _, k := range order {
		out = append(out, *seen[k])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].table != out[j].table {
			return out[i].table < out[j].table
		}
		return out[i].column < out[j].column
	})
	return out
}

func has(schema map[string][]string, table, column string) bool {
	for _, c := range schema[table] {
		if c == column {
			return true
		}
	}
	return false
}

// TestTheLadderSchemaIsTheLadders pins ladder_schema_gen.go to the steps:
// the list it holds is what running the ladder one step at a time gives, and
// RequiredSchema answers, at every version, exactly the tables and columns
// the store holds after that version's step, and nothing for a version the
// ladder does not reach. A step that adds, drops or renames a table or a
// column fails it until the list is written again, which
// BRIDGE_UPDATE_LADDER_SCHEMA=1 does (backlog B653: a backup's verification
// requires that version's tables and columns of the store it checks).
func TestTheLadderSchemaIsTheLadders(t *testing.T) {
	at := stepTheLadder(t)
	got := ladderColumnsOf(t, at)
	if os.Getenv(ladderSchemaUpdateEnv) == "1" {
		writeLadderSchema(t, LadderTop(), got)
		return
	}
	if ladderSchemaTop != LadderTop() || !reflect.DeepEqual(got, ladderColumns) {
		t.Fatalf("ladder_schema_gen.go was written at version %d and the ladder ends at %d, or its list is not the steps' "+
			"(%d columns written, %d made): run `%s=1 go test -run TestTheLadderSchemaIsTheLadders ./internal/manifest/` "+
			"and review the diff", ladderSchemaTop, LadderTop(), len(ladderColumns), len(got), ladderSchemaUpdateEnv)
	}
	for v, want := range at {
		required, ok := RequiredSchema(v)
		if !ok {
			t.Fatalf("RequiredSchema(%d) is not ok, and the ladder reaches it", v)
		}
		if !reflect.DeepEqual(required, want) {
			t.Errorf("RequiredSchema(%d) is not what the store holds after that version's step:\n got %v\nwant %v", v, required, want)
		}
	}
	for _, v := range []int{0, -1, LadderTop() + 1} {
		if tables, ok := RequiredSchema(v); ok || tables != nil {
			t.Errorf("RequiredSchema(%d) = %v, %v; a version the ladder does not reach requires nothing and is not ok", v, tables, ok)
		}
	}
}

// writeLadderSchema writes ladder_schema_gen.go for top and cols.
func writeLadderSchema(t *testing.T, top int, cols []ladderColumn) {
	t.Helper()
	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by TestTheLadderSchemaIsTheLadders (%s=1); DO NOT EDIT.\n\n", ladderSchemaUpdateEnv)
	b.WriteString("package manifest\n\n")
	b.WriteString("// ladderSchemaTop is the ladder's top when ladderColumns was written; the\n")
	b.WriteString("// test that writes it fails while it is not LadderTop().\n")
	fmt.Fprintf(&b, "const ladderSchemaTop = %d\n\n", top)
	b.WriteString("// ladderColumns is every column of every table the ladder makes, with the\n")
	b.WriteString("// first and last version that has it (ladderColumn).\n")
	b.WriteString("var ladderColumns = []ladderColumn{\n")
	for _, c := range cols {
		fmt.Fprintf(&b, "\t{%q, %q, %d, %d},\n", c.table, c.column, c.since, c.until)
	}
	b.WriteString("}\n")
	src, err := format.Source(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("ladder_schema_gen.go", src, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote ladder_schema_gen.go: %d columns at version %d", len(cols), top)
}
