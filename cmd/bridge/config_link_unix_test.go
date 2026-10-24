//go:build !windows

package main

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A config's name is the file the OS opens for it: through a link and the
// `..` after it, the file where the link leads, and through a link at the
// last element, the link's target. Every entry opens, locks, compares and
// replaces that file (backlog B506, third round), and none of them moves the
// directory a config's relative paths resolve against, which stays the one
// config.Load has always taken from the name as given: a data dir or a
// library root that moved would boot serve on a new database and identity.
// Until B506's third round serve cleaned the name before it opened it, so it
// loaded, locked and saved another file than the CLI writers did, and a save
// through a link renamed a file over the link.

// linkedLayout is a config reached through a link, as an entry is given it,
// with the paths it must resolve to.
type linkedLayout struct {
	// name is the config's name as an entry is given it, and file the file
	// the OS opens for it.
	name, file string
	// base is the directory the config's relative paths resolve against:
	// the one config.Load takes from name.
	base string
	// lock is the lock file the turns take, beside file and named after it.
	lock string
	// link, when not empty, is a link at the last element of name, which a
	// save must leave leading to file.
	link string
	// decoy, when not empty, is the file the name cleaned lexically names,
	// which no entry may open.
	decoy string
}

// configText is a config whose data dir and library root are relative, with
// adminAddress and libraryName as given.
func configText(adminAddr, name string) string {
	return "libraryRoots:\n  - Music\ndataDir: data\nadminAddress: " + adminAddr + "\nlibraryName: " + name + "\n"
}

