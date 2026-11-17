package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The native legs' oracles (B897), written out here and never read from the
// script or the workflow they check: the three packages that run at
// -timeout 45m, the four legs the cross-platform job runs, and the platform
// each leg's runner is, as go list takes it.
var (
	nativeSlowPackages = []string{
		"github.com/acoseac/1-bit-bridge/cmd/bridge",
		"github.com/acoseac/1-bit-bridge/internal/backup",
		"github.com/acoseac/1-bit-bridge/internal/manifest",
	}
	nativeLegs    = []string{"macos-latest/rest", "macos-latest/slow", "windows-latest/rest", "windows-latest/slow"}
	nativeTargets = map[string][]string{
		"windows-latest": {"GOOS=windows", "GOARCH=amd64"},
		"macos-latest":   {"GOOS=darwin", "GOARCH=arm64"},
	}
)

// nativeCommand is the go test a native leg runs, as the script prints it.
type nativeCommand struct {
	tags, p, timeout string
	race             bool
	pkgs             []string
}

// runNativeScript runs .github/scripts/native-test.sh for a group with env
// added, from the repository's root, and returns its exit error, its output
// and the command it printed or ran.
func runNativeScript(t *testing.T, env []string, group string) (error, string) {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("the native legs' script is bash, and bash is not on PATH here")
	}
	cmd := exec.Command(bash, ".github/scripts/native-test.sh", group)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return err, string(out)
}

// nativeDryRun is the command a leg runs for a group on a platform, the
// script told to print it (NATIVE_TEST_DRY_RUN), the toolchain this test
// runs first on PATH.
func nativeDryRun(t *testing.T, target []string, group string) nativeCommand {
	t.Helper()
	env := append([]string{"NATIVE_TEST_DRY_RUN=1",
		"PATH=" + filepath.Join(runtime.GOROOT(), "bin") + string(os.PathListSeparator) + os.Getenv("PATH")}, target...)
	err, out := runNativeScript(t, env, group)
	if err != nil {
		t.Fatalf("native-test.sh %s: %v\n%s", group, err, out)
	}
	words := strings.Fields(out)
	if len(words) < 2 || words[0] != "go" || words[1] != "test" {
		t.Fatalf("native-test.sh %s printed %q, want a go test", group, words)
	}
	var c nativeCommand
	for i := 2; i < len(words); i++ {
		switch w := words[i]; {
		case w == "-tags" || w == "-p" || w == "-timeout":
			if i+1 == len(words) {
				t.Fatalf("native-test.sh %s: %s has no value: %q", group, w, words)
			}
			i++
			switch w {
			case "-tags":
				c.tags = words[i]
			case "-p":
				c.p = words[i]
			case "-timeout":
				c.timeout = words[i]
			}
		case w == "-race":
			c.race = true
		case strings.HasPrefix(w, "-"):
			t.Fatalf("native-test.sh %s: a flag this check does not read: %s", group, w)
		default:
			c.pkgs = append(c.pkgs, w)
		}
	}
	return c
}

// nativeJob is gate.yml's cross-platform job as these checks read it: its
// legs, as os/group, its name, timeout, cache key and test step, and what in
// the workflow could let a leg pass without running its tests.
type nativeJob struct {
	legs              []string
	name              string
	minutes           int
	cacheKey, testRun string
	// skips are the job's and its test step's if and continue-on-error (the
	// step named test, and any that runs native-test.sh), and the dry-run
	// switch (NATIVE_TEST_DRY_RUN) anywhere in the workflow: an if can skip a
	// leg's tests, which on the step leaves the leg green, and a
	// continue-on-error that is not false keeps a failure from failing it.
	skips []string
}

// leeway says what a job's or a step's if or continue-on-error lets a leg
// do, or "" for nothing: a node that is not there, or a continue-on-error
// that is the boolean false. Both keys are YAML nodes here, so a boolean, a
// string and an expression all read as they are written.
func leeway(where, key string, n yaml.Node) string {
	if n.Kind == 0 {
		return ""
	}
	if key == "continue-on-error" && n.Kind == yaml.ScalarNode && n.Tag == "!!bool" {
		if v, err := strconv.ParseBool(n.Value); err == nil && !v {
			return ""
		}
	}
	return fmt.Sprintf("%s has %s: %s", where, key, n.Value)
}

