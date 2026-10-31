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

	mu        sync.Mutex
	files     map[string][]byte
	redirects map[string]string
	requests  []string
}

const releasePath = "/acoseac/1-bit-bridge/releases"

func newReleaseServer(t *testing.T, dir string) *releaseServer {
	t.Helper()
	s := &releaseServer{t: t, files: map[string][]byte{}, redirects: map[string]string{}}
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
	target, moved := s.redirects[r.URL.Path]
	s.mu.Unlock()
	if moved {
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(body)
}

// redirect answers a request for path with a redirect to target, a URL the
// test gives.
func (s *releaseServer) redirect(path, target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redirects[path] = target
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
// links into its PATH from the host's; the rest of the PATH is stubs. wget
// is not among them: the installer never downloads with it.
var realTools = []string{
	"awk", "basename", "cat", "chmod", "cp", "curl", "cut", "dirname", "env", "grep", "gzip", "head",
	"ln", "ls", "mkdir", "mktemp", "mv", "od", "readlink", "rm", "rmdir", "sed", "sha256sum", "shasum",
	"sort", "tar", "tee", "touch", "tr",
}

// rig is one box for the installer: a HOME, a system root the script reads
// its /etc, /proc, /run and music folders under, a PATH of real tools and
// stubs, a release server with stampedRelease published, the log every stub
// and the fake bridge append their calls to, and the fake bridge's record of
// each call's words.
type rig struct {
	t       *testing.T
	sh      shell
	dir     string
	home    string
	root    string
	state   string
	bin     string
	tmp     string
	log     string
	argvLog string
	script  string
	server  *releaseServer
	env     map[string]string
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
		t:       t,
		sh:      sh,
		dir:     dir,
		home:    filepath.Join(dir, "home"),
		root:    filepath.Join(dir, "root"),
		state:   filepath.Join(dir, "state"),
		bin:     filepath.Join(dir, "bin"),
		tmp:     filepath.Join(dir, "tmp"),
		log:     filepath.Join(dir, "calls.log"),
		argvLog: filepath.Join(dir, "argv.log"),
		script:  stampedScript,
	}
	for _, d := range []string{r.home, r.state, r.bin, r.tmp, filepath.Join(r.root, "run", "systemd", "system")} {
		mkdirAll(t, d)
	}
	writeFile(t, r.log, nil, 0o644)
	writeFile(t, r.argvLog, nil, 0o644)
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
		"STUB_LOG":                    r.log,
		"STUB_ARGV":                   r.argvLog,
		"STUB_STATE":                  r.state,
	}
	return r
}

// moveHome gives the rig a HOME of another name, made under the rig's
// directory, before anything is laid out in it.
func (r *rig) moveHome(name string) {
	r.home = filepath.Join(r.dir, name)
	mkdirAll(r.t, r.home)
	r.set("HOME", r.home)
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

// mutations are the logged calls that change the box: the service
// manager's but those that only read it (show-environment, cat, is-active),
// lingering turned on, sudo and su, and the bridge's but those that only
// read (version, status, output).
func (r *rig) mutations() []string {
	var out []string
	for _, c := range r.calls() {
		f := strings.Fields(c)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "systemctl":
			if len(f) > 2 && (f[2] == "show-environment" || f[2] == "cat" || f[2] == "is-active") {
				continue
			}
		case "loginctl":
			if !strings.Contains(c, "enable-linger") {
				continue
			}
		case "sudo", "su":
		case "bridge":
			if f[1] == "version" || f[1] == "status" || f[1] == "output" {
				continue
			}
		default:
			continue
		}
		out = append(out, c)
	}
	return out
}

// readArgv reads what the fake bridge or the recorder (testdata/argv)
// recorded: each call the program's name, then its arguments, each as the
// program was given it.
func readArgv(t *testing.T, path string) [][]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "call "):
			calls = append(calls, []string{strings.TrimPrefix(line, "call ")})
		case strings.HasPrefix(line, "arg ") && len(calls) > 0:
			calls[len(calls)-1] = append(calls[len(calls)-1], strings.TrimPrefix(line, "arg "))
		}
	}
	return calls
}

// bridgeCalls are the fake bridge's calls of subcommand sub, each the words
// it was given, sub's first.
func (r *rig) bridgeCalls(sub string) [][]string {
	r.t.Helper()
	var out [][]string
	for _, call := range readArgv(r.t, r.argvLog) {
		if len(call) > 1 && call[1] == sub {
			out = append(out, call[1:])
		}
	}
	return out
}

// printed are the commands the installer's output gives the user to type:
// the lines it indents by four spaces, each a command line as it stands.
func printed(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "    ") && len(line) > 4 && line[4] != ' ' {
			lines = append(lines, line[4:])
		}
	}
	return lines
}

// recorded are the programs a printed command can run that runPrinted puts
// a recorder in place of on the PATH it runs them with; injected stands for
// whatever a word split at a semicolon would run.
var recorded = []string{"bridge", "injected", "loginctl", "rm", "ssh", "sudo", "systemctl"}

