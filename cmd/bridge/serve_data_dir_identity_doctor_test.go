package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// B741's round 2 (Codex on #1441): bridge doctor's player checklist proves the
// library's paths to MPD from the bridge's indexed tracks, and read them from
// the data dir's database whatever the record said of that data dir, so on a
// box whose MPD gate is on it opened, and migrated, a stray bridge.db in the
// bare mount point while the data-dir line said bridge serve will not start
// there. The identity tests set MPD off, which hid it.

// writeMPDConfig writes the config at path serving lib from dataDir with the
// MPD gate on (output.mpd: on), and its loopback route on a port the kernel
// picks.
func writeMPDConfig(t *testing.T, path, lib, dataDir string) {
	t.Helper()
	writeServeConfig(t, path, lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("output:\n    mpd: on\n    loopbackPort: 0\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestTheDoctorsChecklistOpensNoStoreInARefusedDataDir runs bridge doctor's
// player checklist, Linux and the box's MPD stood in for, over gap 2's state
// (a served data dir's volume away, the bare mount point holding a database
// that is not this bridge's): it leaves that database as it was. Where the
// record is removed, so that nothing refuses the data dir, the same checklist
// opens the database, which shows the case reaches the store.
func TestTheDoctorsChecklistOpensNoStoreInARefusedDataDir(t *testing.T) {
	for _, refused := range []bool{true, false} {
		name := "nothing refuses the data dir"
		if refused {
			name = "the record refuses the data dir"
		}
		t.Run(name, func(t *testing.T) {
			v := newIdentityVolume(t)
			v.takeAway(t)
			mkdirs(t, v.dataDir)
			db := filepath.Join(v.dataDir, "bridge.db")
			touch(t, db)
			writeMPDConfig(t, v.cfg, v.lib, v.dataDir)
			if !refused {
				if err := os.Remove(dataDirRecordFile(v.cfg)); err != nil {
					t.Fatal(err)
				}
			}
			before := treeOf(t, v.mnt)
			checks := doctorOutputChecklist(v.cfg, mustLoadConfig(t, v.cfg), linuxBoxSeam(v.lib))(context.Background())
			if len(checks) == 0 {
				t.Fatal("the checklist said nothing: the MPD gate is shut, and nothing reached the store")
			}
			after := treeOf(t, v.mnt)
			switch {
			case refused && !slices.Equal(before, after):
				t.Errorf("the checklist changed what is under the bare mount point:\nbefore %.300q\nafter  %.300q", before, after)
			case !refused:
				if fi, err := os.Stat(db); err != nil || fi.Size() == 0 {
					t.Errorf("the checklist over a data dir nothing refuses did not open its database (%v): the case reaches no store", err)
				}
			}
		})
	}
}
