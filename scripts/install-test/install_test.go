//go:build !windows

package installtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// unitName is the per-user unit bridge init installs.
	unitName = "com.acoseac.1-bit-bridge.service"
	// stampedRelease is the release the stamped copy of the installer came
	// with, the one every rig publishes.
	stampedRelease = "0.2.2"
	// runTimeout bounds one run of the installer: a run that has not ended by
	// then waits on something, the failure the no-terminal tests look for.
	runTimeout = 90 * time.Second
)

// repoScript is the installer as the repository holds it, unstamped.
var repoScript = filepath.Join("..", "install.sh")

// stampedScript is the installer as a release attaches it, stamped with
// stampedRelease by scripts/stamp-install.sh in TestMain.
var stampedScript string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "install-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	stampedScript = filepath.Join(dir, "install.sh")
	if repoScript, err = filepath.Abs(repoScript); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sh, err := exec.LookPath("sh")
	if err == nil {
		var out []byte
		out, err = exec.Command(sh, filepath.Join("..", "stamp-install.sh"), stampedRelease, repoScript, stampedScript).CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%v: %s", err, out)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stamping the installer:", err)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// shell is a POSIX shell the installer runs under: its program and the
// arguments before the script's.
type shell struct {
	name string
	argv []string
}

// knownShells are the POSIX shells the installer is run under where they
// are installed (BRIDGE_INSTALL_TEST_SHELLS=all): Debian's and Ubuntu's sh,
// bash in its POSIX mode, BusyBox's (Volumio's and small images'), and ksh.
var knownShells = []string{"sh", "dash", "bash --posix", "busybox sh", "ksh"}

// shellsUnderTest reads BRIDGE_INSTALL_TEST_SHELLS: shells separated by
// commas, each a command line (dash, bash --posix, busybox sh), or all, the
// knownShells this host has. The default is sh. A shell the list names that
// is not on PATH fails the test: the list is what was asked for.
func shellsUnderTest(t *testing.T) []shell {
	t.Helper()
	spec := strings.TrimSpace(os.Getenv("BRIDGE_INSTALL_TEST_SHELLS"))
	if spec == "" {
		spec = "sh"
	}
	items := strings.Split(spec, ",")
	all := spec == "all"
	if all {
		items = knownShells
	}
	var shells []shell
	for _, item := range items {
		fields := strings.Fields(item)
		if len(fields) == 0 {
			continue
		}
		path, err := exec.LookPath(fields[0])
		if err == nil && len(fields) > 1 {
			// A multi-call program (busybox) may lack the applet.
			err = exec.Command(path, append(fields[1:], "-c", ":")...).Run()
		}
		if err != nil {
			if all {
				continue
			}
			t.Fatalf("BRIDGE_INSTALL_TEST_SHELLS names %q, which this host cannot run: %v", item, err)
		}
		shells = append(shells, shell{name: strings.Join(fields, "_"), argv: append([]string{path}, fields[1:]...)})
	}
	if len(shells) == 0 {
		t.Fatalf("BRIDGE_INSTALL_TEST_SHELLS=%q names no shell this host has", spec)
	}
	return shells
}

// eachShell runs fn as a parallel subtest under every shell under test, in
// parallel with the other tests: every rig is a box of its own.
func eachShell(t *testing.T, fn func(t *testing.T, sh shell)) {
	t.Helper()
	t.Parallel()
	for _, sh := range shellsUnderTest(t) {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			fn(t, sh)
		})
	}
}

// releaseServer serves releases the way GitHub's download URLs do, under
// /acoseac/1-bit-bridge/releases/download/v<version>/<asset>, over TLS with
// a certificate of its own, which the installer's curl trusts through
// CURL_CA_BUNDLE. It records every path asked for.
type releaseServer struct {
	t      *testing.T
	srv    *httptest.Server
	caFile string

	mu       sync.Mutex
	files    map[string][]byte
	requests []string
}

const releasePath = "/acoseac/1-bit-bridge/releases"

func newReleaseServer(t *testing.T, dir string) *releaseServer {
	t.Helper()
	s := &releaseServer{t: t, files: map[string][]byte{}}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	s.caFile = filepath.Join(dir, "ca.pem")
	writeFile(t, s.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw}), 0o644)
	return s
}

func (s *releaseServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.URL.Path)
	body, ok := s.files[r.URL.Path]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(body)
}

func (s *releaseServer) base() string {
	return s.srv.URL + releasePath + "/download"
}

func assetPath(version, name string) string {
	return releasePath + "/download/v" + version + "/" + name
}

func (s *releaseServer) put(path string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[path] = body
}

func (s *releaseServer) remove(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.files, path)
}

func (s *releaseServer) get(path string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.files[path]
}

func (s *releaseServer) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func archiveName(version, arch string) string {
	return "1-bit-bridge_" + version + "_linux_" + arch + ".tar.gz"
}

// publish puts release VERSION on the server as goreleaser lays one out: a
// tar.gz per Linux architecture holding the bridge program (the fake one for
// VERSION, unless program is given) beside the release's documents, and
// checksums.txt with a line for every archive, another platform's included.
func (s *releaseServer) publish(version string, program []byte) {
	s.t.Helper()
	if program == nil {
		program = fakeBridge(s.t, version)
	}
	var sums strings.Builder
	for _, arch := range []string{"amd64", "arm64"} {
		name := archiveName(version, arch)
		archive := tarGz(s.t, []tarEntry{
			{"bridge", program, 0o755},
			{"README.md", []byte("# 1-bit-bridge\n"), 0o644},
			{"LICENSE", []byte("FSL-1.1-MIT\n"), 0o644},
			{"THIRD_PARTY_LICENSES.md", []byte("# Notices\n"), 0o644},
			{"PROTOCOL.md", []byte("# Protocol\n"), 0o644},
		})
		s.put(assetPath(version, name), archive)
		sum := sha256.Sum256(archive)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	fmt.Fprintf(&sums, "%s  1-bit-bridge_%s_macos_arm64.tar.gz\n", strings.Repeat("ab", 32), version)
	s.put(assetPath(version, "checksums.txt"), []byte(sums.String()))
}

type tarEntry struct {
	name string
	body []byte
	mode int64
}

func tarGz(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg, ModTime: time.Unix(1_790_000_000, 0)}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeBridge is testdata/bridge for release VERSION.
func fakeBridge(t *testing.T, version string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "bridge"))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(b, []byte("@VERSION@"), []byte(version))
}

// realTools are the programs the installer and the stubs use that a rig
// links into its PATH from the host's; the rest of the PATH is stubs.
var realTools = []string{
	"awk", "basename", "cat", "chmod", "cp", "curl", "cut", "dirname", "grep", "gzip", "head", "ln",
	"ls", "mkdir", "mktemp", "mv", "od", "readlink", "rm", "rmdir", "sed", "sha256sum", "shasum",
	"tar", "tee", "touch", "tr", "wget",
}

// rig is one box for the installer: a HOME, a system root the script reads
// its /etc, /proc, /run and music folders under, a PATH of real tools and
// stubs, a release server with stampedRelease published, and the log every
// stub and the fake bridge append their calls to.
type rig struct {
	t      *testing.T
	sh     shell
	dir    string
	home   string
	root   string
	state  string
	bin    string
	tmp    string
	log    string
	script string
	server *releaseServer
	env    map[string]string
}