// runPrinted runs each command line as a user pasting it into the shell
// under test would, with the recorder (testdata/argv) in place of every
// program it can name: those in recorded, on its PATH, and the bridge at
// each of programs, its paths. It returns the calls recorded, each the
// program's name and its arguments, and fails the test for a line the
// shell does not run to its end.
func (r *rig) runPrinted(lines []string, programs ...string) [][]string {
	r.t.Helper()
	recorder, err := os.ReadFile(filepath.Join("testdata", "argv"))
	if err != nil {
		r.t.Fatal(err)
	}
	dir := filepath.Join(r.dir, "recorders")
	for _, name := range recorded {
		writeFile(r.t, filepath.Join(dir, name), recorder, 0o755)
	}
	for _, p := range programs {
		writeFile(r.t, p, recorder, 0o755)
	}
	log := filepath.Join(r.dir, "printed.log")
	writeFile(r.t, log, nil, 0o644)
	for _, line := range lines {
		cmd := exec.Command(r.sh.argv[0], append(append([]string{}, r.sh.argv[1:]...), "-c", line)...)
		cmd.Env = []string{"PATH=" + dir, "HOME=" + r.home, "LC_ALL=C", "STUB_ARGV=" + log}
		cmd.Dir = r.home
		if out, err := cmd.CombinedOutput(); err != nil {
			r.t.Errorf("the printed command %q did not run: %v\n%s", line, err, out)
		}
	}
	return readArgv(r.t, log)
}

// sameCalls compares the calls the printed commands made with the ones
// they should have, word for word.
func sameCalls(t *testing.T, got, want [][]string) {
	t.Helper()
	if fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want) {
		t.Errorf("the printed commands ran\n%q\nwant\n%q", got, want)
	}
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
// directory, and changed nothing of the service manager's, lingering's or
// a bridge's: it may have asked the manager what it has.
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
	if m := r.mutations(); len(m) != 0 {
		r.t.Errorf("it changed the box: %q", m)
	}
}

// unchanged checks that a refused run changed nothing: HOME and the
// service's state as they were (home and state, taken before the run),
// nothing downloaded, and no call that changes the box.
func (r *rig) unchanged(home, state map[string]string) {
	r.t.Helper()
	sameTree(r.t, "HOME", home, tree(r.t, r.home))
	sameTree(r.t, "the service's state", state, tree(r.t, r.state))
	if len(r.asked()) != 0 {
		r.t.Errorf("a refused run downloaded %q", r.asked())
	}
	if m := r.mutations(); len(m) != 0 {
		r.t.Errorf("a refused run changed the box: %q", m)
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
				if got, want := fmt.Sprintf("%q", r.bridgeCalls("init")), fmt.Sprintf("%q", [][]string{{"init", "--yes", "--library", music}}); got != want {
					t.Errorf("bridge init calls %s, want %s", got, want)
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
					"The bridge 0.2.2 is running as tester's service, and starts again when this box boots.",
					"ssh -L 7789:127.0.0.1:7789 tester@192.0.2.10",
					"http://127.0.0.1:7789/",
					r.target()+" pair --name 'My iPhone'",
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
		if got, want := fmt.Sprintf("%q", r.bridgeCalls("init")), fmt.Sprintf("%q", [][]string{{"init", "--library", music}}); got != want {
			t.Errorf("bridge init calls %s, want %s", got, want)
		}
		mustSay(t, res, filepath.Join(r.dir, "nowhere")+" is not a folder.")
		if len(r.callsTo("sudo")) != 0 {
			t.Errorf("sudo was called although the session turns lingering on itself: %q", r.callsTo("sudo"))
		}
	})
}

// Without a terminal and without --library, the installer asks nothing (it
// never reads its standard input, which under curl | sh is the script) and
// refuses before anything is downloaded, naming the folders it found and
// --library, so that a second run given one installs (a program left behind
// would make that run refuse).
func TestWithoutATerminalOrALibraryTheInstallerDoesNotWait(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh)
		r.osRelease("PRETTY_NAME=\"Raspbian GNU/Linux 11 (bullseye)\"\nID=raspbian\nVOLUMIO_VERSION=\"3.661\"\n")
		for _, d := range []string{"volumio", "data/configuration", "mnt/NAS", "data/INTERNAL"} {
			mkdirAll(t, filepath.Join(r.root, d))
		}
		writeFile(t, filepath.Join(r.root, "mnt", "USB", "Drive", "01.flac"), []byte("fLaC"), 0o644)
		res := r.runHeldOpen()
		mustExit(t, res, 3)
		if len(r.asked()) != 0 {
			t.Errorf("it downloaded %q", r.asked())
		}
		r.nothingInstalled()
		mustSay(t, res, "Found Volumio 3.661, amd64.", "No music folder was given (--library), and there is no terminal to ask for one.",
			"Music folders found here: "+filepath.Join(r.root, "mnt", "USB"), "--library /path/to/music", "Nothing was changed.")
	})
}

