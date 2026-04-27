// Helpers for the cinematic 8-bit-style boxes / colors / shell-aware
// handoff prints used by `bridge init` (and, in PR 3, by the no-args
// menu launcher). Pure stdlib + go-isatty for TTY detection — no other
// runtime UI deps. All helpers are safe to call when stdout is a pipe
// or `NO_COLOR` is set; output degrades to plain ASCII automatically.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
)

// frameWidth is the total column count for every styled frame the
// CLI renders. Hard-cap at 55 so the cinematic illusion survives a
// half-width tmux pane (typical: 80/2 = 40, 132/2 = 66) without the
// right border wrapping to the next line. Long paths inside frames
// are mid-truncated by truncateMid rather than wrapped.
const frameWidth = 55

// SGR (Select Graphic Rendition) codes for the saturated 8-bit-ish
// palette. We deliberately use the bright variants (90-97) instead of
// the standard 30-37 set — the brights are closer to the saturated
// NES/Amiga colors users expect from the "8-bit" framing, and every
// modern terminal renders them the same.
const (
	cReset        = "\x1b[0m"
	cBold         = "\x1b[1m"
	cBrightCyan   = "\x1b[96m"
	cBrightYellow = "\x1b[93m"
	cBoldMagenta  = "\x1b[1;95m"
	cDim          = "\x1b[90m"
)

// colorState caches the result of TTY+NO_COLOR+TERM detection. The
// probe runs once per process; the inputs (env, fd identity) don't
// change at runtime, so re-running the detection on every paint
// would just waste cycles.
var colorState struct {
	once sync.Once
	on   bool
}

// colorEnabled reports whether the current stdout supports ANSI color
// output. Honors NO_COLOR (no-color.org), TERM=dumb, and non-TTY
// stdout. On Windows, also runs `initTerminal()` (build-tagged) which
// flips ENABLE_VIRTUAL_TERMINAL_PROCESSING via stdlib syscall —
// without that flip, pre-Win10-Anniversary conhost (and some legacy
// configs on newer Windows) prints raw `\e[95m` as literal garbage.
//
// Cached: subsequent calls return the cached boolean.
func colorEnabled() bool {
	colorState.once.Do(func() {
		if os.Getenv("NO_COLOR") != "" {
			colorState.on = false
			return
		}
		t := os.Getenv("TERM")
		if t == "dumb" {
			colorState.on = false
			return
		}
		// Empty TERM is a Unix dumb-terminal signal. Skip on
		// Windows where TERM is unset by default.
		if t == "" && runtime.GOOS != "windows" {
			colorState.on = false
			return
		}
		if !isatty.IsTerminal(os.Stdout.Fd()) {
			colorState.on = false
			return
		}
		// Initialise the platform's terminal mode if needed.
		// initTerminal() is in tty_{windows,posix}.go.
		initTerminal()
		colorState.on = true
	})
	return colorState.on
}

// paint wraps s in an SGR sequence when color is enabled. When color
// is off (NO_COLOR, dumb terminal, piped output), returns s unchanged
// — caller can use this everywhere without branching.
func paint(color, s string) string {
	if !colorEnabled() {
		return s
	}
	return color + s + cReset
}

