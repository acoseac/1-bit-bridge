package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The bridge's own files inside a library root (backlog B884): bridge init,
// bridge doctor and the console's doctor, and serve's journal each say so, a
// warning and never a refusal, from one lexical check and in one set of words
// (internal/config's layout tests pin the check and the words; these, where
// each surface says them).

// layoutRiskWords is the hazard every surface says after a layout's fact,
// word for word (backlog B884's round 4), and what these tests find a
// layout's warning by, whatever its fact says: a file that is a root is said
// with no "inside".
const layoutRiskWords = "a library root is often a mount, and while one stops answering, every bridge command that " +
	"reads what is inside it waits on it, serve, doctor and status among them"

// layoutRemedies are what B884's first three rounds said to do after a
// layout's fact, which no surface says now (round 4): round 3's fixed
// sentence and the README.md section it named, and the journal's three
// sentences of rounds 1 to 3.
var layoutRemedies = []string{
	"move bridge.yaml and the data directory out of every library root, and check every path setting in bridge.yaml",
	"Moving the bridge's files out of a library root",
	"move the data directory outside every library root, and set dataDir in bridge.yaml to its new path",
	"move bridge.yaml outside every library root, first setting dataDir in it to the data directory's absolute path",
	"move bridge.yaml and the data directory outside every library root, keeping dataDir pointed at the data directory",
}

// journalLayoutLine is serve's recorded line saying summary, by settings: the
// fact, then the hazard.
func journalLayoutLine(summary string) string {
	return "WARN " + summary + ": " + layoutRiskWords + " component=bridge"
}

// writeLayoutConfig writes the config at cfgPath naming dataDir and roots
// (each made, as a directory), its listeners on ports the kernel picks, MPD's
// output off, and tail after it.
func writeLayoutConfig(t *testing.T, cfgPath, dataDir string, roots []string, tail string) {
	t.Helper()
	writeLayoutConfigLeaving(t, cfgPath, dataDir, roots, nil, tail)
}

