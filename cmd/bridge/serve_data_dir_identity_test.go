package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// Backlog B741: the data dir's record (B651) gains an identity. B651 took a
// data dir its record names for the one serve served from wherever it held
// its database or its lock file, and a lock file a start made in the bare
// mount point before any record was written (one that failed between its lock
// and its database) then read as that data dir once the volume was away (gap
// 2). Serve now keeps an identity, a random id, in a marker in the data dir and
// in the record, and refuses a data dir the record names whose marker is not
// there, holds another identity or does not read, and a record that does not
// read, before it makes or writes anything there; the doctor and every
// command that writes the data dir refuse it in the same words. A record
// written before B741 holds no identity: the first start of this version
// takes up whatever data dir is there. Two spellings of the data dir that
// differ in case alone were compared by a probe of what is there now, which
// answers for the folder above the mount point once the volume is away (gap
// 3): the record keeps how each folder on the data dir's way compared names
// when it was written, and the check compares by that.

// dataDirMarkerFile is the data dir's identity marker: in the data dir.
func dataDirMarkerFile(dataDir string) string { return filepath.Join(dataDir, "data-dir-id.json") }

// identityOf is the identity the marker in dataDir holds, "" for no marker.
func identityOf(t *testing.T, dataDir string) string {
	t.Helper()
	b, err := os.ReadFile(dataDirMarkerFile(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &m); err != nil || m.ID == "" {
		t.Fatalf("the marker in %s does not read: %v (%q)", dataDir, err, b)
	}
	return m.ID
}

// recordedIdentity is the identity the record beside cfg holds, "" for none.
func recordedIdentity(t *testing.T, cfg string) string {
	t.Helper()
	b, err := os.ReadFile(dataDirRecordFile(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("the record beside %s does not read: %v (%q)", cfg, err, b)
	}
	return r.ID
}

// isIdentity reports whether id is an identity as serve mints one: 16 random
// bytes in lower-case hex.
func isIdentity(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// requireOneIdentity fails the test unless the marker in dataDir holds an
// identity as serve mints one and the record beside cfg holds the same, and
// returns it.
func requireOneIdentity(t *testing.T, cfg, dataDir string) string {
	t.Helper()
	id := identityOf(t, dataDir)
	if !isIdentity(id) {
		t.Fatalf("the marker in the data dir holds the identity %q, want one serve mints", id)
	}
	if got := recordedIdentity(t, cfg); got != id {
		t.Errorf("the record beside the config holds the identity %q, the marker in the data dir %q", got, id)
	}
	return id
}

// writeIdentity writes the marker in dataDir holding id, as a serve of B741
// writes it.
func writeIdentity(t *testing.T, dataDir, id string) {
	t.Helper()
	b, err := json.Marshal(struct {
		ID string `json:"id"`
	}{id})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataDirMarkerFile(dataDir), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writePreIdentityRecord writes the record beside cfg naming dataDir as a
// serve before B741 left it: the data dir as its config gave it and, where it
// resolves, where it led (B651's fifth round), and no identity.
func writePreIdentityRecord(t *testing.T, cfg, dataDir string) {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		writeDataDirRecord(t, cfg, dataDir)
		return
	}
	writeDataDirRecordSpellings(t, cfg, dataDir, resolved)
}

// writeIdentityConfig writes a loopback config serving lib from dataDir, as
// writeServeConfig does, with MPD off, so that bridge doctor reads it without
// asking for an MPD, and returns its path.
func writeIdentityConfig(t *testing.T, path, lib, dataDir string) string {
	t.Helper()
	writeServeConfig(t, path, lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("output:\n    mpd: off\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// mkdirs makes each of dirs.
func mkdirs(t *testing.T, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// touch writes an empty file at each of paths: a lock file, or a database
// as SQLite takes an empty file for one.
func touch(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// treeOf is what is under dir, each entry by its path below dir and, for a
// file, its bytes, sorted; nil where dir is not there.
func treeOf(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		entry := filepath.ToSlash(rel)
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			entry += " " + fmt.Sprintf("%q", b)
		}
		out = append(out, entry)
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// identityVolume is an install whose data dir is on a volume mounted at mnt,
// which a serve has served from once.
type identityVolume struct {
	base, lib, mnt, dataDir, cfg string
	// volume is where the volume's data dir is kept while it is away.
	volume string
}

func newIdentityVolume(t *testing.T) identityVolume {
	t.Helper()
	base := t.TempDir()
	v := identityVolume{base: base, lib: filepath.Join(base, "Music"), mnt: filepath.Join(base, "usb"),
		volume: filepath.Join(base, "volume")}
	mkdirs(t, v.lib, v.mnt)
	v.dataDir = filepath.Join(v.mnt, "bridge")
	v.cfg = writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), v.lib, v.dataDir)
	stopServed(t, bootServe(t, "--config", v.cfg))
	return v
}

// takeAway unmounts the volume: its data dir goes with it, and the bare mount
// point stays.
func (v identityVolume) takeAway(t *testing.T) {
	t.Helper()
	if err := os.Rename(v.dataDir, v.volume); err != nil {
		t.Fatal(err)
	}
}

// The first sentence of each refusal B741 added (dataDirGoneError's what), of
// the data dir dataDir as its config gives it.
func noIdentityLine(dataDir, holds string) string {
	return "the data directory " + dataDir + " holds " + holds +
		" but not the identity of the data directory this bridge served from (data-dir-id.json)"
}

func otherIdentityLine(dataDir string) string {
	return "the data directory " + dataDir +
		" holds the identity of another data directory than the one this bridge served from (data-dir-id.json)"
}

func unreadableIdentityLine(dataDir, why string) string {
	return "the data directory " + dataDir + " holds an identity (data-dir-id.json) that " + why
}

// unreadableRecordLead is how the refusal of a record that does not read
// begins, before it names the record where it is.
func unreadableRecordLead(dataDir string) string {
	return "the data directory " + dataDir + " cannot be checked against the one this bridge served from: its record ("
}

// recordToRemove is the path a refusal says to remove: what follows its last
// "remove ", up to " and ", as the text spells it.
func recordToRemove(t *testing.T, text string) string {
	t.Helper()
	i := strings.LastIndex(text, "remove ")
	if i < 0 {
		t.Fatalf("the refusal names nothing to remove: %q", text)
	}
	record, _, ok := strings.Cut(text[i+len("remove "):], " and ")
	if !ok {
		t.Fatalf("the refusal says to remove something and nothing after: %q", text)
	}
	return record
}

// mustRefuseFor fails the test unless text says first, the refusal's first
// sentence, and names the record at record as the one to remove.
func mustRefuseFor(t *testing.T, text, first, record string) {
	t.Helper()
	if !strings.Contains(text, first) {
		t.Errorf("the refusal does not say %q:\n%s", first, text)
		return
	}
	mustNameTheRecord(t, recordToRemove(t, text), record)
}

// requireServeRefuses runs serve over cfg and requires it to refuse, exit 1
// and serve nothing, saying first, naming the record beside cfg as the one to
// remove, and to leave what is under dir as it was.
func requireServeRefuses(t *testing.T, cfg, dir, first string) {
	t.Helper()
	before := treeOf(t, dir)
	code, served, stdout, stderr := runServeWith(t, serveOpts{configPath: cfg})
	switch {
	case served:
		t.Errorf("serve served; stdout=%s stderr=%s", stdout, stderr)
	case code != 1:
		t.Errorf("serve exited %d, want 1; stderr=%s", code, stderr)
	default:
		mustRefuseFor(t, stderr, "serve: "+first, dataDirRecordFile(cfg))
	}
	if after := treeOf(t, dir); !slices.Equal(before, after) {
		t.Errorf("serve changed what is under %s:\nbefore %q\nafter  %q", dir, before, after)
	}
}

// requireBothDoctorsFail requires bridge doctor and the console's doctor to
// fail their data-dir line over cfg, saying bridge serve will not start and
// first, with the record beside cfg as the one to remove in its fix, and to
// leave what is under dir as it was: a data dir serve refuses may hold a
// database that is not this bridge's, which a store open would migrate.
func requireBothDoctorsFail(t *testing.T, cfg, dir, first string) {
	t.Helper()
	before := treeOf(t, dir)
	said := askBothDoctors(t, cfg)
	if after := treeOf(t, dir); !slices.Equal(before, after) {
		t.Errorf("the doctors changed what is under %s:\nbefore %.600q\nafter  %.600q", dir, before, after)
	}
	c := said.requireLine(t, dataDirCheckSlug, doctor.Fail)
	if want := "bridge serve will not start: " + first; !strings.HasPrefix(c.Summary, want) {
		t.Errorf("the data-dir line says %q, want it to start %q", c.Summary, want)
	}
	mustNameTheRecord(t, recordToRemove(t, c.Hint+" and "), dataDirRecordFile(cfg))
	if said.code != 1 {
		t.Errorf("bridge doctor exited %d over a data dir serve refuses, want 1", said.code)
	}
}

// requireScanRefuses runs bridge scan, a command that opens the store, over
// cfg and requires it to refuse, exit 1, in serve's first sentence under its
// own name, and to leave what is under dir, and the record, as they were.
func requireScanRefuses(t *testing.T, cfg, dir, first string) {
	t.Helper()
	before, record := treeOf(t, dir), mustRead(t, dataDirRecordFile(cfg))
	code, out := runCLI(t, "scan", "--config", cfg)
	switch {
	case code != 1:
		t.Errorf("bridge scan exited %d, want 1:\n%s", code, out)
	case !strings.Contains(out, "bridge scan ") || !strings.Contains(out, "makes and writes nothing there"):
		t.Errorf("bridge scan does not say it makes and writes nothing there:\n%s", out)
	default:
		mustRefuseFor(t, out, "scan: "+first, dataDirRecordFile(cfg))
	}
	if after := treeOf(t, dir); !slices.Equal(before, after) {
		t.Errorf("bridge scan changed what is under %s:\nbefore %q\nafter  %q", dir, before, after)
	}
	if got := mustRead(t, dataDirRecordFile(cfg)); got != record {
		t.Errorf("bridge scan changed the record: %q", got)
	}
}

// TestServeKeepsAnIdentityInTheDataDirAndItsRecord: a fresh install's first
// serve mints an identity and keeps it in a marker in the data dir and in the
// record beside the config, one identity in both; the next start keeps it.
func TestServeKeepsAnIdentityInTheDataDirAndItsRecord(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mkdirs(t, lib)
	dataDir := filepath.Join(base, "data")
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
	stopServed(t, bootServe(t, "--config", cfg))
	id := requireOneIdentity(t, cfg, dataDir)
	stopServed(t, bootServe(t, "--config", cfg))
	if again := requireOneIdentity(t, cfg, dataDir); again != id {
		t.Errorf("the next start changed the data dir's identity from %q to %q", id, again)
	}
}

// TestServeRefusesWhatTheBareMountPointHoldsOnceItsRecordHoldsAnIdentity is
// gap 2: a serve over a data dir on a volume, the volume then taken away, and
// the bare mount point holding a lock file (a start that came before any
// record, on the bare mount point, and failed between its lock and its
// database, leaves one), a database (a command before B727 made one), or
// both. serve, bridge doctor, the console's doctor and bridge scan refuse it,
// each saying what it holds and that it is not the data dir this bridge
// served from, and nothing writes there. On main the lock file or the
// database read as the data dir serve served from, and serve made a TLS pair,
// a token store and an empty database beside them.
func TestServeRefusesWhatTheBareMountPointHoldsOnceItsRecordHoldsAnIdentity(t *testing.T) {
	for _, tc := range []struct{ name, holds string }{
		{"a lock file", serveLockFileName},
		{"a database", "bridge.db"},
		{"both", "bridge.db and " + serveLockFileName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newIdentityVolume(t)
			v.takeAway(t)
			mkdirs(t, v.dataDir)
			for _, name := range strings.Split(tc.holds, " and ") {
				touch(t, filepath.Join(v.dataDir, name))
			}
			first := noIdentityLine(v.dataDir, tc.holds)
			requireServeRefuses(t, v.cfg, v.mnt, first)
			requireBothDoctorsFail(t, v.cfg, v.mnt, first)
			requireScanRefuses(t, v.cfg, v.mnt, first)
		})
	}
}

// TestServeRefusesADataDirHoldingAnotherDataDirsIdentity: with the volume a
// serve served from away, another install's data dir (one its own serve
// served from) is mounted at its path. It holds a database and a lock file,
// which B651 took for the data dir serve served from; its identity is
// another's, and serve, both doctors and bridge scan refuse it.
func TestServeRefusesADataDirHoldingAnotherDataDirsIdentity(t *testing.T) {
	v := newIdentityVolume(t)
	v.takeAway(t)
	other := filepath.Join(v.base, "other")
	mkdirs(t, other)
	otherData := filepath.Join(other, "data")
	otherCfg := writeIdentityConfig(t, filepath.Join(other, "bridge.yaml"), v.lib, otherData)
	stopServed(t, bootServe(t, "--config", otherCfg))
	if err := os.Rename(otherData, v.dataDir); err != nil {
		t.Fatal(err)
	}
	first := otherIdentityLine(v.dataDir)
	requireServeRefuses(t, v.cfg, v.mnt, first)
	requireBothDoctorsFail(t, v.cfg, v.mnt, first)
	requireScanRefuses(t, v.cfg, v.mnt, first)

	// The volume it served from, mounted again, is the data dir it served
	// from.
	if err := os.Rename(v.dataDir, otherData); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(v.volume, v.dataDir); err != nil {
		t.Fatal(err)
	}
	back := bootServe(t, "--config", v.cfg)
	if s := back.stderr.String(); strings.Contains(s, "the data directory "+v.dataDir+" holds") {
		t.Errorf("serve refused the data dir it served from once its volume was back; stderr=%s", s)
	}
}

// TestServeRefusesARecordThatDoesNotRead: a record beside the config that
// does not read as one (a torn write, a hand edit) is its own state, never
// the absence of a record, which a fresh install has: serve, both doctors and
// bridge scan refuse it, saying so and to remove it once the data dir is
// there, and leave it as it is. On main it named nothing, so serve started on
// whatever stood at the data dir's path, the bare mount point included, and
// wrote the record again.
func TestServeRefusesARecordThatDoesNotRead(t *testing.T) {
	v := newIdentityVolume(t)
	torn := "{\"dataDir\": "
	if err := os.WriteFile(dataDirRecordFile(v.cfg), []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}
	const why = ") does not read as one"
	code, served, stdout, stderr := runServeWith(t, serveOpts{configPath: v.cfg})
	switch {
	case served:
		t.Errorf("serve served over a record that does not read; stdout=%s", stdout)
	case code != 1:
		t.Errorf("serve exited %d, want 1; stderr=%s", code, stderr)
	case !strings.Contains(stderr, "serve: "+unreadableRecordLead(v.dataDir)) || !strings.Contains(stderr, why):
		t.Errorf("serve's refusal is not the record's; stderr=%s", stderr)
	default:
		mustNameTheRecord(t, recordToRemove(t, stderr), dataDirRecordFile(v.cfg))
	}
	if got := mustRead(t, dataDirRecordFile(v.cfg)); got != torn {
		t.Errorf("serve wrote the record that does not read again: %q", got)
	}

	before := treeOf(t, v.dataDir)
	said := askBothDoctors(t, v.cfg)
	c := said.requireLine(t, dataDirCheckSlug, doctor.Fail)
	if !strings.HasPrefix(c.Summary, "bridge serve will not start: "+unreadableRecordLead(v.dataDir)) || !strings.Contains(c.Summary, why) {
		t.Errorf("the data-dir line says %q, want the record's refusal", c.Summary)
	}
	if after := treeOf(t, v.dataDir); !slices.Equal(before, after) {
		t.Errorf("the doctors changed the data dir:\nbefore %.600q\nafter  %.600q", before, after)
	}

	code, out := runCLI(t, "scan", "--config", v.cfg)
	if code != 1 || !strings.Contains(out, "scan: "+unreadableRecordLead(v.dataDir)) {
		t.Errorf("bridge scan exited %d over a record that does not read, want 1 and the record's refusal:\n%s", code, out)
	}
	if after := treeOf(t, v.dataDir); !slices.Equal(before, after) {
		t.Errorf("bridge scan changed the data dir:\nbefore %q\nafter  %q", before, after)
	}
	if got := mustRead(t, dataDirRecordFile(v.cfg)); got != torn {
		t.Errorf("bridge scan wrote the record: %q", got)
	}
}

// TestServeRefusesAnIdentityThatDoesNotRead: a marker in the data dir that
// does not read as one is its own state, never the absence of a marker:
// serve, both doctors and bridge scan refuse the data dir, and leave the
// marker as it is.
func TestServeRefusesAnIdentityThatDoesNotRead(t *testing.T) {
	v := newIdentityVolume(t)
	if err := os.WriteFile(dataDirMarkerFile(v.dataDir), []byte("not an identity\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := unreadableIdentityLine(v.dataDir, "does not read as one")
	requireServeRefuses(t, v.cfg, v.dataDir, first)
	requireBothDoctorsFail(t, v.cfg, v.dataDir, first)
	requireScanRefuses(t, v.cfg, v.dataDir, first)
}

// TestServeRefusesWhatItCannotRead: a record or a marker that is there and
// that this user may not read is refused as one that does not read, serve
// saying to make it readable by the user the service runs as, and a command,
// which may be run by another user than the service, saying to run it as that
// user; bridge doctor, which may run as another user too, says it is not
// checked, a warn, as it says of a data dir it may not inspect (B659), and
// its store probes, which open nothing there, say this user cannot read it,
// never that serve refuses it (B741's round 2, the Claude API on #1441).
func TestServeRefusesWhatItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a file whose mode refuses this user a read")
	}
	for _, tc := range []struct {
		name string
		file func(v identityVolume) string
		lead func(v identityVolume) string
	}{
		{"the record", func(v identityVolume) string { return dataDirRecordFile(v.cfg) },
			func(v identityVolume) string { return unreadableRecordLead(v.dataDir) }},
		{"the marker", func(v identityVolume) string { return dataDirMarkerFile(v.dataDir) },
			func(v identityVolume) string { return unreadableIdentityLine(v.dataDir, "cannot be read (") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newIdentityVolume(t)
			p := tc.file(v)
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("serve left nothing at %s to make unreadable: %v", p, err)
			}
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
			code, served, stdout, stderr := runServeWith(t, serveOpts{configPath: v.cfg})
			switch {
			case served:
				t.Errorf("serve served over %s it cannot read; stdout=%s", tc.name, stdout)
			case code != 1:
				t.Errorf("serve exited %d, want 1; stderr=%s", code, stderr)
			case !strings.Contains(stderr, "serve: "+tc.lead(v)) || !strings.Contains(stderr, "make it readable by the user the service runs as"):
				t.Errorf("serve's refusal does not say it cannot read %s and to make it readable; stderr=%s", tc.name, stderr)
			}
			said := askBothDoctors(t, v.cfg)
			c := said.requireLine(t, dataDirCheckSlug, doctor.Warn)
			if !strings.HasPrefix(c.Summary, "not checked: "+tc.lead(v)) {
				t.Errorf("the data-dir line says %q, want it not checked, saying %q", c.Summary, tc.lead(v))
			}
			probe, ok := said.cli["sidecar-paths"]
			switch {
			case !ok:
				t.Error("bridge doctor has no sidecar-paths line over a data dir that holds its database")
			case strings.Contains(probe.Hint, "serve refuses") || !strings.Contains(probe.Hint, "this user cannot read"):
				t.Errorf("the sidecar-paths line says %q, want it to say this user cannot read the data dir's record or identity", probe.Hint)
			}
			code, out := runCLI(t, "scan", "--config", v.cfg)
			switch {
			case code != 1 || !strings.Contains(out, "scan: "+tc.lead(v)):
				t.Errorf("bridge scan exited %d, want 1 and the refusal of %s:\n%s", code, tc.name, out)
			case !strings.Contains(out, "run bridge scan as the user the service runs as"):
				t.Errorf("bridge scan's refusal does not say to run it as the user the service runs as:\n%s", out)
			}
		})
	}
}

// TestServeTakesUpTheRecordOfAServeBeforeIt: the upgrade's states, each laid
// out as a serve before B741 left it, with no marker and a record holding no
// identity. Over a data dir holding its database, its lock file or both
// (the lock file alone is the local fixture's reset, rm bridge.db*), the
// first start of this version starts, puts its identity in the data dir and
// in the record. Over nothing at the data dir's path, or an empty directory
// there (its volume away), it refuses as B651 does, making nothing there.
func TestServeTakesUpTheRecordOfAServeBeforeIt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		holds  []string
		absent bool
		serves bool
	}{
		{name: "over its database and its lock file", holds: []string{"bridge.db", serveLockFileName}, serves: true},
		{name: "over its lock file alone", holds: []string{serveLockFileName}, serves: true},
		{name: "over its database alone", holds: []string{"bridge.db"}, serves: true},
		{name: "over an empty directory", holds: nil},
		{name: "over nothing", absent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			lib, mnt := filepath.Join(base, "Music"), filepath.Join(base, "usb")
			mkdirs(t, lib, mnt)
			dataDir := filepath.Join(mnt, "bridge")
			if !tc.absent {
				mkdirs(t, dataDir)
			}
			for _, name := range tc.holds {
				touch(t, filepath.Join(dataDir, name))
			}
			cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
			writePreIdentityRecord(t, cfg, dataDir)
			if !tc.serves {
				requireServeRefuses(t, cfg, mnt, "the data directory "+dataDir+" held this bridge's database")
				return
			}
			stopServed(t, bootServe(t, "--config", cfg))
			requireOneIdentity(t, cfg, dataDir)
			if got := recordedDataDir(t, cfg); got != dataDir {
				t.Errorf("the record names %q, want %q", got, dataDir)
			}
		})
	}
}

// TestServeTakesTheIdentityItsMarkerHolds: a start interrupted after it
// wrote the marker and before the record gained the identity leaves a marker
// and a record without it (or none); the next start takes the marker's
// identity into the record, and keeps the marker as it is.
func TestServeTakesTheIdentityItsMarkerHolds(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	for _, record := range []bool{true, false} {
		name := "no record"
		if record {
			name = "a record without the identity"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			lib := filepath.Join(base, "Music")
			dataDir := filepath.Join(base, "data")
			mkdirs(t, lib, dataDir)
			touch(t, filepath.Join(dataDir, "bridge.db"), filepath.Join(dataDir, serveLockFileName))
			writeIdentity(t, dataDir, id)
			cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
			if record {
				writePreIdentityRecord(t, cfg, dataDir)
			}
			stopServed(t, bootServe(t, "--config", cfg))
			if got := requireOneIdentity(t, cfg, dataDir); got != id {
				t.Errorf("serve made the identity %q of a data dir whose marker holds %q", got, id)
			}
		})
	}
}

