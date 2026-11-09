package updater

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary stands in for a candidate bridge, on every platform: a copy
// of it is the candidate whose settings check Install runs, or a backup a
// probe asks for its version, and TestMain runs it as that bridge when
// candidateEnv names the directory that scripts it. Its serve parses its
// flags with the flag package, as every release's does, so a candidate that
// predates the check refuses the flag in the flag package's own words.
//
// The script is files in that directory:
//
//   - check: what `serve --validate-config` does: valid, refuse, old (a
//     serve that does not define the flag), hang, exit:<n>, or refuse-key (a
//     release from before update.checkEnabled, whose strict decoder refuses
//     the key on its line where the settings write it, and takes them
//     otherwise; backlog B761's round 3)
//   - refusal: the reasons a refusing check gives, one a line
//   - version: the version `version` names
//   - silent: how long `version` says nothing before it answers, a duration
//
// and the stand-in appends every call's arguments, one call a line, to
// argv there.
const candidateEnv = "UPDATER_TEST_CANDIDATE"

func TestMain(m *testing.M) {
	if dir := os.Getenv(candidateEnv); dir != "" {
		os.Exit(runCandidateStandIn(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// runCandidateStandIn is the stand-in's whole behaviour; it returns the exit
// status.
func runCandidateStandIn(dir string, args []string) int {
	if f, err := os.OpenFile(filepath.Join(dir, "argv"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		fmt.Fprintln(f, strings.Join(args, " "))
		_ = f.Close()
	}
	script := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(b))
	}
	if len(args) == 1 && args[0] == "version" {
		if d, err := time.ParseDuration(script("silent")); err == nil {
			time.Sleep(d)
		}
		v := script("version")
		if v == "" {
			fmt.Fprintln(os.Stderr, "stand-in: no version scripted")
			return 1
		}
		fmt.Printf("1-bit-bridge %s (protocol v1)\n", v)
		return 0
	}
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, "stand-in: no such subcommand")
		return 2
	}
	mode := script("check")
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	configPath := fs.String("config", "", "")
	fs.String("addr", "", "")
	fs.Bool("init-if-missing", false, "")
	var validate *bool
	if mode != "old" {
		validate = fs.Bool(ConfigCheckFlag, false, "")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if validate == nil || !*validate {
		fmt.Fprintln(os.Stderr, "stand-in: asked to serve")
		return 3
	}
	switch {
	case mode == "valid":
		return ConfigCheckValidExit
	case mode == "refuse":
		for _, line := range strings.Split(script("refusal"), "\n") {
			fmt.Fprintln(os.Stderr, ConfigRefusedPrefix+line)
		}
		return ConfigCheckRefusedExit
	case mode == "refuse-key":
		if line := checkEnabledLine(*configPath); line > 0 {
			fmt.Fprintf(os.Stderr, "%sline %d: field checkEnabled not found in type config.UpdateConfig\n", ConfigRefusedPrefix, line)
			return ConfigCheckRefusedExit
		}
		return ConfigCheckValidExit
	case mode == "hang":
		time.Sleep(time.Hour)
		return 0
	case strings.HasPrefix(mode, "exit:"):
		n, err := strconv.Atoi(strings.TrimPrefix(mode, "exit:"))
		if err != nil {
			return 5
		}
		return n
	}
	fmt.Fprintln(os.Stderr, "stand-in: no check scripted")
	return 4
}

// checkEnabledLine is the line of the settings at path that writes a key
// named checkEnabled, as the stand-in's refuse-key mode finds it, 0 where
// none does or the file cannot be read.
func checkEnabledLine(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "checkEnabled:") {
			return i + 1
		}
	}
	return 0
}

// candidateStandIn makes the directory that scripts the stand-in, points
// candidateEnv at it for the rest of the test, and writes each file the
// script names.
func candidateStandIn(t *testing.T, files map[string]string) (script string) {
	t.Helper()
	script = t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(script, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(candidateEnv, script)
	return script
}

// candidateCalls are the stand-in's calls so far, one argument list each.
func candidateCalls(t *testing.T, script string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(script, "argv"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// installCandidateCopy puts a copy of the running test binary at path: a
// hard link where the host makes one, a copy on Windows, where a link to the
// mapped test binary cannot be removed by TempDir's cleanup.
func installCandidateCopy(t *testing.T, path string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && os.Link(exe, path) == nil {
		return path
	}
	b, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
