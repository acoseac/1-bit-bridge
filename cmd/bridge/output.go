package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
)

// outputCmd dispatches `bridge output <verb>`: the box the bridge runs on,
// as an audio output. `dac` reads what the box's sound cards say and
// `player` reads the box's MPD and how it is set. Both write nothing.
func outputCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		outputUsage(stderr)
		return 2
	}
	switch args[0] {
	case "dac":
		return outputDACCmd(ctx, args[1:], stdout, stderr)
	case "player":
		return outputPlayerCmd(ctx, args[1:], stdout, stderr)
	case "pcm-test":
		// Not in outputUsage: the B470 hardware harness, run by hand on a
		// box with a DAC (output_pcmtest.go). It has its own -h.
		return outputPCMTestCmd(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		outputUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown output subcommand: %s\n\n", args[0])
		outputUsage(stderr)
		return 2
	}
}

func outputUsage(w io.Writer) {
	fmt.Fprint(w, `bridge output <subcommand>

Subcommands:
  dac      Report this box's ALSA sound cards: each card's USB id and the
           formats its stream offers, and for a playback substream that is
           open, what the DAC receives (format, rate, channels, the running
           altset, the measured rate) and the thread and process holding it.
           Linux only; reads /proc and writes nothing.
  dac inspect
           Each card's unit (vendor and product id, the serial's last four
           characters, the anchored port) and how the player's binding,
           output.player.device, resolves; --verify-card N runs the check the
           player makes after opening a DAC against card N, opening nothing.
  dac bind <card> --binding serial|uniqueModel|port
           Write that card's unit, or with port the port it is in, into the
           config as the player's binding, the serial into the file alone;
           refused when it would be ambiguous, or while a bridge runs on the
           data dir. With the player enabled, the first bind also writes the
           player's UUID file, <dataDir>/player-uuid.
  player   Report this box's player (Volumio, moOde or MPD) and how MPD is
           set: its version, music directory, outputs, replay gain,
           normalization, resampler and filters. Read-only. Linux only;
           --root, --conf and --mpd name a tree, a file or an endpoint.

Run "bridge output <subcommand> -h" for subcommand-specific flags.
`)
}

// outputWatchInterval is how often --watch reads the cards again.
const outputWatchInterval = time.Second

// outputDACEnv is what `bridge output dac` takes from the host, so a test
// can stand in for each: the OS it refuses off Linux, the clock it stamps
// a reading with, and the ticks --watch polls on.
type outputDACEnv struct {
	goos  string
	now   func() time.Time
	ticks func() (<-chan time.Time, func())
}

// outputDACCmd implements `bridge output dac` on this host, and its
// player-binding verbs, inspect and bind (output_dac_bind.go).
func outputDACCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if isOutputDACBinding(args) {
		return outputDACBindingCmd(ctx, args, stdout, stderr)
	}
	return runOutputDAC(ctx, args, outputDACEnv{
		goos: runtime.GOOS,
		now:  time.Now,
		ticks: func() (<-chan time.Time, func()) {
			t := time.NewTicker(outputWatchInterval)
			return t.C, t.Stop
		},
	}, stdout, stderr)
}

// runOutputDAC is `bridge output dac`.
//
// Exit codes: 0 the reading was printed (or --watch was stopped), 1 there
// is nothing to read (not Linux, or no /proc/asound/cards), 2 usage error.
// A card file that cannot be read does not change the exit code: it is a
// problem on that card, printed with the rest of the reading.
func runOutputDAC(ctx context.Context, args []string, env outputDACEnv, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("output dac", flag.ContinueOnError)
	fs.SetOutput(stderr)
	watch := fs.Bool("watch", false, "read again about once a second and print the reading only when it changes (Ctrl-C to stop)")
	asJSON := fs.Bool("json", false, "print the reading as JSON")
	raw := fs.Bool("raw", false, "print every file the reading reads, verbatim, each under a \"==> path <==\" line (to capture a box as test fixtures)")
	procRoot := fs.String("proc-root", "",
		"read the /proc tree at this directory instead of the host's (default: /proc); its asound/ directory is the ALSA tree, "+
			"so a tree captured on another box reads anywhere")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "output dac: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *raw && (*watch || *asJSON) {
		fmt.Fprintln(stderr, "output dac: --raw prints the files themselves, and takes neither --watch nor --json")
		return 2
	}
	root := *procRoot
	if root == "" {
		if env.goos != "linux" {
			fmt.Fprintf(stderr, "output dac: ALSA is Linux only, and this is %s\n", env.goos)
			fmt.Fprintln(stderr, "  --proc-root reads a /proc tree captured on a Linux box.")
			return 1
		}
		root = hostTrees.proc
	}
	r := alsa.Reader{ProcRoot: root}
	switch {
	case *raw:
		return printRawDAC(r, stdout, stderr)
	case *watch:
		return watchDAC(ctx, r.Read, env, *asJSON, stdout, stderr)
	}
	snap, err := r.Read()
	if err != nil {
		reportDACReadError(stderr, err)
		return 1
	}
	return printDAC(stdout, stderr, dacReport{ReadAt: stampOf(env.now()), Snapshot: snap}, *asJSON)
}

