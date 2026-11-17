package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// The packages the stand-in for go lists, as the module's import paths.
const (
	makeTestBridge   = "github.com/acoseac/1-bit-bridge/cmd/bridge"
	makeTestConfig   = "github.com/acoseac/1-bit-bridge/internal/config"
	makeTestManifest = "github.com/acoseac/1-bit-bridge/internal/manifest"
	makeTestUpdater  = "github.com/acoseac/1-bit-bridge/internal/updater"
)

// goStandIn is the go that make test's recipe runs in these tests. Each call
// writes its TMPDIR and its arguments, NUL-separated, to a file of its own;
// go list answers a few patterns as the module's go would (one that matches
// nothing lists nothing and succeeds, with go's warning), and go test exits
// as the case asks, writing a mark of its role (the slow packages', cmd/bridge
// and internal/manifest, or the other packages') as it starts and as it ends.
// With STUB_OVERLAP set, each go test waits, bounded, for the other to start,
// and marks that it ran alone when the other never did, so a recipe that ran
// them one after the other fails whatever it makes of their status; with
// STUB_SLOW_LATE set, the slow packages' ends a second late, so a recipe that
// did not wait for it ends first.
const goStandIn = `#!/bin/sh
{ printf 'TMPDIR=%s\0' "$TMPDIR"; printf '%s\0' "$@"; } > "$STUB_DIR/call.$$"
case "$1" in
list)
	[ -z "$STUB_LIST_FAIL" ] || { echo "stand-in: go list fails" >&2; exit 1; }
	shift
	skip=
	for a in "$@"; do
		if [ -n "$skip" ]; then skip=; continue; fi
		case "$a" in
		-tags) skip=1 ;;
		-*) ;;
		./...) printf '%s\n' ` + makeTestBridge + ` ` + makeTestConfig + ` ` + makeTestManifest + ` ;;
		./cmd/bridge/) printf '%s\n' ` + makeTestBridge + ` ;;
		./internal/config) printf '%s\n' ` + makeTestConfig + ` ;;
		./internal/manifest) printf '%s\n' ` + makeTestManifest + ` ;;
		./internal/updater/...) printf '%s\n' ` + makeTestUpdater + ` ;;
		./internal/nonexistent/...) echo "go: warning: \"$a\" matched no packages" >&2 ;;
		*) echo "stand-in: no package $a" >&2; exit 1 ;;
		esac
	done ;;
test)
	role=rest other=slow code=${STUB_REST_EXIT:-0}
	for a in "$@"; do
		case "$a" in
		` + makeTestBridge + `|` + makeTestManifest + `) role=slow other=rest code=${STUB_SLOW_EXIT:-0} ;;
		esac
	done
	: > "$STUB_DIR/started.$role"
	if [ -n "$STUB_OVERLAP" ]; then
		n=0
		while [ ! -e "$STUB_DIR/started.$other" ]; do
			n=$((n + 1))
			[ "$n" -le 600 ] || { : > "$STUB_DIR/alone.$role"; echo "stand-in: the $role go test ran alone" >&2; exit 3; }
			sleep 0.05
		done
	fi
	if [ "$role" = slow ] && [ -n "$STUB_SLOW_LATE" ]; then sleep 1; fi
	: > "$STUB_DIR/ended.$role"
	exit "$code" ;;
esac
`

// goCall is one call of the stand-in: the TMPDIR it ran with and its argv.
type goCall struct {
	tmpdir string
	args   []string
}

// makeTestRun is what one make test with the stand-in did: make's output and
// error, the go calls, and the directory it ran in, which make names by its
// links resolved (a macOS temporary directory is below /var, a link).
type makeTestRun struct {
	dir, curdir, out string
	err              error
	calls            []goCall
}

// marked reports whether a go test of the stand-in left the mark named:
// started.<role>, ended.<role> or alone.<role>.
func (r makeTestRun) marked(mark string) bool {
	_, err := os.Stat(filepath.Join(r.dir, "calls", mark))
	return err == nil
}

