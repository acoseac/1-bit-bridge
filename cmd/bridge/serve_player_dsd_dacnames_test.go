package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// B882L: the DSD migration's lines that name a DAC. Each is driven through
// ask, from the binding resolved on one reading of a sysfs tree whose unit
// has the case's serial and product string, with boundOf's own read of that
// tree, and read back from the journal slog's text and JSON handlers write at
// the level logging.Init sets them to.

// dacNamesPlayer binds the GO bar by its model, with dsdMode native and no
// record named: the start's step leaves the DAC's part waiting, and the part,
// once made, writes the DAC a migration_record_missing warning.
var dacNamesPlayer = config.PlayerConfig{Enabled: true, DSDMode: dsdstate.ModeNative,
	Device: config.PlayerDevice{Transport: "usb", Binding: config.BindingUniqueModel, VendorID: "20b1", ProductID: "3009"}}

// dacNamesBox is the Pi 5's GO bar with the case's serial ("" for a unit
// with none) and product string, its other strings and its descriptors as
// goBarIdentityFiles writes them.
func dacNamesBox(t *testing.T, serial, product string) outputTrees {
	t.Helper()
	trees := goBarTrees(t)
	goBarIdentityFiles(t, trees)
	unit := filepath.Join(trees.sysfs, qualifyGoBar)
	if serial == "" {
		if err := os.Remove(filepath.Join(unit, "serial")); err != nil {
			t.Fatal(err)
		}
	} else if err := os.WriteFile(filepath.Join(unit, "serial"), []byte(serial+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, "product"), []byte(product+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return trees
}

// goBarPortSpellings are the GO bar's port as sysfs gives it, with the
// card's configuration and interface after it, and as a port binding names
// it, the port alone.
func goBarPortSpellings(t *testing.T) (sysfs, binding string) {
	t.Helper()
	card, ok := (alsa.SysfsReader{Root: goBarTrees(t).sysfs}).Card(5)
	if !ok || card.USB == nil {
		t.Fatalf("the GO bar's card reads as %+v", card)
	}
	port, err := usbid.ParsePort(card.USB.Port)
	if err != nil {
		t.Fatal(err)
	}
	sysfs, binding = card.USB.Port, port.Place().String()
	if sysfs == binding || !strings.HasPrefix(sysfs, binding) {
		t.Fatalf("the port reads %q in sysfs and %q as a binding names it: want two spellings, the binding's a prefix", sysfs, binding)
	}
	return sysfs, binding
}

// journal is what the bridge's log gets while a test runs: what slog's text
// handler and its JSON handler write, at Info and above as logging.Init
// installs them, and every record, at every level, for the strings a handler
// could write of each (loggingtest.Strings).
type journal struct {
	text, json bytes.Buffer
	all        *loggingtest.Recorder
}

func captureJournal(t *testing.T) *journal {
	t.Helper()
	j := &journal{all: loggingtest.NewRecorder()}
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	loggingtest.SetDefault(t, slog.New(slog.NewMultiHandler(slog.NewTextHandler(&j.text, opts), slog.NewJSONHandler(&j.json, opts), j.all.Logger().Handler())))
	return j
}

// namedLine is a line of the journal that names a DAC: its message and its
// dac attribute.
type namedLine struct{ msg, dac string }

// textAttrs reads a line slog's text handler wrote back into its attributes:
// key=value pairs, a value quoted as strconv.Quote quotes it or bare up to
// the next space.
func textAttrs(t *testing.T, line string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for line != "" {
		k, rest, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("a text line that is no key=value: %q", line)
		}
		var v string
		if strings.HasPrefix(rest, `"`) {
			q, err := strconv.QuotedPrefix(rest)
			if err != nil {
				t.Fatalf("a quoted value that does not read back in %q: %v", line, err)
			}
			if v, err = strconv.Unquote(q); err != nil {
				t.Fatal(err)
			}
			rest = rest[len(q):]
		} else {
			v, rest, _ = strings.Cut(rest, " ")
			rest = " " + rest
		}
		out[k] = v
		line = strings.TrimPrefix(rest, " ")
	}
	return out
}

// named are the lines of each handler's journal that carry a dac attribute,
// in the order they were written, read back as each handler wrote them.
func (j *journal) named(t *testing.T) map[string][]namedLine {
	t.Helper()
	out := map[string][]namedLine{"text": nil, "json": nil}
	for _, l := range strings.Split(strings.TrimSpace(j.text.String()), "\n") {
		if a := textAttrs(t, l); a["dac"] != "" {
			out["text"] = append(out["text"], namedLine{a["msg"], a["dac"]})
		}
	}
	for _, l := range strings.Split(strings.TrimSpace(j.json.String()), "\n") {
		var a map[string]any
		if err := json.Unmarshal([]byte(l), &a); err != nil {
			t.Fatalf("a JSON line that does not decode: %q: %v", l, err)
		}
		if d, _ := a["dac"].(string); d != "" {
			m, _ := a["msg"].(string)
			out["json"] = append(out["json"], namedLine{m, d})
		}
	}
	return out
}

// askForTheBoundDAC opens the player's DSD state in a data directory of its
// own over trees, makes the start's step, which leaves the DAC's part
// waiting, resolves the binding on one reading of the box as the watcher does
// (binding.Settled), runs arm with the state's path and asks the DAC's part
// (ask), which reads the DAC from the box again (boundOf).
func askForTheBoundDAC(t *testing.T, trees outputTrees, arm func(statePath string)) {
	t.Helper()
	dataDir := t.TempDir()
	d := openPlayerDSD(dataDir, dacNamesPlayer, trees, nil)
	if d.store == nil {
		t.Fatal("the player's DSD state did not open")
	}
	ctx := t.Context()
	if err := d.migrate(ctx, nil, nil); err != nil {
		t.Fatalf("the start's step: %v", err)
	}
	if !d.waits() {
		t.Fatal("the start's step left no part for the DAC waiting")
	}
	inv, err := trees.observer().ObserveContext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, settled := binding.Settled(dacNamesPlayer.Device, inv)
	if !settled || r.State != binding.StateAvailable {
		t.Fatalf("the binding resolves to %+v (settled %v), want the GO bar available", r, settled)
	}
	if arm != nil {
		arm(filepath.Join(dataDir, dsdstate.FileName))
	}
	d.ask(ctx, r)
}

// The messages of the lines that name the bound DAC, as they begin.
const (
	lineWarning  = "player: the DSD migration recorded a warning in the player's DSD state"
	lineMigrated = "player: the legacy DSD keys were migrated into the player's DSD state"
	lineLanded   = "player: the DSD migration's part for the bound DAC reached the state's file and is not known to be durable"
	lineNotSaved = "player: the DSD migration's part for the bound DAC was not saved"
)

// dacPartOutcomes are the three ends of the DAC's part that name the DAC, and
// the lines each writes, in order: saved (say's warning line and its Info
// line); its write landed and not known to be durable (say's Warn line, then
// the warning); and its write refused before it landed (ask's Warn line).
// Between them they write every line that names the bound DAC. Each returns
// what to arm once the start's step is made.
var dacPartOutcomes = []struct {
	name  string
	set   func(t *testing.T) func(statePath string)
	lines []string
}{
	{"saved", func(t *testing.T) func(string) {
		persistentMedium(t)
		return nil
	}, []string{lineWarning, lineMigrated}},
	{"landed", func(t *testing.T) func(string) {
		persistentMedium(t)
		var path atomic.Pointer[string]
		var before atomic.Pointer[[]byte]
		var failed atomic.Bool
		t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error {
			p, was := path.Load(), before.Load()
			if p == nil || was == nil {
				return nil
			}
			now, err := os.ReadFile(*p)
			if err == nil && !bytes.Equal(now, *was) && failed.CompareAndSwap(false, true) {
				return errors.New("the test's barrier fails once the part's write is in the file")
			}
			return nil
		}))
		return func(statePath string) {
			b, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			before.Store(&b)
			path.Store(&statePath)
		}
	}, []string{lineLanded, lineWarning}},
	{"refused", func(t *testing.T) func(string) {
		var refusing atomic.Bool
		t.Cleanup(atomicwrite.SetMediumForTest(func(string) error {
			if refusing.Load() {
				return errors.New("the test's medium refuses the part's write")
			}
			return nil
		}))
		return func(string) { refusing.Store(true) }
	}, []string{lineNotSaved}},
}