// newRig builds a box with a Debian system running systemd, and a PATH
// without the tools omit names.
func newRig(t *testing.T, sh shell, omit ...string) *rig {
	t.Helper()
	// Resolved, as bridge init resolves the program's path it writes into the
	// unit (macOS's temporary directory is under the /var link).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{
		t:      t,
		sh:     sh,
		dir:    dir,
		home:   filepath.Join(dir, "home"),
		root:   filepath.Join(dir, "root"),
		state:  filepath.Join(dir, "state"),
		bin:    filepath.Join(dir, "bin"),
		tmp:    filepath.Join(dir, "tmp"),
		log:    filepath.Join(dir, "calls.log"),
		script: stampedScript,
	}
	for _, d := range []string{r.home, r.state, r.bin, r.tmp, filepath.Join(r.root, "run", "systemd", "system")} {
		mkdirAll(t, d)
	}
	writeFile(t, r.log, nil, 0o644)
	r.osRelease("PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nNAME=\"Debian GNU/Linux\"\nID=debian\n")
	writeFile(t, filepath.Join(r.root, "proc", "mounts"), []byte("/dev/root / ext4 rw,relatime 0 0\nproc /proc proc rw 0 0\n"), 0o644)
	writeFile(t, filepath.Join(r.root, "proc", "cmdline"), []byte("console=tty1 root=/dev/mmcblk0p2 rootwait\n"), 0o644)

	skip := map[string]bool{}
	for _, name := range omit {
		skip[name] = true
	}
	for _, name := range realTools {
		if skip[name] {
			continue
		}
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if err := os.Symlink(path, filepath.Join(r.bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	stubs, err := os.ReadDir(filepath.Join("testdata", "bin"))
	if err != nil {
		t.Fatal(err)
	}
	for _, stub := range stubs {
		if skip[stub.Name()] {
			continue
		}
		b, err := os.ReadFile(filepath.Join("testdata", "bin", stub.Name()))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(r.bin, stub.Name()), b, 0o755)
	}

	r.server = newReleaseServer(t, dir)
	r.server.publish(stampedRelease, nil)
	wgetrc := filepath.Join(dir, "wgetrc")
	writeFile(t, wgetrc, []byte("ca_certificate = "+r.server.caFile+"\n"), 0o644)
	r.env = map[string]string{
		"HOME":                        r.home,
		"PATH":                        r.bin,
		"TMPDIR":                      r.tmp,
		"LANG":                        "C",
		"LC_ALL":                      "C",
		"USER":                        "tester",
		"LOGNAME":                     "tester",
		"BRIDGE_INSTALL_BASE_URL":     r.server.base(),
		"BRIDGE_INSTALL_TEST_SYSROOT": r.root,
		"CURL_CA_BUNDLE":              r.server.caFile,
		"WGETRC":                      wgetrc,
		"STUB_LOG":                    r.log,
		"STUB_STATE":                  r.state,
	}
	return r
}

func (r *rig) osRelease(text string) {
	writeFile(r.t, filepath.Join(r.root, "etc", "os-release"), []byte(text), 0o644)
}

func (r *rig) set(kv ...string) {
	for i := 0; i+1 < len(kv); i += 2 {
		r.env[kv[i]] = kv[i+1]
	}
}

func (r *rig) environ() []string {
	keys := make([]string, 0, len(r.env))
	for k := range r.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+r.env[k])
	}
	return env
}

// terminal makes the installer's terminal a file holding these lines, the
// answers typed at it, in the order they are asked for.
func (r *rig) terminal(lines ...string) {
	path := filepath.Join(r.dir, "tty")
	writeFile(r.t, path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	r.env["BRIDGE_INSTALL_TEST_TTY"] = path
}

type result struct {
	code   int
	stdout string
	stderr string
}

func (res result) output() string {
	return res.stdout + res.stderr
}

// run runs the installer as a file, sh install.sh ARGS.
func (r *rig) run(args ...string) result {
	r.t.Helper()
	argv := append(append([]string{}, r.sh.argv...), r.script)
	return r.exec(append(argv, args...), nil)
}

// runPiped runs it as curl | sh -s -- ARGS does: the script on standard input.
func (r *rig) runPiped(args ...string) result {
	r.t.Helper()
	script, err := os.ReadFile(r.script)
	if err != nil {
		r.t.Fatal(err)
	}
	argv := append(append([]string{}, r.sh.argv...), "-s", "--")
	return r.exec(append(argv, args...), bytes.NewReader(script))
}

// runHeldOpen runs it with a standard input that never ends: a pipe whose
// writer stays open. A run that reads it waits until runTimeout.
func (r *rig) runHeldOpen(args ...string) result {
	r.t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		r.t.Fatal(err)
	}
	defer pw.Close()
	defer pr.Close()
	argv := append(append([]string{}, r.sh.argv...), r.script)
	return r.exec(append(argv, args...), pr)
}

// exec runs argv in a session of its own, so it has no controlling terminal
// and /dev/tty cannot be opened, as in CI; a run past runTimeout is killed
// with everything it started, and fails the test.
func (r *rig) exec(argv []string, stdin io.Reader) result {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = r.environ()
	cmd.Dir = r.home
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	err := cmd.Run()
	if ctx.Err() != nil {
		r.t.Fatalf("the installer was still running after %v, waiting on something\nstdout:\n%s\nstderr:\n%s", runTimeout, &stdout, &stderr)
	}
	code := 0
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
	case err != nil:
		r.t.Fatalf("running the installer: %v", err)
	}
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// calls are the logged calls of the stubs and the fake bridge, in order.
func (r *rig) calls() []string {
	b, err := os.ReadFile(r.log)
	if err != nil {
		r.t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(b), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// callsTo are the logged calls that start with prefix ("bridge init").
func (r *rig) callsTo(prefix string) []string {
	var out []string
	for _, c := range r.calls() {
		if c == prefix || strings.HasPrefix(c, prefix+" ") {
			out = append(out, c)
		}
	}
	return out
}

func (r *rig) target() string {
	return filepath.Join(r.home, ".local", "bin", "bridge")
}

func (r *rig) unitFile() string {
	return filepath.Join(r.home, ".config", "systemd", "user", unitName)
}

func (r *rig) configFile() string {
	return filepath.Join(r.home, ".config", "1-bit-bridge", "bridge.yaml")
}

// music makes a music folder with a track in it.
func (r *rig) music(rel string) string {
	dir := filepath.Join(r.dir, rel)
	writeFile(r.t, filepath.Join(dir, "Artist", "Album", "01.flac"), []byte("fLaC"), 0o644)
	return dir
}

// installed lays out the bridge VERSION the way bridge init leaves it: the
// program at bin, the settings at config with a data directory beside them
// (an output block and a paired device included), the per-user unit naming
// both, and the service running VERSION.
func (r *rig) installed(version, bin, config string) {
	r.t.Helper()
	writeFile(r.t, bin, fakeBridge(r.t, version), 0o755)
	writeFile(r.t, config, []byte("libraryRoots:\n  - /srv/music\nlibraryName: Living room\noutput:\n  mpd: auto\n  player:\n    enabled: false\n"), 0o600)
	writeFile(r.t, filepath.Join(filepath.Dir(config), "data", "tokens.json"), []byte(`{"tokens":[{"id":"t1","name":"iPhone"}]}`), 0o600)
	writeFile(r.t, filepath.Join(filepath.Dir(config), "data", "server.crt"), []byte("CERTIFICATE\n"), 0o600)
	writeFile(r.t, r.unitFile(), []byte(unitText(bin, config)), 0o644)
	writeFile(r.t, filepath.Join(r.state, "active"), nil, 0o644)
	writeFile(r.t, filepath.Join(r.state, "served"), []byte(version+"\n"), 0o644)
}

// unitText is the unit bridge init writes (internal/packaging's
// systemd.service.tmpl) for a program and a config.
func unitText(bin, config string) string {
	return "[Unit]\nDescription=1-bit-bridge — companion server for the 1-bit iOS app\n" +
		"After=network-online.target\nWants=network-online.target\n\n[Service]\nType=simple\n" +
		"ExecStart=\"" + bin + "\" serve --config \"" + config + "\"\n" +
		"WorkingDirectory=" + filepath.Join(filepath.Dir(config), "data") + "\nRestart=always\nRestartSec=2\n" +
		"StandardOutput=append:/tmp/bridge.log\nStandardError=append:/tmp/bridge.log\n\n[Install]\nWantedBy=default.target\n"
}

// newRelease writes the fake bridge VERSION where an update can take it
// from, and points the fake bridge's update at it.
func (r *rig) newRelease(version string) {
	path := filepath.Join(r.dir, "release-"+version)
	writeFile(r.t, path, fakeBridge(r.t, version), 0o755)
	r.set("FAKE_UPDATE_TO", path, "FAKE_UPDATE_VERSION", version)
}

func writeFile(t *testing.T, path string, b []byte, mode os.FileMode) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, b, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// tree is every file under dir, by its path relative to dir, with its mode
// and contents: what "nothing changed" compares.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entry += " -> " + target
		case info.Mode().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry += " " + string(b)
		}
		files[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func sameTree(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	for path, entry := range before {
		if after[path] != entry {
			t.Errorf("%s: %s changed", what, path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s: %s appeared", what, path)
		}
	}
}

func mustExit(t *testing.T, res result, code int) {
	t.Helper()
	if res.code != code {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", res.code, code, res.stdout, res.stderr)
	}
}

func mustSay(t *testing.T, res result, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(res.output(), part) {
			t.Errorf("the installer did not say %q\nstdout:\n%s\nstderr:\n%s", part, res.stdout, res.stderr)
		}
	}
}

// nothingInstalled checks that a run left no program, no settings, no unit
// and no directory of its own in HOME, staged nothing, kept no temporary
// directory and called no service manager.
func (r *rig) nothingInstalled() {
	r.t.Helper()
	for _, path := range []string{filepath.Join(r.home, ".local"), filepath.Join(r.home, ".config")} {
		if exists(path) {
			r.t.Errorf("%s was created: %v", path, tree(r.t, r.home))
		}
	}
	if entries, _ := os.ReadDir(r.tmp); len(entries) != 0 {
		r.t.Errorf("the temporary directory was left behind: %v", entries)
	}
	for _, who := range []string{"systemctl", "loginctl", "sudo", "su", "bridge init"} {
		if calls := r.callsTo(who); len(calls) != 0 {
			r.t.Errorf("%s was called: %q", who, calls)
		}
	}
}

// asked are the release paths the installer requested.
func (r *rig) asked() []string {
	return r.server.asked()
}

func (r *rig) askedOnly(version string) {
	r.t.Helper()
	asked := r.asked()
	if len(asked) == 0 {
		r.t.Fatalf("the installer downloaded nothing")
	}
	for _, p := range asked {
		if !strings.HasPrefix(p, releasePath+"/download/v"+version+"/") {
			r.t.Errorf("the installer asked for %s, outside release %s", p, version)
		}
	}
}

// A new box: the release the installer came with is downloaded from its own
// tag, checked, installed as ~/.local/bin/bridge, set up by bridge init with
// the library given, and the service it installs runs that release, with
// lingering turned on through sudo, which says why first.
func TestAFreshInstallSetsTheBridgeUpAsTheUsersService(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		for _, piped := range []bool{false, true} {
			t.Run(map[bool]string{false: "file", true: "piped"}[piped], func(t *testing.T) {
				r := newRig(t, sh)
				music := r.music("music")
				var res result
				if piped {
					res = r.runPiped("--library", music)
				} else {
					res = r.run("--library", music)
				}
				mustExit(t, res, 0)
				r.askedOnly(stampedRelease)
				for _, p := range r.asked() {
					if strings.Contains(p, "latest") {
						t.Errorf("the installer asked for %s: it must not ask GitHub which release is the latest", p)
					}
				}
				info, err := os.Stat(r.target())
				if err != nil {
					t.Fatalf("no program at %s: %v", r.target(), err)
				}
				if info.Mode().Perm() != 0o755 {
					t.Errorf("the program's mode is %v, want 0755", info.Mode().Perm())
				}
				if got := readFile(t, r.target()); got != string(fakeBridge(t, stampedRelease)) {
					t.Errorf("the program installed is not the release's")
				}
				if got, want := r.callsTo("bridge init"), []string{"bridge init --yes --library " + music}; strings.Join(got, "|") != strings.Join(want, "|") {
					t.Errorf("bridge init calls %q, want %q", got, want)
				}
				if !strings.Contains(readFile(t, r.configFile()), music) {
					t.Errorf("the settings do not name the library")
				}
				if !strings.Contains(readFile(t, r.unitFile()), `ExecStart="`+r.target()+`" serve --config "`+r.configFile()+`"`) {
					t.Errorf("the unit does not run the installed program: %s", readFile(t, r.unitFile()))
				}
				if got := r.callsTo("sudo"); len(got) != 1 || got[0] != "sudo -n loginctl enable-linger tester" {
					t.Errorf("sudo calls %q, want one sudo -n loginctl enable-linger tester", got)
				}
				if !exists(filepath.Join(r.state, "linger")) {
					t.Errorf("lingering was not turned on")
				}
				if len(r.callsTo("bridge output player")) != 1 || len(r.callsTo("bridge output dac")) != 1 {
					t.Errorf("the box's player and sound cards were not reported: %q", r.calls())
				}
				for _, c := range r.callsTo("bridge output") {
					if strings.Contains(c, "bind") {
						t.Errorf("the installer bound a DAC: %s", c)
					}
				}
				mustSay(t, res,
					"Found Debian GNU/Linux 12 (bookworm), amd64.",
					"Its SHA-256 matches the release's checksums.txt.",
					"Installed the bridge 0.2.2 at "+r.target(),
					"sudo runs loginctl enable-linger tester",
					"The bridge 0.2.2 is running as tester's service",
					"ssh -L 7789:127.0.0.1:7789 tester@192.0.2.10",
					"http://127.0.0.1:7789/",
					r.target()+` pair --name "My iPhone"`,
				)
				if entries, _ := os.ReadDir(r.tmp); len(entries) != 0 {
					t.Errorf("the temporary directory was left behind: %v", entries)
				}
			})
		}
	})
}

