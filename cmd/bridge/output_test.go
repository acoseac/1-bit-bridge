package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
)

// outputFixtureRoot is a test case's /proc tree in internal/alsa's
// testdata: the trees captured on the Raspberry Pi 5 with the iFi GO bar,
// and the synthetic ones (internal/alsa/testdata/README.md).
func outputFixtureRoot(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(repoRootForCitations(t), "internal", "alsa", "testdata", name, "proc")
}

// outputTestNow is the clock the env-driven tests read.
var outputTestNow = time.Date(2026, 10, 19, 10, 12, 0, 123456789, time.UTC)

// testOutputEnv is an outputDACEnv on the given OS whose clock reads
// outputTestNow and whose --watch polls on ticks.
func testOutputEnv(goos string, ticks chan time.Time) outputDACEnv {
	return outputDACEnv{
		goos:  goos,
		now:   func() time.Time { return outputTestNow },
		ticks: func() (<-chan time.Time, func()) { return ticks, func() {} },
	}
}

// wantDSD256Text is `bridge output dac` on the capture of the Pi playing
// native DSD256: the HDMI cards (whose directories were not captured), and
// the iFi with its four altsets, the running one, the measured rate and
// MPD's output thread holding the substream.
const wantDSD256Text = `card 0: vc4hdmi0 [vc4-hdmi-0]
  driver: vc4-hdmi
  no playback device

card 1: vc4hdmi1 [vc4-hdmi-1]
  driver: vc4-hdmi
  no playback device

card 5: Audio [iFi (by AMR) HD USB Audio]
  driver: USB-Audio
  usb id: 20b1:3009
  device 0: stream0 Running
    interface 1 altset 1: S32_LE, 32 bit, 2 ch, 44100 48000 88200 96000 176400 192000 352800 384000 Hz
    interface 1 altset 2: S24_3LE, 24 bit, 2 ch, 44100 48000 88200 96000 176400 192000 352800 384000 Hz
    interface 1 altset 3: S16_LE, 16 bit, 2 ch, 44100 48000 88200 96000 176400 192000 352800 384000 Hz
    interface 1 altset 4: SPECIAL DSD_U32_BE, 32 bit, 2 ch, 44100 48000 88200 96000 176400 192000 352800 384000 Hz, DSD raw (DOP=0, bitrev=0)
    sub 0: RUNNING
      format:         DSD_U32_BE, 2 ch, 352800 Hz
      running altset: interface 1 altset 4 (SPECIAL DSD_U32_BE, 32 bit)
      momentary freq: 355053 Hz (0x2c.61b2)
      owner:          thread 6584 "output:alsa", process 6551 "mpd"
`

// wantIdleText is the same box with the DAC stopped.
const wantIdleText = `card 0: vc4hdmi0 [vc4-hdmi-0]
  driver: vc4-hdmi
  no playback device

card 1: vc4hdmi1 [vc4-hdmi-1]
  driver: vc4-hdmi
  no playback device

card 5: Audio [iFi (by AMR) HD USB Audio]
  driver: USB-Audio
  usb id: 20b1:3009
  device 0: stream0 Stop
    interface 1 altset 1: S32_LE, 32 bit, 2 ch, 44100 48000 88200 96000 176400 192000 352800 384000 Hz
    interface 1 altset 2: S24_3LE, 24 bit, 2 ch, 44100 48000 88200 96000 176400 192000 352800 384000 Hz
    interface 1 altset 3: S16_LE, 16 bit, 2 ch, 44100 48000 88200 96000 176400 192000 352800 384000 Hz
    interface 1 altset 4: SPECIAL DSD_U32_BE, 32 bit, 2 ch, 44100 48000 88200 96000 176400 192000 352800 384000 Hz, DSD raw (DOP=0, bitrev=0)
    sub 0: closed
`