// layouts lays out, in a directory of its own, a config named through a link
// and the `..` after it, and one whose name is a link to it, each written
// with adminAddr; the library root each base names exists.
func layouts(t *testing.T, adminAddr string) map[string]linkedLayout {
	t.Helper()
	root := resolvedTempDir(t)
	mk := func(dirs ...string) {
		for _, d := range dirs {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	write := func(path, text string) {
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := func(target, at string) {
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}
	}

	dotdot := filepath.Join(root, "dotdot")
	real, cfg := filepath.Join(dotdot, "real"), filepath.Join(dotdot, "cfg")
	mk(filepath.Join(real, "sub"), filepath.Join(cfg, "Music"))
	link(filepath.Join(real, "sub"), filepath.Join(cfg, "link"))
	write(filepath.Join(real, "bridge.yaml"), configText(adminAddr, "Real"))
	decoyData := filepath.Join(dotdot, "decoy-data")
	write(filepath.Join(cfg, "bridge.yaml"), "libraryRoots:\n  - "+filepath.Join(cfg, "Music")+"\ndataDir: "+decoyData+
		"\nadminAddress: "+adminAddr+"\nlibraryName: Decoy\n")

	final := filepath.Join(root, "final")
	dot, etc := filepath.Join(final, "dot"), filepath.Join(final, "etc")
	mk(dot, filepath.Join(etc, "Music"))
	write(filepath.Join(dot, "actual.yaml"), configText(adminAddr, "Linked"))
	link(filepath.Join(dot, "actual.yaml"), filepath.Join(etc, "bridge.yaml"))

	return map[string]linkedLayout{
		"a dot-dot after a link": {
			// Joined by hand: filepath.Join would clean the `..` away.
			name: filepath.Join(cfg, "link") + "/../bridge.yaml", file: filepath.Join(real, "bridge.yaml"),
			base: cfg, lock: filepath.Join(real, ".bridge.yaml.lock"), decoy: filepath.Join(cfg, "bridge.yaml"),
		},
		"a link at the last element": {
			name: filepath.Join(etc, "bridge.yaml"), file: filepath.Join(dot, "actual.yaml"),
			base: etc, lock: filepath.Join(dot, ".actual.yaml.lock"), link: filepath.Join(etc, "bridge.yaml"),
		},
	}
}

// requireOneFile fails the test unless the config at l.file loads with
// libraryName name, its data dir and library roots against l.base (roots
// as given, under l.base), and the turn took the lock beside l.file, named
// after it, and no other: the link, when there is one, still leads to the
// file, and the decoy is as it was.
func requireOneFile(t *testing.T, l linkedLayout, name string, roots []string, decoy []byte) {
	t.Helper()
	cfg, err := config.Load(l.file)
	if err != nil {
		t.Fatalf("the config the name opens: %v", err)
	}
	if cfg.LibraryName != name {
		t.Errorf("%s has libraryName %q, want %q: the entry saved another file", l.file, cfg.LibraryName, name)
	}
	if want := filepath.Join(l.base, "data"); cfg.DataDir != want {
		t.Errorf("%s names the data dir %s, want %s: the base its relative paths resolve against moved", l.file, cfg.DataDir, want)
	}
	if !slices.Equal(cfg.LibraryRoots, roots) {
		t.Errorf("%s names the roots %q, want %q", l.file, cfg.LibraryRoots, roots)
	}
	if _, err := os.Stat(l.lock); err != nil {
		t.Errorf("no lock beside the file, named after it (%s): %v", l.lock, err)
	}
	for _, other := range []string{
		filepath.Join(filepath.Dir(l.name), "."+filepath.Base(l.name)+".lock"),
		filepath.Join(filepath.Clean(filepath.Dir(l.name)), ".bridge.yaml.lock"),
	} {
		if other == l.lock {
			continue
		}
		if _, err := os.Stat(other); err == nil {
			t.Errorf("a lock was taken at %s, beside a file the name does not open", other)
		}
	}
	if l.link != "" {
		if info, err := os.Lstat(l.link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s is no longer a link (%v, %v): a save renamed a file over it", l.link, info, err)
		} else if to, _ := os.Readlink(l.link); to != l.file {
			t.Errorf("%s leads to %s, want %s", l.link, to, l.file)
		}
	}
	if l.decoy != "" {
		if got := readBytes(t, l.decoy); string(got) != string(decoy) {
			t.Errorf("the entry wrote %s, the file the name cleaned lexically names:\n%s", l.decoy, got)
		}
	}
}

// TestServeOpensTheConfigItsNameOpens: serve given a config through a link
// loads, locks and saves the file the OS opens for that name, and resolves
// the config's relative data dir and library root against the directory
// config.Load takes from the name, as it always has: its data dir, where its
// database and identity are, does not move.
func TestServeOpensTheConfigItsNameOpens(t *testing.T) {
	for _, name := range []string{"a dot-dot after a link", "a link at the last element"} {
		t.Run(name, func(t *testing.T) {
			lan, admin := holdLoopback(t), holdLoopback(t)
			l := layouts(t, admin.addr)[name]
			var decoy []byte
			if l.decoy != "" {
				decoy = readBytes(t, l.decoy)
			}
			opts := serveOpts{configPath: l.name, addrOverride: lan.addr, lanListener: lan.ln, adminListener: admin.ln}
			b := &consoleBridge{
				adminBase: "http://" + admin.addr,
				console:   &http.Client{Timeout: 30 * time.Second},
				phone:     &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
			}
			b.servedBridge = launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
				return runServe(ctx, opts, stdout, stderr)
			})
			waitForAdminReady(t, admin.addr, b.done, b.stderr)
			if status, body := patchSettings(t, b, `{"libraryName":"After"}`); status != http.StatusOK {
				t.Fatalf("the console's save: %d %s\nstderr:\n%s", status, body, b.stderr.String())
			}
			requireOneFile(t, l, "After", []string{filepath.Join(l.base, "Music")}, decoy)
			if _, err := os.Stat(filepath.Join(l.base, "data", serveLockFileName)); err != nil {
				t.Errorf("serve's data dir is not %s: %v", filepath.Join(l.base, "data"), err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(l.file), "data")); err == nil && filepath.Dir(l.file) != l.base {
				t.Errorf("serve made a data dir beside %s: the base its relative paths resolve against moved", l.file)
			}
		})
	}
}

// TestLibraryAddWritesTheConfigItsNameOpens: library add, a CLI writer,
// given a config through a link, writes the file the OS opens for that name,
// leaves a link at the last element in place, and resolves the config's
// relative paths against the directory config.Load takes from the name.
func TestLibraryAddWritesTheConfigItsNameOpens(t *testing.T) {
	for _, name := range []string{"a dot-dot after a link", "a link at the last element"} {
		t.Run(name, func(t *testing.T) {
			l := layouts(t, closedAdminAddr(t))[name]
			var decoy []byte
			if l.decoy != "" {
				decoy = readBytes(t, l.decoy)
			}
			extra := filepath.Join(l.base, "Extra")
			if err := os.MkdirAll(extra, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(l.base, "data"), 0o700); err != nil {
				t.Fatal(err)
			}
			if code, out := runLibraryCapture(libraryEnv{}, "add", "--config", l.name, extra); code != 0 {
				t.Fatalf("library add: exit %d\n%s", code, out)
			}
			cfg, err := config.Load(l.file)
			if err != nil {
				t.Fatal(err)
			}
			requireOneFile(t, l, cfg.LibraryName, []string{filepath.Join(l.base, "Music"), extra}, decoy)
		})
	}
}