// TestADatabaseRemovedOnPurposeKeepsItsDataDirsIdentity: the local
// fixture's reset (rm bridge.db*, the lock file kept) takes nothing of the
// data dir's identity: serve starts, makes its database again, and keeps the
// identity it had.
func TestADatabaseRemovedOnPurposeKeepsItsDataDirsIdentity(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mkdirs(t, lib)
	dataDir := filepath.Join(base, "data")
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
	stopServed(t, bootServe(t, "--config", cfg))
	id := requireOneIdentity(t, cfg, dataDir)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(dataDir, "bridge.db"+suffix)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	stopServed(t, bootServe(t, "--config", cfg))
	if got := requireOneIdentity(t, cfg, dataDir); got != id {
		t.Errorf("a start over a database removed on purpose changed the data dir's identity from %q to %q", id, got)
	}
}

// TestADataDirMovedOnPurposeKeepsItsIdentity: a data dir moved on purpose
// and dataDir set to where it is now starts, its identity with it, and the
// record names it where it is now.
func TestADataDirMovedOnPurposeKeepsItsIdentity(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mkdirs(t, lib)
	from, to := filepath.Join(base, "data"), filepath.Join(base, "moved")
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, from)
	stopServed(t, bootServe(t, "--config", cfg))
	id := requireOneIdentity(t, cfg, from)
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	writeIdentityConfig(t, cfg, lib, to)
	stopServed(t, bootServe(t, "--config", cfg))
	if got := requireOneIdentity(t, cfg, to); got != id {
		t.Errorf("the data dir moved on purpose has the identity %q, want %q, the one it had", got, id)
	}
	if got := recordedDataDir(t, cfg); got != to {
		t.Errorf("the record names %q, want %q, where the data dir is now", got, to)
	}
}