// --no-init installs the program alone on a new box, and changes nothing
// else: no settings, no unit, nothing of the service manager's or
// lingering's. It prints the commands that set it up (bridge init, and the
// lingering bridge init does not turn on), never a second run of the
// installer, which would refuse the program it leaves. On a box where the
// bridge is installed it is refused with nothing changed, its service
// running or stopped, and the installed release named or not: no
// lingering, no bridge update, no start.
func TestNoInitInstallsTheProgramAlone(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		t.Run("a new box", func(t *testing.T) {
			r := newRig(t, sh)
			res := r.runHeldOpen("--no-init")
			mustExit(t, res, 0)
			if !exists(r.target()) {
				t.Fatalf("the program was not installed")
			}
			if m := r.mutations(); len(m) != 0 {
				t.Errorf("it changed the box: %q", m)
			}
			if exists(r.configFile()) || exists(r.unitFile()) {
				t.Errorf("settings or a unit were written")
			}
			mustSay(t, res, "Set it up when you are ready, with:")
			if strings.Contains(res.output(), "installer again") {
				t.Errorf("it says to run the installer again, which would refuse the program it left:\n%s", res.output())
			}
			sameCalls(t, r.runPrinted(printed(res.output()), r.target()), [][]string{
				{"bridge", "init"},
				{"loginctl", "enable-linger", "tester"},
			})
		})
		for _, c := range []struct {
			name   string
			active bool
			args   []string
		}{
			{"an installed bridge, running", true, []string{"--no-init"}},
			{"an installed bridge, stopped", false, []string{"--no-init"}},
			{"an installed bridge, running, its release named", true, []string{"--no-init", "--version", "0.2.1"}},
			{"an installed bridge, stopped, its release named", false, []string{"--no-init", "--version", "0.2.1"}},
		} {
			t.Run(c.name, func(t *testing.T) {
				r := newRig(t, sh)
				r.installed("0.2.1", r.target(), r.configFile())
				if !c.active {
					if err := os.Remove(filepath.Join(r.state, "active")); err != nil {
						t.Fatal(err)
					}
				}
				r.newRelease(stampedRelease)
				r.set("FAKE_UPDATE", "ok")
				home, state := tree(t, r.home), tree(t, r.state)
				res := r.run(c.args...)
				mustExit(t, res, 3)
				mustSay(t, res, "--no-init is for a new install", "Nothing was changed.")
				r.unchanged(home, state)
				if calls := r.callsTo("loginctl"); len(calls) != 0 {
					t.Errorf("lingering was asked about: %q", calls)
				}
				sameCalls(t, r.runPrinted(printed(res.output()), r.target()), [][]string{
					{"bridge", "update"},
				})
			})
		}
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
// downloaded: no SHA-256 tool, or no curl. wget never downloads in curl's
// place, GNU's or BusyBox's: GNU wget follows a redirect from HTTPS to plain
// HTTP, and BusyBox's does not check the server's certificate. The refusal
// says how to install curl.
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
		for _, c := range []struct{ name, wget string }{
			{"no curl and no wget", ""},
			{"no curl, and GNU wget", "GNU Wget 1.21.3, a non-interactive network retriever."},
			{"no curl, and BusyBox wget", "BusyBox v1.36.1 (2024-01-01) multi-call binary."},
		} {
			t.Run(c.name, func(t *testing.T) {
				r := newRig(t, sh, "curl")
				if c.wget != "" {
					writeFile(t, filepath.Join(r.bin, "wget"), []byte("#!/bin/sh\nprintf 'wget %s\\n' \"$*\" >>\"$STUB_LOG\"\necho '"+c.wget+"'\nexit 0\n"), 0o755)
				}
				res := r.run("--library", r.music("music"))
				mustExit(t, res, 3)
				mustSay(t, res, "This installer needs: curl.", "sudo apt install curl", "Nothing was changed.")
				if len(r.asked()) != 0 {
					t.Errorf("it downloaded %q", r.asked())
				}
				if calls := r.callsTo("wget"); len(calls) != 0 {
					t.Errorf("wget was run: %q", calls)
				}
				r.nothingInstalled()
			})
		}
	})
}