// writeLayoutConfigLeaving is writeLayoutConfig making every root but those in
// absent, which a case needs not there.
func writeLayoutConfigLeaving(t *testing.T, cfgPath, dataDir string, roots, absent []string, tail string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("libraryRoots:\n")
	for _, r := range roots {
		if !slices.Contains(absent, r) {
			if err := os.MkdirAll(r, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		b.WriteString("  - " + r + "\n")
	}
	b.WriteString("dataDir: " + dataDir + "\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\noutput:\n  mpd: off\n" + tail)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBothDoctorsWarnOfTheBridgesFilesInsideALibraryRoot: bridge doctor and
// the console's doctor carry an install-layout warn, word for word, where the
// data directory lies inside a library root or is one, where the config file
// does, and where both do (one line naming both), its hint the hazard alone,
// the same for every layout (backlog B884's round 4); bridge doctor exits 0
// over it, since a layout is never a refusal. None of them had the line
// before backlog B884.
func TestBothDoctorsWarnOfTheBridgesFilesInsideALibraryRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		// lay returns the config's path, its data directory and roots, and
		// the line's summary.
		lay func(base string) (cfg, data string, roots []string, summary string)
	}{
		{"the data directory inside a root", func(base string) (string, string, []string, string) {
			lib, data := filepath.Join(base, "Music"), filepath.Join(base, "Music", ".bridge")
			return filepath.Join(base, "bridge.yaml"), data, []string{lib},
				"the data directory " + data + " is inside library root " + lib + " (libraryRoots[0])"
		}},
		{"the data directory that is a root", func(base string) (string, string, []string, string) {
			other, lib := filepath.Join(base, "Other"), filepath.Join(base, "Music")
			return filepath.Join(base, "bridge.yaml"), lib, []string{other, lib},
				"the data directory " + lib + " is library root " + lib + " (libraryRoots[1])"
		}},
		{"the config file inside a root", func(base string) (string, string, []string, string) {
			lib, cfg, data := filepath.Join(base, "Music"), filepath.Join(base, "Music", "conf", "bridge.yaml"), filepath.Join(base, "data")
			return cfg, data, []string{lib},
				"the config file " + cfg + " is inside library root " + lib + " (libraryRoots[0])"
		}},
		{"both inside one root", func(base string) (string, string, []string, string) {
			lib, cfg, data := filepath.Join(base, "Music"), filepath.Join(base, "Music", "b", "bridge.yaml"), filepath.Join(base, "Music", "b", "data")
			return cfg, data, []string{lib},
				"the config file " + cfg + " and the data directory " + data + " are inside library root " + lib + " (libraryRoots[0])"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, data, roots, summary := tc.lay(t.TempDir())
			writeLayoutConfig(t, cfg, data, roots, "")
			said := askBothDoctors(t, cfg)
			c := said.requireLine(t, installLayoutCheckName, doctor.Warn)
			if c.Summary != summary {
				t.Errorf("the install-layout line says %q, want %q", c.Summary, summary)
			}
			if c.Hint != layoutRiskWords {
				t.Errorf("the install-layout hint is %q\nwant the hazard alone, %q", c.Hint, layoutRiskWords)
			}
			said.requirePrinted(t, c)
			if said.code != 0 {
				t.Errorf("bridge doctor exited %d over a layout, which is never a refusal; it printed\n%s", said.code, said.printed)
			}
		})
	}
}

// TestNoDoctorWarnsOfASiblingThatSharesARootsPrefix (backlog B884's case 3):
// a data directory and a config in /x/music2 are not inside the root
// /x/music, whose name only begins theirs, and no doctor says they are.
func TestNoDoctorWarnsOfASiblingThatSharesARootsPrefix(t *testing.T) {
	base := t.TempDir()
	sibling := filepath.Join(base, "music2")
	writeLayoutConfig(t, filepath.Join(sibling, "bridge.yaml"), filepath.Join(sibling, "data"), []string{filepath.Join(base, "music")}, "")
	said := askBothDoctors(t, filepath.Join(sibling, "bridge.yaml"))
	// The config loaded: its data directory is graded.
	said.requireLine(t, dataDirCheckSlug, doctor.OK)
	for name, lines := range map[string]map[string]doctor.Check{"bridge doctor": said.cli, "the console's doctor": said.console} {
		if c, ok := lines[installLayoutCheckName]; ok {
			t.Errorf("%s warns of a sibling that shares the root's prefix: %+v", name, c)
		}
	}
	if strings.Contains(said.printed, layoutRiskWords) {
		t.Errorf("bridge doctor printed a layout warning:\n%s", said.printed)
	}
}

// TestADataDirectoryThatIsAFileInsideARootFailsAndWarnsBeside: a data
// directory serve cannot prepare (a regular file at its path) that lies
// inside a library root still fails data-dir, in the words B659 gave it, and
// the layout's warn stands right after it: the new line changes no other
// line's severity or words.
func TestADataDirectoryThatIsAFileInsideARootFailsAndWarnsBeside(t *testing.T) {
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	data := filepath.Join(lib, "data")
	writeLayoutConfig(t, filepath.Join(base, "bridge.yaml"), data, []string{lib}, "")
	writeFileOrFail(t, data)
	said := askBothDoctors(t, filepath.Join(base, "bridge.yaml"))
	failed := said.requireLine(t, dataDirCheckSlug, doctor.Fail)
	if want := "bridge serve will not start: the data directory " + data + " is a regular file, not a directory"; failed.Summary != want {
		t.Errorf("the data-dir line says %q, want %q", failed.Summary, want)
	}
	warned := said.requireLine(t, installLayoutCheckName, doctor.Warn)
	if want := "the data directory " + data + " is inside library root " + lib + " (libraryRoots[0])"; warned.Summary != want {
		t.Errorf("the install-layout line says %q, want %q", warned.Summary, want)
	}
	rep := doctor.Run(context.Background(), buildDoctorDeps(filepath.Join(base, "bridge.yaml")))
	for i, c := range rep.Checks {
		if c.Name == dataDirCheckSlug && (i+1 >= len(rep.Checks) || rep.Checks[i+1].Name != installLayoutCheckName) {
			t.Errorf("the line after data-dir is not install-layout: %+v", rep.Checks)
		}
	}
	if said.code != 1 {
		t.Errorf("bridge doctor exited %d over a data directory serve refuses, want 1", said.code)
	}
}

// TestAManagedBridgesDoctorSaysNothingOfItsLayout: a managed bridge's files
// are the host's to move, and its reader has no shell to move them with
// (doctor.Deps.Managed), so neither doctor carries the line there; the same
// layout, unmanaged, has it.
func TestAManagedBridgesDoctorSaysNothingOfItsLayout(t *testing.T) {
	for _, managed := range []bool{true, false} {
		base := t.TempDir()
		lib := filepath.Join(base, "Music")
		tail := ""
		if managed {
			tail = "deployment:\n  managedControls:\n    - restart\n"
		}
		writeLayoutConfig(t, filepath.Join(base, "bridge.yaml"), filepath.Join(lib, "data"), []string{lib}, tail)
		said := askBothDoctors(t, filepath.Join(base, "bridge.yaml"))
		// The config loaded: its data directory is graded.
		said.requireLine(t, dataDirCheckSlug, doctor.OK)
		if !managed {
			said.requireLine(t, installLayoutCheckName, doctor.Warn)
			continue
		}
		for name, lines := range map[string]map[string]doctor.Check{"bridge doctor": said.cli, "the console's doctor": said.console} {
			if c, ok := lines[installLayoutCheckName]; ok {
				t.Errorf("%s of a managed bridge carries %+v", name, c)
			}
		}
	}
}

// TestTheDoctorsReportOfAHealthyLayoutIsAsItWas (backlog B884's case 7): over
// a layout with neither file inside a root, a sibling with a shared prefix
// among them, bridge doctor's report, as printed, is byte for byte the report
// with no layout check wired at all, which is the report before B884, and
// the console's carries no line either.
func TestTheDoctorsReportOfAHealthyLayoutIsAsItWas(t *testing.T) {
	for name, lay := range map[string]func(base string) (cfg, data string, roots []string){
		"apart": func(base string) (string, string, []string) {
			return filepath.Join(base, "bridge.yaml"), filepath.Join(base, "data"), []string{filepath.Join(base, "Music")}
		},
		"a sibling with a shared prefix": func(base string) (string, string, []string) {
			return filepath.Join(base, "Music2", "bridge.yaml"), filepath.Join(base, "Music2", "data"), []string{filepath.Join(base, "Music")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, data, roots := lay(t.TempDir())
			writeLayoutConfig(t, cfg, data, roots, "")
			with := buildDoctorDeps(cfg)
			if with.InstallLayout == nil {
				t.Fatal("bridge doctor wires no layout check over a config that loads")
			}
			without := with
			without.InstallLayout = nil
			var got, want bytes.Buffer
			printReport(&got, doctor.Run(context.Background(), with))
			printReport(&want, doctor.Run(context.Background(), without))
			if got.String() != want.String() {
				t.Errorf("a healthy layout's report changed:\n%s\nwant\n%s", got.String(), want.String())
			}
			for _, c := range adminDoctorRunner(cfg, nil, nil)(context.Background()).Checks {
				if c.Name == installLayoutCheckName {
					t.Errorf("the console's doctor carries %+v over a healthy layout", c)
				}
			}
		})
	}
}

// initLayoutRun runs bridge init, non-interactive, with no service, and
// returns its exit code and its two streams.
func initLayoutRun(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := initCmd(append([]string{"--yes", "--no-service"}, args...), strings.NewReader(""), &out, &errOut)
	return code, stripANSI(out.String()), stripANSI(errOut.String())
}

// initPorts are the two loopback ports a run with its preflight on writes
// and grades, so the preflight never meets another process on 7788 or 7789.
func initPorts(t *testing.T) []string {
	t.Helper()
	return []string{"--listen-address", "127.0.0.1:" + strconv.Itoa(freeLoopbackPort(t)),
		"--admin-address", "127.0.0.1:" + strconv.Itoa(freeLoopbackPort(t))}
}

// requireOneLayoutWarning requires a run's output to carry one layout
// warning, counted by the hazard whatever its fact says, and that warning to
// be one line of stderr, exactly: summary, the hazard, and the one period
// bridge init's line ends with (backlog B884's round 4).
func requireOneLayoutWarning(t *testing.T, stdout, stderr, summary string) {
	t.Helper()
	if n := strings.Count(stdout+stderr, layoutRiskWords); n != 1 {
		t.Errorf("the run carries %d layout warnings, want one; stdout:\n%s\nstderr:\n%s", n, stdout, stderr)
	}
	line := "warning: " + summary + ": " + layoutRiskWords + ".\n"
	if n := strings.Count(stderr, line); n != 1 {
		t.Errorf("the run's stderr carries the warning %d times, want once:\n%s\nstderr:\n%s", n, line, stderr)
	}
}

// TestBridgeInitWarnsOnceOfTheLayoutItWritesOrKeeps (backlog B884's cases 1
// and 2 for bridge init): a run that writes a config, or keeps one, warns
// once of the layout it actually wrote or kept, on stderr, with its preflight
// on or skipped (the preflight carries no layout line): a first install
// whose data directory is the library root, whose config folder is the
// library's own folder, or whose config folder is inside the library; a run
// that keeps a config, by the config's own roots whatever
// it was given; a --force rewrite, by the roots it writes; a public rewrite
// that keeps the install's roots, which its preflight does not grade; and a
// rewrite that keeps a data directory outside the root while its config is
// inside it. Every run exits 0. None of them warned before B884.
func TestBridgeInitWarnsOnceOfTheLayoutItWritesOrKeeps(t *testing.T) {
	t.Run("a first install whose data directory is the library root", func(t *testing.T) {
		cfgDir := filepath.Join(t.TempDir(), "cfg")
		lib := filepath.Join(cfgDir, "data")
		if err := os.MkdirAll(lib, 0o755); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := initLayoutRun(t, append(initPorts(t), "--dir", cfgDir, "--library", lib)...)
		if code != 0 {
			t.Fatalf("init exited %d:\n%s\n%s", code, stdout, stderr)
		}
		requireOneLayoutWarning(t, stdout, stderr, "the data directory "+lib+" is library root "+lib+" (libraryRoots[0])")
	})
	t.Run("a first install whose config folder is the library's own folder", func(t *testing.T) {
		lib := filepath.Join(t.TempDir(), "Music")
		if err := os.MkdirAll(lib, 0o755); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := initLayoutRun(t, append(initPorts(t), "--dir", lib, "--library", lib)...)
		if code != 0 {
			t.Fatalf("init exited %d:\n%s\n%s", code, stdout, stderr)
		}
		requireOneLayoutWarning(t, stdout, stderr, "the config file "+filepath.Join(lib, "bridge.yaml")+" and the data directory "+
			filepath.Join(lib, "data")+" are inside library root "+lib+" (libraryRoots[0])")
	})
	for _, skip := range []bool{false, true} {
		t.Run("a first install whose config folder is in the library, preflight skipped "+strconv.FormatBool(skip), func(t *testing.T) {
			lib := filepath.Join(t.TempDir(), "Music")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			cfgDir := filepath.Join(lib, "bridge")
			args := append(initPorts(t), "--dir", cfgDir, "--library", lib)
			if skip {
				args = append(args, "--skip-doctor")
			}
			code, stdout, stderr := initLayoutRun(t, args...)
			if code != 0 {
				t.Fatalf("init exited %d:\n%s\n%s", code, stdout, stderr)
			}
			requireOneLayoutWarning(t, stdout, stderr, "the config file "+filepath.Join(cfgDir, "bridge.yaml")+" and the data directory "+
				filepath.Join(cfgDir, "data")+" are inside library root "+lib+" (libraryRoots[0])")
		})
	}
	t.Run("a run that keeps a config, by the config's own roots", func(t *testing.T) {
		base := t.TempDir()
		lib, other := filepath.Join(base, "Music"), filepath.Join(base, "Other")
		cfgDir := filepath.Join(lib, "bridge")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(lib, 0o755); err != nil {
			t.Fatal(err)
		}
		if code, stdout, stderr := initLayoutRun(t, append(initPorts(t), "--skip-doctor", "--dir", cfgDir, "--library", lib)...); code != 0 {
			t.Fatalf("the first init exited %d:\n%s\n%s", code, stdout, stderr)
		}
		// Its preflight on, grading the kept install's own ports.
		code, stdout, stderr := initLayoutRun(t, "--dir", cfgDir, "--library", other)
		if code != 0 || !strings.Contains(stdout, "keeping it") {
			t.Fatalf("the second init exited %d, keeping nothing:\n%s\n%s", code, stdout, stderr)
		}
		requireOneLayoutWarning(t, stdout, stderr, "the config file "+filepath.Join(cfgDir, "bridge.yaml")+" and the data directory "+
			filepath.Join(cfgDir, "data")+" are inside library root "+lib+" (libraryRoots[0])")
	})
	t.Run("a --force rewrite, by the roots it writes", func(t *testing.T) {
		base := t.TempDir()
		lib, cfgDir := filepath.Join(base, "Music"), filepath.Join(base, "cfg")
		if err := os.MkdirAll(lib, 0o755); err != nil {
			t.Fatal(err)
		}
		if code, stdout, stderr := initLayoutRun(t, "--skip-doctor", "--dir", cfgDir, "--library", lib); code != 0 || strings.Contains(stderr, layoutRiskWords) {
			t.Fatalf("the first init exited %d, or warned of a layout with nothing inside a root:\n%s\n%s", code, stdout, stderr)
		}
		code, stdout, stderr := initLayoutRun(t, append(initPorts(t), "--force", "--dir", cfgDir, "--library", base)...)
		if code != 0 {
			t.Fatalf("the rewrite exited %d:\n%s\n%s", code, stdout, stderr)
		}
		requireOneLayoutWarning(t, stdout, stderr, "the config file "+filepath.Join(cfgDir, "bridge.yaml")+" and the data directory "+
			filepath.Join(cfgDir, "data")+" are inside library root "+base+" (libraryRoots[0])")
	})
	t.Run("a public rewrite that keeps the install's roots", func(t *testing.T) {
		lib := filepath.Join(t.TempDir(), "Music")
		if err := os.MkdirAll(lib, 0o755); err != nil {
			t.Fatal(err)
		}
		cfgDir := filepath.Join(lib, "bridge")
		ports := pickPublicInitPorts(t)
		if code, out := publicInit(t, cfgDir, ports, "Existing", "--skip-doctor", "--library", lib); code != 0 {
			t.Fatalf("the first public init exited %d:\n%s", code, out)
		}
		code, out := publicInit(t, cfgDir, ports, "Existing", "--force")
		if code != 0 {
			t.Fatalf("the public rewrite exited %d:\n%s", code, out)
		}
		stdout, stderr, _ := strings.Cut(out, "\n--- stderr ---\n")
		requireOneLayoutWarning(t, stdout, stderr, "the config file "+filepath.Join(cfgDir, "bridge.yaml")+" and the data directory "+
			filepath.Join(cfgDir, "data")+" are inside library root "+lib+" (libraryRoots[0])")
	})
	t.Run("a rewrite that keeps a data directory outside the root its config is in", func(t *testing.T) {
		base := t.TempDir()
		lib, data := filepath.Join(base, "Music"), filepath.Join(base, "data")
		cfgPath := filepath.Join(lib, "conf", "bridge.yaml")
		writeLayoutConfig(t, cfgPath, data, []string{lib}, "")
		code, stdout, stderr := initLayoutRun(t, append(initPorts(t), "--force", "--dir", filepath.Dir(cfgPath), "--library", lib)...)
		if code != 0 {
			t.Fatalf("the rewrite exited %d:\n%s\n%s", code, stdout, stderr)
		}
		requireOneLayoutWarning(t, stdout, stderr, "the config file "+cfgPath+" is inside library root "+lib+" (libraryRoots[0])")
	})
}

// TestBridgeInitSaysNothingOfAHealthyLayout (backlog B884's cases 3 and 7 for
// bridge init): a run over a layout with neither file inside a root, a
// sibling with a shared prefix among them, prints what it printed before
// B884: its stderr stays empty, and no stream says a layout. The layout's
// warning, the one line B884 adds to init, writes nothing for the layout the
// run wrote, or kept.
func TestBridgeInitSaysNothingOfAHealthyLayout(t *testing.T) {
	base := t.TempDir()
	lib, sibling := filepath.Join(base, "music"), filepath.Join(base, "music2")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, run := range []struct {
		name string
		args []string
	}{
		{"a first install beside a root that shares its prefix", append(initPorts(t), "--dir", sibling, "--library", lib)},
		{"a run that keeps it", append(initPorts(t), "--dir", sibling, "--library", lib)},
		{"a rewrite of it", append(initPorts(t), "--force", "--dir", sibling, "--library", lib)},
	} {
		code, stdout, stderr := initLayoutRun(t, run.args...)
		if code != 0 {
			t.Fatalf("%s exited %d:\n%s\n%s", run.name, code, stdout, stderr)
		}
		if stderr != "" || strings.Contains(stdout, layoutRiskWords) {
			t.Errorf("%s said more than before B884; stdout:\n%s\nstderr:\n%s", run.name, stdout, stderr)
		}
	}
	var w bytes.Buffer
	warnInstallLayout(&w, filepath.Join(sibling, "bridge.yaml"), filepath.Join(sibling, "data"), []string{lib})
	if w.Len() != 0 {
		t.Errorf("init's layout warning wrote %q over a healthy layout", w.String())
	}
}

// startLayoutBridge stands up `bridge serve` on the config at cfgPath, its
// listeners handed in, and returns once its console answers.
func startLayoutBridge(t *testing.T, cfgPath string) servedBridge {
	t.Helper()
	lan, admin := holdLoopback(t), holdLoopback(t)
	opts := serveOpts{configPath: cfgPath, addrOverride: lan.addr, lanListener: lan.ln, adminListener: admin.ln}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(raw), "adminAddress: \"127.0.0.1:0\"", "adminAddress: \""+admin.addr+"\"", 1)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	b := launchServe(t, func(ctx context.Context, stdout, stderr io.Writer) int {
		return runServe(ctx, opts, stdout, stderr)
	})
	waitForAdminReady(t, admin.addr, b.done, b.stderr)
	return b
}