// TestOutputDACPrintsTheCapturedReadings drives `bridge output dac`
// through run(), the real entry point, on the stopped and the DSD256
// captures. --proc-root names a tree, so it reads on every OS.
func TestOutputDACPrintsTheCapturedReadings(t *testing.T) {
	for name, want := range map[string]string{"dsd256-native": wantDSD256Text, "idle": wantIdleText} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := runCapture(t, "output", "dac", "--proc-root", outputFixtureRoot(t, name))
			if code != 0 || stderr != "" {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			if stdout != want {
				t.Errorf("output:\n%s\nwant:\n%s", stdout, want)
			}
		})
	}
}

// TestOutputDACPrintsWhatTheSyntheticTreesShow reads the trees made up
// for what the Pi did not show: cards with no stream file, and a stream
// file that is garbled, whose problems are printed under its card and
// whose substream still reads.
func TestOutputDACPrintsWhatTheSyntheticTreesShow(t *testing.T) {
	stdout, stderr, code := runCapture(t, "output", "dac", "--proc-root", outputFixtureRoot(t, "i2s-and-hdmi"))
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, line := range []string{
		"card 1: sndrpihifiberry [snd_rpi_hifiberry_dacplus]\n  driver: HifiberryDacp\n  device 0:\n    sub 0: RUNNING\n",
		"      format:         S32_LE, 2 ch, 192000 Hz\n      owner:          thread 812 \"aplay\", process 812 \"aplay\"\n",
		"card 0: vc4hdmi0 [vc4-hdmi-0]\n  driver: vc4-hdmi\n  device 0:\n    sub 0: closed\n",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("output lacks %q:\n%s", line, stdout)
		}
	}

	stdout, stderr, code = runCapture(t, "output", "dac", "--proc-root", outputFixtureRoot(t, "garbled-stream0"))
	if code != 0 || stderr != "" {
		t.Fatalf("garbled: exit %d, stderr %q; a problem on a card is not a failed reading", code, stderr)
	}
	for _, line := range []string{
		"    interface 1 altset 2: SPECIAL DSD_U32_BE, unknown, 2 ch, 44100 48000 88200 96000 176400 192000 352800 384000 Hz, DSD raw (DOP=0, bitrev=?)\n",
		"      running altset: interface 1 altset ?\n      momentary freq: unknown\n",
		"      owner:          thread 4242 (name unknown), process unknown\n",
		"  problems:\n    card0/stream0: line 6: Altset \"two\" is not a number\n",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("garbled output lacks %q:\n%s", line, stdout)
		}
	}
	if strings.Contains(stdout, "no Bits line") {
		t.Errorf("a missing Bits line was called cut short:\n%s", stdout)
	}
}

// TestOutputDACPrintsEveryFormatAndAnUnknownBitDepth: one Format line is
// the whole list, and a stream from before Linux 5.1 has no Bits line.
func TestOutputDACPrintsEveryFormatAndAnUnknownBitDepth(t *testing.T) {
	stdout, stderr, code := runCapture(t, "output", "dac", "--proc-root", outputFixtureRoot(t, "multi-format"))
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, line := range []string{
		"    interface 1 altset 1: S16_LE S24_3LE, 24 bit, 2 ch, 44100 48000 96000 Hz\n",
		"    interface 1 altset 2: SPECIAL DSD_U32_BE, 32 bit, 2 ch, 352800 Hz, DSD raw (DOP=0, bitrev=0)\n",
		"    interface 1 altset 3: S32_LE DSD_U32_LE, 32 bit, 2 ch, 44100 Hz\n",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("output lacks %q:\n%s", line, stdout)
		}
	}

	stdout, stderr, code = runCapture(t, "output", "dac", "--json", "--proc-root", outputFixtureRoot(t, "multi-format"))
	if code != 0 || stderr != "" {
		t.Fatalf("json: exit %d, stderr %q", code, stderr)
	}
	var rep dacReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	alts := rep.Cards[0].Devices[0].Stream.Playback.Altsets
	if len(alts) != 3 || alts[1].DSDFormat() != "DSD_U32_BE" || alts[2].DSDFormat() != "DSD_U32_LE" || alts[0].DSDFormat() != "" {
		t.Fatalf("json altsets = %+v", alts)
	}

	stdout, stderr, code = runCapture(t, "output", "dac", "--proc-root", outputFixtureRoot(t, "uac1-bits-less"))
	if code != 0 || stderr != "" {
		t.Fatalf("bits-less: exit %d, stderr %q", code, stderr)
	}
	for _, line := range []string{
		"    interface 1 altset 1: S16_LE, unknown, 2 ch, 44100 48000 Hz\n",
		"    interface 1 altset 2: S24_3LE, unknown, 2 ch, 44100 48000 96000 Hz\n",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("bits-less output lacks %q:\n%s", line, stdout)
		}
	}
	if strings.Contains(stdout, "cut short") {
		t.Errorf("a pre-5.1 stream was called cut short:\n%s", stdout)
	}
}