// Every download is HTTPS to its end: a redirect to plain HTTP is not
// followed, for checksums.txt or the archive, and the plain server is never
// asked anything, though it would send the download back to HTTPS.
func TestADownloadIsHeldToHTTPS(t *testing.T) {
	t.Parallel()
	for _, asset := range []string{"checksums.txt", archiveName(stampedRelease, "amd64")} {
		t.Run(asset, func(t *testing.T) {
			eachShell(t, func(t *testing.T, sh shell) {
				r := newRig(t, sh)
				path := assetPath(stampedRelease, asset)
				var mu sync.Mutex
				var plainAsked []string
				plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					mu.Lock()
					plainAsked = append(plainAsked, req.URL.Path)
					mu.Unlock()
					http.Redirect(w, req, r.server.srv.URL+"/moved"+path, http.StatusFound)
				}))
				t.Cleanup(plain.Close)
				r.server.redirect(path, plain.URL+"/plain"+path)
				res := r.run("--library", r.music("music"))
				mustExit(t, res, 1)
				mustSay(t, res, "Could not download", "Nothing was installed.")
				r.nothingInstalled()
				mu.Lock()
				defer mu.Unlock()
				if len(plainAsked) != 0 {
					t.Errorf("the plain HTTP server was asked for %q", plainAsked)
				}
			})
		})
	}
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
			if got, want := fmt.Sprintf("%q", r.bridgeCalls("update")), fmt.Sprintf("%q", [][]string{{"update", "--yes", "--config", config}}); got != want {
				t.Errorf("bridge update calls %s, want %s", got, want)
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
				"Updated the bridge from 0.2.1 to 0.2.2.", "The bridge 0.2.2 is running as tester's service, and starts again when this box boots.")
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
// unit whose ExecStart a drop-in sets, a program in a directory the user
// cannot write, settings no service runs, or anything at all at
// ~/.local/bin/bridge with no service of the user's running it: a new
// install is for a box with nothing of the bridge on it, and only a
// bridge's own bridge update replaces a bridge (the bridge itself there,
// another program, a link or a directory). That refusal names bridge update
// and bridge init as the way on.
func TestLayoutsTheInstallerDoesNotManageAreRefused(t *testing.T) {
	const programThere = "is there, and the bridge is not installed as tester's service"
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
		}, "sets its ExecStart more than once"},
		{"a unit whose program is gone", func(r *rig) {
			r.installed("0.2.1", r.target(), r.configFile())
			if err := os.Remove(r.target()); err != nil {
				r.t.Fatal(err)
			}
		}, "which is not there"},
		{"settings no service runs", func(r *rig) {
			writeFile(r.t, r.configFile(), []byte("libraryRoots: [/srv/music]\n"), 0o600)
		}, "but no service runs them"},
		{"the bridge at the target, with no service", func(r *rig) {
			writeFile(r.t, r.target(), fakeBridge(r.t, "0.2.1"), 0o755)
		}, programThere},
		{"another program at the target", func(r *rig) {
			writeFile(r.t, r.target(), []byte("#!/bin/sh\necho hello\n"), 0o755)
		}, programThere},
		{"a link at the target", func(r *rig) {
			other := filepath.Join(r.dir, "elsewhere", "bridge")
			writeFile(r.t, other, fakeBridge(r.t, "0.2.1"), 0o755)
			mkdirAll(r.t, filepath.Dir(r.target()))
			if err := os.Symlink(other, r.target()); err != nil {
				r.t.Fatal(err)
			}
		}, programThere},
		{"a directory at the target", func(r *rig) {
			mkdirAll(r.t, r.target())
		}, programThere},
	}
	eachShell(t, func(t *testing.T, sh shell) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := newRig(t, sh)
				r.newRelease(stampedRelease)
				r.set("FAKE_UPDATE", "ok")
				c.setup(r)
				home, state := tree(t, r.home), tree(t, r.state)
				res := r.run("--library", r.music("music"))
				mustExit(t, res, 3)
				mustSay(t, res, c.say, "Nothing was changed.")
				r.unchanged(home, state)
				if c.say == programThere {
					// The commands it prints run the program at the target,
					// whatever it is: the recorder stands in for it.
					if err := os.RemoveAll(r.target()); err != nil {
						t.Fatal(err)
					}
					sameCalls(t, r.runPrinted(printed(res.output()), r.target()), [][]string{
						{"bridge", "update"},
						{"bridge", "init"},
					})
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
// the install goes on. What the run says at its end is what lingering is
// then: on, already or by this run, it says the bridge starts at boot; off,
// or with no loginctl to turn it on, it never does.
func TestLingeringIsTurnedOnSoTheServiceOutlivesTheLogin(t *testing.T) {
	const boots = "is running as tester's service, and starts again when this box boots."
	cases := []struct {
		name     string
		upgrade  bool
		omit     []string
		env      []string
		terminal []string
		calls    []string
		linger   bool
		say      []string
	}{
		{name: "already on", linger: true, say: []string{boots}},
		{name: "by the session", env: []string{"STUB_SELF_LINGER", "1"}, linger: true,
			calls: []string{"loginctl --no-ask-password enable-linger"}, say: []string{boots}},
		{name: "by sudo without a terminal", linger: true,
			calls: []string{"sudo -n loginctl enable-linger tester"}, say: []string{"sudo runs loginctl enable-linger tester", boots}},
		{name: "by sudo asking on the terminal", env: []string{"STUB_SUDO_ASKS", "1"}, terminal: []string{"secret", ""}, linger: true,
			calls: []string{"sudo loginctl enable-linger tester", "stub sudo read secret"}, say: []string{boots}},
		{name: "by su without sudo", omit: []string{"sudo"}, terminal: []string{"rootpw", ""}, linger: true,
			calls: []string{"su root -c loginctl enable-linger tester", "stub su read rootpw"}, say: []string{"There is no sudo here", boots}},
		{name: "not at all", env: []string{"STUB_SUDO_ASKS", "1"}, linger: false,
			say: []string{"To turn it on, run as root:", "    loginctl enable-linger tester",
				"is running as tester's service, until tester's last login session on this box ends: lingering is not on"}},
		{name: "not at all, on an upgrade", upgrade: true, env: []string{"STUB_SUDO_ASKS", "1"}, linger: false,
			say: []string{"    loginctl enable-linger tester",
				"is running as tester's service, until tester's last login session on this box ends: lingering is not on"}},
		{name: "no loginctl", omit: []string{"loginctl"}, linger: false,
			say: []string{"There is no loginctl here", "is running as tester's service. This box has no loginctl, so lingering is not on"}},
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
				var res result
				if c.upgrade {
					r.installed("0.2.1", r.target(), r.configFile())
					r.set("FAKE_UPDATE", "latest")
					res = r.run()
				} else {
					res = r.run("--library", r.music("music"))
				}
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
				mustSay(t, res, c.say...)
				if !c.linger && strings.Contains(res.output(), "starts again when this box boots") {
					t.Errorf("with lingering off, it says the bridge starts at boot:\n%s", res.output())
				}
				if want := map[bool]int{false: 1, true: 0}[c.upgrade]; len(r.callsTo("bridge init")) != want {
					t.Errorf("bridge init ran %d times, want %d: %q", len(r.callsTo("bridge init")), want, r.callsTo("bridge init"))
				}
			})
		}
	})
}