// readNativeJob reads the cross-platform job of a workflow's bytes, as
// gate.yml has it.
func readNativeJob(workflow []byte) (nativeJob, error) {
	var nj nativeJob
	var wf struct {
		Jobs map[string]yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &wf); err != nil {
		return nj, err
	}
	node, ok := wf.Jobs["cross-platform"]
	if !ok {
		return nj, fmt.Errorf("no cross-platform job")
	}
	var job struct {
		Name            string    `yaml:"name"`
		If              yaml.Node `yaml:"if"`
		ContinueOnError yaml.Node `yaml:"continue-on-error"`
		Timeout         int       `yaml:"timeout-minutes"`
		Strategy        struct {
			Matrix map[string][]string `yaml:"matrix"`
		} `yaml:"strategy"`
		Steps []struct {
			Name            string         `yaml:"name"`
			If              yaml.Node      `yaml:"if"`
			ContinueOnError yaml.Node      `yaml:"continue-on-error"`
			Uses            string         `yaml:"uses"`
			Run             string         `yaml:"run"`
			With            map[string]any `yaml:"with"`
		} `yaml:"steps"`
	}
	if err := node.Decode(&job); err != nil {
		return nj, fmt.Errorf("the cross-platform job: %w (a matrix key that is no list of names, an include or an exclude, does not read here)", err)
	}
	keys := make([]string, 0, len(job.Strategy.Matrix))
	for k := range job.Strategy.Matrix {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if !reflect.DeepEqual(keys, []string{"group", "os"}) {
		return nj, fmt.Errorf("the cross-platform job's matrix has the keys %q, want exactly os and group: the legs are their product", keys)
	}
	for _, platform := range job.Strategy.Matrix["os"] {
		for _, group := range job.Strategy.Matrix["group"] {
			nj.legs = append(nj.legs, platform+"/"+group)
		}
	}
	slices.Sort(nj.legs)
	nj.name, nj.minutes = job.Name, job.Timeout
	for _, s := range []string{leeway("the job", "if", job.If), leeway("the job", "continue-on-error", job.ContinueOnError)} {
		if s != "" {
			nj.skips = append(nj.skips, s)
		}
	}
	for _, st := range job.Steps {
		if strings.HasPrefix(st.Uses, "actions/cache@") {
			nj.cacheKey, _ = st.With["key"].(string)
		}
		if st.Name == "test" {
			nj.testRun = strings.TrimSpace(st.Run)
		}
		if st.Name == "test" || strings.Contains(st.Run, ".github/scripts/native-test.sh") {
			for _, s := range []string{leeway("its test step", "if", st.If), leeway("its test step", "continue-on-error", st.ContinueOnError)} {
				if s != "" {
					nj.skips = append(nj.skips, s)
				}
			}
		}
	}
	if strings.Contains(string(workflow), "NATIVE_TEST_DRY_RUN") {
		nj.skips = append(nj.skips, "the workflow names NATIVE_TEST_DRY_RUN, which makes a leg print its go test and run none")
	}
	return nj, nil
}

// gateWorkflow returns gate.yml's bytes.
func gateWorkflow(t *testing.T) []byte {
	t.Helper()
	b, err := readRepoBytes("../../.github/workflows/gate.yml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestTheNativeLegsRunEveryPackageOnceAndTheSlowOnesAt45m: the cross-platform
// job runs .github/scripts/native-test.sh's two groups on both platforms, the
// four legs the oracle names, and on each platform's own tagged listing the
// slow group is exactly the oracle's three packages, at -timeout 45m and at
// least -p 3, the rest group every other package at 30m: the two together
// are the listing, no package in both, neither empty, no -race (B897). The
// legs are four jobs, named and cached apart, and nothing in the workflow
// lets one pass without running its tests (readNativeJob's skips).
func TestTheNativeLegsRunEveryPackageOnceAndTheSlowOnesAt45m(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the check runs the script's bash, as CI's Linux and macOS legs do for both platforms' listings; a Windows host's first bash may be WSL's")
	}
	job, err := readNativeJob(gateWorkflow(t))
	if err != nil {
		t.Fatalf("gate.yml: %v", err)
	}
	if !reflect.DeepEqual(job.legs, nativeLegs) {
		t.Errorf("the cross-platform job runs the legs %q, want %q", job.legs, nativeLegs)
	}
	for what, s := range map[string]string{"name": job.name, "cache key": job.cacheKey} {
		if !strings.Contains(s, "${{ matrix.group }}") {
			t.Errorf("the cross-platform job's %s %q does not tell its groups apart", what, s)
		}
	}
	if !strings.Contains(job.name, "${{ matrix.os }}") {
		t.Errorf("the cross-platform job's name %q does not tell its platforms apart", job.name)
	}
	if want := "bash .github/scripts/native-test.sh ${{ matrix.group }}"; job.testRun != want {
		t.Errorf("the cross-platform job's test step runs %q, want %q", job.testRun, want)
	}
	if job.minutes < 75 {
		t.Errorf("the cross-platform job's timeout-minutes is %d: a 45-minute binary's timeout and its setup need 75", job.minutes)
	}
	for _, s := range job.skips {
		t.Errorf("the cross-platform job could pass without its tests: %s", s)
	}
	tags := releaseTags(t)
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	for _, leg := range []string{"windows-latest", "macos-latest"} {
		t.Run(leg, func(t *testing.T) {
			target := nativeTargets[leg]
			list := exec.Command(goTool, "list", "-tags", tags, "./...")
			list.Dir = filepath.Join("..", "..")
			list.Env = append(os.Environ(), target...)
			out, err := list.Output()
			if err != nil {
				t.Fatalf("go list for %s: %v", leg, err)
			}
			listing := strings.Fields(string(out))
			slow := nativeDryRun(t, target, "slow")
			rest := nativeDryRun(t, target, "rest")

			for _, c := range []struct {
				group   string
				cmd     nativeCommand
				timeout string
			}{{"slow", slow, "45m"}, {"rest", rest, "30m"}} {
				if c.cmd.tags != tags {
					t.Errorf("the %s group runs with -tags %q, want the release tags %q", c.group, c.cmd.tags, tags)
				}
				if c.cmd.timeout != c.timeout {
					t.Errorf("the %s group runs with -timeout %q, want %s", c.group, c.cmd.timeout, c.timeout)
				}
				if c.cmd.race {
					t.Errorf("the %s group runs with -race, which the native legs do not take", c.group)
				}
				if len(c.cmd.pkgs) == 0 {
					t.Errorf("the %s group lists no package", c.group)
				}
			}
			if p, err := strconv.Atoi(slow.p); err != nil || p < 3 {
				t.Errorf("the slow group runs with -p %q, want 3 or more: one after another its packages outlast the job", slow.p)
			}
			got := slices.Sorted(slices.Values(slow.pkgs))
			if !reflect.DeepEqual(got, nativeSlowPackages) {
				t.Errorf("the slow group, at 45m, runs %q, want exactly %q", got, nativeSlowPackages)
			}
			seen := map[string]string{}
			for _, c := range []struct {
				group string
				pkgs  []string
			}{{"slow", slow.pkgs}, {"rest", rest.pkgs}} {
				for _, p := range c.pkgs {
					if other, ok := seen[p]; ok {
						t.Errorf("%s runs in the %s group and the %s group", p, other, c.group)
					}
					seen[p] = c.group
				}
			}
			for _, p := range listing {
				if _, ok := seen[p]; !ok {
					t.Errorf("%s, which go list lists for %s, runs in no group", p, leg)
				}
				delete(seen, p)
			}
			for p, group := range seen {
				t.Errorf("the %s group runs %s, which go list does not list for %s", group, p, leg)
			}
		})
	}
}

// TestTheNativeScriptRefusesAnEmptyGroup: a leg that would run nothing
// fails, saying so: the rest group of a module whose listing holds only the
// slow packages, and the slow group where go list does not list one of them;
// neither runs go test (B897).
func TestTheNativeScriptRefusesAnEmptyGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in go is a POSIX sh script")
	}
	for _, tc := range []struct {
		name, group, lists, says string
	}{
		{"the rest, where only the slow packages are listed", "rest", strings.Join(nativeSlowPackages, " "), "native-test: the rest group lists no package"},
		{"the slow group, where one of them is not listed", "slow", nativeSlowPackages[0] + " " + nativeSlowPackages[2], "native-test: " + nativeSlowPackages[1] + " is in the slow group, and go list does not list it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			put(t, dir, "go", "#!/bin/sh\n[ \"$1\" = list ] || { : > \""+dir+"/ran-go-"+"$1\"; exit 0; }\nprintf '%s\\n' "+tc.lists+"\n")
			if err := os.Chmod(filepath.Join(dir, "go"), 0o755); err != nil {
				t.Fatal(err)
			}
			err, out := runNativeScript(t, []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}, tc.group)
			if err == nil {
				t.Fatalf("native-test.sh %s succeeded:\n%s", tc.group, out)
			}
			if !strings.Contains(out, tc.says) {
				t.Errorf("native-test.sh %s said:\n%s\nwant %q", tc.group, out, tc.says)
			}
			if _, err := os.Stat(filepath.Join(dir, "ran-go-test")); err == nil {
				t.Error("native-test.sh ran go test")
			}
		})
	}
}

