// Package alsa reads what the Linux kernel's sound layer says about the
// box the bridge runs on: the sound cards in /proc/asound/cards, a USB
// card's id and stream formats, and, for a playback substream that is
// open, the parameters it was opened with and the thread that holds it.
// It is the first step of the bridge as an OUTPUT (a box with a DAC and
// MPD), where the reading is what lets the app's Signal Path show what
// the DAC receives rather than what the player meant to send.
//
// It only reads. The parsers are pure functions over a file's bytes, and
// Reader walks a /proc tree whose roots it is given, so the tests read
// trees captured from a real box (testdata/) on every CI OS. Nothing here
// needs Linux to compile or to run; only the host's own /proc/asound does.
//
// The parsers follow the kernel's printers (sound/core/init.c for cards,
// sound/usb/card.c and sound/usb/proc.c for usbid and streamN,
// sound/core/pcm.c for hw_params and status) and share three rules:
//
//   - A key they do not know is ignored. Kernels add lines, and a reading
//     that failed on the next one would be no reading at all.
//   - A key they know whose value does not parse is reported, and the
//     value is left out (null in JSON), never guessed. A key the kernel
//     prints once that arrives twice is reported and the first value
//     stands, and so is a block the kernel always prints in full that
//     arrives without one of its lines: that is a file cut short.
//   - What they report is bounded: at most maxProblems lines per file,
//     each holding at most maxQuoted bytes of the input it quotes.
package alsa

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

// maxProblems bounds the problems one parse reports. A garbled file can
// fail on every line, and past a handful the rest say nothing new; the
// last entry then counts the ones left out.
const maxProblems = 8

// maxQuoted bounds how much of an input line a problem quotes.
const maxQuoted = 64

// problems collects what one parse could not read, line by line.
type problems struct {
	list    []problem
	dropped int
}

// problem is one entry of problems: the line it is on (0 for the whole
// file) and what is wrong there.
type problem struct {
	line int
	msg  string
}

// addf records a problem on a 1-based line number, or on the whole file
// when line is 0.
func (p *problems) addf(line int, format string, args ...any) {
	if len(p.list) >= maxProblems {
		p.dropped++
		return
	}
	p.list = append(p.list, problem{line: line, msg: fmt.Sprintf(format, args...)})
}

// result returns the problems, never nil, in line order with the
// whole-file ones last (a block found cut short is reported when the
// parse reaches its end, at the line it began on), and then the count of
// any left out.
func (p *problems) result() []string {
	sorted := append([]problem{}, p.list...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i].line, sorted[j].line
		return a != 0 && (b == 0 || a < b)
	})
	out := make([]string, 0, len(sorted)+1)
	for _, pr := range sorted {
		if pr.line > 0 {
			out = append(out, fmt.Sprintf("line %d: %s", pr.line, pr.msg))
		} else {
			out = append(out, pr.msg)
		}
	}
	if p.dropped > 0 {
		out = append(out, fmt.Sprintf("%d more not shown", p.dropped))
	}
	return out
}

// blank reports whether a file holds nothing but whitespace.
func blank(ls []string) bool {
	for _, l := range ls {
		if strings.TrimSpace(l) != "" {
			return false
		}
	}
	return true
}

// quote renders input text for a problem: Go-quoted, so a control byte
// cannot reach a terminal raw, and cut to maxQuoted bytes.
func quote(s string) string {
	if len(s) > maxQuoted {
		return strconv.Quote(s[:maxQuoted]) + "…"
	}
	return strconv.Quote(s)
}

// lines splits a file into lines without a line-length limit (a
// bufio.Scanner stops at 64 KiB) and drops one trailing '\r' from each, so
// a capture that crossed a CRLF checkout reads as the kernel wrote it.
func lines(b []byte) []string {
	s := string(b)
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	out := strings.Split(s, "\n")
	for i, l := range out {
		out[i] = strings.TrimSuffix(l, "\r")
	}
	return out
}

// cutKey splits a "key: value" or "key = value" line at the first sep,
// trimming both sides. ok is false when sep does not occur.
func cutKey(line, sep string) (key, value string, ok bool) {
	k, v, ok := strings.Cut(line, sep)
	if !ok {
		return "", "", false
	}
	return strings.TrimSpace(k), strings.TrimSpace(v), true
}

// atoiNonNeg parses a decimal int that must not be negative.
func atoiNonNeg(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// intPtr returns a pointer to its argument, for the optional fields.
func intPtr(n int) *int { return &n }

// int64Ptr returns a pointer to its argument, for the optional fields.
func int64Ptr(n int64) *int64 { return &n }

// boolPtr returns a pointer to its argument, for the optional fields.
func boolPtr(b bool) *bool { return &b }

// maxFileBytes bounds one read of a /proc/asound file. The largest real
// one is a stream file listing every altset of a busy USB interface, a
// few KiB; a fake tree cannot make a reading hold more than this.
const maxFileBytes = 1 << 20

// maxProcFileBytes bounds one read of a /proc/<tid> file: a comm is at
// most 16 bytes, and a status about 1.5 KiB.
const maxProcFileBytes = 64 << 10

// readCapped reads a whole file, refusing one longer than limit bytes. A
// /proc file reports size 0 to stat, so the limit is applied to what is
// read, not to what the file claims.
func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("longer than %d bytes", limit)
	}
	return b, nil
}