// dacReport is the --json shape: one reading and when it was taken. It is
// a diagnostic shape, not a wire contract, and it is STABLE: every key is
// always present, a value that could not be read is null, and a list is
// [] rather than absent, so a script reads the same keys whatever the box
// is doing.
type dacReport struct {
	ReadAt time.Time `json:"readAt"`
	alsa.Snapshot
}

// stampOf is a reading's time as --json and --watch print it: UTC, to the
// millisecond.
func stampOf(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// reportDACReadError explains a reading that could not start.
func reportDACReadError(stderr io.Writer, err error) {
	if errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(stderr, "output dac: no ALSA sound cards here: %v\n", err)
		fmt.Fprintln(stderr, "  No sound driver is loaded, or the tree read is not a Linux /proc.")
		return
	}
	fmt.Fprintf(stderr, "output dac: %v\n", err)
}

// printDAC prints one reading, as JSON or for a person.
func printDAC(stdout, stderr io.Writer, rep dacReport, asJSON bool) int {
	if asJSON {
		return writeJSONIndent(stdout, stderr, "output dac", rep)
	}
	renderDAC(stdout, rep.Snapshot)
	return 0
}

// watchDAC is --watch: it takes a reading with read on every tick and
// prints one only when it differs from the last one printed, compared
// without the values that move on every read while audio plays
// (steadyView). A reading that cannot be taken is reported on stderr the
// same way, once per change, and the watch goes on; it ends, with 0, when
// ctx does (Ctrl-C).
func watchDAC(ctx context.Context, read func() (alsa.Snapshot, error), env outputDACEnv, asJSON bool, stdout, stderr io.Writer) int {
	ticks, stop := env.ticks()
	defer stop()
	var last string
	for first := true; ; first = false {
		snap, err := read()
		sig := watchSignature(snap, err)
		if first || sig != last {
			last = sig
			at := stampOf(env.now())
			switch {
			case err != nil:
				fmt.Fprintf(stderr, "%s ", at.Format(time.RFC3339Nano))
				reportDACReadError(stderr, err)
			case asJSON:
				if code := writeJSONIndent(stdout, stderr, "output dac", dacReport{ReadAt: at, Snapshot: snap}); code != 0 {
					return code
				}
			default:
				fmt.Fprintf(stdout, "--- %s\n", at.Format(time.RFC3339Nano))
				renderDAC(stdout, snap)
			}
		}
		select {
		case <-ctx.Done():
			return 0
		case <-ticks:
		}
	}
}

// watchSignature is what --watch compares one reading to the next by.
func watchSignature(snap alsa.Snapshot, err error) string {
	if err != nil {
		return "error: " + err.Error()
	}
	b, merr := json.Marshal(steadyView(snap))
	if merr != nil {
		return "unmarshalable: " + merr.Error()
	}
	return string(b)
}

// steadyView is a copy of a reading without what moves on every read while
// audio plays: a substream's buffer pointers, delay, avail and timestamps,
// and a running endpoint's measured rate and packet size. Compared with
// them, --watch would print once a second for as long as anything played;
// what it exists to show is a change of card, format, rate, altset, state
// or owner, and every one of those is still in the copy.
func steadyView(snap alsa.Snapshot) alsa.Snapshot {
	out := snap
	out.Cards = make([]alsa.CardReading, len(snap.Cards))
	for i, c := range snap.Cards {
		c.Devices = append([]alsa.Device(nil), c.Devices...)
		for j, d := range c.Devices {
			if d.Stream != nil {
				st := *d.Stream
				st.Playback, st.Capture = steadyDirection(st.Playback), steadyDirection(st.Capture)
				d.Stream = &st
			}
			d.Substreams = append([]alsa.Substream(nil), d.Substreams...)
			for k, s := range d.Substreams {
				if s.Status != nil {
					st := *s.Status
					st.TriggerTime, st.Timestamp = "", ""
					st.Delay, st.Avail, st.AvailMax, st.HWPtr, st.ApplPtr = nil, nil, nil, nil, nil
					s.Status = &st
				}
				d.Substreams[k] = s
			}
			c.Devices[j] = d
		}
		out.Cards[i] = c
	}
	return out
}