// TestADataDirSetToANewDirectoryOnPurposeStarts: dataDir set on purpose to a
// new directory, empty or not made yet, which the record does not name, is
// refused nothing: serve starts there with an identity of its own, and the
// data dir it served from before keeps its own.
func TestADataDirSetToANewDirectoryOnPurposeStarts(t *testing.T) {
	for _, made := range []bool{true, false} {
		name := "not made yet"
		if made {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			lib := filepath.Join(base, "Music")
			mkdirs(t, lib)
			old, fresh := filepath.Join(base, "data"), filepath.Join(base, "fresh")
			cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, old)
			stopServed(t, bootServe(t, "--config", cfg))
			oldID := requireOneIdentity(t, cfg, old)
			if made {
				mkdirs(t, fresh)
			}
			writeIdentityConfig(t, cfg, lib, fresh)
			stopServed(t, bootServe(t, "--config", cfg))
			freshID := requireOneIdentity(t, cfg, fresh)
			if freshID == oldID {
				t.Errorf("the new data dir took the identity %q of the data dir before it", oldID)
			}
			if got := identityOf(t, old); got != oldID {
				t.Errorf("the data dir served from before holds the identity %q, want %q, as it was", got, oldID)
			}
			if got := recordedDataDir(t, cfg); got != fresh {
				t.Errorf("the record names %q, want %q", got, fresh)
			}
		})
	}
}