// layoutLines are the lines serve logged about a layout, at any level,
// found by the hazard each says, whatever its fact says.
func layoutLines(rec *loggingtest.Recorder) []string {
	var out []string
	for _, l := range rec.All() {
		if strings.Contains(l, layoutRiskWords) {
			out = append(out, l)
		}
	}
	return out
}

// TestServeSaysOnceAtItsStartThatItsDataDirectoryIsInsideALibraryRoot
// (backlog B884's case 1 for serve): a serve whose data directory lies inside
// a library root starts, and its journal says so once, at Warn, naming the
// root by its place in libraryRoots and the data directory by its setting,
// and no path. Serve said nothing of it before B884.
func TestServeSaysOnceAtItsStartThatItsDataDirectoryIsInsideALibraryRoot(t *testing.T) {
	rec := loggingtest.Record(t)
	base := t.TempDir()
	lib, data, cfg := filepath.Join(base, "Music"), filepath.Join(base, "Music", ".bridge"), filepath.Join(base, "bridge.yaml")
	writeLayoutConfig(t, cfg, data, []string{lib}, "")
	startLayoutBridge(t, cfg)
	lines := layoutLines(rec)
	want := journalLayoutLine("the data directory (dataDir) is inside library root libraryRoots[0]")
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("serve logged %q of its layout, want the one line %q", lines, want)
	}
	if strings.Contains(lines[0], base) {
		t.Errorf("serve's layout line names a path: %q", lines[0])
	}
}