// With a terminal, the installer offers the box's usual music folders and
// takes the choice, sudo asks for its password on the terminal, and bridge
// init runs with its questions on the terminal too: none of them reads the
// piped script.
func TestAFreshInstallAsksOnTheTerminal(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh)
		r.osRelease("PRETTY_NAME=\"Raspbian GNU/Linux 12 (bookworm)\"\nID=raspbian\n")
		writeFile(t, filepath.Join(r.root, "usr", "local", "bin", "moodeutl"), []byte("#!/bin/sh\n"), 0o755)
		for _, d := range []string{"mnt/SDCARD", "media", "mnt/NAS"} {
			mkdirAll(t, filepath.Join(r.root, d))
		}
		writeFile(t, filepath.Join(r.root, "media", "USB", "01.flac"), []byte("fLaC"), 0o644)
		r.set("STUB_SUDO_ASKS", "1")
		r.terminal("2", "secret", "Living room")
		res := r.runPiped()
		mustExit(t, res, 0)
		media := filepath.Join(r.root, "media")
		if got := r.callsTo("bridge init"); len(got) != 1 || got[0] != "bridge init --library "+media {
			t.Errorf("bridge init calls %q, want one bridge init --library %s (no --yes: it asks on the terminal)", got, media)
		}
		calls := strings.Join(r.calls(), "\n")
		for _, want := range []string{"stub sudo read secret", "fake init read name Living room"} {
			if !strings.Contains(calls, want) {
				t.Errorf("the calls do not show %q:\n%s", want, calls)
			}
		}
		if strings.Index(calls, "stub sudo read secret") > strings.Index(calls, "fake init read name") {
			t.Errorf("sudo asked after bridge init:\n%s", calls)
		}
		mustSay(t, res, "Found moOde audio player, on Raspbian GNU/Linux 12 (bookworm), amd64.",
			"1) "+filepath.Join(r.root, "mnt", "SDCARD")+" (empty now)", "2) "+media+"\n", "4) another folder")
		if strings.Contains(res.output(), "2) "+media+" (empty now)") {
			t.Errorf("a folder with music in it was called empty")
		}
	})
}

// With a terminal and no usual folder, the installer asks for the path.
func TestAFreshInstallAsksForAFolderWhenItKnowsNone(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh)
		music := r.music("Music Library")
		r.set("STUB_SELF_LINGER", "1")
		r.terminal(filepath.Join(r.dir, "nowhere"), music, "")
		res := r.run()
		mustExit(t, res, 0)
		if got := r.callsTo("bridge init"); len(got) != 1 || got[0] != "bridge init --library "+music {
			t.Errorf("bridge init calls %q", got)
		}
		mustSay(t, res, filepath.Join(r.dir, "nowhere")+" is not a folder.")
		if len(r.callsTo("sudo")) != 0 {
			t.Errorf("sudo was called although the session turns lingering on itself: %q", r.callsTo("sudo"))
		}
	})
}

// Without a terminal and without --library, the installer installs the
// program, asks nothing (it never reads its standard input, which under curl
// | sh is the script), names the folders it found and the command that
// finishes, and exits 4.
func TestWithoutATerminalOrALibraryTheInstallerDoesNotWait(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh)
		r.osRelease("PRETTY_NAME=\"Raspbian GNU/Linux 11 (bullseye)\"\nID=raspbian\nVOLUMIO_VERSION=\"3.661\"\n")
		for _, d := range []string{"volumio", "data/configuration", "mnt/NAS", "data/INTERNAL"} {
			mkdirAll(t, filepath.Join(r.root, d))
		}
		writeFile(t, filepath.Join(r.root, "mnt", "USB", "Drive", "01.flac"), []byte("fLaC"), 0o644)
		res := r.runHeldOpen()
		mustExit(t, res, 4)
		if !exists(r.target()) {
			t.Errorf("the program was not installed")
		}
		if len(r.callsTo("bridge init")) != 0 || len(r.callsTo("sudo")) != 0 || len(r.callsTo("loginctl")) != 0 {
			t.Errorf("setup ran without a library: %q", r.calls())
		}
		mustSay(t, res, "Found Volumio 3.661, amd64.", "but not set up: no music folder was given",
			"Music folders found here: "+filepath.Join(r.root, "mnt", "USB"), "--library /path/to/music")
	})
}

// --no-init installs or upgrades the program and sets nothing up.
func TestNoInitInstallsTheProgramAlone(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh)
		res := r.runHeldOpen("--no-init")
		mustExit(t, res, 0)
		if !exists(r.target()) {
			t.Fatalf("the program was not installed")
		}
		for _, who := range []string{"bridge init", "systemctl", "loginctl", "sudo"} {
			if len(r.callsTo(who)) != 0 {
				t.Errorf("%s was called: %q", who, r.callsTo(who))
			}
		}
		if exists(r.configFile()) || exists(r.unitFile()) {
			t.Errorf("settings or a unit were written")
		}
		mustSay(t, res, "Set it up when you are ready")
	})
}

