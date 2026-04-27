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

// colorEnabled reports whether the bridge's box / frame helpers can
// safely emit ANSI escape sequences. Conservative — returns true ONLY
// when:
//   - NO_COLOR is unset (https://no-color.org)
//   - TERM is not "dumb" (and not empty on POSIX)
//   - BOTH os.Stdout AND os.Stderr are TTYs. Frames go to stderr at
//     two error sites in init.go (service-install fail, spawn fail);
//     a stdout-only check would leak raw escapes into a redirected
//     stderr log.
//   - On Windows, ENABLE_VIRTUAL_TERMINAL_PROCESSING is now active
//     for stdout (initTerminal returned true). A SetConsoleMode
//     failure on legacy conhost would otherwise leave us emitting
//     raw `\e[95m` text the console can't parse.
//
// Cached via sync.Once — none of the inputs change at runtime, so a
// re-probe on every paint() call would just waste cycles.
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
		// Both streams must be TTYs. Frames are written to stderr
		// at two sites (install-fail / spawn-fail) and we can't
		// branch the boolean per-stream without threading the
		// writer through every call site — conservative AND keeps
		// the API tiny.
		if !isatty.IsTerminal(os.Stdout.Fd()) || !isatty.IsTerminal(os.Stderr.Fd()) {
			colorState.on = false
			return
		}
		// Windows VT mode flip MUST succeed before we commit to
		// color. initTerminal returns false if SetConsoleMode
		// failed (legacy conhost / minimal env / no console).
		// POSIX impl always returns true.
		colorState.on = initTerminal()
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