// The installer asks the user's systemd manager what it has before anything
// changes, and bridge init installs the service through it. It finds the
// manager through /run/user/<uid> when the session did not say where (su, a
// script), changing nothing; with none, every run is refused with nothing
// changed (no download, no lingering turned on, which would start the
// manager), naming the lingering that would.
func TestTheUserManagerIsFoundOrNothingChanges(t *testing.T) {
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
		t.Run("none, on a new box", func(t *testing.T) {
			r := newRig(t, sh)
			r.set("STUB_NO_BUS", "1")
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 3)
			mustSay(t, res, "There is no systemd user session for tester here", "not with su or sudo", "Nothing was changed.")
			if len(r.asked()) != 0 {
				t.Errorf("it downloaded %q", r.asked())
			}
			r.nothingInstalled()
			if calls := r.callsTo("loginctl"); len(calls) != 0 {
				t.Errorf("lingering was asked about: %q", calls)
			}
			sameCalls(t, r.runPrinted(printed(res.output())), [][]string{{"loginctl", "enable-linger", "tester"}})
		})
		t.Run("none, with the bridge installed", func(t *testing.T) {
			r := newRig(t, sh)
			r.installed("0.2.1", r.target(), r.configFile())
			r.newRelease(stampedRelease)
			r.set("STUB_NO_BUS", "1", "FAKE_UPDATE", "ok")
			home, state := tree(t, r.home), tree(t, r.state)
			res := r.run()
			mustExit(t, res, 3)
			mustSay(t, res, "There is no systemd user session for tester here", "Nothing was changed.")
			r.unchanged(home, state)
		})
	})
}

// A system that does not run systemd gets the program and the commands to
// run it by hand (exit 4), and no service manager is asked anything.
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
		if strings.Contains(res.output(), "installer again") {
			t.Errorf("it says to run the installer again, which would refuse the program it left:\n%s", res.output())
		}
		sameCalls(t, r.runPrinted(printed(res.output()), r.target()), [][]string{
			{"bridge", "init", "--no-service", "--library", "/path/to/music"},
			{"bridge", "serve"},
		})
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

// A program at ~/.local/bin/bridge that no service of the user runs is
// never replaced, even when it is a bridge and running: only a bridge's own
// bridge update replaces a bridge, after checking its paired devices and
// keeping the previous program. The run is refused, the file is the one it
// was, and the program running from it goes on.
func TestAProgramAtTheTargetIsNeverReplaced(t *testing.T) {
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
		// A real executable that carries the bridge's module path, as
		// every bridge program does.
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
		home, state := tree(t, r.home), tree(t, r.state)
		res := r.run("--library", r.music("music"))
		mustExit(t, res, 3)
		mustSay(t, res, r.target()+" is there", "Nothing was changed.")
		r.unchanged(home, state)
		if err := running.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("the running program did not go on: %v", err)
		}
		after, err := os.Stat(r.target())
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(before, after) {
			t.Errorf("the program's file was replaced")
		}
	})
}

// A run stopped between staging the program and renaming it into place
// leaves nothing: no program, no staged copy, no directory it made for
// them, no temporary directory, and no setup.
func TestAnInterruptedInstallLeavesNothing(t *testing.T) {
	chmod, err := exec.LookPath("chmod")
	if err != nil {
		t.Fatal(err)
	}
	eachShell(t, func(t *testing.T, sh shell) {
		r := newRig(t, sh, "chmod")
		writeFile(t, filepath.Join(r.bin, "chmod"), []byte("#!/bin/sh\n"+
			"case \"$2\" in */.bridge.install.*) kill -TERM \"$PPID\" ;; esac\n"+
			"exec "+chmod+" \"$@\"\n"), 0o755)
		res := r.run("--library", r.music("music"))
		mustExit(t, res, 143)
		r.nothingInstalled()
		if len(r.callsTo("bridge init")) != 0 {
			t.Errorf("setup ran after the stop")
		}
	})
}

// What a run leaves decides the next one, which never replaces it. After
// --no-init, after a box without systemd and after an init that failed, the
// program is there with no service: the next run refuses with nothing
// changed, and the first named the commands that finish (bridge init),
// never a second run. A refusal for want of a library changes nothing, so
// the next run, given one, installs; and the run after an install upgrades
// it through bridge update.
func TestARunAfterOneThatDidNotFinish(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		refusedNext := func(t *testing.T, r *rig, first result) {
			t.Helper()
			if strings.Contains(first.output(), "installer again") {
				t.Errorf("the first run says to run the installer again, which refuses what it left:\n%s", first.output())
			}
			home, state := tree(t, r.home), tree(t, r.state)
			asked := len(r.asked())
			calls := len(r.mutations())
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 3)
			mustSay(t, res, r.target()+" is there", "Nothing was changed.")
			sameTree(t, "HOME", home, tree(t, r.home))
			sameTree(t, "the service's state", state, tree(t, r.state))
			if len(r.asked()) != asked || len(r.mutations()) != calls {
				t.Errorf("the second run acted: %q %q", r.asked()[asked:], r.mutations()[calls:])
			}
		}
		t.Run("after --no-init", func(t *testing.T) {
			r := newRig(t, sh)
			first := r.run("--no-init")
			mustExit(t, first, 0)
			refusedNext(t, r, first)
		})
		t.Run("after a box without systemd", func(t *testing.T) {
			r := newRig(t, sh)
			if err := os.RemoveAll(filepath.Join(r.root, "run", "systemd")); err != nil {
				t.Fatal(err)
			}
			first := r.run("--library", r.music("music"))
			mustExit(t, first, 4)
			refusedNext(t, r, first)
		})
		t.Run("after an init that failed", func(t *testing.T) {
			r := newRig(t, sh)
			r.set("FAKE_INIT_FAILS", "1")
			music := r.music("music")
			first := r.run("--library", music)
			mustExit(t, first, 1)
			mustSay(t, first, "bridge init stopped (exit 1)", "    "+r.target()+" init --library "+music)
			refusedNext(t, r, first)
		})
		t.Run("after a refusal for want of a library", func(t *testing.T) {
			r := newRig(t, sh)
			first := r.runHeldOpen()
			mustExit(t, first, 3)
			r.nothingInstalled()
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 0)
			mustSay(t, res, "is running as tester's service")
		})
		t.Run("after an install", func(t *testing.T) {
			r := newRig(t, sh)
			mustExit(t, r.run("--library", r.music("music")), 0)
			r.set("FAKE_UPDATE", "latest")
			res := r.run()
			mustExit(t, res, 0)
			mustSay(t, res, "Found the bridge 0.2.2, installed as tester's service", "The bridge 0.2.2 is the latest release.")
			if got, want := fmt.Sprintf("%q", r.bridgeCalls("update")), fmt.Sprintf("%q", [][]string{{"update", "--yes", "--config", r.configFile()}}); got != want {
				t.Errorf("bridge update calls %s, want %s", got, want)
			}
		})
	})
}