// jsonShape walks a decoded JSON document and records, for each kind of
// object (its path with array indices dropped), the keys it carries.
func jsonShape(v any, path string, into map[string][]string) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		into[path] = keys
		for _, k := range keys {
			jsonShape(x[k], path+"."+k, into)
		}
	case []any:
		for _, e := range x {
			jsonShape(e, path+"[]", into)
		}
	}
}

// wantDACJSONShape is the --json shape: every kind of object the DSD256
// capture's reading holds, with its keys.
var wantDACJSONShape = map[string][]string{
	"":                                   {"asoundRoot", "cards", "problems", "procRoot", "readAt"},
	".cards[]":                           {"devices", "driver", "id", "index", "longName", "name", "problems", "usbId"},
	".cards[].devices[]":                 {"device", "stream", "substreams"},
	".cards[].devices[].stream":          {"capture", "playback"},
	".cards[].devices[].stream.playback": {"altsets", "running", "status"},
	".cards[].devices[].stream.playback.running":          {"altset", "feedbackFormat", "interface", "momentaryFreq", "momentaryFreqHz", "packetSize"},
	".cards[].devices[].stream.playback.altsets[]":        {"altset", "bits", "channelMap", "channels", "dsdRaw", "formats", "interface", "rateRange", "rates"},
	".cards[].devices[].stream.playback.altsets[].dsdRaw": {"bitrev", "dop"},
	".cards[].devices[].substreams[]":                     {"hwParams", "owner", "status", "subdevice"},
	".cards[].devices[].substreams[].hwParams": {"access", "bufferSize", "channels", "closed", "format", "noSetup", "periodSize",
		"rate", "rateDen", "rateNum", "subformat"},
	".cards[].devices[].substreams[].status": {"applPtr", "avail", "availMax", "closed", "delay", "error", "hwPtr", "ownerPid",
		"state", "triggerTime", "tstamp"},
	".cards[].devices[].substreams[].owner": {"pid", "process", "thread", "tid"},
}

// nullList matches a list-valued key printed as null.
var nullList = regexp.MustCompile(`"(cards|problems|devices|substreams|altsets|formats|rates|channelMap)": null`)

// TestOutputDACJSONHasOneShapeInEveryState pins --json's promise that the
// shape does not depend on what the box is doing: every kind of object
// carries the same keys whether the DAC plays, stops or reads garbled, a
// value that is not there is null, and a list is [] (an object kind the
// idle reading lacks is one whose parent is null there).
func TestOutputDACJSONHasOneShapeInEveryState(t *testing.T) {
	for _, name := range []string{"dsd256-native", "idle", "pcm-44k1-16", "i2s-and-hdmi", "garbled-stream0"} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := runCapture(t, "output", "dac", "--json", "--proc-root", outputFixtureRoot(t, name))
			if code != 0 || stderr != "" {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			var doc any
			if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
				t.Fatalf("not JSON: %v\n%s", err, stdout)
			}
			shape := map[string][]string{}
			jsonShape(doc, "", shape)
			for path, keys := range shape {
				if want, ok := wantDACJSONShape[path]; !ok || !reflect.DeepEqual(keys, want) {
					t.Errorf("%q has keys %q, want %q", path, keys, want)
				}
			}
			if name == "dsd256-native" && len(shape) != len(wantDACJSONShape) {
				t.Errorf("the DSD256 reading has %d kinds of object, want all %d", len(shape), len(wantDACJSONShape))
			}
			if m := nullList.FindString(stdout); m != "" {
				t.Errorf("a list printed as null (%s); an empty list is []", m)
			}
		})
	}
}