// foldingRecordBody is the record of the data dir recorded as a serve of B741
// writes it: both of its spellings (one here), an identity, and how each
// folder on its way compared names when it was written, each case-sensitive
// but the folder folds, which compared them without case.
func foldingRecordBody(t *testing.T, recorded, folds string, insensitive bool) []byte {
	t.Helper()
	var folding []fsutil.CaseFolding
	for child := recorded; ; {
		parent := filepath.Dir(child)
		if parent == child {
			break
		}
		f := fsutil.CaseSensitive
		if insensitive && parent == folds {
			f = fsutil.CaseInsensitive
		}
		folding = append([]fsutil.CaseFolding{f}, folding...)
		child = parent
	}
	letters := fsutil.FormatFolding(folding)
	b, err := json.Marshal(struct {
		DataDir         string `json:"dataDir"`
		Resolved        string `json:"resolved"`
		ID              string `json:"id"`
		DataDirFolding  string `json:"dataDirFolding"`
		ResolvedFolding string `json:"resolvedFolding"`
	}{recorded, recorded, "00112233445566778899aabbccddeeff", letters, letters})
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestServeRefusesTheDataDirItRecordedInAnotherCaseWhereItsFolderFoldsCase
// is gap 3: a record of the data dir as `usb/Bridge`, written while the
// volume at `usb` was mounted, which compared the names in it without case,
// and a config naming `usb/bridge` since. With the volume away, the folder
// left at `usb` is the bare mount point on a volume that keeps case, and a
// probe of it says the two spellings are two directories; the record says how
// the folder compared names when it was written, and serve refuses the data
// dir by that, making nothing there. Where the record says the folder kept
// case (the sibling), the two are two directories, and serve starts on the
// one its config names. On main the probe decided, and serve made the data
// dir in the bare mount point.
func TestServeRefusesTheDataDirItRecordedInAnotherCaseWhereItsFolderFoldsCase(t *testing.T) {
	for _, folds := range []bool{true, false} {
		name := "the folder kept case"
		if folds {
			name = "the folder folded case"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			lib, mnt := filepath.Join(base, "Music"), filepath.Join(base, "usb")
			mkdirs(t, lib, mnt)
			recorded, configured := filepath.Join(mnt, "Bridge"), filepath.Join(mnt, "bridge")
			cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, configured)
			if err := os.WriteFile(dataDirRecordFile(cfg), foldingRecordBody(t, recorded, mnt, folds), 0o600); err != nil {
				t.Fatal(err)
			}
			if folds {
				requireServeRefuses(t, cfg, mnt, "the data directory "+configured+" held this bridge's database")
				return
			}
			_, served, _, stderr := runServeWith(t, serveOpts{configPath: cfg})
			if !served {
				t.Errorf("serve refused a data dir in another case than its record names, in a folder the record says keeps case; stderr=%s", stderr)
			}
		})
	}
}