// runMakeTest runs the repository's Makefile's test target in a directory of
// its own, with the stand-in first on PATH and nothing else of the caller's
// environment: a make test that runs this test passes MAKEFLAGS, and with it
// its own P and TEST_PKGS, and the Makefile takes TEST_PKGS and TMPDIR from
// the environment too.
func runMakeTest(t *testing.T, env []string, vars ...string) makeTestRun {
	t.Helper()
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Skip("make test needs make, and make is not on PATH here")
	}
	makefile, err := readRepoBytes("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	r := makeTestRun{dir: t.TempDir()}
	if r.curdir, err = filepath.EvalSymlinks(r.dir); err != nil {
		t.Fatal(err)
	}
	put(t, r.dir, "Makefile", string(makefile))
	bin := filepath.Join(r.dir, "bin")
	put(t, bin, "go", goStandIn)
	if err := os.Chmod(filepath.Join(bin, "go"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(r.dir, "calls")
	if err := os.Mkdir(calls, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(makePath, append([]string{"-s", "test"}, vars...)...)
	cmd.Dir = r.dir
	cmd.Env = append([]string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "STUB_DIR=" + calls}, env...)
	// Into a file, never a pipe: Wait waits for every holder of a pipe it
	// copies from to close it, so a go test the recipe left running would
	// hold Run until it ended, and a recipe that returned without waiting
	// for it would pass.
	out, err := os.Create(filepath.Join(r.dir, "make.out"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = out, out
	r.err = cmd.Run()
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	r.out = string(b)
	entries, err := os.ReadDir(calls)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "call.") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(calls, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
		r.calls = append(r.calls, goCall{tmpdir: strings.TrimPrefix(fields[0], "TMPDIR="), args: fields[1:]})
	}
	return r
}

// verb returns the calls of one go verb.
func (r makeTestRun) verb(v string) []goCall {
	var calls []goCall
	for _, c := range r.calls {
		if len(c.args) > 0 && c.args[0] == v {
			calls = append(calls, c)
		}
	}
	return calls
}

// tests returns the go test calls, the slow packages' (cmd/bridge's and
// internal/manifest's) apart from the other packages', and fails the case on
// a second call of either.
func (r makeTestRun) tests(t *testing.T) (slow, rest *goCall) {
	t.Helper()
	calls := r.verb("test")
	for i := range calls {
		c := &calls[i]
		side := &rest
		for _, a := range c.args {
			if a == makeTestBridge || a == makeTestManifest {
				side = &slow
			}
		}
		if *side != nil {
			t.Fatalf("two go test calls of one side: %q and %q", (*side).args, c.args)
		}
		*side = c
	}
	return slow, rest
}

func releaseTags(t *testing.T) string {
	t.Helper()
	config, err := readRepoBytes("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	build, err := releaseBuild(config)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(build.tags, ",")
}

// TestMakeTestRunsTheSlowPackagesInAGoTestOfTheirOwn: make test runs
// cmd/bridge's and internal/manifest's suites in a go test of their own with
// -timeout 45m, at the same time as the go test of every other package, which
// keeps 30m (B897): go test applies one -timeout to each package's test
// binary of an invocation. With nothing else selected the slow go test takes
// P; beside the other it takes 2 for two slow packages (the other P-1) and 1
// for one (the other P), so the two never run more than P+1 things at once,
// and at P=1 both take 1. Both run with the release tags, -race and the
// Makefile's TMPDIR; TEST_PKGS's packages are go list's, and its other words
// go to each go test after the packages, as they stand.
func TestMakeTestRunsTheSlowPackagesInAGoTestOfTheirOwn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("make test's recipe is POSIX sh, run by make on macOS and Linux; CI's Windows legs run go test directly")
	}
	tags := releaseTags(t)
	argv := func(p, timeout string, rest ...string) []string {
		return append([]string{"test", "-tags", tags, "-p", p, "-race", "-timeout", timeout}, rest...)
	}
	for _, tc := range []struct {
		name       string
		vars       []string
		list       []string
		slow, rest []string
		// overlap: both go tests must run at once (a P of 2 or more).
		overlap bool
	}{
		{
			name:    "every package",
			list:    []string{"./..."},
			slow:    argv("2", "45m", makeTestBridge, makeTestManifest),
			rest:    argv("3", "30m", makeTestConfig),
			overlap: true,
		},
		{
			name:    "every package, P=8",
			vars:    []string{"P=8"},
			list:    []string{"./..."},
			slow:    argv("2", "45m", makeTestBridge, makeTestManifest),
			rest:    argv("7", "30m", makeTestConfig),
			overlap: true,
		},
		{
			name:    "every package, P=2",
			vars:    []string{"P=2"},
			list:    []string{"./..."},
			slow:    argv("2", "45m", makeTestBridge, makeTestManifest),
			rest:    argv("1", "30m", makeTestConfig),
			overlap: true,
		},
		{
			name: "every package, P=1",
			vars: []string{"P=1"},
			list: []string{"./..."},
			slow: argv("1", "45m", makeTestBridge, makeTestManifest),
			rest: argv("1", "30m", makeTestConfig),
		},
		{
			name: "one other package",
			vars: []string{"TEST_PKGS=./internal/config"},
			list: []string{"./internal/config"},
			rest: argv("4", "30m", makeTestConfig),
		},
		{
			name: "cmd/bridge alone",
			vars: []string{"TEST_PKGS=./cmd/bridge/"},
			list: []string{"./cmd/bridge/"},
			slow: argv("4", "45m", makeTestBridge),
		},
		{
			name: "internal/manifest alone",
			vars: []string{"TEST_PKGS=./internal/manifest"},
			list: []string{"./internal/manifest"},
			slow: argv("4", "45m", makeTestManifest),
		},
		{
			name: "both slow packages",
			vars: []string{"TEST_PKGS=./cmd/bridge/ ./internal/manifest"},
			list: []string{"./cmd/bridge/", "./internal/manifest"},
			slow: argv("4", "45m", makeTestBridge, makeTestManifest),
		},
		{
			name:    "internal/manifest and another package",
			vars:    []string{"TEST_PKGS=./internal/manifest ./internal/config"},
			list:    []string{"./internal/manifest", "./internal/config"},
			slow:    argv("1", "45m", makeTestManifest),
			rest:    argv("4", "30m", makeTestConfig),
			overlap: true,
		},
		{
			name:    "flags before the packages",
			vars:    []string{"P=8", "TEST_PKGS=-count=1 -parallel 8 ./..."},
			list:    []string{"./..."},
			slow:    argv("2", "45m", makeTestBridge, makeTestManifest, "-count=1", "-parallel", "8"),
			rest:    argv("7", "30m", makeTestConfig, "-count=1", "-parallel", "8"),
			overlap: true,
		},
		{
			name: "a timeout of the caller's own",
			vars: []string{"TEST_PKGS=-timeout 60m ./internal/updater/..."},
			list: []string{"./internal/updater/..."},
			rest: argv("4", "30m", makeTestUpdater, "-timeout", "60m"),
		},
		{
			name: "a quoted value",
			vars: []string{"TEST_PKGS=-run 'A B' ./internal/config"},
			list: []string{"./internal/config"},
			rest: argv("4", "30m", makeTestConfig, "-run", "A B"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var env []string
			if tc.overlap {
				env = append(env, "STUB_OVERLAP=1")
			}
			r := runMakeTest(t, env, tc.vars...)
			for _, role := range []string{"slow", "rest"} {
				if r.marked("alone." + role) {
					t.Errorf("the %s go test ran with the other's not started", role)
				}
			}
			if r.err != nil {
				t.Fatalf("make test: %v\n%s", r.err, r.out)
			}
			var lists [][]string
			for _, c := range r.verb("list") {
				lists = append(lists, c.args)
			}
			if want := [][]string{append([]string{"list", "-tags", tags, "-race"}, tc.list...)}; !reflect.DeepEqual(lists, want) {
				t.Errorf("go list calls %q, want %q", lists, want)
			}
			slow, rest := r.tests(t)
			for _, side := range []struct {
				name string
				got  *goCall
				want []string
			}{{"the slow packages'", slow, tc.slow}, {"the other packages'", rest, tc.rest}} {
				switch {
				case side.want == nil && side.got != nil:
					t.Errorf("%s go test ran: %q", side.name, side.got.args)
				case side.want != nil && side.got == nil:
					t.Errorf("%s go test did not run; make said:\n%s", side.name, r.out)
				case side.want != nil && !reflect.DeepEqual(side.got.args, side.want):
					t.Errorf("%s go test ran with\n\t%q\nwant\n\t%q", side.name, side.got.args, side.want)
				}
				if want := filepath.Join(r.curdir, ".tmp"); side.got != nil && side.got.tmpdir != want {
					t.Errorf("%s go test ran with TMPDIR %q, want the Makefile's %q", side.name, side.got.tmpdir, want)
				}
			}
		})
	}
}

// TestMakeTestFailsWhenEitherGoTestFails: make test answers for both its go
// tests, each waited for whatever the other answers; a go list that fails
// runs no go test, and so does a selection go list answers with no package,
// which make test refuses in words of its own (B897).
func TestMakeTestFailsWhenEitherGoTestFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("make test's recipe is POSIX sh, run by make on macOS and Linux; CI's Windows legs run go test directly")
	}
	for _, tc := range []struct {
		name  string
		env   []string
		vars  []string
		tests int
		says  string
	}{
		{name: "the slow packages' fails", env: []string{"STUB_SLOW_EXIT=1"}, tests: 2},
		{name: "the other packages' fails", env: []string{"STUB_REST_EXIT=1", "STUB_SLOW_LATE=1"}, tests: 2},
		{name: "both fail", env: []string{"STUB_SLOW_EXIT=1", "STUB_REST_EXIT=1"}, tests: 2},
		{name: "go list fails", env: []string{"STUB_LIST_FAIL=1"}},
		{
			name: "a selection that matches nothing",
			vars: []string{"TEST_PKGS=./internal/nonexistent/..."},
			says: "make test: no package matches ./internal/nonexistent/...",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runMakeTest(t, tc.env, tc.vars...)
			if r.err == nil {
				t.Fatalf("make test succeeded:\n%s", r.out)
			}
			if n := len(r.verb("test")); n != tc.tests {
				t.Errorf("%d go test calls ran, want %d; make said:\n%s", n, tc.tests, r.out)
			}
			for _, role := range []string{"slow", "rest"} {
				if tc.tests > 0 && !r.marked("ended."+role) {
					t.Errorf("make test returned before the %s go test ended", role)
				}
			}
			if tc.says != "" && !strings.Contains(r.out, tc.says) {
				t.Errorf("make test said:\n%s\nwant a line saying %q", r.out, tc.says)
			}
		})
	}
}