// TestTheMigrationsLinesNameTheBoundDACByItsCleanedProductString: every line
// of the DSD migration that names the bound DAC names it by its USB IDs, then
// its product string with the unit's port, in each of its spellings, and its
// serial taken out, or by its USB IDs alone where the cleaned product string
// still holds the serial in another spelling, and, failing closed, for a
// serial inside the [redacted] marker once folded or with no letter or digit.
// Each case runs through ask for each outcome of the DAC's part, from a
// reading of the box, so the unit is the one boundOf read; a line naming the
// DAC by anything but that label, the serial or the port in a spelling the
// case puts in the product string reached by any handler, or the expected
// lines missing, fails.
func TestTheMigrationsLinesNameTheBoundDACByItsCleanedProductString(t *testing.T) {
	needsDirectorySync(t)
	sysfsPort, bindingPort := goBarPortSpellings(t)
	for _, tc := range []struct {
		name, serial, product, want string
		// absent are texts no string a handler could write may hold.
		absent []string
	}{
		{"the serial in another case drops the product string", goBarSerial, "GO bar gobar-7q2x-91kz-4471",
			"20b1:3009", []string{goBarSerial, "gobar-7q2x-91kz-4471", "GO bar"}},
		{"the serial with spaces between its groups drops the product string", goBarSerial, "GO bar GOBAR 7Q2X 91KZ 4471",
			"20b1:3009", []string{goBarSerial, "GOBAR 7Q2X 91KZ 4471", "GO bar"}},
		{"the serial with other marks between its letters and digits drops the product string", goBarSerial, "GO bar G.O.B.A.R/7Q2X_91KZ:4471",
			"20b1:3009", []string{goBarSerial, "7Q2X_91KZ:4471", "GO bar"}},
		{"the port as sysfs spells it is taken out", goBarSerial, "GO bar at " + sysfsPort,
			"20b1:3009 GO bar at [redacted]", []string{goBarSerial, bindingPort}},
		{"the port as a binding names it is taken out", goBarSerial, "GO bar at " + bindingPort,
			"20b1:3009 GO bar at [redacted]", []string{goBarSerial, bindingPort}},
		{"the serial once as the unit has it is taken out", goBarSerial, "GO bar " + goBarSerial,
			"20b1:3009 GO bar [redacted]", []string{goBarSerial, "7Q2X"}},
		{"the serial and the port are both taken out", goBarSerial, "GO bar " + goBarSerial + " " + sysfsPort,
			"20b1:3009 GO bar [redacted] [redacted]", []string{goBarSerial, bindingPort}},
		{"a product string without the serial stays", goBarSerial, "GO bar",
			"20b1:3009 GO bar", []string{goBarSerial}},
		{"a unit with no serial keeps its product string minus the port", "", "GO bar at " + bindingPort,
			"20b1:3009 GO bar at [redacted]", []string{bindingPort}},
		{"a serial inside the redaction marker drops the product string by design", "ACT", "GO bar ACT",
			"20b1:3009", []string{"GO bar"}},
		{"a serial with no letter or digit drops the product string by design", "----", "GO bar",
			"20b1:3009", []string{"GO bar"}},
	} {
		for _, o := range dacPartOutcomes {
			t.Run(tc.name+"/"+o.name, func(t *testing.T) {
				trees := dacNamesBox(t, tc.serial, tc.product)
				arm := o.set(t)
				j := captureJournal(t)
				askForTheBoundDAC(t, trees, arm)
				for handler, lines := range j.named(t) {
					if len(lines) != len(o.lines) {
						t.Errorf("the %s journal names the DAC in %d lines, want %d (%q): %+v", handler, len(lines), len(o.lines), o.lines, lines)
						continue
					}
					for i, l := range lines {
						if !strings.HasPrefix(l.msg, o.lines[i]) {
							t.Errorf("the %s journal's line %d is %q, want %q", handler, i, l.msg, o.lines[i])
						}
						if l.dac != tc.want {
							t.Errorf("the %s journal's line %q names the DAC %q, want %q", handler, l.msg, l.dac, tc.want)
						}
					}
				}
				if carrying := j.all.Carrying(tc.absent...); len(carrying) != 0 {
					t.Errorf("a line holds one of %q: %q", tc.absent, carrying)
				}
				for _, s := range tc.absent {
					if strings.Contains(j.text.String(), s) || strings.Contains(j.json.String(), s) {
						t.Errorf("the journal holds %q:\n%s\n%s", s, j.text.String(), j.json.String())
					}
				}
			})
		}
	}
}

