package dsdstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// schemaOneWithAnEntry is a schema-1 file an earlier build wrote: one DAC
// model's choice and acknowledgement, no maker named.
const schemaOneWithAnEntry = `{"schemaVersion":1,"defaultMode":"auto","dacs":[{"key":{"vendorId":"20b1","productId":"3009","interface":1,"product":"GO bar"},` +
	`"choice":{"mode":"native","maxBitRate":0,"revision":1,"setAt":"2026-11-05T10:00:00Z","setBy":"app"},"dopConfirmed":null,` +
	`"firstUse":[{"transport":"native","bcdDevice":"0123","acknowledgedAt":"2026-11-05T10:00:00Z"}],"listener":[],"evidence":[]}],"migratedFrom":null,"warnings":[]}` + "\n"

// TestASchemaOneFileHoldingWhatSchemaOneHasNotDoesNotRead: schema 1's reader
// refuses what its writer did not write, so a file that says schema 1 and
// holds a stamp on an entry, or a schema_1 marker, is no state of schema 1:
// Decode refuses it, and the store keeps it aside and refuses DSD, as it does
// any file that does not read; the same file without them reads.
func TestASchemaOneFileHoldingWhatSchemaOneHasNotDoesNotRead(t *testing.T) {
	durable(t)
	if _, err := Decode([]byte(schemaOneWithAnEntry)); err != nil {
		t.Fatalf("the schema-1 file does not read: %v", err)
	}
	for name, file := range map[string]string{
		"a stamp": strings.Replace(schemaOneWithAnEntry, `"product":"GO bar"},`,
			`"product":"GO bar"},"stamp":"s1-c382419e7cb5a229735d6ed02a2113d164f6eb67ab60c5dcdf6e0ead0c50840d",`, 1),
		"a schema_1 marker": strings.Replace(schemaOneWithAnEntry, `"migratedFrom":null`, `"migratedFrom":{"from":"schema_1"}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if file == schemaOneWithAnEntry {
				t.Fatal("the case's file is the schema-1 file as it was")
			}
			if _, err := Decode([]byte(file)); err == nil {
				t.Fatalf("a schema-1 file holding %s read", name)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, FileName), []byte(file), 0o600); err != nil {
				t.Fatal(err)
			}
			s := openAt(t, dir, options{})
			if r := s.Report(); r.Outcome != Damaged || r.Aside == "" || !s.State().Unreadable() {
				t.Fatalf("the store opens it as %+v (unreadable %v), want it kept aside and DSD refused", r, s.State().Unreadable())
			}
		})
	}
}