// steadyDirection is steadyView for one direction of a stream file.
func steadyDirection(d *alsa.StreamDirection) *alsa.StreamDirection {
	if d == nil || d.Running == nil {
		return d
	}
	cp := *d
	run := *d.Running
	run.PacketSize, run.MomentaryFreqHz, run.MomentaryFreq = nil, nil, ""
	cp.Running = &run
	return &cp
}

// printRawDAC is --raw: every file a reading reads, verbatim, each under
// a "==> path <==" line naming where it sits in a /proc tree, so the dump
// can be laid out as a tree that --proc-root reads back. A file that does
// not end in a newline gets one, and a line saying so, so the next header
// stays on its own line and the cut is visible.
func printRawDAC(r alsa.Reader, stdout, stderr io.Writer) int {
	files, err := r.Raw()
	if err != nil {
		reportDACReadError(stderr, err)
		return 1
	}
	for _, f := range files {
		fmt.Fprintf(stdout, "==> %s <==\n", f.Path)
		if f.Err != nil {
			fmt.Fprintf(stdout, "(unreadable: %v)\n", f.Err)
			continue
		}
		_, _ = stdout.Write(f.Data)
		if len(f.Data) > 0 && f.Data[len(f.Data)-1] != '\n' {
			fmt.Fprint(stdout, "\n\\ No newline at end of file\n")
		}
	}
	return 0
}

// renderDAC prints a reading for a person.
func renderDAC(w io.Writer, snap alsa.Snapshot) {
	for _, p := range snap.Problems {
		fmt.Fprintf(w, "problem: %s\n", p)
	}
	if len(snap.Cards) == 0 {
		fmt.Fprintf(w, "no sound cards: %s lists none\n", filepath.Join(snap.AsoundRoot, "cards"))
		return
	}
	for i, c := range snap.Cards {
		if i > 0 {
			fmt.Fprintln(w)
		}
		renderCard(w, c)
	}
}

// renderCard prints one card.
func renderCard(w io.Writer, c alsa.CardReading) {
	fmt.Fprintf(w, "card %d: %s [%s]\n", c.Index, c.ID, c.Name)
	fmt.Fprintf(w, "  driver: %s\n", c.Driver)
	if c.USBID != nil {
		fmt.Fprintf(w, "  usb id: %s\n", c.USBID)
	}
	if len(c.Devices) == 0 {
		fmt.Fprintln(w, "  no playback device")
	}
	for _, d := range c.Devices {
		renderDevice(w, d)
	}
	if len(c.Problems) > 0 {
		fmt.Fprintln(w, "  problems:")
		for _, p := range c.Problems {
			fmt.Fprintf(w, "    %s\n", p)
		}
	}
}

// renderDevice prints one PCM device: its stream's altsets and endpoint
// status, then each substream.
func renderDevice(w io.Writer, d alsa.Device) {
	var play *alsa.StreamDirection
	if d.Stream != nil {
		play = d.Stream.Playback
	}
	if play == nil {
		fmt.Fprintf(w, "  device %d:\n", d.Device)
	} else {
		status := play.Status
		if status == "" {
			status = "status unknown"
		}
		fmt.Fprintf(w, "  device %d: stream%d %s\n", d.Device, d.Device, status)
		for _, a := range play.Altsets {
			fmt.Fprintf(w, "    %s: %s\n", altsetName(a), altsetSummary(a))
		}
	}
	if len(d.Substreams) == 0 {
		fmt.Fprintln(w, "    no playback substream")
	}
	for _, s := range d.Substreams {
		renderSubstream(w, s, play)
	}
}