// refuseSaying runs serve n times over cfg, each a new start of a service
// manager's retries, and returns whether each said line; every one must
// refuse, exit 1 and serve nothing.
func refuseSaying(t *testing.T, cfg, line string, n int) []bool {
	t.Helper()
	var said []bool
	for i := 0; i < n; i++ {
		code, served, stdout, stderr := runServeWith(t, serveOpts{configPath: cfg})
		if served || code != 1 {
			t.Fatalf("start %d: served %v, exit %d, want a refusal; stdout=%s stderr=%s", i+1, served, code, stdout, stderr)
		}
		said = append(said, strings.Contains(stderr, line))
	}
	return said
}

// TestServeSaysItsRefusalAgainWhenItsCauseChanges: the note that serve said
// its refusal is of one state, the data dir and why it was refused, so a
// refusal of the same data dir for another cause is said, once: the volume
// away (the data dir not there), then the bare mount point holding a lock
// file.
func TestServeSaysItsRefusalAgainWhenItsCauseChanges(t *testing.T) {
	v := newIdentityVolume(t)
	v.takeAway(t)
	if said := refuseSaying(t, v.cfg, dataDirGoneLine(v.dataDir), 2); !said[0] || said[1] {
		t.Errorf("starts said the refusal of a data dir not there %v, want the first alone", said)
	}
	mkdirs(t, v.dataDir)
	touch(t, filepath.Join(v.dataDir, serveLockFileName))
	if said := refuseSaying(t, v.cfg, "serve: "+noIdentityLine(v.dataDir, serveLockFileName), 2); !said[0] || said[1] {
		t.Errorf("starts said the refusal of a lock file in the bare mount point %v, want the first alone", said)
	}
}