// truncateMid keeps the head + tail of s with `...` between when len
// exceeds max. Used for long config / library paths inside frames so
// they don't blow the frameWidth budget. Counts runes (not bytes) so
// box-drawing / non-ASCII chars come out at their visual width.
func truncateMid(s string, max int) string {
	if max < 5 {
		// Degenerate budget — return whatever fits, even if ugly.
		return s
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	keep := (max - 3) / 2
	head := string(runes[:keep])
	tail := string(runes[len(runes)-keep:])
	return head + "..." + tail
}

// runeWidth returns the rune count, used for frame budgeting. A
// dedicated function (vs. inlining utf8.RuneCountInString) so future
// support for wide-char East-Asian glyphs has a single hook.
func runeWidth(s string) int { return utf8.RuneCountInString(s) }

// boxRunes selects the border glyphs based on color state. Unicode
// double-line / single-line on color-capable terminals, ASCII fallback
// on NO_COLOR / dumb-terminal so a piped run doesn't sprinkle
// box-drawing bytes in a log file.
type boxRunes struct {
	tl, tr, bl, br, h, v string
}

func boxStyle(double bool) boxRunes {
	if !colorEnabled() {
		// ASCII fallback: no color → assume the operator is reading
		// in a context where Unicode is also unwelcome (NO_COLOR
		// often correlates with stripped-down environments).
		if double {
			return boxRunes{tl: "+", tr: "+", bl: "+", br: "+", h: "=", v: "|"}
		}
		return boxRunes{tl: "+", tr: "+", bl: "+", br: "+", h: "-", v: "|"}
	}
	if double {
		return boxRunes{tl: "╔", tr: "╗", bl: "╚", br: "╝", h: "═", v: "║"}
	}
	return boxRunes{tl: "┌", tr: "┐", bl: "└", br: "┘", h: "─", v: "│"}
}

// box renders title (centered in the top border) + lines (left-aligned,
// padded to inner width) inside a double-line border at frameWidth.
// Title is paint()-wrapped in bright cyan; line content is rendered
// raw (caller can paint() individual lines if they want highlighted
// content). Returns a single string with trailing newline.
func box(title string, lines []string) string {
	r := boxStyle(true)
	inner := frameWidth - 2 // minus the two vertical borders
	var b strings.Builder
	// Top border with optional title inset.
	if title != "" {
		// "═══ title ═══" pattern — pad lefts/rights so the total
		// is frameWidth-2 fill chars.
		t := " " + title + " "
		tw := runeWidth(t)
		if tw > inner-4 {
			t = " " + truncateMid(title, inner-6) + " "
			tw = runeWidth(t)
		}
		left := (inner - tw) / 2
		right := inner - tw - left
		b.WriteString(paint(cBrightCyan, r.tl+strings.Repeat(r.h, left)))
		b.WriteString(paint(cBoldMagenta, t))
		b.WriteString(paint(cBrightCyan, strings.Repeat(r.h, right)+r.tr))
		b.WriteByte('\n')
	} else {
		b.WriteString(paint(cBrightCyan, r.tl+strings.Repeat(r.h, inner)+r.tr))
		b.WriteByte('\n')
	}
	// Body lines, padded.
	for _, ln := range lines {
		// Trim/truncate to fit inner-2 (leave 1 col padding each side).
		body := ln
		if runeWidth(body) > inner-2 {
			body = truncateMid(body, inner-2)
		}
		pad := inner - 2 - runeWidth(body)
		b.WriteString(paint(cBrightCyan, r.v))
		b.WriteByte(' ')
		b.WriteString(body)
		if pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteByte(' ')
		b.WriteString(paint(cBrightCyan, r.v))
		b.WriteByte('\n')
	}
	// Bottom border.
	b.WriteString(paint(cBrightCyan, r.bl+strings.Repeat(r.h, inner)+r.br))
	b.WriteByte('\n')
	return b.String()
}

// frame is box's single-line-border sibling, used for sub-boxes that
// shouldn't compete visually with the main box. Same width / padding /
// truncation rules as box.
func frame(title string, lines []string) string {
	r := boxStyle(false)
	inner := frameWidth - 2
	var b strings.Builder
	if title != "" {
		t := " " + title + " "
		tw := runeWidth(t)
		if tw > inner-4 {
			t = " " + truncateMid(title, inner-6) + " "
			tw = runeWidth(t)
		}
		left := 2
		right := inner - tw - left
		if right < 1 {
			right = 1
		}
		b.WriteString(paint(cBrightCyan, r.tl+strings.Repeat(r.h, left)))
		b.WriteString(paint(cBrightYellow, t))
		b.WriteString(paint(cBrightCyan, strings.Repeat(r.h, right)+r.tr))
		b.WriteByte('\n')
	} else {
		b.WriteString(paint(cBrightCyan, r.tl+strings.Repeat(r.h, inner)+r.tr))
		b.WriteByte('\n')
	}
	for _, ln := range lines {
		body := ln
		if runeWidth(body) > inner-2 {
			body = truncateMid(body, inner-2)
		}
		pad := inner - 2 - runeWidth(body)
		b.WriteString(paint(cBrightCyan, r.v))
		b.WriteByte(' ')
		b.WriteString(body)
		if pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteByte(' ')
		b.WriteString(paint(cBrightCyan, r.v))
		b.WriteByte('\n')
	}
	b.WriteString(paint(cBrightCyan, r.bl+strings.Repeat(r.h, inner)+r.br))
	b.WriteByte('\n')
	return b.String()
}

// shellHandoff renders a frame that shows the user how to start the
// bridge in their current shell. We detect $SHELL / $ComSpec /
// $PSModulePath to single out one of three forms (PowerShell,
// cmd.exe, bash/zsh). When detection is ambiguous (e.g. SSH with no
// $SHELL set), we print all three so the user picks the right one.
//
// PowerShell uses `&` invocation so a bare path resolves regardless
// of CWD — exactly the bug the original transcript hit.
// bash / zsh single-quote the path to survive spaces.
// cmd.exe assumes `bridge.exe` is on PATH (warns via output if not).
func shellHandoff(binPath, cfgPath string) string {
	pickPS := strings.Contains(strings.ToLower(os.Getenv("PSModulePath")), "windowspowershell") ||
		strings.Contains(strings.ToLower(os.Getenv("PSModulePath")), "powershell")
	pickCmd := os.Getenv("ComSpec") != "" && !pickPS && runtime.GOOS == "windows"
	pickPosix := !pickPS && !pickCmd && runtime.GOOS != "windows"
	// Detection failed → print all three.
	all := !(pickPS || pickCmd || pickPosix)
	var lines []string
	lines = append(lines, "")
	if pickPS || all {
		lines = append(lines, paint(cBrightYellow, "PowerShell:"))
		lines = append(lines, "  & \""+binPath+"\" `")
		lines = append(lines, "    serve --config \""+cfgPath+"\"")
		lines = append(lines, "")
	}
	if pickCmd || all {
		lines = append(lines, paint(cBrightYellow, "cmd.exe:"))
		// cmd has no escape for " inside "..." other than ""; paths
		// with literal quotes are pathological and we don't try to
		// support them here — a real % would be the user's problem.
		lines = append(lines, "  bridge.exe serve --config \""+cfgPath+"\"")
		lines = append(lines, "")
	}
	if pickPosix || all {
		lines = append(lines, paint(cBrightYellow, "bash / zsh:"))
		lines = append(lines, "  bridge serve --config '"+cfgPath+"'")
		lines = append(lines, "")
	}
	return frame("to start the bridge later, run:", lines)
}

// fprintBox is a convenience for the common "render box, write to w"
// pattern used at every init.go handoff site. Wraps Fprint so callers
// don't have to discard the (n int, err error) return.
func fprintBox(w io.Writer, s string) {
	_, _ = fmt.Fprint(w, s)
}