// renderSubstream prints one substream: closed, or the live block.
func renderSubstream(w io.Writer, s alsa.Substream, play *alsa.StreamDirection) {
	st, hw := s.Status, s.HWParams
	switch {
	case st != nil && st.Closed:
		fmt.Fprintf(w, "    sub %d: closed\n", s.Subdevice)
		return
	case st != nil && st.Error != nil:
		fmt.Fprintf(w, "    sub %d: status error %d\n", s.Subdevice, *st.Error)
		return
	case st != nil && st.State != "":
		fmt.Fprintf(w, "    sub %d: %s\n", s.Subdevice, st.State)
	default:
		fmt.Fprintf(w, "    sub %d: state unknown\n", s.Subdevice)
	}
	switch {
	case hw == nil:
		fmt.Fprintln(w, "      format:         unknown (no hw_params)")
	case hw.Closed:
		fmt.Fprintln(w, "      format:         none (hw_params says closed)")
	case hw.NoSetup:
		fmt.Fprintln(w, "      format:         not set yet (hw_params says no setup)")
	default:
		fmt.Fprintf(w, "      format:         %s, %s ch, %s Hz\n", orUnknown(hw.Format), intOr(hw.Channels, "?"), intOr(hw.Rate, "?"))
	}
	if play != nil && play.Running != nil {
		run := play.Running
		alt := "unknown"
		if run.Interface != nil || run.Altset != nil {
			alt = "interface " + intOr(run.Interface, "?") + " altset " + intOr(run.Altset, "?")
			if a, ok := findAltset(play.Altsets, run); ok {
				alt += " (" + strings.Join(a.Formats, " ") + ", " + bitsPhrase(a.Bits) + ")"
			}
		}
		fmt.Fprintf(w, "      running altset: %s\n", alt)
		freq := "unknown"
		if run.MomentaryFreqHz != nil {
			freq = strconv.FormatInt(*run.MomentaryFreqHz, 10) + " Hz"
			if run.MomentaryFreq != "" {
				freq += " (" + run.MomentaryFreq + ")"
			}
		}
		fmt.Fprintf(w, "      momentary freq: %s\n", freq)
	}
	if s.Owner != nil {
		fmt.Fprintf(w, "      owner:          %s\n", ownerSummary(*s.Owner))
	}
}

// altsetName is "interface 1 altset 4", with "?" for a number the stream
// file did not give.
func altsetName(a alsa.Altset) string {
	return "interface " + intOr(a.Interface, "?") + " altset " + intOr(a.Altset, "?")
}

// altsetSummary is an altset's formats, bits, channels, rates and DSD
// framing on one line.
func altsetSummary(a alsa.Altset) string {
	parts := []string{orUnknown(strings.Join(a.Formats, " ")), bitsPhrase(a.Bits), intOr(a.Channels, "?") + " ch"}
	switch {
	case a.RateRange != nil:
		parts = append(parts, fmt.Sprintf("%d - %d Hz", a.RateRange.Min, a.RateRange.Max))
	case len(a.Rates) > 0:
		rates := make([]string, len(a.Rates))
		for i, r := range a.Rates {
			rates[i] = strconv.Itoa(r)
		}
		parts = append(parts, strings.Join(rates, " ")+" Hz")
	default:
		parts = append(parts, "no rates")
	}
	if a.DSDRaw != nil {
		parts = append(parts, "DSD raw (DOP="+flagOr(a.DSDRaw.DOP)+", bitrev="+flagOr(a.DSDRaw.Bitrev)+")")
	}
	return strings.Join(parts, ", ")
}

// findAltset finds the altset a running endpoint names.
func findAltset(altsets []alsa.Altset, run *alsa.RunningEndpoint) (alsa.Altset, bool) {
	if run.Interface == nil || run.Altset == nil {
		return alsa.Altset{}, false
	}
	for _, a := range altsets {
		if a.Interface != nil && a.Altset != nil && *a.Interface == *run.Interface && *a.Altset == *run.Altset {
			return a, true
		}
	}
	return alsa.Altset{}, false
}

// ownerSummary names a substream's owner: thread, then process, each
// "unknown" where /proc could not say.
func ownerSummary(o alsa.Owner) string {
	if o.TID <= 0 {
		return fmt.Sprintf("unknown (owner_pid %d: the owner is not visible from this pid namespace)", o.TID)
	}
	thread := fmt.Sprintf("thread %d ", o.TID) + nameOr(o.Thread)
	process := "process unknown"
	if o.PID != nil {
		process = fmt.Sprintf("process %d ", *o.PID) + nameOr(o.Process)
	}
	return thread + ", " + process
}

// nameOr quotes a task name, or says it is unknown.
func nameOr(s *string) string {
	if s == nil {
		return "(name unknown)"
	}
	return strconv.Quote(*s)
}

// bitsPhrase is an altset's bit depth. A stream with no Bits line — every
// USB device on a kernel before 5.1 — reads "unknown", not a guessed width.
func bitsPhrase(n *int) string {
	if n == nil {
		return "unknown"
	}
	return strconv.Itoa(*n) + " bit"
}

// intOr renders an optional number.
func intOr(n *int, unknown string) string {
	if n == nil {
		return unknown
	}
	return strconv.Itoa(*n)
}

// flagOr renders an optional 0/1 flag the way the kernel prints it.
func flagOr(b *bool) string {
	switch {
	case b == nil:
		return "?"
	case *b:
		return "1"
	default:
		return "0"
	}
}

// orUnknown renders an optional string.
func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