// TestServeSaysEachLayoutAtItsStart (backlog B884's cases 1 to 3 for serve):
// each layout is said once, at the start, by a serve that goes no further (an
// --addr it refuses, after the roots' check), in the journal's words, the
// fact by setting and the hazard, and no path (B505; backlog B884's round 4):
// the data directory that is the root, the config file inside a root, both;
// and nothing for a sibling with a shared prefix, or files apart from the
// roots.
func TestServeSaysEachLayoutAtItsStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		lay  func(base string) (cfg, data string, roots []string)
		want string
	}{
		{"the data directory that is a root", func(base string) (string, string, []string) {
			return filepath.Join(base, "bridge.yaml"), filepath.Join(base, "Music"), []string{filepath.Join(base, "Other"), filepath.Join(base, "Music")}
		}, journalLayoutLine("the data directory (dataDir) is library root libraryRoots[1]")},
		{"the config file inside a root", func(base string) (string, string, []string) {
			return filepath.Join(base, "Music", "conf", "bridge.yaml"), filepath.Join(base, "data"), []string{filepath.Join(base, "Music")}
		}, journalLayoutLine("the config file is inside library root libraryRoots[0]")},
		{"both inside a root", func(base string) (string, string, []string) {
			return filepath.Join(base, "Music", "b", "bridge.yaml"), filepath.Join(base, "Music", "b", "data"), []string{filepath.Join(base, "Music")}
		}, journalLayoutLine("the config file and the data directory (dataDir) are inside library root libraryRoots[0]")},
		{"a sibling with a shared prefix", func(base string) (string, string, []string) {
			return filepath.Join(base, "Music2", "bridge.yaml"), filepath.Join(base, "Music2", "data"), []string{filepath.Join(base, "Music")}
		}, ""},
		{"apart", func(base string) (string, string, []string) {
			return filepath.Join(base, "bridge.yaml"), filepath.Join(base, "data"), []string{filepath.Join(base, "Music")}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := loggingtest.Record(t)
			base := t.TempDir()
			cfg, data, roots := tc.lay(base)
			writeLayoutConfig(t, cfg, data, roots, "")
			_, stderr, code := runCapture(t, "serve", "--config", cfg, "--addr", "notaport")
			if code != 2 || !strings.Contains(stderr, "invalid --addr") {
				t.Fatalf("serve exited %d, not at its --addr refusal: %s", code, stderr)
			}
			lines := layoutLines(rec)
			switch {
			case tc.want == "" && len(lines) != 0:
				t.Errorf("serve logged %q of a healthy layout", lines)
			case tc.want != "" && (len(lines) != 1 || lines[0] != tc.want):
				t.Errorf("serve logged %q of its layout, want the one line %q", lines, tc.want)
			}
			for _, l := range lines {
				if strings.Contains(l, base) {
					t.Errorf("serve's layout line names a path: %q", l)
				}
			}
		})
	}
}