// The installer downloads from one release's tag: the one it was stamped
// with, or the one --version names; never latest, which can change between
// the script's download and the archive's.
func TestTheReleaseIsPinned(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		t.Run("stamped", func(t *testing.T) {
			r := newRig(t, sh)
			r.server.publish("0.2.3", nil)
			r.server.put(releasePath+"/latest/download/checksums.txt", r.server.get(assetPath("0.2.3", "checksums.txt")))
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 0)
			r.askedOnly(stampedRelease)
		})
		for _, given := range []string{"0.2.1", "v0.2.1"} {
			t.Run("version "+given, func(t *testing.T) {
				r := newRig(t, sh)
				r.server.publish("0.2.1", nil)
				res := r.run("--version", given, "--library", r.music("music"))
				mustExit(t, res, 0)
				r.askedOnly("0.2.1")
				if !strings.Contains(readFile(t, r.target()), "VERSION='0.2.1'") {
					t.Errorf("the program installed is not 0.2.1")
				}
			})
		}
		t.Run("unpublished", func(t *testing.T) {
			r := newRig(t, sh)
			res := r.run("--version", "9.9.9", "--library", r.music("music"))
			mustExit(t, res, 1)
			mustSay(t, res, "is 9.9.9 a published release?")
			r.nothingInstalled()
		})
		for _, bad := range []string{"latest", "0.2", "0.2.2;rm", "1.2.3\n4.5.6", ""} {
			t.Run(fmt.Sprintf("refused %q", bad), func(t *testing.T) {
				r := newRig(t, sh)
				res := r.run("--version", bad)
				mustExit(t, res, 2)
				if len(r.asked()) != 0 {
					t.Errorf("a refused version downloaded %q", r.asked())
				}
				r.nothingInstalled()
			})
		}
		t.Run("unstamped", func(t *testing.T) {
			r := newRig(t, sh)
			r.script = repoScript
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 2)
			mustSay(t, res, "is not from a release", "--version X.Y.Z")
			if len(r.asked()) != 0 {
				t.Errorf("an unstamped copy downloaded %q", r.asked())
			}
			r.nothingInstalled()
			res = r.run("--version", stampedRelease, "--library", r.music("music"))
			mustExit(t, res, 0)
		})
	})
}

// A download that does not check out installs nothing: an archive whose
// SHA-256 is not the release's, a checksums.txt that does not give it one, a
// failed download, an archive without the program, or a program that does
// not run here or is another release.
func TestADownloadThatDoesNotCheckOutInstallsNothing(t *testing.T) {
	archive := assetPath(stampedRelease, archiveName(stampedRelease, "amd64"))
	sums := assetPath(stampedRelease, "checksums.txt")
	cases := []struct {
		name   string
		change func(r *rig)
		say    string
	}{
		{"tampered archive", func(r *rig) {
			b := r.server.get(archive)
			r.server.put(archive, append(append([]byte(nil), b...), 0))
		}, "it is not the release's archive"},
		{"no checksum line", func(r *rig) {
			r.server.put(sums, []byte(strings.Repeat("ab", 32)+"  1-bit-bridge_0.2.2_linux_arm64.tar.gz\n"))
		}, "does not give 1-bit-bridge_0.2.2_linux_amd64.tar.gz one SHA-256"},
		{"two checksum lines", func(r *rig) {
			b := r.server.get(sums)
			r.server.put(sums, append(append([]byte(nil), b...), b...))
		}, "does not give 1-bit-bridge_0.2.2_linux_amd64.tar.gz one SHA-256"},
		{"malformed checksum", func(r *rig) {
			r.server.put(sums, []byte("not-a-hash  1-bit-bridge_0.2.2_linux_amd64.tar.gz\n"))
		}, "does not give 1-bit-bridge_0.2.2_linux_amd64.tar.gz one SHA-256"},
		{"a suffix of the name", func(r *rig) {
			sum := sha256.Sum256(r.server.get(archive))
			r.server.put(sums, []byte(hex.EncodeToString(sum[:])+"  x1-bit-bridge_0.2.2_linux_amd64.tar.gz\n"))
		}, "does not give 1-bit-bridge_0.2.2_linux_amd64.tar.gz one SHA-256"},
		{"no archive", func(r *rig) { r.server.remove(archive) }, "Could not download"},
		{"no checksums", func(r *rig) { r.server.remove(sums) }, "Could not download"},
		{"no program in the archive", func(r *rig) {
			r.republish(archive, tarGz(r.t, []tarEntry{{"README.md", []byte("x"), 0o644}}))
		}, "Could not unpack the bridge"},
		{"a program that does not run", func(r *rig) {
			r.republish(archive, tarGz(r.t, []tarEntry{{"bridge", []byte("#!/nonexistent/interpreter\n"), 0o755}}))
		}, "does not run on this system"},
		{"another release's program", func(r *rig) {
			r.republish(archive, tarGz(r.t, []tarEntry{{"bridge", fakeBridge(r.t, "0.1.9"), 0o755}}))
		}, "says it is the bridge 0.1.9, not 0.2.2"},
	}
	eachShell(t, func(t *testing.T, sh shell) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := newRig(t, sh)
				c.change(r)
				res := r.run("--library", r.music("music"))
				mustExit(t, res, 1)
				mustSay(t, res, c.say, "Nothing was installed.")
				r.nothingInstalled()
			})
		}
	})
}

// republish replaces the archive at path and gives it its line in a
// checksums.txt of its own, so only what is inside it is wrong.
func (r *rig) republish(path string, body []byte) {
	r.server.put(path, body)
	sum := sha256.Sum256(body)
	r.server.put(assetPath(stampedRelease, "checksums.txt"), []byte(hex.EncodeToString(sum[:])+"  "+filepath.Base(path)+"\n"))
}

// A box without what the download needs is refused before anything is
// downloaded: no SHA-256 tool, no curl and no wget, or BusyBox's wget alone,
// which does not check certificates. GNU wget does instead of curl.
func TestTheToolsTheDownloadNeedsAreCheckedFirst(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		t.Run("no SHA-256 tool", func(t *testing.T) {
			r := newRig(t, sh, "sha256sum", "shasum")
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 3)
			mustSay(t, res, "This installer needs: sha256sum.")
			if len(r.asked()) != 0 {
				t.Errorf("it downloaded %q", r.asked())
			}
			r.nothingInstalled()
		})
		t.Run("no downloader", func(t *testing.T) {
			r := newRig(t, sh, "curl", "wget")
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 3)
			mustSay(t, res, "This installer needs: curl.")
			r.nothingInstalled()
		})
		t.Run("BusyBox wget", func(t *testing.T) {
			r := newRig(t, sh, "curl", "wget")
			writeFile(t, filepath.Join(r.bin, "wget"), []byte("#!/bin/sh\necho 'BusyBox v1.36.1 (2024-01-01) multi-call binary.' >&2\nexit 1\n"), 0o755)
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 3)
			mustSay(t, res, "BusyBox's wget does not check the server's certificate")
			r.nothingInstalled()
		})
		t.Run("GNU wget", func(t *testing.T) {
			if _, err := exec.LookPath("wget"); err != nil {
				t.Skip("no wget on this host")
			}
			r := newRig(t, sh, "curl")
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 0)
			r.askedOnly(stampedRelease)
		})
	})
}

// Only Linux on amd64 or arm64 is installed; anything else is refused with
// what to do instead, before anything is downloaded or written.
func TestOnlyLinuxOnAmd64OrArm64IsInstalled(t *testing.T) {
	cases := []struct {
		system, machine string
		say             string
	}{
		{"Darwin", "arm64", "On a Mac, download the macOS archive"},
		{"MINGW64_NT-10.0-19045", "x86_64", "On Windows, download the Windows zip"},
		{"FreeBSD", "amd64", "this system is FreeBSD"},
		{"Linux", "armv7l", "32-bit ARM system (armv7l)"},
		{"Linux", "armv6l", "32-bit ARM system (armv6l)"},
		{"Linux", "i686", "32-bit x86 system (i686)"},
		{"Linux", "riscv64", "This box is riscv64"},
	}
	eachShell(t, func(t *testing.T, sh shell) {
		for _, c := range cases {
			t.Run(c.system+"/"+c.machine, func(t *testing.T) {
				r := newRig(t, sh)
				r.set("STUB_UNAME_S", c.system, "STUB_UNAME_M", c.machine)
				res := r.run("--library", r.music("music"))
				mustExit(t, res, 3)
				mustSay(t, res, c.say)
				if len(r.asked()) != 0 {
					t.Errorf("it downloaded %q", r.asked())
				}
				r.nothingInstalled()
			})
		}
	})
}

