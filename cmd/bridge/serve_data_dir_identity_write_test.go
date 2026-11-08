package main

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// B741's round 2: the record and the marker as serve writes and reads them
// (Codex's and the Claude API's reviews of #1441).

// TestServeRefusesARecordThatIsALinkToNothing: a record that is a link to
// nothing (the record kept on the volume that went, a link to it left beside
// the config) is there and cannot be read: serve, both doctors and bridge
// scan refuse it as a record that does not read, and it is left as it is. It
// read as no record until round 2, since the open of a link to nothing
// answers as the absence of a file, so serve made the data dir in the bare
// mount point and renamed a fresh record over the link (Codex on #1441).
func TestServeRefusesARecordThatIsALinkToNothing(t *testing.T) {
	v := newIdentityVolume(t)
	record := dataDirRecordFile(v.cfg)
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, filepath.Join(v.dataDir, "the-record-kept-on-the-volume"), record)
	v.takeAway(t)
	const why = "cannot be read ("
	code, served, stdout, stderr := runServeWith(t, serveOpts{configPath: v.cfg})
	switch {
	case served:
		t.Errorf("serve served over a record that is a link to nothing; stdout=%s", stdout)
	case code != 1:
		t.Errorf("serve exited %d, want 1; stderr=%s", code, stderr)
	case !strings.Contains(stderr, "serve: "+unreadableRecordLead(v.dataDir)) || !strings.Contains(stderr, why):
		t.Errorf("serve's refusal is not the record's; stderr=%s", stderr)
	}
	if fi, err := os.Lstat(record); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the record that is a link to nothing is not as it was (%v)", err)
	}
	if got := treeOf(t, v.mnt); len(got) != 0 {
		t.Errorf("serve made %q in the bare mount point", got)
	}
	said := askBothDoctors(t, v.cfg)
	if c := said.requireLine(t, dataDirCheckSlug, doctor.Fail); !strings.HasPrefix(c.Summary, "bridge serve will not start: "+unreadableRecordLead(v.dataDir)) {
		t.Errorf("the data-dir line says %q, want the record's refusal", c.Summary)
	}
	if code, out := runCLI(t, "scan", "--config", v.cfg); code != 1 || !strings.Contains(out, "scan: "+unreadableRecordLead(v.dataDir)) {
		t.Errorf("bridge scan exited %d, want 1 and the record's refusal:\n%s", code, out)
	}
	if got := treeOf(t, v.mnt); len(got) != 0 {
		t.Errorf("the doctors or bridge scan made %q in the bare mount point", got)
	}
}

// TestServeKeepsTheFoldingOfADataDirHoweverDeep is gap 3 at a depth an
// ordinary path reaches: a data dir 200 folders deep, reached through a link,
// its database's path about 430 bytes, within SQLite's 512 (a deeper data dir
// serve cannot open its database in). Until round 2 the record held how each
// folder on the way of both spellings compared names keyed by the folder's
// whole path, about 95 KB of it here, past the 64 KiB the record's reader
// takes, so serve wrote it without, and the check fell back to the probe,
// which with the volume away answers for the folder left there: a config
// naming the data dir in another case read as another data dir (Codex on
// #1441, whose 495-byte path of 240 folders with no link made 66,186 bytes).
// A short base under /tmp keeps the path within SQLite's bound; the folder
// `Bridge` is looked up in stands in for a volume that folds case.
func TestServeKeepsTheFoldingOfADataDirHoweverDeep(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a link and a path past Windows' MAX_PATH")
	}
	base, err := os.MkdirTemp("/tmp", "b741")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	lib := filepath.Join(base, "M")
	deep := filepath.Join(base, "r")
	for i := 0; i < 200; i++ {
		deep = filepath.Join(deep, "d")
	}
	mkdirs(t, lib, deep)
	link := filepath.Join(base, "l")
	symlinkOrSkip(t, filepath.Join(base, "r"), link)
	viaLink := filepath.Join(link, strings.TrimPrefix(deep, filepath.Join(base, "r")+string(filepath.Separator)))
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, filepath.Join(viaLink, "Bridge"))
	probe := func(dir string) fsutil.CaseFolding {
		if dir == deep || dir == viaLink {
			return fsutil.CaseInsensitive
		}
		return fsutil.FoldingIn(dir)
	}
	stopServed(t, serveWithOpts(t, serveOpts{configPath: cfg, dataDirRecording: dataDirRecording{folding: probe}}))
	if err := os.Rename(filepath.Join(deep, "Bridge"), filepath.Join(base, "v")); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(viaLink, "bridge")
	writeIdentityConfig(t, cfg, lib, configured)
	requireServeRefuses(t, cfg, deep, "the data directory "+configured+" held this bridge's database")
}