// TestOutputDACJSONOnTheCapturedDSD256Tree reads the values a later step
// will hand the app: what the DAC receives and who sends it.
func TestOutputDACJSONOnTheCapturedDSD256Tree(t *testing.T) {
	stdout, _, code := runCapture(t, "output", "dac", "--json", "--proc-root", outputFixtureRoot(t, "dsd256-native"))
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var rep struct {
		ReadAt string `json:"readAt"`
		Cards  []struct {
			Index   int     `json:"index"`
			USBID   *string `json:"usbId"`
			Devices []struct {
				Stream struct {
					Playback struct {
						Running struct {
							Altset          *int   `json:"altset"`
							MomentaryFreqHz *int64 `json:"momentaryFreqHz"`
						} `json:"running"`
					} `json:"playback"`
				} `json:"stream"`
				Substreams []struct {
					HWParams struct {
						Format   string `json:"format"`
						Rate     *int   `json:"rate"`
						Channels *int   `json:"channels"`
					} `json:"hwParams"`
					Status struct {
						State string `json:"state"`
					} `json:"status"`
					Owner struct {
						TID     int     `json:"tid"`
						Thread  *string `json:"thread"`
						PID     *int    `json:"pid"`
						Process *string `json:"process"`
					} `json:"owner"`
				} `json:"substreams"`
			} `json:"devices"`
		} `json:"cards"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339Nano, rep.ReadAt)
	if err != nil || !strings.HasSuffix(rep.ReadAt, "Z") || at.Nanosecond()%int(time.Millisecond) != 0 {
		t.Errorf("readAt %q is not a UTC RFC 3339 time to the millisecond (%v)", rep.ReadAt, err)
	}
	if len(rep.Cards) != 3 || rep.Cards[2].Index != 5 || rep.Cards[2].USBID == nil || *rep.Cards[2].USBID != "20b1:3009" {
		t.Fatalf("cards = %+v", rep.Cards)
	}
	d := rep.Cards[2].Devices[0]
	run, sub := d.Stream.Playback.Running, d.Substreams[0]
	if run.Altset == nil || *run.Altset != 4 || run.MomentaryFreqHz == nil || *run.MomentaryFreqHz != 355053 {
		t.Errorf("running = %+v; want altset 4 at 355053 Hz", run)
	}
	if sub.HWParams.Format != "DSD_U32_BE" || sub.HWParams.Rate == nil || *sub.HWParams.Rate != 352800 ||
		sub.HWParams.Channels == nil || *sub.HWParams.Channels != 2 || sub.Status.State != "RUNNING" {
		t.Errorf("substream = %+v", sub)
	}
	o := sub.Owner
	if o.TID != 6584 || o.Thread == nil || *o.Thread != "output:alsa" || o.PID == nil || *o.PID != 6551 || o.Process == nil || *o.Process != "mpd" {
		t.Errorf("owner = %+v", o)
	}
}

// TestOutputDACRefusesTheHostsTreeOffLinux: without --proc-root the
// command reads this host's /proc, which only Linux has, so elsewhere it
// says so and exits 1 before reading anything; with --proc-root it reads
// the tree on any OS. On Linux, a tree without asound/cards is a host with
// no ALSA, and exits 1 too.
func TestOutputDACRefusesTheHostsTreeOffLinux(t *testing.T) {
	for _, goos := range []string{"darwin", "windows"} {
		var stdout, stderr bytes.Buffer
		code := runOutputDAC(context.Background(), nil, testOutputEnv(goos, nil), &stdout, &stderr)
		if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "ALSA is Linux only, and this is "+goos) {
			t.Errorf("%s: exit %d, stdout %q, stderr %q; want 1 and the refusal", goos, code, stdout.String(), stderr.String())
		}
		stdout.Reset()
		stderr.Reset()
		code = runOutputDAC(context.Background(), []string{"--proc-root", outputFixtureRoot(t, "idle")}, testOutputEnv(goos, nil), &stdout, &stderr)
		if code != 0 || stdout.String() != wantIdleText {
			t.Errorf("%s with --proc-root: exit %d, stderr %q", goos, code, stderr.String())
		}
	}
	var stdout, stderr bytes.Buffer
	empty := t.TempDir()
	code := runOutputDAC(context.Background(), []string{"--proc-root", empty}, testOutputEnv("linux", nil), &stdout, &stderr)
	cards := filepath.Join(empty, "asound", "cards")
	if code != 1 || !strings.Contains(stderr.String(), "no ALSA sound cards here: read "+cards+": ") ||
		strings.Count(stderr.String(), cards) != 1 {
		t.Errorf("a tree with no cards file: exit %d, stderr %q; want 1, naming %s once", code, stderr.String(), cards)
	}
}

// TestOutputCommandUsage covers the usage errors, which come before
// anything is read and so behave alike on every host, and the help.
func TestOutputCommandUsage(t *testing.T) {
	tests := []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"output"}, 2, "bridge output <subcommand>"},
		{[]string{"output", "frob"}, 2, "unknown output subcommand: frob"},
		{[]string{"output", "dac", "5"}, 2, `output dac: unexpected argument "5"`},
		{[]string{"output", "dac", "--raw", "--json"}, 2, "takes neither --watch nor --json"},
		{[]string{"output", "dac", "--raw", "--watch"}, 2, "takes neither --watch nor --json"},
		{[]string{"output", "dac", "--bogus"}, 2, "flag provided but not defined: -bogus"},
	}
	for _, tc := range tests {
		stdout, stderr, code := runCapture(t, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.stderr) || stdout != "" {
			t.Errorf("%q: exit %d, stdout %q, stderr %q; want %d and %q", tc.args, code, stdout, stderr, tc.code, tc.stderr)
		}
	}
	stdout, _, code := runCapture(t, "output", "help")
	if code != 0 || !strings.Contains(stdout, "  dac ") {
		t.Errorf("output help: exit %d, %q", code, stdout)
	}
	if !strings.Contains(stdout, "With the\n           player enabled, the first bind also writes the player's UUID\n           file, <dataDir>/player-uuid.") {
		t.Errorf("output help does not say dac bind writes the player's UUID file:\n%s", stdout)
	}
	var b bytes.Buffer
	usage(&b)
	if !strings.Contains(b.String(), "\n  output   This box as an audio output: bridge output dac") {
		t.Errorf("bridge's usage does not list output:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "Linux only; only dac bind changes anything: the config file, and\n"+
		"           the player's UUID file the first time it binds an enabled player.") {
		t.Errorf("usage does not say which output command writes, and what:\n%s", b.String())
	}
}

// rawHeader is a --raw dump's line naming the file that follows.
var rawHeader = regexp.MustCompile(`^==> (.+) <==$`)

// layOutRawDump writes a --raw dump out as the /proc tree it came from,
// honouring the marker of a file that had no newline at its end.
func layOutRawDump(t *testing.T, dump, root string) {
	t.Helper()
	files := map[string]*strings.Builder{}
	var cur *strings.Builder
	for _, line := range strings.SplitAfter(dump, "\n") {
		if line == "" {
			continue
		}
		if m := rawHeader.FindStringSubmatch(strings.TrimSuffix(line, "\n")); m != nil {
			cur = &strings.Builder{}
			files[m[1]] = cur
			continue
		}
		if cur == nil {
			t.Fatalf("dump text before any header: %q", line)
		}
		if line == "\\ No newline at end of file\n" {
			s := strings.TrimSuffix(cur.String(), "\n")
			cur.Reset()
			cur.WriteString(s)
			continue
		}
		cur.WriteString(line)
	}
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// readAtLine is the one line of --json output that depends on the clock.
var readAtLine = regexp.MustCompile(`"readAt": "[^"]*"`)

// normalizedDACJSON is --json output with its clock and its roots
// replaced, so two readings of different trees compare by what they read.
func normalizedDACJSON(t *testing.T, out, root string) string {
	t.Helper()
	for _, r := range []string{filepath.Join(root, "asound"), root} {
		quoted, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		out = strings.ReplaceAll(out, string(quoted), `"ROOT"`)
	}
	return readAtLine.ReplaceAllString(out, `"readAt": "NOW"`)
}

// TestOutputDACRawDumpReadsBackAsTheSameReading: --raw exists to capture a
// box as fixtures, so a dump laid out as a tree must read back, through
// --proc-root, as the reading of the tree it was taken from. The garbled
// tree's stream0 ends without a newline, which the dump marks and the
// layout restores.
func TestOutputDACRawDumpReadsBackAsTheSameReading(t *testing.T) {
	for _, name := range []string{"dsd256-native", "garbled-stream0", "i2s-and-hdmi"} {
		t.Run(name, func(t *testing.T) {
			src := outputFixtureRoot(t, name)
			dump, stderr, code := runCapture(t, "output", "dac", "--raw", "--proc-root", src)
			if code != 0 || stderr != "" {
				t.Fatalf("--raw: exit %d, stderr %q", code, stderr)
			}
			copied := t.TempDir()
			layOutRawDump(t, dump, copied)
			// Byte for byte: a parse forgives a trailing newline, a fixture
			// must not gain one.
			laidOut := 0
			err := filepath.WalkDir(copied, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return err
				}
				rel, err := filepath.Rel(copied, path)
				if err != nil {
					return err
				}
				got, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				orig, err := os.ReadFile(filepath.Join(src, rel))
				if err != nil {
					return err
				}
				if !bytes.Equal(got, orig) {
					t.Errorf("%s: the dump lays out %q, the tree holds %q", filepath.ToSlash(rel), got, orig)
				}
				laidOut++
				return nil
			})
			if err != nil || laidOut == 0 {
				t.Fatalf("walking the laid-out dump: %v (%d files)", err, laidOut)
			}
			want, _, _ := runCapture(t, "output", "dac", "--json", "--proc-root", src)
			got, _, code := runCapture(t, "output", "dac", "--json", "--proc-root", copied)
			if code != 0 {
				t.Fatalf("reading the laid-out dump: exit %d", code)
			}
			if g, w := normalizedDACJSON(t, got, copied), normalizedDACJSON(t, want, src); g != w {
				t.Errorf("the dump reads back differently:\n%s\nwant\n%s", g, w)
			}
		})
	}
}