// A 64-bit kernel running a 32-bit system (Raspberry Pi OS's 32-bit image on
// a Pi 4 or 5) gets the arm64 build, which is static and runs on the kernel.
func TestA64BitKernelWithA32BitSystemGetsTheArm64Build(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh)
		r.set("STUB_UNAME_M", "aarch64", "STUB_LONG_BIT", "32")
		res := r.run("--library", r.music("music"))
		mustExit(t, res, 0)
		mustSay(t, res, "arm64 (a 64-bit kernel with a 32-bit system")
		archive := assetPath(stampedRelease, archiveName(stampedRelease, "arm64"))
		found := false
		for _, p := range r.asked() {
			found = found || p == archive
			if strings.Contains(p, "amd64") {
				t.Errorf("it downloaded %s", p)
			}
		}
		if !found {
			t.Errorf("it did not download %s: %q", archive, r.asked())
		}
	})
}

// A bridge installed as the user's service is upgraded through its own
// bridge update, never through the installer's download: the paired devices
// are checked first, the previous program is kept as .bak, the settings and
// data are left as they are, and the version the restarted service serves
// is the one reported.
func TestAnInstalledBridgeIsUpgradedThroughBridgeUpdate(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		t.Run("in its own place", func(t *testing.T) {
			r := newRig(t, sh)
			bin := filepath.Join(r.home, "apps", "bridge")
			config := filepath.Join(r.home, "bridge-settings", "bridge.yaml")
			r.installed("0.2.1", bin, config)
			r.newRelease(stampedRelease)
			r.set("FAKE_UPDATE", "ok")
			settings := tree(t, filepath.Dir(config))
			res := r.run()
			mustExit(t, res, 0)
			if len(r.asked()) != 0 {
				t.Errorf("an upgrade downloaded %q itself", r.asked())
			}
			if got := r.callsTo("bridge update"); len(got) != 1 || got[0] != "bridge update --yes --config "+config {
				t.Errorf("bridge update calls %q", got)
			}
			if len(r.callsTo("bridge init")) != 0 {
				t.Errorf("an upgrade ran bridge init")
			}
			sameTree(t, "the settings and data", settings, tree(t, filepath.Dir(config)))
			if !strings.Contains(readFile(t, bin), "VERSION='0.2.2'") || !strings.Contains(readFile(t, bin+".bak"), "VERSION='0.2.1'") {
				t.Errorf("the program was not replaced in place with its .bak kept")
			}
			if exists(r.target()) {
				t.Errorf("an upgrade installed a second program at %s", r.target())
			}
			mustSay(t, res, "Found the bridge 0.2.1, installed as tester's service: "+bin,
				"Updated the bridge from 0.2.1 to 0.2.2.", "The bridge 0.2.2 is running as tester's service.")
		})
		t.Run("through a link", func(t *testing.T) {
			r := newRig(t, sh)
			real := filepath.Join(r.home, "bridge-0.2.1", "bridge")
			link := filepath.Join(r.home, "bin", "bridge")
			config := filepath.Join(r.home, ".config", "1-bit-bridge", "bridge.yaml")
			r.installed("0.2.1", real, config)
			writeFile(t, r.unitFile(), []byte(unitText(link, config)), 0o644)
			mkdirAll(t, filepath.Dir(link))
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			r.newRelease(stampedRelease)
			r.set("FAKE_UPDATE", "ok")
			res := r.run()
			mustExit(t, res, 0)
			if target, err := os.Readlink(link); err != nil || target != real {
				t.Errorf("the link now points at %q (%v)", target, err)
			}
			if !strings.Contains(readFile(t, real), "VERSION='0.2.2'") {
				t.Errorf("the link's target was not updated")
			}
		})
		upgraded := func(t *testing.T, sh shell) (*rig, string) {
			r := newRig(t, sh)
			bin := filepath.Join(r.home, ".local", "bin", "bridge")
			r.installed("0.2.1", bin, r.configFile())
			r.newRelease(stampedRelease)
			return r, bin
		}
		t.Run("the latest already", func(t *testing.T) {
			r, _ := upgraded(t, sh)
			r.set("FAKE_UPDATE", "latest")
			res := r.run()
			mustExit(t, res, 0)
			mustSay(t, res, "The bridge 0.2.1 is the latest release.", "The bridge 0.2.1 is running")
		})
		t.Run("a check that failed", func(t *testing.T) {
			r, bin := upgraded(t, sh)
			r.set("FAKE_UPDATE", "checkerror")
			res := r.run()
			mustExit(t, res, 1)
			mustSay(t, res, "could not find out about a new release", "nothing was changed")
			if !strings.Contains(readFile(t, bin), "VERSION='0.2.1'") {
				t.Errorf("the program changed")
			}
		})
		t.Run("a paired device it would orphan", func(t *testing.T) {
			r, _ := upgraded(t, sh)
			r.set("FAKE_UPDATE", "compat")
			before := tree(t, r.home)
			res := r.run()
			mustExit(t, res, 1)
			mustSay(t, res, "would orphan a paired iOS client", "--override-client-floor",
				"bridge update stopped (exit 1), and the bridge is still 0.2.1: nothing was changed.")
			sameTree(t, "the install", before, tree(t, r.home))
			if len(r.callsTo("systemctl --user restart")) != 0 {
				t.Errorf("the service was restarted")
			}
		})
		t.Run("a restart that failed", func(t *testing.T) {
			r, _ := upgraded(t, sh)
			r.set("FAKE_UPDATE", "restartfail")
			res := r.run()
			mustExit(t, res, 1)
			mustSay(t, res, "bridge update installed 0.2.2, then stopped", "systemctl --user restart "+unitName)
		})
		t.Run("a service still serving the old release", func(t *testing.T) {
			r, _ := upgraded(t, sh)
			r.set("FAKE_UPDATE", "ok", "STUB_SERVED_FROZEN", "1")
			res := r.run()
			mustExit(t, res, 1)
			mustSay(t, res, "The service still serves the bridge 0.2.1, not 0.2.2")
		})
		t.Run("a stopped service", func(t *testing.T) {
			r, _ := upgraded(t, sh)
			if err := os.Remove(filepath.Join(r.state, "active")); err != nil {
				t.Fatal(err)
			}
			r.set("FAKE_UPDATE", "latest")
			res := r.run()
			mustExit(t, res, 0)
			if len(r.callsTo("bridge start")) != 1 {
				t.Errorf("the stopped service was not started with bridge start: %q", r.calls())
			}
		})
		t.Run("the installed release named", func(t *testing.T) {
			r, _ := upgraded(t, sh)
			res := r.run("--version", "0.2.1")
			mustExit(t, res, 0)
			if len(r.callsTo("bridge update")) != 0 {
				t.Errorf("bridge update ran for the release installed")
			}
		})
		t.Run("another release named", func(t *testing.T) {
			r, _ := upgraded(t, sh)
			before := tree(t, r.home)
			res := r.run("--version", "0.2.0")
			mustExit(t, res, 3)
			mustSay(t, res, "moves only to the latest release, through bridge update")
			if len(r.callsTo("bridge update")) != 0 || len(r.asked()) != 0 {
				t.Errorf("a refused version changed something: %q %q", r.calls(), r.asked())
			}
			sameTree(t, "the install", before, tree(t, r.home))
		})
	})
}