// consoleDoctor asks the console's doctor, GET /api/doctor, for its lines by
// name.
func consoleDoctor(t *testing.T, b *consoleBridge) map[string]doctor.Check {
	t.Helper()
	resp, err := b.console.Get(b.adminBase + "/api/doctor")
	if err != nil {
		t.Fatalf("GET /api/doctor: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Available bool `json:"available"`
		Report    struct {
			Checks []struct{ Name, Status, Summary, Hint string } `json:"checks"`
		} `json:"report"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !body.Available {
		t.Fatalf("GET /api/doctor answered %d, available %v: %v", resp.StatusCode, body.Available, err)
	}
	out := map[string]doctor.Check{}
	for _, c := range body.Report.Checks {
		out[c.Name] = doctor.Check{Name: c.Name, Status: doctor.Status(c.Status), Summary: c.Summary, Hint: c.Hint}
	}
	return out
}

// TestTheConsolesDoctorSaysTheLayoutOfARootAddedWhileServeRuns: the console's
// doctor reads the roots the config holds at each run, so a root added
// through POST /api/roots that holds the data directory is said at its next
// run; serve's journal, which says its layout once at the start, says nothing
// of it.
func TestTheConsolesDoctorSaysTheLayoutOfARootAddedWhileServeRuns(t *testing.T) {
	rec := loggingtest.Record(t)
	b := startConsoleBridge(t, "", nil)
	if c, ok := consoleDoctor(t, b)[installLayoutCheckName]; ok {
		t.Fatalf("the console's doctor warns of a layout with nothing inside a root: %+v", c)
	}
	payload, err := json.Marshal(map[string]string{"path": b.dataDir})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := b.console.Post(b.adminBase+"/api/roots", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /api/roots: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/roots answered %d: %s", resp.StatusCode, raw)
	}
	c, ok := consoleDoctor(t, b)[installLayoutCheckName]
	want := "the data directory " + b.dataDir + " is library root " + b.dataDir + " (libraryRoots[1])"
	if !ok || c.Status != doctor.Warn || c.Summary != want || c.Hint != layoutRiskWords {
		t.Errorf("the console's doctor says %+v (there: %v) after the root was added, want a warn %q with the hint %q", c, ok, want, layoutRiskWords)
	}
	if lines := layoutLines(rec); len(lines) != 0 {
		t.Errorf("serve's journal says %q, of a layout that was healthy at its start", lines)
	}
}

// TestServeWithARelativeConfigInsideARootWarnsOnce: serve makes a relative
// --config absolute before anything reads it (fsutil.AbsAsWritten in
// runServe), so run from inside a root with --config bridge.yaml, the layout
// check sees the config file where it is and serve warns once. A regression
// case from B884's second review, green from the start.
func TestServeWithARelativeConfigInsideARootWarnsOnce(t *testing.T) {
	rec := loggingtest.Record(t)
	base := t.TempDir()
	lib := filepath.Join(base, "Music")
	writeLayoutConfig(t, filepath.Join(lib, "bridge.yaml"), filepath.Join(base, "data"), []string{lib}, "")
	t.Chdir(lib)
	_, stderr, code := runCapture(t, "serve", "--config", "bridge.yaml", "--addr", "notaport")
	if code != 2 || !strings.Contains(stderr, "invalid --addr") {
		t.Fatalf("serve exited %d, not at its --addr refusal: %s", code, stderr)
	}
	lines := layoutLines(rec)
	if want := journalLayoutLine("the config file is inside library root libraryRoots[0]"); len(lines) != 1 || lines[0] != want {
		t.Errorf("serve logged %q of its layout, want the one line %q", lines, want)
	}
}

// TestBridgeInitWarnsOfAKeptConfigsDefaultDataDirectory: a kept config that
// writes no dataDir has the default, ./data beside it (priorInstallFrom
// substitutes and resolves it), so a run that keeps a config inside a root
// says its data directory too, in the CLI's words. A regression case from
// B884's second review, green from the start in its summary; its words are
// round 4's.
func TestBridgeInitWarnsOfAKeptConfigsDefaultDataDirectory(t *testing.T) {
	base := t.TempDir()
	lib, other := filepath.Join(base, "Music"), filepath.Join(base, "Other")
	cfgDir := filepath.Join(lib, "conf")
	for _, d := range []string{cfgDir, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := "libraryRoots:\n  - " + lib + "\nlistenAddress: \"127.0.0.1:0\"\nadminAddress: \"127.0.0.1:0\"\noutput:\n  mpd: off\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "bridge.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := initLayoutRun(t, "--skip-doctor", "--dir", cfgDir, "--library", other)
	if code != 0 || !strings.Contains(stdout, "keeping it") {
		t.Fatalf("init exited %d, keeping nothing:\n%s\n%s", code, stdout, stderr)
	}
	requireOneLayoutWarning(t, stdout, stderr, "the config file "+filepath.Join(cfgDir, "bridge.yaml")+" and the data directory "+
		filepath.Join(cfgDir, "data")+" are inside library root "+lib+" (libraryRoots[0])")
}

// TestServeSaysItsLayoutBeforeItTouchesARoot (backlog B884's round 2): serve
// logs its layout's line before it enters its roots' check
// (CheckLibraryRootsAccessible), and the case reads the journal as that check
// is entered (serveOpts.rootsCheck): over roots that answer, serve stopping at
// its --addr refusal after the check, and over a root that is not there,
// which the check refuses on stderr, outside the journal, serve stopping
// there. Round 1's serve tests read the journal once serve had returned, so a
// line moved after the check still passed them. It proves the order against
// that check alone, not against every touch of a root (round 3): the config's
// load validates a non-empty upscale.variantsDir or upscale.tempDir through
// fsutil.IsUnderAny, which resolves each root's links, before the line, and
// this config sets neither.
func TestServeSaysItsLayoutBeforeItTouchesARoot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		absent bool
		stop   string
	}{
		{"roots that answer", false, "invalid --addr"},
		{"a root that is not there", true, "config: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := loggingtest.Record(t)
			base := t.TempDir()
			lib := filepath.Join(base, "Music")
			cfgPath := filepath.Join(base, "bridge.yaml")
			var absent []string
			if tc.absent {
				absent = []string{lib}
			}
			writeLayoutConfigLeaving(t, cfgPath, filepath.Join(lib, ".bridge"), []string{lib}, absent, "")
			if _, err := os.Stat(lib); errors.Is(err, fs.ErrNotExist) != tc.absent {
				t.Fatalf("the root %s is there: %v, want %v", lib, err == nil, !tc.absent)
			}
			entered := 0
			var said []string
			opts := serveOpts{configPath: cfgPath, addrOverride: "notaport",
				rootsCheck: func(c *config.Config) []*config.LibraryRootError {
					entered++
					said = layoutLines(rec)
					return c.CheckLibraryRootsAccessible()
				}}
			var stdout, stderr bytes.Buffer
			if code := runServe(context.Background(), opts, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), tc.stop) {
				t.Fatalf("serve exited %d, not where the case stops it (%q): %s", code, tc.stop, stderr.String())
			}
			want := journalLayoutLine("the data directory (dataDir) is inside library root libraryRoots[0]")
			switch {
			case entered != 1:
				t.Errorf("serve entered its roots' check %d times, want once", entered)
			case len(said) != 1 || said[0] != want:
				t.Errorf("as serve entered its roots' check it had logged %q of its layout, want its one line %q", said, want)
			}
		})
	}
}

// layoutSaid is a layout under a test's base, and what each surface says of
// it.
type layoutSaid struct {
	cfg, data string
	roots     []string
	// initArgs are bridge init's arguments that write the layout, nil for a
	// config the case writes, which init keeps.
	initArgs []string
	// cli and journal are the layout's fact by path and by setting, "" for
	// a layout with nothing inside a root.
	cli, journal string
	// stop is what serve's stderr says where it stops, its --addr refusal
	// unless the case says otherwise.
	stop string
}

// TestEverySurfaceSaysALayoutsFactAndHazardAlone (backlog B884's round 4):
// for each layout round 4 names, and two beside them, bridge init's line,
// both doctors' install-layout line and serve's journal line are the
// layout's fact and the hazard, word for word, or nothing where nothing lies
// inside a root; and no stream any of them writes carries round 3's fixed
// sentence, the README.md section it named, or the journal's three sentences
// of rounds 1 to 3 (layoutRemedies). The layouts: a first install into the
// library's own folder (--dir D --library D, both inside the one root); a
// first install whose library is the data directory it makes (--dir C
// --library C/data: the data directory is the root, and the config inside
// none); a data directory that holds the root, with the config apart
// (nothing: a root inside the data directory is not the data directory
// inside a root); the same with the config inside the root (the config
// alone); the config inside the root the data directory is; and a root that
// names the config file itself, which Load takes, its roots' check refusing
// it after serve's line. bridge init writes the first two and keeps the
// others; the doctors and serve read each from a config of the test's own
// at the same paths, MPD's output off.
func TestEverySurfaceSaysALayoutsFactAndHazardAlone(t *testing.T) {
	for _, tc := range []struct {
		name string
		lay  func(t *testing.T, base string) layoutSaid
	}{
		{"bridge init --dir D --library D", func(t *testing.T, base string) layoutSaid {
			d := filepath.Join(base, "Music")
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg, data := filepath.Join(d, "bridge.yaml"), filepath.Join(d, "data")
			return layoutSaid{cfg: cfg, data: data, roots: []string{d}, initArgs: []string{"--dir", d, "--library", d},
				cli:     "the config file " + cfg + " and the data directory " + data + " are inside library root " + d + " (libraryRoots[0])",
				journal: "the config file and the data directory (dataDir) are inside library root libraryRoots[0]"}
		}},
		{"bridge init --dir C --library C/data", func(t *testing.T, base string) layoutSaid {
			c := filepath.Join(base, "cfg")
			lib := filepath.Join(c, "data")
			if err := os.MkdirAll(lib, 0o755); err != nil {
				t.Fatal(err)
			}
			return layoutSaid{cfg: filepath.Join(c, "bridge.yaml"), data: lib, roots: []string{lib}, initArgs: []string{"--dir", c, "--library", lib},
				cli:     "the data directory " + lib + " is library root " + lib + " (libraryRoots[0])",
				journal: "the data directory (dataDir) is library root libraryRoots[0]"}
		}},
		{"a data directory that holds the root, the config apart", func(t *testing.T, base string) layoutSaid {
			srv := filepath.Join(base, "srv")
			return layoutSaid{cfg: filepath.Join(base, "etc", "bridge.yaml"), data: srv, roots: []string{filepath.Join(srv, "music")}}
		}},
		{"a data directory that holds the root, the config inside the root", func(t *testing.T, base string) layoutSaid {
			srv := filepath.Join(base, "srv")
			music := filepath.Join(srv, "music")
			cfg := filepath.Join(music, "bridge.yaml")
			return layoutSaid{cfg: cfg, data: srv, roots: []string{music},
				cli:     "the config file " + cfg + " is inside library root " + music + " (libraryRoots[0])",
				journal: "the config file is inside library root libraryRoots[0]"}
		}},
		{"the config inside the root the data directory is", func(t *testing.T, base string) layoutSaid {
			d := filepath.Join(base, "Music")
			cfg := filepath.Join(d, "bridge.yaml")
			return layoutSaid{cfg: cfg, data: d, roots: []string{d},
				cli: "the config file " + cfg + " is inside library root " + d + " (libraryRoots[0]), and the data directory " + d +
					" is library root " + d + " (libraryRoots[0])",
				journal: "the config file is inside library root libraryRoots[0], and the data directory (dataDir) is library root libraryRoots[0]"}
		}},
		{"a root that names the config file itself", func(t *testing.T, base string) layoutSaid {
			cfg := filepath.Join(base, "cfg", "bridge.yaml")
			return layoutSaid{cfg: cfg, data: filepath.Join(base, "data"), roots: []string{cfg},
				cli:     "the config file " + cfg + " is library root " + cfg + " (libraryRoots[0])",
				journal: "the config file is library root libraryRoots[0]",
				stop:    "config: "}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			l := tc.lay(t, base)
			// A root that names the config file is no directory to make.
			var absent []string
			if slices.Contains(l.roots, l.cfg) {
				absent = []string{l.cfg}
			}
			var code int
			var stdout, stderr string
			if l.initArgs != nil {
				code, stdout, stderr = initLayoutRun(t, append(append(initPorts(t), "--skip-doctor"), l.initArgs...)...)
			} else {
				writeLayoutConfigLeaving(t, l.cfg, l.data, l.roots, absent, "")
				other := filepath.Join(base, "Other")
				if err := os.MkdirAll(other, 0o755); err != nil {
					t.Fatal(err)
				}
				code, stdout, stderr = initLayoutRun(t, "--skip-doctor", "--dir", filepath.Dir(l.cfg), "--library", other)
				if !strings.Contains(stdout, "keeping it") {
					t.Fatalf("init kept no config:\n%s\n%s", stdout, stderr)
				}
			}
			if code != 0 {
				t.Fatalf("init exited %d:\n%s\n%s", code, stdout, stderr)
			}
			if l.cli == "" {
				if stderr != "" || strings.Contains(stdout, layoutRiskWords) {
					t.Errorf("init warned of a layout with nothing inside a root; stdout:\n%s\nstderr:\n%s", stdout, stderr)
				}
			} else {
				requireOneLayoutWarning(t, stdout, stderr, l.cli)
			}

			writeLayoutConfigLeaving(t, l.cfg, l.data, l.roots, absent, "")
			said := askBothDoctors(t, l.cfg)
			lines := 0
			if l.cli == "" {
				for name, checks := range map[string]map[string]doctor.Check{"bridge doctor": said.cli, "the console's doctor": said.console} {
					if c, ok := checks[installLayoutCheckName]; ok {
						t.Errorf("%s carries %+v over a layout with nothing inside a root", name, c)
					}
				}
			} else {
				lines = 1
				c := said.requireLine(t, installLayoutCheckName, doctor.Warn)
				if c.Summary != l.cli || c.Hint != layoutRiskWords {
					t.Errorf("the install-layout line says %q, its hint %q\nwant %q, its hint %q", c.Summary, c.Hint, l.cli, layoutRiskWords)
				}
				said.requirePrinted(t, c)
			}
			if n := strings.Count(said.printed, layoutRiskWords); n != lines {
				t.Errorf("bridge doctor printed %d layout warnings, want %d:\n%s", n, lines, said.printed)
			}

			rec := loggingtest.Record(t)
			stop := l.stop
			if stop == "" {
				stop = "invalid --addr"
			}
			_, serveErr, serveCode := runCapture(t, "serve", "--config", l.cfg, "--addr", "notaport")
			if serveCode != 2 || !strings.Contains(serveErr, stop) {
				t.Fatalf("serve exited %d, not where the case stops it (%q): %s", serveCode, stop, serveErr)
			}
			logged := layoutLines(rec)
			switch {
			case l.journal == "" && len(logged) != 0:
				t.Errorf("serve logged %q of a layout with nothing inside a root", logged)
			case l.journal != "" && (len(logged) != 1 || logged[0] != journalLayoutLine(l.journal)):
				t.Errorf("serve logged %q of its layout, want the one line %q", logged, journalLayoutLine(l.journal))
			}

			streams := map[string]string{"bridge init's stdout": stdout, "bridge init's stderr": stderr,
				"bridge doctor": said.printed, "serve's stderr": serveErr, "serve's journal": strings.Join(rec.All(), "\n")}
			for _, c := range said.console {
				streams["the console's doctor"] += c.Summary + "\n" + c.Hint + "\n"
			}
			for name, s := range streams {
				for _, line := range strings.Split(s, "\n") {
					var found []string
					for _, r := range layoutRemedies {
						if strings.Contains(line, r) {
							found = append(found, r)
						}
					}
					if len(found) > 0 {
						t.Errorf("%s says %q in the line %q", name, found, line)
					}
				}
			}
		})
	}
}
