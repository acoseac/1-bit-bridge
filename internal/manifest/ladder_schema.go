package manifest

import "sort"

// ladderColumn is one column of one table the migration ladder makes: the
// first version whose step leaves it in the store (since) and the last one
// (until, 0 while the top of the ladder still has it). A table is its
// columns. The list is ladder_schema_gen.go's, written by
// TestTheLadderSchemaIsTheLadders from the steps themselves, run one at a
// time over an empty database, and checked against them on every run: a
// step that adds, drops or renames a table or a column fails that test until
// the list is written again (BRIDGE_UPDATE_LADDER_SCHEMA=1).
type ladderColumn struct {
	table, column string
	since, until  int
}

// LadderTop is the version this build's migration ladder ends at: the
// user_version every store it opens is migrated to, and the newest a
// backup's verification takes (backlog B653).
func LadderTop() int { return migrations[len(migrations)-1].version }

// RequiredSchema is what a store at user_version v holds: every table the
// ladder has made by that version and not dropped since, each with the
// columns it has then, sorted by name. SQLite's own tables (sqlite_sequence,
// sqlite_stat*) are no part of it. ok is false for a version the ladder does
// not reach, below 1 or past LadderTop, of which nothing is required: a
// backup's verification refuses such a store (internal/backup, backlog B653).
//
// It names what must be there, never all that may be: a store holding a table
// or a column more (one a rolled-back binary's step added, say) still holds
// every one the version requires.
func RequiredSchema(v int) (tables map[string][]string, ok bool) {
	if v < 1 || v > LadderTop() {
		return nil, false
	}
	tables = make(map[string][]string)
	for _, c := range ladderColumns {
		if c.since > v || (c.until != 0 && c.until < v) {
			continue
		}
		tables[c.table] = append(tables[c.table], c.column)
	}
	for _, cols := range tables {
		sort.Strings(cols)
	}
	return tables, true
}