// stripANSI returns s with all CSI / SGR escape sequences removed.
// Box / frame width budgeting must measure visible runes only —
// without this, a `\x1b[95m` SGR byte cluster counts as 5 runes
// against frameWidth and the right border drifts left on every
// colored body line. Handles the standard `\x1b[ ... <final-byte>`
// shape (final byte in 0x40-0x7E) which covers SGR, cursor moves,
// and the rest of the CSI alphabet.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			// Skip until a final byte (0x40-0x7E) closes the CSI.
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7E) {
				j++
			}
			if j < len(s) {
				j++ // include the final byte
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// runeWidth returns the visible-rune count of s, ignoring any ANSI
// escape sequences. This is the right measure for frame budgeting:
// a `\x1b[95mhello\x1b[0m` should render as 5 columns wide, not 13.
func runeWidth(s string) int { return utf8.RuneCountInString(stripANSI(s)) }

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
		// If the line is too long we strip ANSI before truncating so the
		// slice never lands inside an escape sequence (which would emit
		// half a `\e[95m` and corrupt the rest of the terminal).
		body := ln
		if runeWidth(body) > inner-2 {
			body = truncateMid(stripANSI(body), inner-2)
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

// quotePS escapes a string for use inside a PowerShell double-quoted
// argument. PS treats `"` as the quote character and uses backtick (`)
// as its escape; literal backticks must be doubled. `$` is also
// expanded inside double quotes — escape it. Newlines / carriage
// returns are stripped (paths can't contain them on any reasonable
// filesystem; defensive).
func quotePS(s string) string {
	r := strings.NewReplacer(
		"`", "``",
		`"`, "`\"",
		"$", "`$",
		"\n", "",
		"\r", "",
	)
	return `"` + r.Replace(s) + `"`
}

// quoteCmd escapes a string for use inside a cmd.exe double-quoted
// argument. cmd's only escape inside `"..."` is `""` for a literal
// quote — backslash is NOT special, paths like `C:\Users\...`
// round-trip intact. CR/LF would terminate the line, NUL is invalid
// in any path; both stripped. Mirrors the same shape as
// `internal/packaging.CmdEscape` but used for command-line printing
// rather than .cmd-file generation, so backslashes are left alone.
func quoteCmd(s string) string {
	r := strings.NewReplacer(
		`"`, `""`,
		"\n", "",
		"\r", "",
		"\x00", "",
	)
	return `"` + r.Replace(s) + `"`
}

// quotePosix wraps a string in single quotes for bash / zsh / sh,
// using the standard `'\”` trick to embed a literal single quote
// inside a single-quoted string. Single quotes disable all shell
// expansion in POSIX shells, which is exactly what we want for an
// arbitrary path that may contain `$` / `~` / spaces / newlines.
func quotePosix(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

// shellHandoff renders a frame showing the user how to start the
// bridge in their current shell. On Windows we ALWAYS print both
// PowerShell and cmd.exe variants because PSModulePath is set in
// both shell environments — guessing wrong with a single-shell
// print would just bring the original transcript bug back. On
// POSIX we print the bash/zsh variant. SSH connections with $SHELL
// unset get all three so a remote operator can pick.
//
// All paths are shell-quoted: PS backtick-escapes, cmd doubles
// embedded quotes, posix uses single-quote-with-`'\”` escape.
// Spaces in paths (common on Windows: "C:\Program Files\bridge.exe")
// no longer break the printed command.
func shellHandoff(binPath, cfgPath string) string {
	var lines []string
	lines = append(lines, "")
	if runtime.GOOS == "windows" {
		lines = append(lines, paint(cBrightYellow, "PowerShell:"))
		// `& <path>` invocation works regardless of CWD — the
		// PowerShell rule that bit the original transcript user.
		lines = append(lines, "  & "+quotePS(binPath)+" `")
		lines = append(lines, "    serve --config "+quotePS(cfgPath))
		lines = append(lines, "")
		lines = append(lines, paint(cBrightYellow, "cmd.exe:"))
		// cmd needs the .exe name (PATH-resolved if installed) or
		// a quoted full path. We print the full quoted path for
		// determinism — works whether the binary is on PATH or not.
		lines = append(lines, "  "+quoteCmd(binPath)+" serve --config "+quoteCmd(cfgPath))
		lines = append(lines, "")
	} else {
		lines = append(lines, paint(cBrightYellow, "bash / zsh:"))
		lines = append(lines, "  "+quotePosix(binPath)+" serve --config "+quotePosix(cfgPath))
		lines = append(lines, "")
		// SSH-from-Windows or other ambiguous environments get the
		// PS / cmd alternatives appended so a remote operator can
		// still copy the right one. Detection: $PSModulePath set
		// while GOOS=linux/darwin means we're inside an SSH session
		// that originated from Windows (rare but real).
		if os.Getenv("PSModulePath") != "" {
			lines = append(lines, paint(cBrightYellow, "PowerShell (if reachable):"))
			lines = append(lines, "  & "+quotePS(binPath)+" `")
			lines = append(lines, "    serve --config "+quotePS(cfgPath))
			lines = append(lines, "")
		}
	}
	return frame("to start the bridge later, run:", lines)
}

// fprintBox is a convenience for the common "render box, write to w"
// pattern used at every init.go handoff site. Wraps Fprint so callers
// don't have to discard the (n int, err error) return.
func fprintBox(w io.Writer, s string) {
	_, _ = fmt.Fprint(w, s)
}

// logo renders the menu's top banner — title + subtitle inside a
// double-line box at frameWidth. Kept deliberately simple (no
// multi-line ASCII art letters) so it fits the 55-col budget on
// every supported terminal. The version string is pulled from the
// bridge's `version` package; subtitle is the protocol number.
//
// Caller is expected to write the result to a real TTY; on
// NO_COLOR / dumb terminals, the ASCII fallback box-drawing chars
// (+/-/|/=) take over automatically via boxStyle.
func logo(serverVersion string, protocol int) string {
	title := fmt.Sprintf("1-bit-bridge  v%s", serverVersion)
	subtitle := fmt.Sprintf("companion server · protocol v%d", protocol)
	return box("", []string{
		"",
		"  " + paint(cBoldMagenta, title),
		"  " + paint(cBrightCyan, subtitle),
		"",
	})
}