// A service that runs, but whose release bridge status cannot read, is not
// reported as running the release: the run ends incomplete (exit 4) with
// the command that checks it, after an upgrade and after a new install, and
// nothing after that (no pairing) is said as if it were.
func TestAServiceWhoseReleaseCannotBeReadIsNotReportedRunning(t *testing.T) {
	eachShell(t, func(t *testing.T, sh shell) {
		for _, upgrade := range []bool{true, false} {
			t.Run(map[bool]string{true: "an upgrade", false: "a new install"}[upgrade], func(t *testing.T) {
				r := newRig(t, sh)
				r.set("FAKE_STATUS_UNREADABLE", "1")
				var res result
				if upgrade {
					r.installed("0.2.1", r.target(), r.configFile())
					r.newRelease(stampedRelease)
					r.set("FAKE_UPDATE", "ok")
					res = r.run()
				} else {
					res = r.run("--library", r.music("music"))
				}
				mustExit(t, res, 4)
				mustSay(t, res, "bridge status could not say which release it serves", "Check it with:")
				for _, claim := range []string{"is running as tester's service", "Pair your iPhone"} {
					if strings.Contains(res.output(), claim) {
						t.Errorf("an unread release is reported as running (%q):\n%s", claim, res.output())
					}
				}
				if !exists(filepath.Join(r.state, "active")) {
					t.Errorf("the service is not active")
				}
				sameCalls(t, r.runPrinted(printed(res.output()), r.target()), [][]string{{"bridge", "status"}})
			})
		}
	})
}

// The bridge the installer runs (bridge init, bridge update, bridge status)
// must read the settings its service reads, and the bridge takes a BRIDGE_
// variable over its bridge.yaml, --config or not. So a run is refused, with
// nothing changed, where the service's environment holds what this run's
// would not, or this run's what the service's would not: an Environment=,
// EnvironmentFile= or PassEnvironment= line in the unit or in any drop-in,
// beside it or elsewhere (bridge init writes none), a BRIDGE_ variable the
// user's service manager gives every service, or one this shell exports.
// Each is named, never its value.
func TestAnInstallWhoseServiceHasAnotherEnvironmentIsRefused(t *testing.T) {
	const secret = "/srv/bridge-data-s3cret"
	cases := []struct {
		name  string
		setup func(r *rig)
		say   string
	}{
		{"Environment= in a drop-in beside the unit", func(r *rig) {
			writeFile(r.t, r.unitFile()+".d/data.conf", []byte("[Service]\nEnvironment=BRIDGE_DATA_DIR="+secret+"\n"), 0o644)
		}, "Environment, in " + "UNIT.d/data.conf"},
		{"EnvironmentFile= in a drop-in elsewhere", func(r *rig) {
			writeFile(r.t, filepath.Join(r.root, "etc", "systemd", "user", unitName+".d", "env.conf"), []byte("[Service]\nEnvironmentFile="+secret+"/bridge.env\n"), 0o644)
		}, "EnvironmentFile, in "},
		{"Environment= in a drop-in for every service", func(r *rig) {
			writeFile(r.t, filepath.Join(r.home, ".config", "systemd", "user", "service.d", "env.conf"), []byte("[Service]\nEnvironment=\"BRIDGE_ADMIN_ADDRESS=127.0.0.1:7790\" "+secret+"\n"), 0o644)
		}, "Environment, in "},
		{"PassEnvironment= in the unit", func(r *rig) {
			writeFile(r.t, r.unitFile(), []byte(strings.Replace(unitText(r.target(), r.configFile()), "Type=simple\n", "Type=simple\nPassEnvironment=BRIDGE_DATA_DIR "+secret+"\n", 1)), 0o644)
		}, "PassEnvironment, in "},
		{"a BRIDGE_ variable of the service manager's", func(r *rig) {
			r.set("STUB_MANAGER_ENV", "BRIDGE_DATA_DIR="+secret)
		}, "gives every service of tester BRIDGE_DATA_DIR"},
		{"a BRIDGE_ variable this shell exports", func(r *rig) {
			r.set("BRIDGE_DATA_DIR", secret)
		}, "This shell exports BRIDGE_DATA_DIR"},
	}
	eachShell(t, func(t *testing.T, sh shell) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := newRig(t, sh)
				r.installed("0.2.1", r.target(), r.configFile())
				r.newRelease(stampedRelease)
				r.set("FAKE_UPDATE", "ok")
				c.setup(r)
				home, state := tree(t, r.home), tree(t, r.state)
				res := r.run()
				mustExit(t, res, 3)
				mustSay(t, res, strings.Replace(c.say, "UNIT", r.unitFile(), 1), "Nothing was changed.")
				r.unchanged(home, state)
				if strings.Contains(res.output(), "s3cret") {
					t.Errorf("the refusal quotes the value:\n%s", res.output())
				}
			})
		}
		for _, c := range cases[4:] {
			t.Run(c.name+", on a new box", func(t *testing.T) {
				r := newRig(t, sh)
				c.setup(r)
				res := r.run("--library", r.music("music"))
				mustExit(t, res, 3)
				mustSay(t, res, c.say, "Nothing was changed.")
				r.nothingInstalled()
				if len(r.asked()) != 0 {
					t.Errorf("it downloaded %q", r.asked())
				}
				if strings.Contains(res.output(), "s3cret") {
					t.Errorf("the refusal quotes the value:\n%s", res.output())
				}
			})
		}
	})
}