// TestOutputDACWatchPrintsOnlyOnAChange feeds --watch a scripted run of
// readings: the box stopped, then playing DSD256, then the same stream a
// second on (every counter and the measured rate moved), then 44.1/16, then
// two reads that fail alike, then 44.1/16 again. It prints the first
// reading, each change, and the failure once, and nothing for the second
// that only moved; it stops with 0 when its context ends.
func TestOutputDACWatchPrintsOnlyOnAChange(t *testing.T) {
	read := func(name string) alsa.Snapshot {
		snap, err := alsa.Reader{ProcRoot: outputFixtureRoot(t, name)}.Read()
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	moved := read("dsd256-native")
	sub := moved.Cards[2].Devices[0].Substreams[0].Status
	*sub.HWPtr, *sub.ApplPtr, *sub.Delay, *sub.Avail, *sub.AvailMax = 1, 2, 3, 4, 5
	sub.TriggerTime, sub.Timestamp = "3508.000000000", "1.000000000"
	run := moved.Cards[2].Devices[0].Stream.Playback.Running
	*run.MomentaryFreqHz, *run.PacketSize, run.MomentaryFreq = 352799, 396, "0x2c.1999"
	gone := errors.New("read /proc/asound/cards: no such file or directory")
	script := []struct {
		snap alsa.Snapshot
		err  error
	}{
		{read("idle"), nil}, {read("dsd256-native"), nil}, {moved, nil}, {read("pcm-44k1-16"), nil},
		{alsa.Snapshot{}, gone}, {alsa.Snapshot{}, gone}, {read("pcm-44k1-16"), nil},
	}
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[asJSON], func(t *testing.T) {
			next := 0
			readNext := func() (alsa.Snapshot, error) {
				s := script[next]
				next++
				return s.snap, s.err
			}
			ticks := make(chan time.Time)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var stdout, stderr bytes.Buffer
			// Buffered, so a test that fails before reading it leaves no
			// goroutine blocked on the send.
			done := make(chan int, 1)
			go func() { done <- watchDAC(ctx, readNext, testOutputEnv("linux", ticks), asJSON, &stdout, &stderr) }()
			// Each tick is taken only once the read before it has been
			// printed or skipped, so the script advances in step. A watch
			// that stops taking ticks or never returns fails this case
			// within the bound, not the whole binary at its deadline.
			const bound = 30 * time.Second
			for range script[1:] {
				select {
				case ticks <- time.Time{}:
				case <-time.After(bound):
					t.Fatalf("watch stopped taking ticks after %d reads", next)
				}
			}
			cancel()
			select {
			case code := <-done:
				if code != 0 {
					t.Errorf("watch exited %d, want 0 when its context ends", code)
				}
			case <-time.After(bound):
				t.Fatal("watch did not return after its context ended")
			}
			if next != len(script) {
				t.Fatalf("%d reads, want %d", next, len(script))
			}
			var formats []string
			if asJSON {
				dec := json.NewDecoder(&stdout)
				for {
					var rep dacReport
					if err := dec.Decode(&rep); errors.Is(err, io.EOF) {
						break
					} else if err != nil {
						t.Fatal(err)
					}
					if !rep.ReadAt.Equal(stampOf(outputTestNow)) {
						t.Errorf("readAt %v", rep.ReadAt)
					}
					hw := rep.Cards[2].Devices[0].Substreams[0].HWParams
					formats = append(formats, map[bool]string{true: "closed", false: hw.Format}[hw.Closed])
				}
			} else {
				out := stdout.String()
				if n := strings.Count(out, "--- 2026-10-19T10:12:00.123Z\n"); n != 4 {
					t.Errorf("%d stamped readings, want 4:\n%s", n, out)
				}
				for _, block := range strings.Split(out, "--- ")[1:] {
					f := "closed"
					for _, w := range []string{"DSD_U32_BE, 2 ch, 352800 Hz", "S16_LE, 2 ch, 44100 Hz"} {
						if strings.Contains(block, w) {
							f = strings.SplitN(w, ",", 2)[0]
						}
					}
					formats = append(formats, f)
				}
			}
			if want := []string{"closed", "DSD_U32_BE", "S16_LE", "S16_LE"}; !reflect.DeepEqual(formats, want) {
				t.Errorf("printed readings = %q, want %q", formats, want)
			}
			if n := strings.Count(stderr.String(), "no such file or directory"); n != 1 {
				t.Errorf("the failure was reported %d times, want once: %q", n, stderr.String())
			}
		})
	}
}

// TestSteadyViewLeavesTheReadingItCopies: --watch compares a copy with the
// moving values cleared, and the reading it then prints must still carry
// them.
func TestSteadyViewLeavesTheReadingItCopies(t *testing.T) {
	snap, err := alsa.Reader{ProcRoot: outputFixtureRoot(t, "dsd256-native")}.Read()
	if err != nil {
		t.Fatal(err)
	}
	steady := steadyView(snap)
	d, sd := snap.Cards[2].Devices[0], steady.Cards[2].Devices[0]
	if sd.Substreams[0].Status.HWPtr != nil || sd.Stream.Playback.Running.MomentaryFreqHz != nil {
		t.Fatal("the steady copy still carries the moving values")
	}
	if sd.Substreams[0].Status.State != "RUNNING" || *sd.Stream.Playback.Running.Altset != 4 {
		t.Error("the steady copy lost the state or the altset, which --watch compares")
	}
	if d.Substreams[0].Status.HWPtr == nil || d.Stream.Playback.Running.MomentaryFreqHz == nil ||
		d.Stream.Playback.Running.MomentaryFreq != "0x2c.61b2" || d.Substreams[0].Status.TriggerTime == "" {
		t.Error("steadyView cleared the moving values in the reading it copied")
	}
}