// repointLink makes link lead to to, in one rename, as a dotfiles restow or a
// home-manager switch replaces it.
func repointLink(t *testing.T, link, to string) {
	t.Helper()
	tmp := link + ".repoint"
	if err := os.Symlink(to, tmp); err != nil {
		t.Error(err)
		return
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Error(err)
	}
}

// twinBeside writes a file holding file's bytes beside it, named
// switched.yaml, and returns its path and those bytes.
func twinBeside(t *testing.T, file string) (string, []byte) {
	t.Helper()
	raw := readBytes(t, file)
	twin := filepath.Join(filepath.Dir(file), "switched.yaml")
	if err := os.WriteFile(twin, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return twin, raw
}

// TestLibraryAddRefusesANameRepointedWhileItWaited: library add, waiting for
// the lock beside the file its config's name opened while the link at that
// name is repointed to a file holding the same bytes, and the lock given back
// with the file unchanged, refuses once it holds the lock, saying the name now
// opens another file and to run it again: neither file is written, and the
// library index is not touched (the wipe a second root takes never runs).
// Until B506's fourth round it saved the file the name no longer opened, and
// wiped the index, and said it had added the root.
func TestLibraryAddRefusesANameRepointedWhileItWaited(t *testing.T) {
	l := layouts(t, closedAdminAddr(t))["a link at the last element"]
	moved, raw := twinBeside(t, l.file)
	extra := filepath.Join(l.base, "Extra")
	dataDir := filepath.Join(l.base, "data")
	for _, d := range []string{extra, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	release := holdConfigLock(t, l.file)
	waited, stop := config.LockWaitedForTest(l.name)
	defer stop()
	type result struct {
		code int
		out  string
	}
	done := make(chan result, 1)
	go func() {
		code, out := runLibraryCapture(libraryEnv{}, "add", "--config", l.name, extra)
		done <- result{code, out}
	}()
	select {
	case <-waited:
	case r := <-done:
		t.Fatalf("library add went through while another held the lock: exit %d\n%s", r.code, r.out)
	case <-time.After(30 * time.Second):
		t.Fatal("library add neither waited for the lock nor finished")
	}
	repointLink(t, l.link, moved)
	release()
	var r result
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("library add did not finish once the lock was given back")
	}
	if r.code == 0 || !strings.Contains(r.out, "now opens another file") || !strings.Contains(r.out, "run it again") {
		t.Errorf("library add whose config's name was repointed while it waited: exit %d\n%s\nwant a refusal that says the name moved and to run it again", r.code, r.out)
	}
	for _, p := range []string{l.file, moved} {
		if got := readBytes(t, p); string(got) != string(raw) {
			t.Errorf("library add wrote %s:\n%s", p, got)
		}
	}
	requireLinkTo(t, l.link, moved)
	if _, err := os.Stat(filepath.Join(dataDir, "bridge.db")); !os.IsNotExist(err) {
		t.Errorf("library add changed the library index before it refused (bridge.db: %v)", err)
	}
}

// repointDuringRead makes link lead to moved just before the nth read of the
// config that a turn or a writer's own comparison makes through
// config.ReadFile, counting from the turn's own read once it holds the lock,
// and returns the path that read was of, "" until it is made.
func repointDuringRead(t *testing.T, link, moved string, n int) (readOf *string) {
	t.Helper()
	reads, of := 0, ""
	t.Cleanup(config.SetTurnReadForTest(func(p string) ([]byte, error) {
		if reads++; reads == n {
			repointLink(t, link, moved)
			of = p
		}
		return fsutil.ReadAsFile(p)
	}))
	return &of
}

// TestLibraryAddRefusesANameRepointedDuringItsTurnsRead: library add whose
// config's name is repointed while its turn makes its own read, once it holds
// the lock, to a file holding the same bytes, refuses before it changes
// anything, saying the name now opens another file and to run it again: the
// wipe a second root takes never runs, and neither file is written. Until
// B506's fifth round the turn resolved the name before that read, the add
// wiped the index on a turn whose name no longer opened the file it had read,
// and only its save refused.
func TestLibraryAddRefusesANameRepointedDuringItsTurnsRead(t *testing.T) {
	l := layouts(t, closedAdminAddr(t))["a link at the last element"]
	moved, raw := twinBeside(t, l.file)
	extra := filepath.Join(l.base, "Extra")
	dataDir := filepath.Join(l.base, "data")
	for _, d := range []string{extra, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	readOf := repointDuringRead(t, l.link, moved, 1)
	code, out := runLibraryCapture(libraryEnv{}, "add", "--config", l.name, extra)
	if *readOf != l.file {
		t.Fatalf("premise: the first read was of %q, want the turn's own, of %s", *readOf, l.file)
	}
	if code == 0 || !strings.Contains(out, "now opens another file") || !strings.Contains(out, "run it again") {
		t.Errorf("library add whose config's name moved during its turn's read: exit %d\n%s\nwant a refusal that says the name moved and to run it again", code, out)
	}
	for _, p := range []string{l.file, moved} {
		if got := readBytes(t, p); string(got) != string(raw) {
			t.Errorf("library add wrote %s:\n%s", p, got)
		}
	}
	requireLinkTo(t, l.link, moved)
	if _, err := os.Stat(filepath.Join(dataDir, "bridge.db")); !os.IsNotExist(err) {
		t.Errorf("library add changed the library index before it refused (bridge.db: %v)", err)
	}
}

// TestBindRefusesANameRepointedDuringItsOwnComparison: bind compares the
// config through its name once more before its save's rename (SaveIf), and a
// link at that name repointed during that read, to a file holding the same
// bytes, refuses the binding, saying the name now opens another file and to
// run it again: neither file is written. Until B506's fifth round the name was
// resolved before bind's comparison, and the binding was written into the
// file the name no longer opened.
func TestBindRefusesANameRepointedDuringItsOwnComparison(t *testing.T) {
	root := dacTree(t)
	link := dacConfig(t, "  player:\n    room: Study\n")
	store := filepath.Join(filepath.Dir(link), "store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	actual := filepath.Join(store, "actual.yaml")
	if err := os.Rename(link, actual); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	moved, raw := twinBeside(t, actual)
	readOf := repointDuringRead(t, link, moved, 3)
	_, errOut, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", link}, treeFlags(root)...)...)
	if *readOf != link {
		t.Fatalf("premise: the third read was of %q, want bind's own comparison's, of %s", *readOf, link)
	}
	if code == 0 || !strings.Contains(errOut, "now opens another file") || !strings.Contains(errOut, "run it again") {
		t.Errorf("bind whose config's name moved during its own comparison: exit %d, stderr %q; want a refusal that says the name moved and to run it again", code, errOut)
	}
	for _, p := range []string{actual, moved} {
		if got := readBytes(t, p); string(got) != string(raw) {
			t.Errorf("bind wrote %s:\n%s", p, got)
		}
	}
	requireLinkTo(t, link, moved)
}

// TestServeRecordsTheFileItsBootReadOpened: serve's live holder is told the
// file its boot read opened, resolved at that read, never later: a link at
// the config's name repointed between the read and the holder's record (a
// thousand lines of boot apart), to a file holding the same bytes, makes the
// first console save refuse, both files left as they are. Until B506's fourth
// round the holder resolved the name when it was told, and recorded the file
// the link led to by then, which serve never read.
func TestServeRecordsTheFileItsBootReadOpened(t *testing.T) {
	lan, admin := holdLoopback(t), holdLoopback(t)
	l := layouts(t, admin.addr)["a link at the last element"]
	moved, raw := twinBeside(t, l.file)
	opts := serveOpts{configPath: l.name, addrOverride: lan.addr, lanListener: lan.ln, adminListener: admin.ln,
		afterConfigRead: func() { repointLink(t, l.link, moved) }}
	b := &consoleBridge{
		adminBase: "http://" + admin.addr,
		console:   &http.Client{Timeout: 30 * time.Second},
		phone:     &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
	}
	b.servedBridge = launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, opts, stdout, stderr)
	})
	waitForAdminReady(t, admin.addr, b.done, b.stderr)
	if status, body := patchSettings(t, b, `{"libraryName":"After"}`); status != http.StatusInternalServerError || !strings.Contains(body, "save-config") {
		t.Errorf("the first console save after the name moved past serve's boot read: %d %s, want it refused", status, body)
	}
	for _, p := range []string{l.file, moved} {
		if got := readBytes(t, p); string(got) != string(raw) {
			t.Errorf("the console save wrote %s:\n%s", p, got)
		}
	}
	requireLinkTo(t, l.link, moved)
}