// What the user's service manager has of the bridge's unit, wherever its
// files are (systemctl --user cat), decides as the files in HOME do: a unit
// of that name or a drop-in for it elsewhere, with none in HOME, is not a
// new box; a unit the manager runs from elsewhere in place of the one in
// HOME, or a drop-in elsewhere that sets its ExecStart, is not the install
// bridge init made; and a manager that cannot say is refused too, all with
// nothing changed. A drop-in of other settings changes nothing of what the
// upgrade runs.
func TestTheServiceManagerIsAskedWhatItRuns(t *testing.T) {
	elsewhere := func(r *rig, dirs ...string) string {
		return filepath.Join(append([]string{r.root}, dirs...)...)
	}
	cases := []struct {
		name      string
		installed bool
		setup     func(r *rig)
		say       string
	}{
		{"a unit of that name elsewhere, none here", false, func(r *rig) {
			writeFile(r.t, elsewhere(r, "etc", "systemd", "user", unitName), []byte(unitText("/opt/bridge/bridge", "/etc/bridge.yaml")), 0o644)
		}, "from files bridge init did not write"},
		{"a drop-in for it elsewhere, none here", false, func(r *rig) {
			writeFile(r.t, elsewhere(r, "usr", "lib", "systemd", "user", unitName+".d", "nice.conf"), []byte("[Service]\nNice=5\n"), 0o644)
		}, "from files bridge init did not write"},
		{"a manager that cannot say, on a new box", false, func(r *rig) {
			r.set("STUB_CAT_FAILS", "1")
		}, "Connection timed out"},
		{"a unit that takes the place of the one here", true, func(r *rig) {
			writeFile(r.t, elsewhere(r, "run", "user", "1000", "systemd", "transient", unitName), []byte(unitText("/opt/bridge/bridge", r.configFile())), 0o644)
		}, "runs the bridge's service from " + "TRANSIENT"},
		{"a drop-in elsewhere that sets its ExecStart", true, func(r *rig) {
			writeFile(r.t, elsewhere(r, "etc", "systemd", "user", unitName+".d", "exec.conf"), []byte("[Service]\nExecStart=\nExecStart=/opt/bridge/bridge serve\n"), 0o644)
		}, "sets its ExecStart more than once"},
		{"a manager that cannot say, with the bridge installed", true, func(r *rig) {
			r.set("STUB_CAT_FAILS", "1")
		}, "Connection timed out"},
	}
	eachShell(t, func(t *testing.T, sh shell) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				r := newRig(t, sh)
				r.newRelease(stampedRelease)
				r.set("FAKE_UPDATE", "ok")
				if c.installed {
					r.installed("0.2.1", r.target(), r.configFile())
				}
				c.setup(r)
				home, state := tree(t, r.home), tree(t, r.state)
				res := r.run("--library", r.music("music"))
				mustExit(t, res, 3)
				mustSay(t, res, strings.Replace(c.say, "TRANSIENT", elsewhere(r, "run", "user", "1000", "systemd", "transient", unitName), 1), "Nothing was changed.")
				r.unchanged(home, state)
			})
		}
		t.Run("a drop-in of other settings", func(t *testing.T) {
			r := newRig(t, sh)
			r.installed("0.2.1", r.target(), r.configFile())
			writeFile(t, r.unitFile()+".d/nice.conf", []byte("[Service]\nNice=5\nCPUWeight=50\n"), 0o644)
			r.newRelease(stampedRelease)
			r.set("FAKE_UPDATE", "ok")
			res := r.run()
			mustExit(t, res, 0)
			mustSay(t, res, "Updated the bridge from 0.2.1 to 0.2.2.")
		})
	})
}