// TestTheNativeScriptFailsAsItsGoTestFails: a leg's step fails with its go
// test's status, so a failing test fails the leg's job, and the gate after it
// (B897): the script runs go test last, under set -euo pipefail.
func TestTheNativeScriptFailsAsItsGoTestFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in go is a POSIX sh script")
	}
	for _, group := range []string{"slow", "rest"} {
		t.Run(group, func(t *testing.T) {
			dir := t.TempDir()
			put(t, dir, "go", "#!/bin/sh\nif [ \"$1\" = test ]; then exit 3; fi\nprintf '%s\\n' "+strings.Join(nativeSlowPackages, " ")+" "+makeTestConfig+"\n")
			if err := os.Chmod(filepath.Join(dir, "go"), 0o755); err != nil {
				t.Fatal(err)
			}
			err, out := runNativeScript(t, []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}, group)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 3 {
				t.Errorf("native-test.sh %s ended with %v, want its go test's exit status 3:\n%s", group, err, out)
			}
		})
	}
}

// TestTheNativeJobCheckRefusesALegThatCanPassWithoutItsTests: readNativeJob,
// which TestTheNativeLegsRunEveryPackageOnceAndTheSlowOnesAt45m runs on
// gate.yml, finds what would let a leg pass without its tests in gate.yml
// with one key added: an if on the job or on its test step (an if there can
// skip the tests, and on the step leaves the leg green), a continue-on-error
// on either that is not the boolean false (a boolean true and an expression
// alike), and the dry-run switch in a step's environment (B897). gate.yml as
// it is, and with continue-on-error: false, has none.
func TestTheNativeJobCheckRefusesALegThatCanPassWithoutItsTests(t *testing.T) {
	gate := string(gateWorkflow(t))
	const jobAnchor, stepAnchor = "    runs-on: ${{ matrix.os }}\n", "      - name: test\n"
	with := func(t *testing.T, anchor, added string) []byte {
		t.Helper()
		if n := strings.Count(gate, anchor); n != 1 {
			t.Fatalf("gate.yml holds %q %d times, want once", anchor, n)
		}
		return []byte(strings.Replace(gate, anchor, anchor+added, 1))
	}
	for _, tc := range []struct {
		name, anchor, added, says string
	}{
		{"an if on the job", jobAnchor, "    if: github.event_name == 'push'\n", "the job has if: github.event_name == 'push'"},
		{"an if false on the job", jobAnchor, "    if: false\n", "the job has if: false"},
		{"continue-on-error true on the job", jobAnchor, "    continue-on-error: true\n", "the job has continue-on-error: true"},
		{"continue-on-error as an expression on the job", jobAnchor, "    continue-on-error: ${{ matrix.os == 'macos-latest' }}\n", "the job has continue-on-error: ${{ matrix.os == 'macos-latest' }}"},
		{"an if on the test step", stepAnchor, "        if: matrix.group == 'rest'\n", "its test step has if: matrix.group == 'rest'"},
		{"an if false on the test step", stepAnchor, "        if: ${{ false }}\n", "its test step has if: ${{ false }}"},
		{"continue-on-error true on the test step", stepAnchor, "        continue-on-error: true\n", "its test step has continue-on-error: true"},
		{"the dry-run switch in the test step's environment", stepAnchor, "        env:\n          NATIVE_TEST_DRY_RUN: 1\n", "names NATIVE_TEST_DRY_RUN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			job, err := readNativeJob(with(t, tc.anchor, tc.added))
			if err != nil {
				t.Fatalf("readNativeJob: %v", err)
			}
			if len(job.skips) != 1 || !strings.Contains(job.skips[0], tc.says) {
				t.Errorf("readNativeJob found %q, want one finding saying %q", job.skips, tc.says)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		workflow []byte
	}{
		{"gate.yml as it is", []byte(gate)},
		{"continue-on-error false on the job", with(t, jobAnchor, "    continue-on-error: false\n")},
	} {
		t.Run("none: "+tc.name, func(t *testing.T) {
			job, err := readNativeJob(tc.workflow)
			if err != nil {
				t.Fatalf("readNativeJob: %v", err)
			}
			if len(job.skips) != 0 {
				t.Errorf("readNativeJob found %q, want nothing", job.skips)
			}
		})
	}
}