// What the installer does not manage it refuses, with nothing changed: a
// system service, a unit changed by hand or naming what is not there, a
// program in a directory the user cannot write, settings no service runs, or
// a file at ~/.local/bin/bridge that is not the bridge.
func TestLayoutsTheInstallerDoesNotManageAreRefused(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *rig)
		say   string
	}{
		{"a system service", func(r *rig) {
			writeFile(r.t, filepath.Join(r.root, "etc", "systemd", "system", "1-bit-bridge.service"), []byte("[Service]\n"), 0o644)
		}, "runs the bridge as a system service"},
		{"a unit changed by hand", func(r *rig) {
			r.installed("0.2.1", r.target(), r.configFile())
			writeFile(r.t, r.unitFile(), []byte("[Service]\nExecStart="+r.target()+" serve --config "+r.configFile()+" --addr :9000\n"), 0o644)
		}, "was changed since bridge init wrote it"},
		{"a unit with a drop-in ExecStart", func(r *rig) {
			r.installed("0.2.1", r.target(), r.configFile())
			writeFile(r.t, r.unitFile()+".d/override.conf", []byte("[Service]\nExecStart=\nExecStart=/opt/bridge serve\n"), 0o644)
		}, "sets the service's ExecStart"},
		{"a unit whose program is gone", func(r *rig) {
			r.installed("0.2.1", r.target(), r.configFile())
			if err := os.Remove(r.target()); err != nil {
				r.t.Fatal(err)
			}
		}, "which is not there"},
		{"settings no service runs", func(r *rig) {
			writeFile(r.t, r.configFile(), []byte("libraryRoots: [/srv/music]\n"), 0o600)
		}, "but no service runs them"},
		{"another program at the target", func(r *rig) {
			writeFile(r.t, r.target(), []byte("#!/bin/sh\necho hello\n"), 0o755)
		}, "is not the 1-bit bridge"},
		{"a directory at the target", func(r *rig) {
			mkdirAll(r.t, r.target())
		}, "is not the 1-bit bridge"},
	}
	eachShell(t, func(t *testing.T, sh shell) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := newRig(t, sh)
				r.newRelease(stampedRelease)
				r.set("FAKE_UPDATE", "ok")
				c.setup(r)
				before := tree(t, r.home)
				res := r.run("--library", r.music("music"))
				mustExit(t, res, 3)
				mustSay(t, res, c.say, "Nothing was changed.")
				sameTree(t, "the box", before, tree(t, r.home))
				if len(r.asked()) != 0 || len(r.callsTo("bridge update")) != 0 || len(r.callsTo("bridge init")) != 0 {
					t.Errorf("a refused layout was acted on: %q %q", r.asked(), r.calls())
				}
			})
		}
		t.Run("a program in a directory the user cannot write", func(t *testing.T) {
			if os.Geteuid() == 0 {
				t.Skip("root writes any directory")
			}
			r := newRig(t, sh)
			bin := filepath.Join(r.home, "opt", "bridge")
			r.installed("0.2.1", bin, r.configFile())
			if err := os.Chmod(filepath.Dir(bin), 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Dir(bin), 0o755) })
			r.set("FAKE_UPDATE", "ok")
			res := r.run()
			mustExit(t, res, 3)
			mustSay(t, res, "where tester cannot write", "Nothing was changed.")
			if len(r.callsTo("bridge update")) != 0 {
				t.Errorf("bridge update ran")
			}
		})
	})
}

// The installer runs as the user the service is for: root, sudo's included,
// is refused with the command to run instead.
func TestRootIsRefused(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		for _, sudoUser := range []string{"", "pi"} {
			t.Run("sudo user "+sudoUser, func(t *testing.T) {
				r := newRig(t, sh)
				r.set("STUB_UID", "0", "STUB_USER", "root")
				if sudoUser != "" {
					r.set("SUDO_USER", sudoUser)
				}
				res := r.run("--library", r.music("music"))
				mustExit(t, res, 3)
				if sudoUser != "" {
					mustSay(t, res, "Run this installer without sudo, as pi")
				} else {
					mustSay(t, res, "not as root", "adduser")
				}
				r.nothingInstalled()
				if len(r.asked()) != 0 {
					t.Errorf("it downloaded %q", r.asked())
				}
			})
		}
	})
}

// Lingering, which keeps the user's service running after the login ends and
// starts it at boot, is turned on the least privileged way that works, after
// saying why: by the session itself, then sudo (its password asked on the
// terminal), then su without sudo; failing those, the command is printed and
// the install goes on.
func TestLingeringIsTurnedOnSoTheServiceOutlivesTheLogin(t *testing.T) {
	cases := []struct {
		name     string
		omit     []string
		env      []string
		terminal []string
		calls    []string
		linger   bool
		say      string
	}{
		{name: "already on", env: nil, linger: true},
		{name: "by the session", env: []string{"STUB_SELF_LINGER", "1"}, linger: true,
			calls: []string{"loginctl --no-ask-password enable-linger"}},
		{name: "by sudo without a terminal", linger: true,
			calls: []string{"sudo -n loginctl enable-linger tester"}, say: "sudo runs loginctl enable-linger tester"},
		{name: "by sudo asking on the terminal", env: []string{"STUB_SUDO_ASKS", "1"}, terminal: []string{"secret", ""}, linger: true,
			calls: []string{"sudo loginctl enable-linger tester", "stub sudo read secret"}},
		{name: "by su without sudo", omit: []string{"sudo"}, terminal: []string{"rootpw", ""}, linger: true,
			calls: []string{"su root -c loginctl enable-linger tester", "stub su read rootpw"}, say: "There is no sudo here"},
		{name: "not at all", env: []string{"STUB_SUDO_ASKS", "1"}, linger: false,
			say: "To turn it on, run as root: loginctl enable-linger tester"},
	}
	eachShell(t, func(t *testing.T, sh shell) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := newRig(t, sh, c.omit...)
				r.set(c.env...)
				if c.name == "already on" {
					writeFile(t, filepath.Join(r.state, "linger"), nil, 0o644)
				}
				if c.terminal != nil {
					r.terminal(c.terminal...)
				}
				res := r.run("--library", r.music("music"))
				mustExit(t, res, 0)
				calls := strings.Join(r.calls(), "\n")
				for _, want := range c.calls {
					if !strings.Contains(calls, want) {
						t.Errorf("the calls do not show %q:\n%s", want, calls)
					}
				}
				if c.name == "already on" && (strings.Contains(calls, "enable-linger") || strings.Contains(calls, "sudo")) {
					t.Errorf("lingering was turned on again:\n%s", calls)
				}
				if got := exists(filepath.Join(r.state, "linger")); got != c.linger {
					t.Errorf("lingering on: %v, want %v", got, c.linger)
				}
				if c.say != "" {
					mustSay(t, res, c.say)
				}
				if len(r.callsTo("bridge init")) != 1 {
					t.Errorf("bridge init did not run once: %q", r.callsTo("bridge init"))
				}
			})
		}
	})
}

// bridge init installs the service through the user's systemd manager. The
// installer finds it through /run/user/<uid> when the session did not say
// where (su, a script), and waits for it after turning lingering on, which
// starts it; with none, the program is installed and setup waits (exit 4).
func TestTheUserManagerIsFoundOrSetupWaits(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		t.Run("through the runtime directory", func(t *testing.T) {
			r := newRig(t, sh)
			runtime := filepath.Join(r.root, "run", "user", "1000")
			socketDir, err := os.MkdirTemp("/tmp", "b475-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(socketDir) })
			l, err := net.Listen("unix", filepath.Join(socketDir, "bus"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { l.Close() })
			mkdirAll(t, runtime)
			if err := os.Symlink(filepath.Join(socketDir, "bus"), filepath.Join(runtime, "bus")); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(r.state, "linger"), nil, 0o644)
			r.set("STUB_NO_BUS", "1", "STUB_BUS_RUNTIME", runtime)
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 0)
			if len(r.callsTo("bridge init")) != 1 {
				t.Errorf("bridge init did not run")
			}
		})
		t.Run("started by lingering", func(t *testing.T) {
			r := newRig(t, sh)
			r.set("STUB_NO_BUS", "1")
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 0)
		})
		t.Run("none", func(t *testing.T) {
			r := newRig(t, sh)
			r.set("STUB_NO_BUS", "1", "STUB_LINGER_NO_BUS", "1")
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 4)
			mustSay(t, res, "there is no systemd user session for tester", "not with su or sudo")
			if !exists(r.target()) || len(r.callsTo("bridge init")) != 0 {
				t.Errorf("want the program installed and init not run: %q", r.calls())
			}
		})
	})
}

// A system that does not run systemd gets the program and the commands to
// run it by hand (exit 4).
func TestWithoutSystemdSetupWaits(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh)
		if err := os.RemoveAll(filepath.Join(r.root, "run", "systemd")); err != nil {
			t.Fatal(err)
		}
		res := r.run("--library", r.music("music"))
		mustExit(t, res, 4)
		mustSay(t, res, "does not run systemd", "init --no-service")
		if !exists(r.target()) || len(r.callsTo("bridge init")) != 0 || len(r.callsTo("systemctl")) != 0 {
			t.Errorf("want the program installed and nothing set up: %q", r.calls())
		}
	})
}

