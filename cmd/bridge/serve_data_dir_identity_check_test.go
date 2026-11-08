package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// The check's answers and the refusal's words, backlog B741, without a serve:
// every state of the record against every state of the data dir it names.

// TestTheCheckDecidesByTheIdentity pins checkServeDataDirRecord's answer for
// each state of the record (none, one that does not read, one written before
// B741 holding no identity, one holding an identity, one naming another data
// dir) over each state of the data dir (not there, empty, holding its lock
// file, its database or both, a marker of the record's identity, of another,
// or one that does not read).
func TestTheCheckDecidesByTheIdentity(t *testing.T) {
	const id, other = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	type dir struct {
		name   string
		absent bool
		holds  []string
		marker string // an identity, "torn", or "" for none
	}
	dirs := []dir{
		{name: "not there", absent: true},
		{name: "empty"},
		{name: "its lock file", holds: []string{serveLockFileName}},
		{name: "its database", holds: []string{"bridge.db"}},
		{name: "both", holds: []string{"bridge.db", serveLockFileName}},
		{name: "the record's identity alone", marker: id},
		{name: "the record's identity and both", holds: []string{"bridge.db", serveLockFileName}, marker: id},
		{name: "another identity", holds: []string{"bridge.db", serveLockFileName}, marker: other},
		{name: "a marker that does not read", holds: []string{"bridge.db", serveLockFileName}, marker: "torn"},
	}
	// want is each cause by the data dir's state's name; a name it does not
	// hold is no refusal.
	type record struct {
		name  string
		write func(t *testing.T, cfg, dataDir string)
		want  map[string]dataDirCause
	}
	everyDir := func(c dataDirCause) map[string]dataDirCause {
		m := map[string]dataDirCause{}
		for _, d := range dirs {
			m[d.name] = c
		}
		return m
	}
	records := []record{
		{name: "no record", write: func(*testing.T, string, string) {}},
		{name: "a record that does not read", want: everyDir(recordUnreadable),
			write: func(t *testing.T, cfg, _ string) {
				if err := os.WriteFile(dataDirRecordFile(cfg), []byte("{\"dataDir\": "), 0o600); err != nil {
					t.Fatal(err)
				}
			}},
		{name: "a record written before B741", write: func(t *testing.T, cfg, dataDir string) { writeDataDirRecord(t, cfg, dataDir) },
			want: map[string]dataDirCause{"not there": dataDirNotThere, "empty": dataDirNotThere,
				"a marker that does not read": identityUnreadable}},
		{name: "a record holding an identity", write: func(t *testing.T, cfg, dataDir string) { writeRecordWithIdentity(t, cfg, dataDir, id) },
			want: map[string]dataDirCause{"not there": dataDirNotThere, "empty": dataDirNotThere,
				"its lock file": noIdentity, "its database": noIdentity, "both": noIdentity,
				"another identity": otherIdentity, "a marker that does not read": identityUnreadable}},
		{name: "a record of another data dir", write: func(t *testing.T, cfg, dataDir string) {
			writeRecordWithIdentity(t, cfg, filepath.Join(filepath.Dir(dataDir), "elsewhere"), id)
		}},
	}
	for _, r := range records {
		for _, d := range dirs {
			t.Run(r.name+" over "+d.name, func(t *testing.T) {
				base := t.TempDir()
				cfg := filepath.Join(base, "bridge.yaml")
				dataDir := filepath.Join(base, "usb", "bridge")
				mkdirs(t, filepath.Dir(dataDir))
				if !d.absent {
					mkdirs(t, dataDir)
				}
				for _, name := range d.holds {
					touch(t, filepath.Join(dataDir, name))
				}
				switch d.marker {
				case "":
				case "torn":
					if err := os.WriteFile(dataDirMarkerFile(dataDir), []byte("{\"id\": "), 0o600); err != nil {
						t.Fatal(err)
					}
				default:
					writeIdentity(t, dataDir, d.marker)
				}
				r.write(t, cfg, dataDir)
				err := checkServeDataDirRecord(dataDirRecordFile(cfg), dataDir)
				want, refused := r.want[d.name]
				var gone *dataDirGoneError
				switch {
				case !refused && err != nil:
					t.Errorf("the check refused, %v; want none", err)
				case !refused:
				case !errors.As(err, &gone):
					t.Errorf("the check answered %v, want its refusal for %s", err, want)
				case gone.cause != want:
					t.Errorf("the check refused for %s, want %s: %v", gone.cause, want, err)
				case want == noIdentity && !slices.Equal(gone.held, d.holds):
					t.Errorf("the refusal says the data dir holds %q, want %q", gone.held, d.holds)
				case gone.dataDir != dataDir || gone.record != dataDirRecordFile(cfg):
					t.Errorf("the refusal names the data dir %q and the record %q", gone.dataDir, gone.record)
				}
			})
		}
	}
}