// TestAWarningOfAnotherModelIsNamedByItsUSBIDs: a step's warning that names
// another model than the bound DAC's is given no unit, so its line names that
// model by its USB IDs alone, while the bound DAC's own warning beside it
// keeps its cleaned product string. Migrate writes a warning for the bound
// DAC alone, so no step through ask holds one of another model, and say is
// handed such a step here.
func TestAWarningOfAnotherModelIsNamedByItsUSBIDs(t *testing.T) {
	iface, product, other := 0, "GO bar "+goBarSerial, "Brooklyn DAC+ MB-0042"
	key, err := dacprofile.NewKey(0x20b1, 0x3009, &iface, &product)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := dacprofile.NewKey(0x262a, 0x9302, &iface, &other)
	if err != nil {
		t.Fatal(err)
	}
	unit := alsa.USBIdentity{ID: alsa.USBID{Vendor: 0x20b1, Product: 0x3009}, Serial: usbid.New(goBarSerial), Port: "platform-xhci-hcd.0/usb1-1:1.0"}
	step := dsdstate.Step{Warnings: []dsdstate.Warning{
		{Code: dsdstate.WarningMigrationRecordMissing, DAC: &key},
		{Code: dsdstate.WarningMigrationRecordRefused, Reason: dsdstate.ReasonIdentityMismatch, DAC: &otherKey},
	}}
	j := captureJournal(t)
	(&playerDSD{}).say(step, nil, &dsdstate.Bound{Key: key}, &unit)
	want := []namedLine{{lineWarning, "20b1:3009 GO bar [redacted]"}, {lineWarning, "262a:9302"}}
	for handler, lines := range j.named(t) {
		if len(lines) != len(want) {
			t.Fatalf("the %s journal names %+v, want %+v", handler, lines, want)
		}
		for i := range want {
			if !strings.HasPrefix(lines[i].msg, want[i].msg) || lines[i].dac != want[i].dac {
				t.Errorf("the %s journal's line %d is %+v, want %+v", handler, i, lines[i], want[i])
			}
		}
	}
	if carrying := j.all.Carrying(goBarSerial, other); len(carrying) != 0 {
		t.Errorf("a line holds the serial or the other model's product string: %q", carrying)
	}
}