// A HOME that does not outlive a reboot is refused: everything installed
// would be gone at the next boot. Volumio's overlay, whose upper layer is
// on its data partition, is not one, and gets its note.
func TestAHomeThatDoesNotOutliveARebootIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		mounts  func(home string) string
		cmdline string
	}{
		{"tmpfs", func(home string) string {
			return "/dev/root / ext4 rw 0 0\ntmpfs " + filepath.Dir(home) + " tmpfs rw 0 0\n"
		}, ""},
		{"an overlay over tmpfs", func(string) string {
			return "tmpfs /media/root-rw tmpfs rw 0 0\noverlayroot / overlay rw,lowerdir=/media/root-ro,upperdir=/media/root-rw/overlay,workdir=/media/root-rw/overlay-workdir 0 0\n"
		}, ""},
		{"Raspberry Pi OS's Overlay File System", func(string) string { return "overlay / overlay rw 0 0\n" }, "console=tty1 boot=overlay root=/dev/mmcblk0p2"},
	}
	eachShell(t, func(t *testing.T, sh shell) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := newRig(t, sh)
				writeFile(t, filepath.Join(r.root, "proc", "mounts"), []byte(c.mounts(r.home)), 0o644)
				if c.cmdline != "" {
					writeFile(t, filepath.Join(r.root, "proc", "cmdline"), []byte(c.cmdline+"\n"), 0o644)
				}
				res := r.run("--library", r.music("music"))
				mustExit(t, res, 3)
				mustSay(t, res, "keeps nothing written to "+r.home+" through a reboot", "Nothing was changed.")
				r.nothingInstalled()
			})
		}
		t.Run("Volumio", func(t *testing.T) {
			r := newRig(t, sh)
			for _, d := range []string{"volumio", "data/configuration"} {
				mkdirAll(t, filepath.Join(r.root, d))
			}
			writeFile(t, filepath.Join(r.root, "proc", "mounts"), []byte("/dev/mmcblk0p3 /mnt/ext ext4 rw 0 0\noverlay / overlay rw,lowerdir=/mnt/static,upperdir=/mnt/ext/dyn,workdir=/mnt/ext/work 0 0\n"), 0o644)
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 0)
			mustSay(t, res, "On Volumio, a factory reset or a system update can remove what is installed outside /data")
		})
	})
}

// A clock before 2026 fails every secure download: the installer says so
// first, with how to check it.
func TestAClockBefore2026IsRefused(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh)
		r.set("STUB_YEAR", "1970")
		res := r.run("--library", r.music("music"))
		mustExit(t, res, 3)
		mustSay(t, res, "says the year is 1970", "timedatectl")
		if len(r.asked()) != 0 {
			t.Errorf("it downloaded %q", r.asked())
		}
		r.nothingInstalled()
	})
}

// The installer names the system it found.
func TestTheSystemIsNamed(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *rig)
		say   string
	}{
		{"Volumio", func(r *rig) {
			r.osRelease("PRETTY_NAME=\"Raspbian GNU/Linux 11 (bullseye)\"\nID=raspbian\nVOLUMIO_VERSION=\"3.661\"\n")
			mkdirAll(r.t, filepath.Join(r.root, "volumio"))
			mkdirAll(r.t, filepath.Join(r.root, "data", "configuration"))
		}, "Found Volumio 3.661, amd64."},
		{"moOde", func(r *rig) {
			r.osRelease("PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\n")
			writeFile(r.t, filepath.Join(r.root, "usr", "local", "bin", "moodeutl"), []byte("#!/bin/sh\n"), 0o755)
		}, "Found moOde audio player, on Debian GNU/Linux 12 (bookworm), amd64."},
		{"Raspberry Pi OS", func(r *rig) {
			r.osRelease("PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\n")
			writeFile(r.t, filepath.Join(r.root, "etc", "rpi-issue"), []byte("Raspberry Pi reference 2024-07-04\n"), 0o644)
		}, "Found Raspberry Pi OS (Debian GNU/Linux 12 (bookworm)), amd64."},
		{"Ubuntu", func(r *rig) {
			r.osRelease("PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\nID_LIKE=debian\n")
		}, "Found Ubuntu 24.04.1 LTS, amd64."},
		{"Debian", func(r *rig) {}, "Found Debian GNU/Linux 12 (bookworm), amd64."},
		{"another", func(r *rig) {
			r.osRelease("PRETTY_NAME='Fedora Linux 41'\nID=fedora\n")
		}, "Found Fedora Linux 41, amd64."},
	}
	eachShell(t, func(t *testing.T, sh shell) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := newRig(t, sh)
				c.setup(r)
				res := r.run("--uninstall")
				mustExit(t, res, 0)
				mustSay(t, res, c.say)
			})
		}
	})
}

// --uninstall prints the commands that remove the service and the program, in
// the order they must run (the service stopped, and checked stopped, before
// its program is removed), the settings' removal apart, and changes nothing,
// offline. The music is never among them; root is refused as for an install.
func TestUninstallPrintsTheStepsAndChangesNothing(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		t.Run("installed", func(t *testing.T) {
			r := newRig(t, sh)
			music := r.music("music")
			r.installed(stampedRelease, r.target(), r.configFile())
			r.server.srv.Close()
			before := tree(t, r.home)
			for range 2 {
				res := r.run("--uninstall")
				mustExit(t, res, 0)
				out := res.stdout
				order := []string{
					"systemctl --user disable --now " + unitName,
					"systemctl --user is-active " + unitName,
					"rm " + r.unitFile(),
					"systemctl --user daemon-reload",
					"rm -f " + r.target() + " " + r.target() + ".bak",
					"rm -rf " + filepath.Dir(r.configFile()),
				}
				at := -1
				for _, step := range order {
					i := strings.Index(out, step)
					if i < 0 {
						t.Fatalf("the steps do not include %q:\n%s", step, out)
					}
					if i < at {
						t.Errorf("%q comes too early:\n%s", step, out)
					}
					at = i
				}
				if strings.Contains(out, music) {
					t.Errorf("the steps name the music folder:\n%s", out)
				}
				mustSay(t, res, "This installer removed nothing.")
				sameTree(t, "the install", before, tree(t, r.home))
			}
			for _, who := range []string{"systemctl", "loginctl", "sudo", "bridge"} {
				if len(r.callsTo(who)) != 0 {
					t.Errorf("--uninstall called %s: %q", who, r.callsTo(who))
				}
			}
		})
		t.Run("through a link", func(t *testing.T) {
			r := newRig(t, sh)
			real := filepath.Join(r.home, "bridge-0.2.1", "bridge")
			link := filepath.Join(r.home, "bin", "bridge")
			r.installed("0.2.1", real, r.configFile())
			writeFile(t, r.unitFile(), []byte(unitText(link, r.configFile())), 0o644)
			mkdirAll(t, filepath.Dir(link))
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			res := r.run("--uninstall")
			mustExit(t, res, 0)
			mustSay(t, res, "rm -f "+link+" "+link+".bak", "rm -f "+real+" "+real+".bak")
		})
		t.Run("nothing installed", func(t *testing.T) {
			r := newRig(t, sh)
			res := r.run("--uninstall")
			mustExit(t, res, 0)
			mustSay(t, res, "there is nothing to remove")
			r.nothingInstalled()
		})
		t.Run("with another option", func(t *testing.T) {
			r := newRig(t, sh)
			res := r.run("--uninstall", "--no-init")
			mustExit(t, res, 2)
		})
		t.Run("as root", func(t *testing.T) {
			r := newRig(t, sh)
			r.set("STUB_UID", "0", "STUB_USER", "root")
			res := r.run("--uninstall")
			mustExit(t, res, 3)
		})
	})
}

// A program the installer replaces may be running: the new one is put in
// place by a rename, a new file under the old name, so the running one goes
// on and nothing writes into it (on Linux, a write into a running program
// fails with ETXTBSY).
func TestReplacingAProgramThatIsRunning(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep program to stand in for a running bridge")
	}
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh)
		program, err := os.ReadFile(sleep)
		if err != nil {
			t.Fatal(err)
		}
		// A real executable that carries the bridge's module path, as the
		// installer checks before it replaces a file.
		writeFile(t, r.target(), append(program, []byte("github.com/acoseac/1-bit-bridge")...), 0o755)
		running := exec.Command(r.target(), "60")
		if err := running.Start(); err != nil {
			t.Skipf("this host does not run the copied program: %v", err)
		}
		t.Cleanup(func() {
			_ = running.Process.Kill()
			_ = running.Wait()
		})
		before, err := os.Stat(r.target())
		if err != nil {
			t.Fatal(err)
		}
		res := r.run("--library", r.music("music"))
		mustExit(t, res, 0)
		if err := running.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("the running program did not go on: %v", err)
		}
		after, err := os.Stat(r.target())
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(before, after) {
			t.Errorf("the program was written in place, not renamed into place")
		}
		if got := readFile(t, r.target()); got != string(fakeBridge(t, stampedRelease)) {
			t.Errorf("the program was not replaced")
		}
	})
}

