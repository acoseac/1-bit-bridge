package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/doctor"
)

// Backlog B651: a NAS that starts the bridge before its volumes are mounted
// shows serve a data dir that is not there, or the bare mount point, empty.
// Serve made a fresh data dir in its place, with a new TLS pair, no paired
// device and an empty database, on the system disk under the mount point.
// Now the record beside the config names the data dir serve last served
// from, and serve refuses that data dir, before it makes or writes anything,
// while it holds neither its database nor its lock file.

// dataDirRecordFile is where serve keeps the record of the data dir the
// config at cfg names: beside it, named after it.
func dataDirRecordFile(cfg string) string {
	return filepath.Join(filepath.Dir(cfg), "."+filepath.Base(cfg)+".datadir")
}

// recordedDataDir is the data dir the record beside cfg names, "" for none.
func recordedDataDir(t *testing.T, cfg string) string {
	t.Helper()
	b, err := os.ReadFile(dataDirRecordFile(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		DataDir string `json:"dataDir"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("the record beside %s does not read: %v (%q)", cfg, err, b)
	}
	return r.DataDir
}

// stopServed stops a serve bootServe started and waits for it to exit 0.
func stopServed(t *testing.T, b servedBridge) {
	t.Helper()
	b.stop()
	select {
	case code := <-b.done:
		if code != 0 {
			t.Fatalf("serve exited %d; stderr=%s", code, b.stderr.String())
		}
	case <-serveGiveUp(t):
		t.Fatalf("serve did not stop; stderr=%s\nserve's goroutines:\n%s", b.stderr.String(), serveStacks())
	}
}

// dataDirGoneLine is what serve's refusal of dataDir says first.
func dataDirGoneLine(dataDir string) string {
	return "serve: the data directory " + dataDir + " held this bridge's database"
}

// TestServeRefusesTheDataDirItServedFromWhileItsVolumeIsAway runs a serve
// over a data dir below a volume's mount point, takes the volume away, and
// requires the next serves to refuse, exit 1, naming the data dir, and make
// and write nothing there, before the player's state preflight: while the
// data dir is not there (a directory on the volume), and while an empty
// directory stands at its path (the bare mount point). Once the volume is back
// serve starts on what it held.
func TestServeRefusesTheDataDirItServedFromWhileItsVolumeIsAway(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	mountPoint := filepath.Join(base, "usb")
	for _, d := range []string{lib, mountPoint} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := filepath.Join(mountPoint, "bridge")
	cfg := writeServeConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
	stopServed(t, bootServe(t, "--config", cfg))
	if got := recordedDataDir(t, cfg); got != dataDir {
		t.Fatalf("after serve opened its database the record beside the config names %q, want %q", got, dataDir)
	}

	volume := filepath.Join(base, "volume")
	if err := os.Rename(dataDir, volume); err != nil {
		t.Fatal(err)
	}
	for _, bare := range []bool{false, true} {
		name := "the data dir is not there"
		if bare {
			name = "an empty directory stands at the data dir's path"
		}
		t.Run(name, func(t *testing.T) {
			if bare {
				if err := os.Mkdir(dataDir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var preflights atomic.Int32
			code, served, stdout, stderr := runServeWith(t, serveOpts{configPath: cfg,
				playerStatePreflight: func(string, bool) error { preflights.Add(1); return nil }})
			switch {
			case served:
				t.Errorf("serve served while its data dir's volume was away; stdout=%s", stdout)
			case code != 1:
				t.Errorf("serve exited %d while its data dir's volume was away, want 1; stderr=%s", code, stderr)
			case !strings.Contains(stderr, dataDirGoneLine(dataDir)):
				t.Errorf("serve's refusal does not say %q; stderr=%s", dataDirGoneLine(dataDir), stderr)
			}
			if n := preflights.Load(); n != 0 {
				t.Errorf("serve called the player's state preflight %d time(s) before it refused the data dir", n)
			}
			entries, err := os.ReadDir(dataDir)
			switch {
			case !bare && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("serve made the data dir its record names while it was away (read: %v, %d entries)", err, len(entries))
			case bare && err != nil:
				t.Fatal(err)
			case bare && len(entries) != 0:
				t.Errorf("serve wrote %d entries into the empty directory at its data dir's path, the first %s", len(entries), entries[0].Name())
			}
		})
	}

	if err := os.Remove(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(volume, dataDir); err != nil {
		t.Fatal(err)
	}
	back := bootServe(t, "--config", cfg)
	if s := back.stderr.String(); strings.Contains(s, dataDirGoneLine(dataDir)) {
		t.Errorf("serve refused its data dir once its volume was back; stderr=%s", s)
	}
}

// TestServeRefusesADataDirItsRecordNamesThatHoldsNeitherFile is the refusal
// alone, over a record a serve before it wrote (here, by hand): a data dir it
// names that is not there, or that is an empty directory, is refused with the
// record named as the way past it, and nothing is made or written there, the
// TLS pair and the token store included.
func TestServeRefusesADataDirItsRecordNamesThatHoldsNeitherFile(t *testing.T) {
	for _, bare := range []bool{false, true} {
		name := "the data dir is not there"
		if bare {
			name = "an empty directory stands at the data dir's path"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			lib := filepath.Join(base, "Music")
			mountPoint := filepath.Join(base, "usb")
			for _, d := range []string{lib, mountPoint} {
				if err := os.Mkdir(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			dataDir := filepath.Join(mountPoint, "bridge")
			if bare {
				if err := os.Mkdir(dataDir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cfg := writeServeConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
			writeDataDirRecord(t, cfg, dataDir)
			code, served, stdout, stderr := runServeWith(t, serveOpts{configPath: cfg})
			switch {
			case served:
				t.Errorf("serve served over a data dir its record names that holds neither file; stdout=%s", stdout)
			case code != 1:
				t.Errorf("serve exited %d, want 1; stderr=%s", code, stderr)
			case !strings.Contains(stderr, dataDirGoneLine(dataDir)) || !strings.Contains(stderr, "remove "+dataDirRecordFile(cfg)):
				t.Errorf("serve's refusal does not say %q and name the record to remove; stderr=%s", dataDirGoneLine(dataDir), stderr)
			}
			entries, err := os.ReadDir(dataDir)
			switch {
			case !bare && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("serve made the data dir (read: %v, %d entries)", err, len(entries))
			case bare && (err != nil || len(entries) != 0):
				t.Errorf("serve wrote into the empty directory at the data dir's path (read: %v, %d entries)", err, len(entries))
			}
		})
	}
}

// TestServeStartsAgainOverADatabaseRemovedOnPurpose: a data dir whose
// database was removed and whose lock file was kept (the local fixture's
// reset, rm bridge.db*) is the data dir serve served from, and serve makes
// its database again, as it always has.
func TestServeStartsAgainOverADatabaseRemovedOnPurpose(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	if err := os.Mkdir(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(base, "data")
	cfg := writeServeConfig(t, filepath.Join(base, "bridge.yaml"), lib, dataDir, "127.0.0.1:0", "127.0.0.1:0")
	stopServed(t, bootServe(t, "--config", cfg))
	db := filepath.Join(dataDir, "bridge.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(db + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	again := bootServe(t, "--config", cfg)
	if s := again.stderr.String(); strings.Contains(s, dataDirGoneLine(dataDir)) {
		t.Errorf("serve refused a data dir whose database alone was removed; stderr=%s", s)
	}
	if _, err := os.Stat(db); err != nil {
		t.Errorf("serve did not make its database again: %v", err)
	}
}

// writeDataDirRecord writes the record beside cfg naming dataDir, as a serve
// that opened its database there writes it.
func writeDataDirRecord(t *testing.T, cfg, dataDir string) {
	t.Helper()
	b, err := json.Marshal(struct {
		DataDir string `json:"dataDir"`
	}{dataDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dataDirRecordFile(cfg), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBothDoctorsFailADataDirItsRecordNamesWhileItsVolumeIsAway: bridge
// doctor and the console's doctor say on their data-dir line what serve's
// refusal says, word for word in both: a data dir the record names that holds
// neither its database nor its lock file, not there or an empty directory, is
// a fail saying bridge serve will not start, with the record to remove as the
// fix for a data dir removed on purpose, and bridge doctor exits 1; once the
// database is back the line is ok.
func TestBothDoctorsFailADataDirItsRecordNamesWhileItsVolumeIsAway(t *testing.T) {
	for _, bare := range []bool{false, true} {
		name := "the data dir is not there"
		if bare {
			name = "an empty directory stands at the data dir's path"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			mountPoint := filepath.Join(base, "usb")
			dataDir := filepath.Join(mountPoint, "bridge")
			made := mountPoint
			if bare {
				made = dataDir
			}
			if err := os.MkdirAll(made, 0o700); err != nil {
				t.Fatal(err)
			}
			cfg := writeDataDirConfig(t, base, dataDir, false)
			writeDataDirRecord(t, cfg, dataDir)
			said := askBothDoctors(t, cfg)
			c := said.requireLine(t, dataDirCheckSlug, doctor.Fail)
			if want := "bridge serve will not start: the data directory " + dataDir + " held this bridge's database"; !strings.HasPrefix(c.Summary, want) {
				t.Errorf("the data-dir line says %q, want it to start %q", c.Summary, want)
			}
			if !strings.Contains(c.Hint, dataDirRecordFile(cfg)) {
				t.Errorf("the data-dir line's fix %q does not name the record %s", c.Hint, dataDirRecordFile(cfg))
			}
			said.requirePrinted(t, c)
			if said.code != 1 {
				t.Errorf("bridge doctor exited %d over a data dir serve refuses, want 1", said.code)
			}

			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dataDir, "bridge.db"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			askBothDoctors(t, cfg).requireLine(t, dataDirCheckSlug, doctor.OK)
		})
	}
}

// TestTheRecordRefusesOnlyTheDataDirItNamesWhileItHoldsNeitherFile pins the
// check's answers: no record, one that does not read (a torn write, a hand
// edit) and one naming another data dir (the config's dataDir changed) refuse
// nothing; the data dir the record names refuses while it holds neither its
// database nor its lock file, and not once either is there.
func TestTheRecordRefusesOnlyTheDataDirItNamesWhileItHoldsNeitherFile(t *testing.T) {
	base := t.TempDir()
	cfg := filepath.Join(base, "bridge.yaml")
	if err := os.WriteFile(cfg, []byte("libraryRoots: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(base, "usb", "bridge")
	refused := func() bool {
		t.Helper()
		var gone *dataDirGoneError
		err := checkServeDataDirRecord(cfg, dataDir)
		if err != nil && !errors.As(err, &gone) {
			t.Fatalf("the check answered %v, want nil or a *dataDirGoneError", err)
		}
		return err != nil
	}
	if refused() {
		t.Error("the check refused a data dir with no record beside the config")
	}
	if err := os.WriteFile(dataDirRecordFile(cfg), []byte("{\"dataDir\": "), 0o600); err != nil {
		t.Fatal(err)
	}
	if refused() {
		t.Error("the check refused on a record that does not read")
	}
	writeDataDirRecord(t, cfg, filepath.Join(base, "elsewhere"))
	if refused() {
		t.Error("the check refused a data dir its record does not name")
	}
	writeDataDirRecord(t, cfg, dataDir)
	if !refused() {
		t.Error("the check took a data dir its record names that is not there")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !refused() {
		t.Error("the check took an empty directory at the path of a data dir its record names")
	}
	for _, name := range []string{"bridge.db", serveLockFileName} {
		p := filepath.Join(dataDir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if refused() {
			t.Errorf("the check refused a data dir holding %s", name)
		}
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheRecordIsBesideTheFileTheConfigsNameOpens: a config named through a
// link keeps its record beside the file the link leads to, named after it, as
// its lock is (config.LockFile), so every name of one config shares a record.
func TestTheRecordIsBesideTheFileTheConfigsNameOpens(t *testing.T) {
	base := t.TempDir()
	store := filepath.Join(base, "store")
	if err := os.Mkdir(store, 0o700); err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(store, "actual.yaml")
	if err := os.WriteFile(actual, []byte("libraryRoots: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "bridge.yaml")
	symlinkOrSkip(t, actual, link)
	resolved, err := filepath.EvalSymlinks(store)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{link, actual} {
		got, err := dataDirRecordPath(name)
		if want := filepath.Join(resolved, ".actual.yaml.datadir"); err != nil || got != want {
			t.Errorf("the record of %s is %q (%v), want %q", name, got, err, want)
		}
	}
}

// TestInitAndTheServeAfterItStartAsBefore: bridge init makes the data dir and
// its TLS pair and no database, and writes no record; the first serve after
// it starts, and records the data dir once its database is open.
func TestInitAndTheServeAfterItStartAsBefore(t *testing.T) {
	cfgDir := filepath.Join(t.TempDir(), "config")
	code, out := runInit(t, "--yes", "--no-service", "--skip-doctor", "--dir", cfgDir, "--library", testLibrary(t),
		"--listen-address", "127.0.0.1:0", "--admin-address", "127.0.0.1:0")
	if code != 0 {
		t.Fatalf("init exited %d:\n%s", code, out)
	}
	cfg := filepath.Join(cfgDir, "bridge.yaml")
	if got := recordedDataDir(t, cfg); got != "" {
		t.Fatalf("init wrote a record naming %q; only serve, once its database is open, writes one", got)
	}
	dataDir := initDataDirFor(cfgDir)
	stopServed(t, bootServe(t, "--config", cfg))
	if got := recordedDataDir(t, cfg); got != dataDir {
		t.Errorf("the first serve after init recorded %q, want %q", got, dataDir)
	}
}