// Every command the installer prints for the user to type runs as printed,
// pasted into the shell: each word quoted where it needs it, so that a path
// with a space, an apostrophe or a semicolon in it stays one word and runs
// nothing else, and the settings named with --config wherever they are not
// where the bridge looks without it, on the commands that read them (pair,
// status, serve and update; init, logs and start take no --config).
func TestThePrintedCommandsRunAsPrinted(t *testing.T) {
	const odd = "it's; injected ;x"
	eachShell(t, func(t *testing.T, sh shell) {
		t.Run("after an upgrade of a bridge with its settings elsewhere", func(t *testing.T) {
			r := newRig(t, sh)
			bin := filepath.Join(r.home, "apps "+odd, "bridge")
			config := filepath.Join(r.home, "settings "+odd, "bridge.yaml")
			r.installed("0.2.1", bin, config)
			r.newRelease(stampedRelease)
			r.set("FAKE_UPDATE", "ok")
			res := r.run()
			mustExit(t, res, 0)
			sameCalls(t, r.runPrinted(printed(res.output()), bin), [][]string{
				{"ssh", "-L", "7789:127.0.0.1:7789", "tester@192.0.2.10"},
				{"bridge", "pair", "--name", "My iPhone", "--config", config},
				{"bridge", "status", "--config", config},
			})
		})
		t.Run("a new install in a HOME of that name", func(t *testing.T) {
			r := newRig(t, sh)
			r.moveHome("home " + odd)
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 0)
			sameCalls(t, r.runPrinted(printed(res.output()), r.target()), [][]string{
				{"ssh", "-L", "7789:127.0.0.1:7789", "tester@192.0.2.10"},
				{"bridge", "pair", "--name", "My iPhone"},
				{"bridge", "status"},
			})
		})
		t.Run("a new install the PATH finds", func(t *testing.T) {
			r := newRig(t, sh)
			r.set("PATH", r.bin+":"+filepath.Dir(r.target()))
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 0)
			mustSay(t, res, "    bridge pair --name 'My iPhone'", "    bridge status")
			sameCalls(t, r.runPrinted(printed(res.output())), [][]string{
				{"ssh", "-L", "7789:127.0.0.1:7789", "tester@192.0.2.10"},
				{"bridge", "pair", "--name", "My iPhone"},
				{"bridge", "status"},
			})
		})
		t.Run("an init that failed, its library of that name", func(t *testing.T) {
			r := newRig(t, sh)
			r.set("FAKE_INIT_FAILS", "1")
			music := r.music("music " + odd)
			res := r.run("--library", music)
			mustExit(t, res, 1)
			sameCalls(t, r.runPrinted(printed(res.output()), r.target()), [][]string{
				{"bridge", "init", "--library", music},
			})
		})
		t.Run("a box without systemd, in a HOME of that name", func(t *testing.T) {
			r := newRig(t, sh)
			r.moveHome("home " + odd)
			if err := os.RemoveAll(filepath.Join(r.root, "run", "systemd")); err != nil {
				t.Fatal(err)
			}
			res := r.run("--library", r.music("music"))
			mustExit(t, res, 4)
			sameCalls(t, r.runPrinted(printed(res.output()), r.target()), [][]string{
				{"bridge", "init", "--no-service", "--library", "/path/to/music"},
				{"bridge", "serve"},
			})
		})
		t.Run("a service that does not start, its program elsewhere", func(t *testing.T) {
			r := newRig(t, sh)
			bin := filepath.Join(r.home, "apps "+odd, "bridge")
			config := filepath.Join(r.home, "settings "+odd, "bridge.yaml")
			r.installed("0.2.1", bin, config)
			if err := os.Remove(filepath.Join(r.state, "active")); err != nil {
				t.Fatal(err)
			}
			r.set("FAKE_UPDATE", "latest", "STUB_SERVICE_FAILS", "1")
			res := r.run()
			mustExit(t, res, 1)
			sameCalls(t, r.runPrinted(printed(res.output()), bin), [][]string{{"bridge", "logs"}})
		})
		t.Run("a service whose release cannot be read, its settings elsewhere", func(t *testing.T) {
			r := newRig(t, sh)
			bin := filepath.Join(r.home, "apps "+odd, "bridge")
			config := filepath.Join(r.home, "settings "+odd, "bridge.yaml")
			r.installed("0.2.1", bin, config)
			r.set("FAKE_UPDATE", "latest", "FAKE_STATUS_UNREADABLE", "1")
			res := r.run()
			mustExit(t, res, 4)
			sameCalls(t, r.runPrinted(printed(res.output()), bin), [][]string{{"bridge", "status", "--config", config}})
		})
		t.Run("a restart that failed", func(t *testing.T) {
			r := newRig(t, sh)
			r.installed("0.2.1", r.target(), r.configFile())
			r.newRelease(stampedRelease)
			r.set("FAKE_UPDATE", "restartfail")
			res := r.run()
			mustExit(t, res, 1)
			sameCalls(t, r.runPrinted(printed(res.output())), [][]string{{"systemctl", "--user", "restart", unitName}})
		})
		t.Run("--no-init on a bridge with its settings elsewhere", func(t *testing.T) {
			r := newRig(t, sh)
			bin := filepath.Join(r.home, "apps "+odd, "bridge")
			config := filepath.Join(r.home, "settings "+odd, "bridge.yaml")
			r.installed("0.2.1", bin, config)
			res := r.run("--no-init")
			mustExit(t, res, 3)
			sameCalls(t, r.runPrinted(printed(res.output()), bin), [][]string{{"bridge", "update", "--config", config}})
		})
		t.Run("--uninstall in a HOME of that name", func(t *testing.T) {
			r := newRig(t, sh)
			r.moveHome("home " + odd)
			r.installed(stampedRelease, r.target(), r.configFile())
			res := r.run("--uninstall")
			mustExit(t, res, 0)
			sameCalls(t, r.runPrinted(printed(res.output())), [][]string{
				{"systemctl", "--user", "disable", "--now", unitName},
				{"systemctl", "--user", "is-active", unitName},
				{"rm", r.unitFile()},
				{"systemctl", "--user", "daemon-reload"},
				{"rm", "-f", r.target(), r.target() + ".bak"},
				{"rm", "-rf", filepath.Dir(r.configFile())},
			})
		})
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