// TestTheEndOfARefusalCallsBackOnlyTheDataDirHoldingItsIdentity: once a
// refusal was said, the data dir it refused is back only where it holds the
// identity the refusal was of. A lock file in the bare mount point is no such
// data dir: with the record removed and the volume still away, the start
// that gets past the preparation and the lock says the refusal no longer
// applies, never that the data dir is back. On main a lock file there read as
// the data dir back.
func TestTheEndOfARefusalCallsBackOnlyTheDataDirHoldingItsIdentity(t *testing.T) {
	const back, over = refusedBackLine, "no longer applies"
	v := newIdentityVolume(t)
	v.takeAway(t)
	if said := refuseTimes(t, v.cfg, v.dataDir, 1); !said[0] {
		t.Fatal("the refusal was not said")
	}
	mkdirs(t, v.dataDir)
	touch(t, filepath.Join(v.dataDir, serveLockFileName))
	if err := os.Remove(dataDirRecordFile(v.cfg)); err != nil {
		t.Fatal(err)
	}
	rec := loggingtest.Record(t)
	if _, served, _, stderr := runServeWith(t, serveOpts{configPath: v.cfg}); !served {
		t.Fatalf("serve did not start with its record removed; stderr=%s", stderr)
	}
	if got := rec.Carrying(back); len(got) != 0 {
		t.Errorf("a start over a lock file in the bare mount point said the refused data dir is back:\n%s", strings.Join(got, "\n"))
	}
	if got := rec.Carrying(over); len(got) != 1 {
		t.Errorf("the start said %q %d time(s), want once:\n%s", over, len(got), strings.Join(rec.All(), "\n"))
	}
}

// TestServeRefusesTheDataDirInAnotherCaseOnACaseInsensitiveVolume is gap 3 on
// a real volume that folds case, which this suite cannot make: run it with
// BRIDGE_TEST_CASE_INSENSITIVE_DIR naming a directory on one whose own name
// the folder above it compares with case (the root of a vfat or exFAT volume
// mounted on ext4, or an ext4 directory with chattr +F). A serve records its
// data dir there as `Bridge`; the data dir's folder goes, as with its volume
// away, and the config names `bridge`: serve refuses it, and makes nothing.
func TestServeRefusesTheDataDirInAnotherCaseOnACaseInsensitiveVolume(t *testing.T) {
	const env = "BRIDGE_TEST_CASE_INSENSITIVE_DIR"
	root := os.Getenv(env)
	if root == "" {
		t.Skip("set " + env + " to a directory on a volume that folds case, whose own name the folder above it compares with case, to run this")
	}
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mkdirs(t, lib)
	folder := filepath.Join(root, fmt.Sprintf("b741-%d", os.Getpid()))
	away := folder + "-away"
	t.Cleanup(func() { _ = os.RemoveAll(folder); _ = os.RemoveAll(away) })
	mkdirs(t, folder)
	if f, err := os.Stat(filepath.Join(root, strings.ToUpper(filepath.Base(folder)))); err != nil {
		t.Fatalf("%s=%s names a directory whose volume keeps case: %v", env, root, err)
	} else if g, err := os.Stat(folder); err != nil || !os.SameFile(f, g) {
		t.Fatalf("%s=%s names a directory whose volume keeps case", env, root)
	}
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, filepath.Join(folder, "Bridge"))
	stopServed(t, bootServe(t, "--config", cfg))
	if err := os.Rename(folder, away); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(folder, "bridge")
	writeIdentityConfig(t, cfg, lib, configured)
	requireServeRefuses(t, cfg, folder, "the data directory "+configured+" held this bridge's database")
}

// serveWithOpts runs serve with opts, as bootServe runs it from a command
// line, and returns once it listens: serve writes its record and its marker
// before it listens.
func serveWithOpts(t *testing.T, opts serveOpts) servedBridge {
	t.Helper()
	return launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, opts, stdout, stderr)
	})
}

// TestAStartWhoseMarkerCannotBeWrittenGoesOnWithoutAnIdentity: a marker
// serve cannot write refuses nothing: the start goes on, says so at Warn,
// and the record it writes holds no identity, so the next start tells the
// data dir by its database and its lock file, as before B741, and starts,
// writing the marker then.
func TestAStartWhoseMarkerCannotBeWrittenGoesOnWithoutAnIdentity(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mkdirs(t, lib)
	dataDir := filepath.Join(base, "data")
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
	rec := loggingtest.Record(t)
	stopServed(t, serveWithOpts(t, serveOpts{configPath: cfg, dataDirRecording: dataDirRecording{
		marker: func(string, string) error { return errors.New("the test's marker that cannot be written") }}}))
	if lines := rec.Carrying("could not record the data directory's identity in it"); len(lines) != 1 || !strings.HasPrefix(lines[0], "WARN") {
		t.Errorf("the start said it could not write its marker %d time(s), want once at Warn:\n%s", len(lines), strings.Join(rec.All(), "\n"))
	}
	if got := identityOf(t, dataDir); got != "" {
		t.Errorf("the data dir holds the identity %q from a marker that could not be written", got)
	}
	if got := recordedDataDir(t, cfg); got != dataDir {
		t.Errorf("the record names %q, want %q", got, dataDir)
	}
	if got := recordedIdentity(t, cfg); got != "" {
		t.Errorf("the record holds the identity %q, which no marker in the data dir holds", got)
	}
	stopServed(t, bootServe(t, "--config", cfg))
	requireOneIdentity(t, cfg, dataDir)
}

