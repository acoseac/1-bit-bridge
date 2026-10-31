package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// The one-line installer, scripts/install.sh, runs the bridge it installs
// (init, update, status, start, version, output) and prints commands for the
// rest. Its own tests (scripts/install-test) run it against a fake bridge, so
// these hold it to this CLI: every flag it passes or prints is one the
// subcommand defines, every flag the fake takes is one too, and what it reads
// from bridge version, bridge status and bridge update is what they print.
var (
	installerScriptPath = filepath.Join("..", "..", "scripts", "install.sh")
	installerFakePath   = filepath.Join("..", "..", "scripts", "install-test", "testdata", "bridge")
)

// installerSubcommand is the bridge's subcommands the installer names,
// output's verbs counted as subcommands of their own.
const installerSubcommand = `(init|update|status|start|stop|restart|version|output (?:player|dac)|pair|serve|logs)\b`

// installerCall finds a call of the bridge in a line of the installer, run or
// printed: the program (bridge_line "$X", which prints the command for the
// user to type, or "$TARGET", $TARGET or bridge), the subcommand and the
// words after it, up to the end of the command.
var installerCall = regexp.MustCompile(`(?:\bbridge_line "\$[A-Za-z_0-9]+"|"\$[A-Za-z_0-9]+"|\$[A-Za-z_0-9]+|\bbridge) ` +
	installerSubcommand + `([^<>|;&#]*)`)

// installerPrinted finds a bridge command the installer prints through
// bridge_line, and installerUnquoted one it writes out around a variable,
// $TARGET pair, or around a quoted one outside bridge_line,
// $(sh_quote "$TARGET") pair.
var (
	installerPrinted  = regexp.MustCompile(`\bbridge_line "\$[A-Za-z_0-9]+" ` + installerSubcommand + `([^<>|;&#]*)`)
	installerUnquoted = regexp.MustCompile(`(?:(?:^|[^"A-Za-z_0-9])\$[A-Za-z_0-9]+|\$\(sh_quote "[^"]*"\)) ` + installerSubcommand)
)

var installerFlag = regexp.MustCompile(`(?:^|\s)(--?[A-Za-z][A-Za-z0-9-]*)`)

// definedFlags are the flags `bridge <sub...> -h` lists, read from the flag
// set's own usage.
func definedFlags(t *testing.T, sub string) map[string]bool {
	t.Helper()
	var stdout, stderr bytes.Buffer
	run(context.Background(), append(strings.Fields(sub), "-h"), &stdout, &stderr)
	flags := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^  -([A-Za-z0-9][A-Za-z0-9-]*)`).FindAllStringSubmatch(stderr.String(), -1) {
		flags["-"+m[1]] = true
		flags["--"+m[1]] = true
	}
	return flags
}

// installerBridgeFlags reads the installer's calls of the bridge: the
// subcommand of each, and the flags given to it.
func installerBridgeFlags(t *testing.T) map[string][]string {
	t.Helper()
	b, err := os.ReadFile(installerScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string][]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, m := range installerCall.FindAllStringSubmatch(line, -1) {
			sub := m[1]
			if _, ok := calls[sub]; !ok {
				calls[sub] = nil
			}
			for _, f := range installerFlag.FindAllStringSubmatch(m[2], -1) {
				calls[sub] = append(calls[sub], f[1])
			}
		}
	}
	return calls
}

func TestTheInstallerCallsTheBridgeWithFlagsItDefines(t *testing.T) {
	calls := installerBridgeFlags(t)
	// The ones it runs or prints, so a pattern that stopped matching cannot
	// pass here.
	for _, want := range []string{"init", "update", "status", "start", "version", "output player", "output dac", "pair", "serve", "logs"} {
		if _, ok := calls[want]; !ok {
			t.Errorf("the installer no longer names bridge %s, or this test no longer finds it", want)
		}
	}
	subs := make([]string, 0, len(calls))
	for sub := range calls {
		subs = append(subs, sub)
	}
	sort.Strings(subs)
	for _, sub := range subs {
		defined := definedFlags(t, sub)
		for _, flag := range calls[sub] {
			if !defined[flag] {
				t.Errorf("the installer gives bridge %s the flag %s, which it does not define", sub, flag)
			}
		}
	}
	for _, want := range []string{"--yes", "--library"} {
		if !strings.Contains(" "+strings.Join(calls["init"], " ")+" ", " "+want+" ") {
			t.Errorf("the installer's bridge init calls lost %s: %q", want, calls["init"])
		}
	}
	b, err := os.ReadFile(installerScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`output dac (bind|inspect)`).Match(b) {
		t.Errorf("the installer binds or inspects a DAC: it configures no output")
	}
}

// Every bridge command the installer prints for the user to type goes
// through bridge_line, which quotes each word: one written out around a
// variable ($TARGET pair) splits a path at its spaces and runs what follows
// a semicolon in it. And each printed command that reads the settings
// (pair, status, serve, update) names them with --config, so that a bridge
// installed with its settings elsewhere is told where they are; bridge_line
// leaves out a --config naming the settings file the bridge reads without
// one. The installer's own tests run what it prints, word for word.
func TestTheInstallerPrintsTheBridgesCommandsQuoted(t *testing.T) {
	b, err := os.ReadFile(installerScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	printed := map[string]bool{}
	for i, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if installerUnquoted.MatchString(line) {
			t.Errorf("line %d writes a bridge command out around a variable, unquoted: %s", i+1, strings.TrimSpace(line))
		}
		for _, m := range installerPrinted.FindAllStringSubmatch(line, -1) {
			printed[m[1]] = true
			switch m[1] {
			case "pair", "status", "serve", "update":
				if !regexp.MustCompile(`(?:^|\s)--config\s`).MatchString(m[2]) {
					t.Errorf("line %d prints bridge %s without --config: %s", i+1, m[1], strings.TrimSpace(line))
				}
			}
		}
	}
	for _, want := range []string{"pair", "status", "serve", "update", "init", "logs"} {
		if !printed[want] {
			t.Errorf("the installer prints no bridge %s through bridge_line, or this test no longer finds it", want)
		}
	}
}

// The fake bridge the installer's tests run takes no flag the real one does
// not define, so a call the fake accepts is one the real CLI accepts.
func TestTheInstallersFakeBridgeTakesOnlyRealFlags(t *testing.T) {
	b, err := os.ReadFile(installerFakePath)
	if err != nil {
		t.Fatal(err)
	}
	lines := regexp.MustCompile(`(?m)^# flags ([a-z ]+):(.*)$`).FindAllStringSubmatch(string(b), -1)
	if len(lines) < 5 {
		t.Fatalf("the fake bridge lists %d subcommands' flags", len(lines))
	}
	for _, m := range lines {
		defined := definedFlags(t, m[1])
		for _, flag := range strings.Fields(m[2]) {
			if !defined[flag] {
				t.Errorf("the fake bridge takes bridge %s %s, which the real one does not define", m[1], flag)
			}
		}
	}
}

