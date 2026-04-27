package main

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// resetColorState restores the colorEnabled cache so each test sees
// the env it actually configured. sync.Once has no public reset; we
// zero-assign the whole struct so the next colorEnabled() runs the
// once.Do body again. Safe in a serial test (no concurrent readers
// of colorState during the swap).
func resetColorState(t *testing.T) {
	t.Helper()
	colorState.once = sync.Once{}
	colorState.on = false
}

// TestColorStrippedNoColor pins the contract: when NO_COLOR is set
// (per https://no-color.org), the rendered output must contain zero
// `\x1b[` SGR sequences and zero Unicode box-drawing chars (the
// boxStyle ASCII-fallback should engage).
func TestColorStrippedNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	resetColorState(t)
	out := box("hello", []string{"world"})
	if strings.Contains(out, "\x1b[") {
		t.Errorf("NO_COLOR=1 frame contains ANSI escape: %q", out)
	}
	if strings.ContainsAny(out, "╔╗╚╝═║┌┐└┘─│") {
		t.Errorf("NO_COLOR=1 frame contains Unicode box-drawing: %q", out)
	}
}

// TestColorStrippedDumbTerm covers the TERM=dumb branch — historically
// dumb-terminal users want no escapes either.
func TestColorStrippedDumbTerm(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "dumb")
	resetColorState(t)
	out := box("hello", []string{"world"})
	if strings.Contains(out, "\x1b[") {
		t.Errorf("TERM=dumb frame contains ANSI escape: %q", out)
	}
}

// TestTruncateMid covers head + tail preservation with a `...` middle.
func TestTruncateMid(t *testing.T) {
	cases := []struct {
		in       string
		max      int
		wantSame bool // when true, output should equal input (no truncation)
	}{
		{"", 30, true},
		{"short", 30, true},
		{"/Users/me/very/long/path/to/something/bridge.yaml", 30, false},
		{strings.Repeat("x", 60), 30, false},
	}
	for _, c := range cases {
		got := truncateMid(c.in, c.max)
		if c.wantSame {
			if got != c.in {
				t.Errorf("truncateMid(%q,%d) = %q; expected unchanged", c.in, c.max, got)
			}
			continue
		}
		if utf8.RuneCountInString(got) > c.max {
			t.Errorf("truncateMid(%q,%d) = %q has %d runes > %d max", c.in, c.max, got, utf8.RuneCountInString(got), c.max)
		}
		if !strings.Contains(got, "...") {
			t.Errorf("truncateMid(%q,%d) = %q; expected `...` ellipsis", c.in, c.max, got)
		}
	}
}

// TestFrameWidthBudget asserts every line of a rendered frame is
// exactly frameWidth runes wide. Catches accidental over-wide lines
// that would wrap on a narrow terminal.
func TestFrameWidthBudget(t *testing.T) {
	t.Setenv("NO_COLOR", "1") // strip color so rune-counting is the visible width
	resetColorState(t)
	cases := []struct {
		name string
		out  string
	}{
		{"box-no-title", box("", []string{"a", "bb"})},
		{"box-with-title", box("title", []string{"line"})},
		{"frame-no-title", frame("", []string{"x"})},
		{"frame-with-title", frame("title", []string{"x"})},
	}
	for _, c := range cases {
		for i, ln := range strings.Split(strings.TrimRight(c.out, "\n"), "\n") {
			w := utf8.RuneCountInString(ln)
			if w != frameWidth {
				t.Errorf("%s line %d width = %d (want %d): %q", c.name, i, w, frameWidth, ln)
			}
		}
	}
}

// TestShellHandoffContainsAllForms covers the unknown-shell fallback:
// when neither $PSModulePath nor $ComSpec resolves we should see at
// least one shell label rendered (which one depends on host GOOS).
func TestShellHandoffContainsAllForms(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	resetColorState(t)
	out := shellHandoff("/usr/local/bin/bridge", "/etc/bridge.yaml")
	if out == "" {
		t.Fatal("shellHandoff returned empty")
	}
	hasAny := strings.Contains(out, "PowerShell") ||
		strings.Contains(out, "cmd.exe") ||
		strings.Contains(out, "bash / zsh")
	if !hasAny {
		t.Errorf("shellHandoff output missing all shell labels: %q", out)
	}
}

// TestQuoteHelpersHandleHostilePaths pins the path-escaping contract
// the original transcript was missing — paths with spaces / quotes /
// shell-meaningful characters must round-trip through the per-shell
// quote helper into a token a copy-paste produces verbatim. A
// regression here brings back the "command line breaks on weird
// path" failure mode the PR is meant to fix.
func TestQuoteHelpersHandleHostilePaths(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		ps    []string // substrings the PS form must contain
		cmd   []string // substrings the cmd.exe form must contain
		posix []string // substrings the bash/zsh form must contain
	}{
		{
			name:  "windows-with-spaces",
			raw:   `C:\Program Files\1-bit-bridge\bridge.exe`,
			ps:    []string{`"C:\Program Files\1-bit-bridge\bridge.exe"`},
			cmd:   []string{`"C:\Program Files\1-bit-bridge\bridge.exe"`},
			posix: []string{`'C:\Program Files\1-bit-bridge\bridge.exe'`},
		},
		{
			name:  "embedded-quote",
			raw:   `weird"name`,
			ps:    []string{"`\""}, // PS backtick-escapes the quote
			cmd:   []string{`""`},  // cmd doubles the quote
			posix: []string{`'weird"name'`},
		},
		{
			name:  "single-quote",
			raw:   `Bob's Music`,
			ps:    []string{`Bob's Music`}, // single quote is literal in PS double-quoted
			cmd:   []string{`Bob's Music`},
			posix: []string{`'Bob'\''s Music'`}, // posix `'\''` trick
		},
		{
			name:  "dollar-expansion",
			raw:   `$HOME/bridge.yaml`,
			ps:    []string{"`$"},                  // backtick-escape $ in PS
			cmd:   []string{`$HOME/bridge.yaml`},   // cmd doesn't expand $
			posix: []string{`'$HOME/bridge.yaml'`}, // single quotes disable expansion
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ps := quotePS(c.raw)
			for _, sub := range c.ps {
				if !strings.Contains(ps, sub) {
					t.Errorf("quotePS(%q) = %q; want substring %q", c.raw, ps, sub)
				}
			}
			cmd := quoteCmd(c.raw)
			for _, sub := range c.cmd {
				if !strings.Contains(cmd, sub) {
					t.Errorf("quoteCmd(%q) = %q; want substring %q", c.raw, cmd, sub)
				}
			}
			pos := quotePosix(c.raw)
			for _, sub := range c.posix {
				if !strings.Contains(pos, sub) {
					t.Errorf("quotePosix(%q) = %q; want substring %q", c.raw, pos, sub)
				}
			}
		})
	}
}