// TestAStartWhoseRecordCannotBeWrittenLeavesItsMarkerToTheNext: a start that
// wrote its marker and not its record (here, a record that cannot be
// written), as a crash between the two would leave them, goes on, and the
// next start takes the marker's identity into the record.
func TestAStartWhoseRecordCannotBeWrittenLeavesItsMarkerToTheNext(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mkdirs(t, lib)
	dataDir := filepath.Join(base, "data")
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
	stopServed(t, serveWithOpts(t, serveOpts{configPath: cfg, dataDirRecording: dataDirRecording{
		record: func(string, []byte) error { return errors.New("the test's record that cannot be written") }}}))
	marked := identityOf(t, dataDir)
	if !isIdentity(marked) {
		t.Fatalf("the data dir's marker holds %q after a start whose record could not be written, want an identity", marked)
	}
	if _, err := os.Stat(dataDirRecordFile(cfg)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a record serve could not write is there: %v", err)
	}
	stopServed(t, bootServe(t, "--config", cfg))
	if got := requireOneIdentity(t, cfg, dataDir); got != marked {
		t.Errorf("the next start recorded the identity %q, want %q, the marker's", got, marked)
	}
}

// markerSteps is a dataDirRecording that keeps, in order, each step that
// makes the marker durable and the record's write, each step then taken as
// its own unless fail names it, which fails it as a crash or a fault there
// would: what the record held, and what the marker held as the record was
// written. steps is appended to on serve's goroutine, and read once serve has
// exited.
func markerSteps(dataDir string, steps *[]string, fail string) dataDirRecording {
	own := markerDurability{}.steps()
	step := func(name string, take func() error) error {
		*steps = append(*steps, name)
		if name == fail {
			return errors.New("the test's " + name + " that fails")
		}
		return take()
	}
	return dataDirRecording{
		durability: markerDurability{
			syncStaged: func(f *os.File) error {
				return step("sync the staged marker", func() error { return own.syncStaged(f) })
			},
			rename: func(from, to string) error {
				return step("rename it into place", func() error { return own.rename(from, to) })
			},
			syncFile: func(path string) error {
				return step("sync it in place", func() error { return own.syncFile(path) })
			},
			syncDir: func(path string) error {
				return step("sync its directory", func() error { return own.syncDir(path) })
			},
		},
		record: func(path string, data []byte) error {
			var r struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(data, &r)
			marker, err := os.ReadFile(dataDirMarkerFile(dataDir))
			*steps = append(*steps, fmt.Sprintf("record %q, the marker holding %s (%v)", r.ID, strings.TrimSpace(string(marker)), err))
			return os.WriteFile(path, data, 0o600)
		},
	}
}

// TestTheMarkerHoldsTheIdentityBeforeTheRecordDoes: the record gains the
// identity only once the marker in the data dir holds it durably: its staged
// bytes synced, renamed into place, synced where it is (on Windows its bytes
// and metadata flushed through a handle of its own), and its directory
// synced, each step before the next, so a crash anywhere in between leaves a
// marker the next start takes up, never a record holding an identity no
// marker holds, which every start after it would refuse. Until round 2 this
// pinned the order of the two writes alone, and dropping either sync passed
// it (Codex on #1441).
func TestTheMarkerHoldsTheIdentityBeforeTheRecordDoes(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mkdirs(t, lib)
	dataDir := filepath.Join(base, "data")
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
	var steps []string
	stopServed(t, serveWithOpts(t, serveOpts{configPath: cfg, dataDirRecording: markerSteps(dataDir, &steps, "")}))
	id := identityOf(t, dataDir)
	want := []string{"sync the staged marker", "rename it into place", "sync it in place", "sync its directory",
		fmt.Sprintf("record %q, the marker holding {\"id\":%q} (<nil>)", id, id)}
	if !slices.Equal(steps, want) {
		t.Errorf("serve took the steps %q, want %q", steps, want)
	}
}

// TestAMarkerWhoseBarrierFailedIsTakenUpOnceDurable: a start whose marker was
// renamed into place and whose barrier after the rename failed (as a crash
// there leaves it, the marker in place and not known durable) goes on, its
// record holding no identity; the next start takes the marker's identity up
// into the record only once its own barrier has made the marker durable where
// it is, and a start whose record holds it already makes none (B741's round
// 2: the failure stood in for the whole write, never one after the rename;
// Codex on #1441).
func TestAMarkerWhoseBarrierFailedIsTakenUpOnceDurable(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mkdirs(t, lib)
	dataDir := filepath.Join(base, "data")
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
	var steps []string
	stopServed(t, serveWithOpts(t, serveOpts{configPath: cfg, dataDirRecording: markerSteps(dataDir, &steps, "sync its directory")}))
	id := identityOf(t, dataDir)
	if !isIdentity(id) {
		t.Fatalf("the marker renamed into place holds %q, want an identity", id)
	}
	want := []string{"sync the staged marker", "rename it into place", "sync it in place", "sync its directory",
		fmt.Sprintf("record %q, the marker holding {\"id\":%q} (<nil>)", "", id)}
	if !slices.Equal(steps, want) {
		t.Errorf("the start whose barrier failed took the steps %q, want %q", steps, want)
	}

	steps = nil
	stopServed(t, serveWithOpts(t, serveOpts{configPath: cfg, dataDirRecording: markerSteps(dataDir, &steps, "")}))
	want = []string{"sync it in place", "sync its directory", fmt.Sprintf("record %q, the marker holding {\"id\":%q} (<nil>)", id, id)}
	if !slices.Equal(steps, want) {
		t.Errorf("the next start took the steps %q, want %q: the marker made durable where it is before the record takes up its identity", steps, want)
	}
	if got := requireOneIdentity(t, cfg, dataDir); got != id {
		t.Errorf("the record holds %q, want the marker's %q", got, id)
	}

	steps = nil
	stopServed(t, serveWithOpts(t, serveOpts{configPath: cfg, dataDirRecording: markerSteps(dataDir, &steps, "")}))
	if len(steps) != 0 {
		t.Errorf("a start whose record holds its marker's identity took the steps %q, want none", steps)
	}
}