// TestServesSeedRefusesALinkThatLeadsNowhere: serve's seed, given a config
// name that is a link to no file, writes nothing and says what to do: it
// neither makes the file the link names nor replaces the link with one.
func TestServesSeedRefusesALinkThatLeadsNowhere(t *testing.T) {
	dir := resolvedTempDir(t)
	link := filepath.Join(dir, "bridge.yaml")
	nowhere := filepath.Join(dir, "elsewhere", "bridge.yaml")
	if err := os.Symlink(nowhere, link); err != nil {
		t.Fatal(err)
	}
	wrote, err := writeAutoInitConfig(link)
	if err == nil || wrote {
		t.Fatalf("the seed at a link that leads nowhere: wrote %v, %v; want a refusal", wrote, err)
	}
	if msg := err.Error(); !strings.Contains(msg, link) || !strings.Contains(msg, "point the link") {
		t.Errorf("refusal %q: want it to name %s and say what to do", msg, link)
	}
	requireLinkTo(t, link, nowhere)
	if _, err := os.Lstat(nowhere); !os.IsNotExist(err) {
		t.Errorf("the seed made the link's target: %v", err)
	}
}

// TestInitRefusesALinkThatLeadsNowhereBeforeItWritesAnything: init, whose
// config is a link to no file, refuses before it writes anything, its data
// dir and TLS pair included, and says what to do.
func TestInitRefusesALinkThatLeadsNowhereBeforeItWritesAnything(t *testing.T) {
	cfgDir := filepath.Join(resolvedTempDir(t), "cfg")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cfgDir, "bridge.yaml")
	nowhere := filepath.Join(filepath.Dir(cfgDir), "gone.yaml")
	if err := os.Symlink(nowhere, link); err != nil {
		t.Fatal(err)
	}
	code, out := runInit(t, "--yes", "--no-service", "--skip-doctor", "--dir", cfgDir, "--library", testLibrary(t))
	if code == 0 {
		t.Fatalf("init over a config that is a link to nothing exited 0:\n%s", out)
	}
	if !strings.Contains(out, link) || !strings.Contains(out, "point the link") {
		t.Errorf("init's refusal does not name %s and say what to do:\n%s", link, out)
	}
	requireLinkTo(t, link, nowhere)
	for _, made := range []string{nowhere, initDataDirFor(cfgDir)} {
		if _, err := os.Lstat(made); !os.IsNotExist(err) {
			t.Errorf("init wrote %s before refusing: %v", made, err)
		}
	}
}

