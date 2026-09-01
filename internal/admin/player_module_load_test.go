package admin

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The layer that catches the defect class `go test` cannot see.
//
// Twice, a misplaced `boot();` put the whole player module in the
// temporal dead zone and the page rendered nothing — no failing test, no
// symptom but a ReferenceError in a console nobody was watching. Running
// each ES module under node reproduces that in milliseconds.
//
// Three things this file learned by experiment rather than by reasoning,
// because both reviews of the plan disagreed about them:
//
//  1. A STUB IS REQUIRED. Under bare Node 26, four of the eight player
//     modules throw at import — every one on `window`, then `location`.
//     Node provides neither and never will, so "drop the stub on Node
//     22+" is not available.
//
//  2. IT MUST NOT BE A PERMISSIVE PROXY. The obvious "answer every
//     property" stub HANGS: a module loops while a truthy value keeps
//     coming back. Values have to terminate — querySelectorAll returns
//     [], parentNode is null, contains is false.
//
//  3. HENCE THE WATCHDOG. A stub-induced infinite loop must be reported
//     as a failure, not hang CI.
//
// Honest limitation: this catches IMPORT-TIME errors only. Runtime
// coverage needs a real browser, which is a deliberate follow-on. It also
// covers the MODULE TREE only — app.js is a classic script, so the
// deleted-helper class there stays with TestAppJSHasNoCallsToDeletedHelpers.

const moduleLoadTimeout = 20 * time.Second

func TestEveryPlayerModuleLoads(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped client modules")
	}

	entries, err := os.ReadDir(filepath.Join("static", "player"))
	if err != nil {
		t.Fatal(err)
	}
	var mods []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".js") {
			mods = append(mods, e.Name())
		}
	}
	if len(mods) == 0 {
		t.Fatal("no player modules found; the test would pass vacuously")
	}

	stub, err := filepath.Abs(filepath.Join("testdata", "domstub.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.Abs(filepath.Join("static", "player"))
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range mods {
		t.Run(m, func(t *testing.T) {
			out, err := runModuleUnderNode(t, node, stub, filepath.Join(dir, m))
			if err != nil {
				t.Fatalf("node failed to run: %v\n%s", err, out)
			}
			if strings.TrimSpace(out) != "OK" {
				t.Errorf("%s does not reach the end of module evaluation:\n%s", m, out)
			}
		})
	}
}

// runModuleUnderNode imports one module with the DOM stub installed and a
// self-kill watchdog, returning the single-line verdict.
func runModuleUnderNode(t *testing.T, node, stub, module string) (string, error) {
	t.Helper()
	script := fmt.Sprintf(`
const t = setTimeout(() => { console.log('TIMEOUT'); process.exit(0); }, %d);
await import(%q);
try { await import(%q); console.log('OK'); }
catch (e) { console.log('THROW: ' + e.constructor.name + ': ' + String(e.message).slice(0, 200)); }
clearTimeout(t);
process.exit(0);
`, moduleLoadTimeout.Milliseconds(), fileURL(stub), fileURL(module))

	cmd := exec.Command(node, "--input-type=module", "-e", script)
	raw, err := cmd.CombinedOutput()
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	return lines[len(lines)-1], err
}

// TestPlayerModuleLoadCatchesATemporalDeadZone is the control: it proves
// the layer above can actually catch the incident it exists for, rather
// than passing because everything happens to import cleanly.
//
// The mutation is copied into a temp dir WITH ITS SIBLINGS, because the
// modules import each other relatively — mutating one in isolation fails
// on a missing sibling rather than on the mutation, which would be a
// green control for the wrong reason.
func TestPlayerModuleLoadCatchesATemporalDeadZone(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	src := filepath.Join("static", "player")
	boot := readFile(t, filepath.Join(src, "boot.js"))
	if !strings.Contains(boot, "\nboot();") {
		t.Skip("boot.js no longer ends with a bare boot(); the control needs rewriting")
	}

	tmp := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if e.Name() == "boot.js" {
			// Hoist the call above every declaration — the exact shape of
			// the two real incidents.
			b = []byte("boot();\n" + strings.Replace(string(b), "\nboot();", "", 1))
		}
		if err := os.WriteFile(filepath.Join(tmp, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	stub, err := filepath.Abs(filepath.Join("testdata", "domstub.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := runModuleUnderNode(t, node, stub, filepath.Join(tmp, "boot.js"))
	if err != nil {
		t.Fatalf("node failed to run: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) == "OK" {
		t.Fatal("a boot() call hoisted above the module's declarations loaded cleanly — " +
			"TestEveryPlayerModuleLoads would not catch the incident it exists for")
	}
	if !strings.Contains(out, "ReferenceError") {
		t.Errorf("control produced %q; expected a ReferenceError from the temporal dead zone", out)
	}
}

// fileURL turns an absolute filesystem path into a file:// URL node will
// accept on every platform.
//
// The naive "file://" + path is POSIX-only: a Windows path is
// `D:\a\x.js`, and both the backslashes and the missing leading slash
// make `file://D:\a\x.js` invalid — node rejects it and every module
// "fails to load" for a reason that has nothing to do with the module.
// (Caught by the Windows gate leg, on the first PR after that leg started
// blocking. The same class the internal/dsn helper exists for on the
// SQLite side.)
//
// url.URL does the percent-escaping, which matters because a runner's
// temp path can contain characters a hand-built URL would corrupt.
func fileURL(p string) string {
	// ToSlash alone is a NO-OP on POSIX, so a Windows-shaped path handed
	// to this on a Mac keeps its backslashes — which is why the test
	// below covers the Windows shape explicitly and why the ReplaceAll is
	// here. Exactly the accommodation internal/dsn.File documents for
	// SQLite URIs, for the same reason.
	slashed := strings.ReplaceAll(filepath.ToSlash(p), `\`, "/")
	// Windows absolute paths start with a drive letter, not a slash;
	// file:// requires one (file:///D:/a/x.js).
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	u := url.URL{Scheme: "file", Path: slashed}
	return u.String()
}

// TestFileURLHandlesBothPlatformShapes pins the builder against the
// Windows shape explicitly, because the POSIX shape works by accident
// with the naive concatenation and would not catch the bug.
func TestFileURLHandlesBothPlatformShapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/home/runner/work/x.js", "file:///home/runner/work/x.js"},
		{`D:\a\1-bit-bridge\x.js`, "file:///D:/a/1-bit-bridge/x.js"},
		{`C:\Users\RUNNER~1\AppData\Local\Temp\t\boot.js`,
			"file:///C:/Users/RUNNER~1/AppData/Local/Temp/t/boot.js"},
		// A space must be escaped, not passed through.
		{"/tmp/with space/x.js", "file:///tmp/with%20space/x.js"},
	}
	for _, tc := range cases {
		if got := fileURL(tc.in); got != tc.want {
			t.Errorf("fileURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