// writeRecordWithIdentity writes the record beside cfg naming dataDir and
// holding id, as a serve of B741 writes it before gap 3's folding (a record
// that holds none of it reads as one).
func writeRecordWithIdentity(t *testing.T, cfg, dataDir, id string) {
	t.Helper()
	body := `{"dataDir":` + quoteJSON(dataDir) + `,"id":"` + id + `"}` + "\n"
	if err := os.WriteFile(dataDirRecordFile(cfg), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// quoteJSON is s as a JSON string.
func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// TestTheRecordReadsOnlyAsOne pins readDataDirRecord: nothing there is no
// record; a record that is there and is not one (not JSON, no data dir, an
// identity out of shape, more bytes than a record ever holds, a directory, a
// link to nothing) does not read, with an error, never as no record; one
// written before B741 and one holding an identity read, and so does one
// holding a field a later version adds.
func TestTheRecordReadsOnlyAsOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".bridge.yaml.datadir")
	if _, ok, err := readDataDirRecord(path); ok || err != nil {
		t.Errorf("no record read as ok %v, err %v; want neither", ok, err)
	}
	for _, tc := range []struct {
		name, body string
		reads      bool
	}{
		{"not JSON", `{"dataDir": `, false},
		{"no data dir", `{"resolved":"/mnt/usb/bridge"}`, false},
		{"an empty data dir", `{"dataDir":""}`, false},
		{"an identity too short", `{"dataDir":"/mnt/usb/bridge","id":"0123"}`, false},
		{"an identity in capitals", `{"dataDir":"/mnt/usb/bridge","id":"0123456789ABCDEF0123456789ABCDEF"}`, false},
		{"an identity that is no string", `{"dataDir":"/mnt/usb/bridge","id":5}`, false},
		{"more than a record holds", `{"dataDir":"/mnt/usb/bridge"}` + strings.Repeat(" ", dataDirRecordMax), false},
		{"written before B741", `{"dataDir":"/mnt/usb/bridge","resolved":"/mnt/usb/bridge"}`, true},
		{"holding an identity", `{"dataDir":"/mnt/usb/bridge","id":"0123456789abcdef0123456789abcdef"}`, true},
		{"holding a field a later version adds", `{"dataDir":"/mnt/usb/bridge","later":{"a":1}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			r, ok, err := readDataDirRecord(path)
			switch {
			case tc.reads && (!ok || err != nil || r.DataDir != "/mnt/usb/bridge"):
				t.Errorf("read %+v, ok %v, err %v; want the record", r, ok, err)
			case !tc.reads && (ok || !errors.Is(err, errDoesNotRead)):
				t.Errorf("read %+v, ok %v, err %v; want it not to read as one", r, ok, err)
			}
		})
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	mkdirs(t, path)
	if _, ok, err := readDataDirRecord(path); ok || err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a directory at the record's path read as ok %v, err %v; want an error that is no absence", ok, err)
	}
	link := filepath.Join(dir, ".linked.yaml.datadir")
	symlinkOrSkip(t, filepath.Join(dir, "not-there"), link)
	if _, ok, err := readDataDirRecord(link); ok || !errors.Is(err, errLinkToNothing) || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a link to nothing at the record's path read as ok %v, err %v; want a link to nothing, which is no absence", ok, err)
	}
}

// TestTheMarkerReadsOnlyAsOne pins readDataDirMarker: a marker holding an
// identity reads; one that is not JSON, holds no identity or one out of
// shape, or more bytes than a marker ever holds, does not.
func TestTheMarkerReadsOnlyAsOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), dataDirMarkerName)
	for _, tc := range []struct {
		name, body string
		reads      bool
	}{
		{"an identity", `{"id":"0123456789abcdef0123456789abcdef"}` + "\n", true},
		{"not JSON", `{"id": `, false},
		{"no identity", `{}`, false},
		{"an identity out of shape", `{"id":"not an identity"}`, false},
		{"more than a marker holds", `{"id":"0123456789abcdef0123456789abcdef"}` + strings.Repeat(" ", dataDirMarkerMax), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			id, err := readDataDirMarker(path)
			switch {
			case tc.reads && (err != nil || id != "0123456789abcdef0123456789abcdef"):
				t.Errorf("read %q, err %v; want the identity", id, err)
			case !tc.reads && (id != "" || !errors.Is(err, errDoesNotRead)):
				t.Errorf("read %q, err %v; want it not to read as one", id, err)
			}
		})
	}
}

// TestAMintedIdentityIsOneOfItsOwn: every identity minted is well formed and
// none repeats.
func TestAMintedIdentityIsOneOfItsOwn(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := mintDataDirIdentity()
		if !isDataDirIdentity(id) || !isIdentity(id) {
			t.Fatalf("minted %q, which is no identity", id)
		}
		if seen[id] {
			t.Fatalf("minted %q twice", id)
		}
		seen[id] = true
	}
}

// TestTheRefusalNoteIsOfOneState: the note is of the data dir refused, its
// cause and the identity its record held, so a refusal of the same data dir
// for another cause, or for another identity, is said again, and the start
// after it is quiet; a note written before B741, which holds no cause, is
// said again once.
func TestTheRefusalNoteIsOfOneState(t *testing.T) {
	const id, other = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	record := filepath.Join(t.TempDir(), ".bridge.yaml.datadir")
	if err := os.WriteFile(record+dataDirRefusalSuffix,
		[]byte(`{"dataDir":"/mnt/usb/bridge","saidAt":"`+time.Now().UTC().Format(time.RFC3339Nano)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	states := []struct {
		cause    dataDirCause
		identity string
	}{
		{dataDirNotThere, id}, {dataDirNotThere, id},
		{noIdentity, id}, {noIdentity, id},
		{noIdentity, other}, {noIdentity, other},
		{otherIdentity, other},
	}
	var said []bool
	for i, s := range states {
		var out strings.Builder
		sayServeRefusal(&dataDirGoneError{dataDir: "/mnt/usb/bridge", record: record, cause: s.cause, identity: s.identity,
			held: []string{serveLockFileName}}, &out, now.Add(time.Duration(i)*2*time.Second))
		said = append(said, strings.Contains(out.String(), "serve: the data directory /mnt/usb/bridge"))
	}
	if want := []bool{true, false, true, false, true, false, true}; !slices.Equal(said, want) {
		t.Errorf("starts refusing in turn said the refusal %v, want %v", said, want)
	}
}

// TestEachRefusalSaysWhatItIsAndHowToGoOn: each cause's first sentence names
// the data dir as its config gives it, and only the refusal of a data dir
// that holds neither file says it holds neither (B651's words, which the
// other causes would make false); each says how to go on by hand, ending in
// the record to remove, under serve's name, a command's and bridge doctor's;
// and of a record or a marker this user may not read, serve says to make it
// readable by the user the service runs as, and a command and bridge doctor
// to run as that user (B741's round 2), a warn on the doctor's line. A data
// dir holding no identity, or another's, is the one to serve from where the
// operator removes the record, which each surface says in those words (round
// 3: the window behind it is backlog B813's, never the operator's to read).
func TestEachRefusalSaysWhatItIsAndHowToGoOn(t *testing.T) {
	const dataDir, record = "/mnt/usb/bridge", "/etc/bridge/.bridge.yaml.datadir"
	denied := &fs.PathError{Op: "open", Path: record, Err: fs.ErrPermission}
	for _, e := range []*dataDirGoneError{
		{cause: dataDirNotThere},
		{cause: noIdentity, held: []string{serveLockFileName}},
		{cause: otherIdentity},
		{cause: identityUnreadable, err: errDoesNotRead},
		{cause: identityUnreadable, err: denied},
		{cause: recordUnreadable, err: errDoesNotRead},
		{cause: recordUnreadable, err: denied},
	} {
		e.dataDir, e.record = dataDir, record
		t.Run(e.cause.String()+" "+e.why(), func(t *testing.T) {
			what := e.what()
			if !strings.HasPrefix(what, "the data directory "+dataDir+" ") {
				t.Errorf("the refusal begins %q, want it to name the data directory as its config gives it", what)
			}
			if says := strings.Contains(what, "holds neither"); says != (e.cause == dataDirNotThere) {
				t.Errorf("the refusal for %s says %q", e.cause, what)
			}
			status, _, hint := e.doctorLine()
			for _, s := range []struct{ surface, text, readable string }{
				{"serve", e.Error(), "make it readable by the user the service runs as; "},
				{"a command", e.commandRefusal("scan"), "run bridge scan as the user the service runs as, and make it readable by that user if it is not; "},
				{"bridge doctor", hint + " and ", "run bridge doctor as the user the service runs as, and make it readable by that user if it is not; "},
			} {
				if got := recordToRemove(t, s.text); got != record {
					t.Errorf("%s says to remove %q, want the record %q:\n%s", s.surface, got, record, s.text)
				}
				if readable := strings.Contains(s.text, s.readable); readable != errors.Is(e.err, fs.ErrPermission) {
					t.Errorf("%s says %q: %v:\n%s", s.surface, s.readable, readable, s.text)
				}
				if s.surface != "serve" && strings.Contains(s.text, "make it readable by the user the service runs as") {
					t.Errorf("%s says to make it readable by the user the service runs as, which may read it already:\n%s", s.surface, s.text)
				}
				if (e.cause == noIdentity || e.cause == otherIdentity) && !strings.Contains(s.text, "or it is the data directory to serve from") {
					t.Errorf("%s does not say to remove the record for the data directory to serve from:\n%s", s.surface, s.text)
				}
			}
			if r := e.recovery("scan"); strings.Contains(r, "(") {
				t.Errorf("the advice explains itself in an aside, %q: it says what to do and no more", r)
			}
			if want := map[bool]doctor.Status{true: doctor.Warn, false: doctor.Fail}[errors.Is(e.err, fs.ErrPermission)]; status != want {
				t.Errorf("bridge doctor's line is %v, want %v", status, want)
			}
			if cmd := e.commandRefusal("scan"); !strings.Contains(cmd, "bridge scan ") || !strings.Contains(cmd, "makes and writes nothing there") ||
				!strings.HasSuffix(cmd, "run bridge scan again") {
				t.Errorf("the command's refusal does not say bridge scan makes and writes nothing there and to run it again:\n%s", cmd)
			}
			if !strings.HasSuffix(e.Error(), "start bridge serve again") {
				t.Errorf("serve's refusal does not end saying to start bridge serve again:\n%s", e.Error())
			}
			if e.unreadableHere() != errors.Is(e.err, fs.ErrPermission) {
				t.Errorf("unreadableHere is %v for %v", e.unreadableHere(), e.err)
			}
		})
	}
}