// TestInitMakesANestedConfigDirUnderALinkedAncestor: init's config dir may
// not exist yet, below an ancestor reached through a link: the config is
// written where the OS puts it, and loads.
func TestInitMakesANestedConfigDirUnderALinkedAncestor(t *testing.T) {
	base := resolvedTempDir(t)
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(base, "sym")); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(base, "sym", "new", "nested", "dir")
	listen, adminPort := freeLoopbackPort(t), freeLoopbackPort(t)
	code, out := runInit(t, "--yes", "--no-service", "--skip-doctor", "--dir", cfgDir, "--library", testLibrary(t),
		"--listen-address", loopbackAddr(listen), "--admin-address", loopbackAddr(adminPort))
	if code != 0 {
		t.Fatalf("init in a new nested dir under a linked ancestor: exit %d\n%s", code, out)
	}
	if _, err := config.Load(filepath.Join(real, "new", "nested", "dir", "bridge.yaml")); err != nil {
		t.Errorf("the config init wrote, where the OS puts it: %v", err)
	}
}

// resolvedTempDir is t.TempDir() as the OS resolves it (on macOS the temp
// directory is reached through /var, a link to /private/var), so that the
// paths these tests expect are spelled as the files' own.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// requireLinkTo fails the test unless link is still a symbolic link leading
// to target.
func requireLinkTo(t *testing.T, link, target string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is no longer a link (%v, %v)", link, info, err)
	}
	if to, err := os.Readlink(link); err != nil || to != target {
		t.Errorf("%s leads to %q (%v), want %s", link, to, err, target)
	}
}