// installerFunction runs one of the installer's functions, the script
// sourced without running it, on input, and returns what it printed without
// its trailing newlines, as the script's $(...) takes it.
func installerFunction(t *testing.T, input string, function string, args ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the installer is a POSIX shell script for Linux")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(installerScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sh, append([]string{"-c", `. "$1"; shift; "$@"`, "sh", script, function}, args...)...)
	cmd.Env = append(os.Environ(), "BRIDGE_INSTALL_TEST_SOURCE=1")
	cmd.Stdin = strings.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v", function, err)
	}
	return strings.TrimRight(string(out), "\n")
}

func TestTheInstallerReadsWhatBridgeVersionAndStatusPrint(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("bridge version: exit %d: %s", code, &stderr)
	}
	if got, want := installerFunction(t, stdout.String(), "version_from_text"), strings.TrimPrefix(version.ServerVersion, "v"); got != want {
		t.Errorf("the installer reads %q from %q, want %q", got, stdout.String(), want)
	}

	var status bytes.Buffer
	writeStatusHuman(&status, map[string]any{
		"libraryName":     "Living room",
		"serverVersion":   "0.2.2",
		"protocolVersion": float64(1),
		"uptimeSec":       float64(5),
		"listenAddress":   ":7788",
		"adminAddress":    "127.0.0.1:7789",
		"fingerprint":     "AA:BB",
	}, nil)
	for label, want := range map[string]string{"Server version": "0.2.2", "Admin address": "127.0.0.1:7789"} {
		if got := installerFunction(t, status.String(), "status_field", label); got != want {
			t.Errorf("the installer reads %s as %q from bridge status's\n%s\nwant %q", label, got, status.String(), want)
		}
	}
}

// The installer tells "no update" from a check that failed by the line
// bridge update prints for the first; both exit 0.
func TestTheInstallerMatchesWhatBridgeUpdatePrints(t *testing.T) {
	b, err := os.ReadFile(installerScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`grep -qF "([^"]+)" "\$TMP_DIR/update\.log"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("the installer no longer reads bridge update's output this way; change this test with it")
	}
	want := string(m[1])
	file, err := parser.ParseFile(token.NewFileSet(), "update.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "updateCmd" {
			return true
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(s, want) {
					found = true
				}
			}
			return true
		})
		return false
	})
	if !found {
		t.Errorf("updateCmd prints no %q, which the installer reads as no update", want)
	}
}
