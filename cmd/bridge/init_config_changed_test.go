package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/term"
)

// A run reads the config at its start, which is what a rewrite keeps, and
// decides whether to keep or replace it at its "Overwrite?", both before its
// preflight and its name prompt, which wait on the operator. A config another
// process writes while they wait (a second init, an edit, the console's
// save) is one the run neither read nor asked about. Moving "Overwrite?"
// ahead of the preflight (backlog B61) widened that window to take in the
// name prompt, and CodeRabbit's security review of #1106 named it: the run
// wrote over such a config.

// TestInitRefusesAConfigThatChangedWhileItRan: a config written while the
// name prompt waits is left as that writer left it, and the run exits 1
// having written nothing. With nothing written meanwhile, the run is not
// refused.
func TestInitRefusesAConfigThatChangedWhileItRan(t *testing.T) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		t.Skip("stdin is a terminal, where init would offer to start the bridge after a run it does not refuse")
	}
	for _, tc := range []changedWhileItRan{
		{"a first install, nothing written meanwhile", nil, []string{"Mine\n"}, nil, 0, "Mine"},
		{"a first install, another init's config written meanwhile", nil, []string{"Mine\n"},
			func(t *testing.T, cfgDir, lib string) {
				writeLoopbackInstall(t, cfgDir, lib, freeLoopbackPort(t), freeLoopbackPort(t))
			}, 1, "Existing"},
		{"a rewrite, nothing written meanwhile", installAnyLoopback, []string{"y\n", "Rewritten\n"}, nil, 0, "Rewritten"},
		{"a rewrite, the config edited meanwhile", installAnyLoopback, []string{"y\n", "Rewritten\n"},
			func(t *testing.T, cfgDir, _ string) {
				appendToConfig(t, cfgDir, "# an edit made while init waited at its name prompt\n")
			}, 1, "Existing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgDir := filepath.Join(t.TempDir(), "cfg")
			code, printed, written := tc.run(t, cfgDir)
			defer logRunOnFailure(t, printed)
			if code != tc.wantCode {
				t.Fatalf("the run exited %d, want %d", code, tc.wantCode)
			}
			if tc.meanwhile != nil {
				checkLeftAsWritten(t, cfgDir, written, printed)
			}
			if got := loadInstallConfig(t, cfgDir).LibraryName; got != tc.wantName {
				t.Errorf("libraryName = %q, want %q", got, tc.wantName)
			}
		})
	}
}

// changedWhileItRan is a case of TestInitRefusesAConfigThatChangedWhileItRan.
type changedWhileItRan struct {
	name string
	// install writes the config the run starts from, if any.
	install func(t *testing.T, cfgDir, lib string)
	// answers are the run's stdin lines; the last one is its name.
	answers []string
	// meanwhile writes the config while the name prompt waits; nil writes
	// nothing.
	meanwhile func(t *testing.T, cfgDir, lib string)
	wantCode  int
	wantName  string
}

// run runs init over cfgDir with the case's answers on stdin, calling
// meanwhile as the last answer, the name, is read. It returns the exit code,
// what init printed, and the config as meanwhile left it, or "".
func (c changedWhileItRan) run(t *testing.T, cfgDir string) (code int, printed, written string) {
	t.Helper()
	lib := testLibrary(t)
	if c.install != nil {
		c.install(t, cfgDir, lib)
	}
	stdin := &linesWithAHook{lines: c.answers, before: func(i int) {
		if i == len(c.answers)-1 && c.meanwhile != nil {
			c.meanwhile(t, cfgDir, lib)
			written = readConfigFile(t, cfgDir)
		}
	}}
	var out, errOut strings.Builder
	code = initCmd([]string{"--no-service", "--skip-doctor", "--dir", cfgDir, "--library", lib}, stdin, &out, &errOut)
	return code, stripANSI(out.String() + errOut.String()), written
}

// checkLeftAsWritten checks that a run a config's writer overtook left that
// config as written, and said it changed.
func checkLeftAsWritten(t *testing.T, cfgDir, written, printed string) {
	t.Helper()
	if got := readConfigFile(t, cfgDir); got != written {
		t.Errorf("the run wrote over the config written while it waited:\n%s", got)
	}
	if !strings.Contains(printed, "changed while this init ran") {
		t.Errorf("the run does not say the config changed while it ran")
	}
}

// installAnyLoopback writes a loopback install at cfgDir on two free ports.
func installAnyLoopback(t *testing.T, cfgDir, lib string) {
	t.Helper()
	writeLoopbackInstall(t, cfgDir, lib, freeLoopbackPort(t), freeLoopbackPort(t))
}

// linesWithAHook is stdin for an interactive init that returns one line per
// Read and calls before(i) ahead of returning line i, so a test can act at
// the moment a prompt reads its answer.
type linesWithAHook struct {
	lines  []string
	before func(i int)
	next   int
}

// Read returns the next line whole, after calling before with its index, and
// io.EOF once every line has been read.
func (r *linesWithAHook) Read(p []byte) (int, error) {
	if r.next >= len(r.lines) {
		return 0, io.EOF
	}
	if r.before != nil {
		r.before(r.next)
	}
	n := copy(p, r.lines[r.next])
	r.next++
	return n, nil
}