// TestADataDirWhoseIdentityDoesNotReadKeepsItsRecordAsItWas: dataDir set on
// purpose to a data dir whose marker does not read (a hand edit, a copy of
// another data dir), which the record does not name, starts as it did, every
// time: serve writes no record over it. Until round 2 the first start wrote a
// record naming it with no identity, and every start after refused it for
// that marker (the Claude API on #1441).
func TestADataDirWhoseIdentityDoesNotReadKeepsItsRecordAsItWas(t *testing.T) {
	v := newIdentityVolume(t)
	other := filepath.Join(v.base, "other")
	mkdirs(t, other)
	touch(t, filepath.Join(other, "bridge.db"), filepath.Join(other, serveLockFileName))
	if err := os.WriteFile(dataDirMarkerFile(other), []byte("not an identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeIdentityConfig(t, v.cfg, v.lib, other)
	record := mustRead(t, dataDirRecordFile(v.cfg))
	for i := 0; i < 2; i++ {
		stopServed(t, bootServe(t, "--config", v.cfg))
		if got := mustRead(t, dataDirRecordFile(v.cfg)); got != record {
			t.Fatalf("start %d wrote the record %q over a data dir whose marker does not read, was %q", i+1, got, record)
		}
	}
}

// TestARecordKeepsItsIdentityWhereTheMarkerCannotBeReadAtItsWrite: a marker
// that read at the check and does not read at the record's write (an EIO on a
// flaky volume) leaves the record as it was, its identity with it. Until
// round 2 the record was written again without it, and with the volume away
// at the next start it read as one written before B741, which takes up
// whatever the bare mount point holds (the Claude API on #1441).
func TestARecordKeepsItsIdentityWhereTheMarkerCannotBeReadAtItsWrite(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	base := t.TempDir()
	dataDir := filepath.Join(base, "data")
	mkdirs(t, dataDir)
	touch(t, filepath.Join(dataDir, "bridge.db"), filepath.Join(dataDir, serveLockFileName))
	if err := os.WriteFile(dataDirMarkerFile(dataDir), []byte("{\"id\": "), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(base, "bridge.yaml")
	writeRecordWithIdentity(t, cfg, dataDir, id)
	record := mustRead(t, dataDirRecordFile(cfg))
	recordServeDataDir(dataDirRecordFile(cfg), dataDir, dataDirRecording{})
	if got := mustRead(t, dataDirRecordFile(cfg)); got != record {
		t.Errorf("serve wrote the record %q where the marker did not read, was %q", got, record)
	}
}

// TestTheMarkerIsReadableByWhoeverReadsTheDatabase: the marker holds no
// secret, and a user who may read the data dir's database may read it, so
// that a command or bridge doctor run as a member of the service's group
// does not take a healthy data dir for one whose identity does not read
// (the Claude API on #1441: it was the owner's alone).
func TestTheMarkerIsReadableByWhoeverReadsTheDatabase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	v := newIdentityVolume(t)
	fi, err := os.Stat(dataDirMarkerFile(v.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o044 != 0o044 || perm&0o022 != 0 {
		t.Errorf("the marker's mode is %v, want readable by its group and others and writable by its owner alone", perm)
	}
}

// TestARestoreKeepsTheDataDirsIdentity: bridge restore writes a snapshot's
// files into the data dir it restores into, each in place, and no snapshot
// carries the marker, so the marker stays and serve starts after the restore
// with the identity it had (the Claude API asked it of #1441).
func TestARestoreKeepsTheDataDirsIdentity(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mkdirs(t, lib)
	dataDir := filepath.Join(base, "data")
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
	stopServed(t, bootServe(t, "--config", cfg))
	id := requireOneIdentity(t, cfg, dataDir)
	snap, err := backup.Snapshot(context.Background(), backup.Sources{
		DataDir: dataDir, ManifestDB: manifest.DefaultDBPath(dataDir), BridgeYAML: cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(snap, "data-dir-id.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the snapshot carries the marker (%v)", err)
	}
	if code, out, errOut := runRestoreYes(t, cfg, snap); code != 0 {
		t.Fatalf("bridge restore exited %d:\n%s%s", code, out, errOut)
	}
	stopServed(t, bootServe(t, "--config", cfg))
	if got := requireOneIdentity(t, cfg, dataDir); got != id {
		t.Errorf("after the restore the data dir's identity is %q, want %q, as it was", got, id)
	}
}