// recordedFolding is how the record beside cfg says each folder on its data
// dir's way, as its config spells it, compared names: one letter a folder,
// its volume's root first (fsutil.FormatFolding).
func recordedFolding(t *testing.T, cfg string) string {
	t.Helper()
	b, err := os.ReadFile(dataDirRecordFile(cfg))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		DataDirFolding string `json:"dataDirFolding"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("the record beside %s does not read: %v (%q)", cfg, err, b)
	}
	return r.DataDirFolding
}

// TestServeRecordsHowEachFolderOnItsDataDirsWayComparesNames is gap 3's
// write: serve's record holds, for every folder on the data dir's way, how it
// compares the names in it, as asked while the data dir is there (here, a
// stand-in for a volume at `usb` that folds case, which this host does not
// have, and the host's own folders above it); once that volume is away, a
// config naming the data dir in another case below the mount point is
// refused, though the folder left there keeps case.
func TestServeRecordsHowEachFolderOnItsDataDirsWayComparesNames(t *testing.T) {
	base := t.TempDir()
	lib, mnt := filepath.Join(base, "Music"), filepath.Join(base, "usb")
	mkdirs(t, lib, mnt)
	dataDir := filepath.Join(mnt, "Bridge")
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
	probe := func(dir string) fsutil.CaseFolding {
		if dir == mnt {
			return fsutil.CaseInsensitive
		}
		return fsutil.FoldingIn(dir)
	}
	stopServed(t, serveWithOpts(t, serveOpts{configPath: cfg, dataDirRecording: dataDirRecording{folding: probe}}))
	var want []fsutil.CaseFolding
	for child := dataDir; ; {
		parent := filepath.Dir(child)
		if parent == child {
			break
		}
		want = append([]fsutil.CaseFolding{probe(parent)}, want...)
		child = parent
	}
	if got, want := recordedFolding(t, cfg), fsutil.FormatFolding(want); got != want {
		t.Errorf("the record says the folders on %s's way compare names %q, want %q, the root's first", dataDir, got, want)
	}

	if err := os.Rename(dataDir, filepath.Join(base, "volume")); err != nil {
		t.Fatal(err)
	}
	configured := filepath.Join(mnt, "bridge")
	writeIdentityConfig(t, cfg, lib, configured)
	requireServeRefuses(t, cfg, mnt, "the data directory "+configured+" held this bridge's database")
}

// TestARecordItsReaderWouldRefuseKeepsNoFolding: a record whose folding would
// take it past what its reader takes, which would refuse every start after
// it, is written keeping no folding, each spelling's the one letter u, and
// reads; a record holding that compares a spelling in another case as one
// whose folders could not tell, taking it for the data dir, the direction
// that refuses, and probes nothing (B741's round 2: round 1 wrote such a
// record with no folding at all, which the check compared by the probe, and
// with the volume away a probe answers for the folder left at the mount
// point; Codex on #1441). A record longer than its reader takes without its
// folding is not written. Neither is the record of a path an OS takes: one
// letter a folder keeps a data dir 2,000 folders deep, in both its
// spellings, in a few kilobytes.
func TestARecordItsReaderWouldRefuseKeepsNoFolding(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	// "/mnt" and n folders below it: n+1 folders on the way, 2n+4 bytes.
	deepAs := func(n int) (string, string) {
		return "/mnt" + strings.Repeat("/d", n), strings.Repeat("s", n+1)
	}
	// Its paths under the bound, its folding past it.
	dir, letters := deepAs(dataDirRecordMax * 2 / 5)
	b, err := marshalDataDirRecord(dataDirRecord{DataDir: dir, ID: id, DataDirFolding: letters})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ".bridge.yaml.datadir")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readDataDirRecord(path)
	switch {
	case !ok || err != nil:
		t.Errorf("the record written does not read: ok %v, %v (%d bytes)", ok, err, len(b))
	case got.DataDir != dir || got.ID != id || got.DataDirFolding != "u" || got.ResolvedFolding != "":
		t.Errorf("the record written reads with its folding %.20q and its identity %q, want the one letter u and %q", got.DataDirFolding, got.ID, id)
	}

	// A spelling in another case, by a record keeping no folding of its
	// folders, and by one keeping each folder's.
	usb := filepath.Join(filepath.VolumeName(os.TempDir())+string(filepath.Separator), "mnt", "usb")
	kept := dataDirRecord{DataDir: filepath.Join(usb, "Bridge"), ID: id, DataDirFolding: "u"}
	if !kept.names(filepath.Join(usb, "bridge")) {
		t.Error("a record keeping no folding of its folders read a spelling in another case as another data dir")
	}
	kept.DataDirFolding = "sss"
	if kept.names(filepath.Join(usb, "bridge")) {
		t.Error("a record keeping its folders as case-sensitive read a spelling in another case as its data dir")
	}

	// Longer than the bound without its folding: refused, never written.
	dir, letters = deepAs(dataDirRecordMax / 2)
	if b, err := marshalDataDirRecord(dataDirRecord{DataDir: dir, ID: id, DataDirFolding: letters}); !errors.Is(err, errRecordTooLong) {
		t.Errorf("a record longer than its reader takes was made (%d bytes, %v), want errRecordTooLong", len(b), err)
	}

	// A data dir 2,000 folders deep, in both its spellings.
	dir, letters = deepAs(2000)
	r := dataDirRecord{DataDir: dir, Resolved: dir, ID: id, DataDirFolding: letters, ResolvedFolding: letters}
	if b, err := marshalDataDirRecord(r); err != nil || len(b) > 16<<10 || !strings.Contains(string(b), letters) {
		t.Errorf("the record of a data dir 2,000 folders deep is %d bytes (%v), want its folding kept in a few kilobytes", len(b), err)
	}
}

// TestACommandThatOpensTheStoreMakesNoMarker: only serve writes the marker;
// bridge scan over a fresh install makes the data dir and its database, as
// before, and no marker, which the next serve writes.
func TestACommandThatOpensTheStoreMakesNoMarker(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mkdirs(t, lib)
	dataDir := filepath.Join(base, "data")
	cfg := writeIdentityConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir)
	if code, out := runCLI(t, "scan", "--config", cfg); code != 0 {
		t.Fatalf("bridge scan exited %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "bridge.db")); err != nil {
		t.Fatalf("bridge scan made no database: %v", err)
	}
	if got := identityOf(t, dataDir); got != "" {
		t.Errorf("bridge scan wrote the marker %q", got)
	}
	stopServed(t, bootServe(t, "--config", cfg))
	requireOneIdentity(t, cfg, dataDir)
}