// A run stopped between staging the new program and renaming it into place
// leaves the program it found, and neither the staged copy nor its
// temporary directory.
func TestAnInterruptedReplacementLeavesTheProgramItFound(t *testing.T) {
	chmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatal(err)
	}
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh, "chmod")
		writeFile(t, filepath.Join(r.bin, "chmod"), []byte("#!/bin/sh\n"+
			"case \"$2\" in */.bridge.install.*) kill -TERM \"$PPID\" ;; esac\n"+
			"exec "+chmod+" \"$@\"\n"), 0o755)
		old := string(fakeBridge(t, "0.2.1"))
		writeFile(t, r.target(), []byte(old), 0o755)
		res := r.run("--library", r.music("music"))
		mustExit(t, res, 143)
		if got := readFile(t, r.target()); got != old {
			t.Errorf("the program it found was changed")
		}
		entries, err := os.ReadDir(filepath.Dir(r.target()))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("left beside the program: %v", entries)
		}
		if entries, _ := os.ReadDir(r.tmp); len(entries) != 0 {
			t.Errorf("the temporary directory was left behind: %v", entries)
		}
		if len(r.callsTo("bridge init")) != 0 {
			t.Errorf("setup ran after the stop")
		}
	})
}

// A download cut short runs nothing: every prefix of the script that ends
// before its last command's closing brace is either function definitions
// alone or a syntax error. Every prefix within the last 256 bytes, which
// holds that command whole and cut at each byte, is run as a file and as
// standard input, and must print, call, ask for and write nothing; each of
// them also holds everything above it, so a command anywhere at the top
// level would run in all of them. Every tenth line before that is cut too.
func TestATruncatedDownloadRunsNothing(t *testing.T) {
	script, err := os.ReadFile(stampedScript)
	if err != nil {
		t.Fatal(err)
	}
	last := bytes.LastIndexByte(script, '}')
	if last < 0 || strings.TrimSpace(string(script[last+1:])) != "" {
		t.Fatalf("the script does not end with its compound command's closing brace")
	}
	cuts := map[int]bool{}
	lines := 0
	for i, b := range script[:last] {
		if b == '\n' {
			if lines%10 == 0 {
				cuts[i+1] = true
			}
			lines++
		}
	}
	for i := max(0, last-256); i <= last; i++ {
		cuts[i] = true
	}
	var offsets []int
	for i := range cuts {
		offsets = append(offsets, i)
	}
	sort.Ints(offsets)
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh)
		music := r.music("music")
		prefix := filepath.Join(r.dir, "cut.sh")
		r.script = prefix
		home := tree(t, r.home)
		for _, n := range offsets {
			writeFile(t, prefix, script[:n], 0o644)
			for _, piped := range []bool{false, true} {
				var res result
				if piped {
					res = r.runPiped("--library", music)
				} else {
					res = r.run("--library", music)
				}
				if res.stdout != "" || len(r.calls()) != 0 || len(r.asked()) != 0 {
					t.Fatalf("the script cut to %d bytes (piped %v) ran: exit %d\nstdout:\n%s\ncalls: %q\nrequests: %q\nit ends:\n%s",
						n, piped, res.code, res.stdout, r.calls(), r.asked(), script[max(0, n-120):n])
				}
			}
		}
		sameTree(t, "HOME", home, tree(t, r.home))
		// The whole script runs.
		writeFile(t, prefix, script, 0o644)
		mustExit(t, r.run("--library", music), 0)
	})
}

// shellWords are names a shell gives its own meaning before it looks for a
// function: POSIX's builtins and reserved words, and ksh93's predefined
// aliases, which win over a function of the same name (a function named
// stop, which the installer had, ran ksh93's kill -s STOP).
var shellWords = strings.Fields(`
	alias bg break cd command continue eval exec exit export false fc fg getopts hash jobs kill
	newgrp printf pwd read readonly return set shift test times trap true type ulimit umask
	unalias unset wait case do done elif else esac fi for function if in select then until while
	autoload compound float functions history integer nameref nohup r redirect source stop suspend
	builtin declare echo let local typeset
`)

// No function of the installer is named after a word a shell keeps for
// itself: such a function runs only in the shells that do not.
func TestNoInstallerFunctionIsNamedAfterAShellWord(t *testing.T) {
	t.Parallel()
	defined := regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)\(\) \{`).FindAllStringSubmatch(readFile(t, repoScript), -1)
	if len(defined) < 40 {
		t.Fatalf("found %d function definitions in the installer; the pattern no longer reads them", len(defined))
	}
	for _, m := range defined {
		for _, word := range shellWords {
			if m[1] == word {
				t.Errorf("the installer defines a function %s, a word some shell keeps for itself", m[1])
			}
		}
	}
}

// The release builds the stamped installer and attaches it: goreleaser's
// before-hook runs scripts/stamp-install.sh with the release's version,
// release.extra_files uploads the install.sh it writes at the repository
// root (gitignored, as release-meta.json is), and checksum.extra_files gives
// it a line in checksums.txt.
func TestTheReleaseAttachesTheStampedInstaller(t *testing.T) {
	t.Parallel()
	var config struct {
		Before struct {
			Hooks []string `yaml:"hooks"`
		} `yaml:"before"`
		Checksum struct {
			ExtraFiles []struct {
				Glob string `yaml:"glob"`
			} `yaml:"extra_files"`
		} `yaml:"checksum"`
		Release struct {
			ExtraFiles []struct {
				Glob string `yaml:"glob"`
			} `yaml:"extra_files"`
		} `yaml:"release"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join("..", "..", ".goreleaser.yaml"))), &config); err != nil {
		t.Fatal(err)
	}
	hooks := 0
	for _, hook := range config.Before.Hooks {
		if hook == `sh scripts/stamp-install.sh "{{ .Version }}"` {
			hooks++
		}
	}
	if hooks != 1 {
		t.Errorf("the before-hooks run scripts/stamp-install.sh with the release's version %d times, want once: %q", hooks, config.Before.Hooks)
	}
	globs := func(files []struct {
		Glob string `yaml:"glob"`
	}) []string {
		var out []string
		for _, f := range files {
			out = append(out, f.Glob)
		}
		return out
	}
	for what, list := range map[string][]string{
		"release.extra_files":  globs(config.Release.ExtraFiles),
		"checksum.extra_files": globs(config.Checksum.ExtraFiles),
	} {
		found := false
		for _, g := range list {
			found = found || g == "install.sh"
		}
		if !found {
			t.Errorf("%s does not attach install.sh: %q", what, list)
		}
	}
	ignored := false
	for _, line := range strings.Split(readFile(t, filepath.Join("..", "..", ".gitignore")), "\n") {
		ignored = ignored || strings.TrimSpace(line) == "/install.sh"
	}
	if !ignored {
		t.Errorf(".gitignore does not ignore the stamped /install.sh, so a release's tree would be dirty")
	}
}

// scripts/stamp-install.sh writes the installer a release attaches: the
// repository's copy with its one unstamped line made the release's version,
// and nothing else changed. It refuses what is not a release version, and a
// source without that line or with two, writing nothing.
func TestTheStampChangesOneLine(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	stamp := func(t *testing.T, args ...string) (string, error) {
		out, err := exec.Command(sh, append([]string{filepath.Join("..", "stamp-install.sh")}, args...)...).CombinedOutput()
		return string(out), err
	}
	source := readFile(t, repoScript)
	dir := t.TempDir()
	out := filepath.Join(dir, "install.sh")
	if msg, err := stamp(t, "1.2.3-rc.1", repoScript, out); err != nil {
		t.Fatalf("stamping: %v: %s", err, msg)
	}
	stamped := readFile(t, out)
	src, dst := strings.Split(source, "\n"), strings.Split(stamped, "\n")
	if len(src) != len(dst) {
		t.Fatalf("the stamp changed the number of lines")
	}
	changed := 0
	for i := range src {
		if src[i] != dst[i] {
			changed++
			if strings.TrimSpace(src[i]) != "STAMPED_VERSION=''" || strings.TrimSpace(dst[i]) != "STAMPED_VERSION='1.2.3-rc.1'" {
				t.Errorf("line %d changed from %q to %q", i+1, src[i], dst[i])
			}
		}
	}
	if changed != 1 {
		t.Errorf("%d lines changed, want 1", changed)
	}
	for _, bad := range []string{"", "latest", "1.2", "1.2.3/x", "1.2.3'"} {
		target := filepath.Join(dir, "bad.sh")
		if msg, err := stamp(t, bad, repoScript, target); err == nil {
			t.Errorf("version %q was stamped: %s", bad, msg)
		}
		if exists(target) {
			t.Errorf("version %q wrote %s", bad, target)
		}
	}
	for name, text := range map[string]string{
		"none": strings.Replace(source, "STAMPED_VERSION=''", "STAMPED_VERSION='0.0.1'", 1),
		"two":  source + "\tSTAMPED_VERSION=''\n",
	} {
		src := filepath.Join(dir, name+".sh")
		writeFile(t, src, []byte(text), 0o644)
		target := filepath.Join(dir, name+"-out.sh")
		if msg, err := stamp(t, "1.2.3", src, target); err == nil {
			t.Errorf("a source with %s unstamped lines was stamped: %s", name, msg)
		}
		if exists(target) {
			t.Errorf("a source with %s unstamped lines wrote %s", name, target)
		}
	}
}
