package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"golang.org/x/term"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
)

type nativeTestSink interface {
	Activate(context.Context, uint64) error
	BeginRelease(alsapcm.ReleaseMode)
	WriteDSD(context.Context, uint64, []byte, []byte, int, int) (int, error)
	Finish(context.Context) error
	Release(context.Context, alsapcm.ReleaseMode) error
	Pause(context.Context) error
	Resume(context.Context) error
	Params() alsapcm.Params
	Position() alsapcm.DSDPosition
	Done() <-chan struct{}
}
type nativePCMSink interface {
	Write(context.Context, []byte) (int, error)
	Drain(context.Context) error
	Close() error
}
type nativeEndpoint struct {
	alsapcm.Endpoint
	Subdevice int
}

type outputDSDTestEnv struct {
	openPCM    func(context.Context, alsapcm.SinkConfig) (nativePCMSink, error)
	open       func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error)
	controls   func(int) (alsactl.Snapshot, error)
	attachment func(int) (qualification.Attachment, error)
	now        func() time.Time
	at         func(time.Time) (<-chan time.Time, func())
	stdin      io.Reader
	tty        bool
	goos       string
	resolve    func(context.Context, string) (nativeEndpoint, func(alsapcm.NodeInfo) error, error)
}

// productionNativeOpen has its own test guard, in addition to the ALSA guard.
func productionNativeOpen(ctx context.Context, c alsapcm.DSDConfig) (nativeTestSink, error) {
	if testing.Testing() {
		return nil, errors.New("native hardware opener prohibited in tests")
	}
	return alsapcm.OpenDSDSink(ctx, c)
}
func outputDSDTestCmd(ctx context.Context, args []string, out, errout io.Writer) int {
	ctx, stopSignal := signal.NotifyContext(ctx, os.Interrupt)
	defer stopSignal()
	rc, err := os.Stdin.SyscallConn()
	if err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	fd := -1
	if err = rc.Control(func(d uintptr) { fd = int(d) }); err != nil {
		fmt.Fprintln(errout, err)
		return 1
	}
	if term.IsTerminal(fd) {
		state, e := term.MakeRaw(fd)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		defer term.Restore(fd, state)
	}
	env := outputDSDTestEnv{goos: runtime.GOOS, open: productionNativeOpen, controls: (alsactl.Reader{}).Read, now: time.Now, stdin: os.Stdin, tty: term.IsTerminal(fd), resolve: resolveNativeTest, openPCM: productionNativePCMOpen}
	env.at = func(at time.Time) (<-chan time.Time, func()) {
		t := time.NewTimer(time.Until(at))
		return t.C, func() { t.Stop() }
	}
	env.attachment = func(card int) (qualification.Attachment, error) {
		q, e := (alsa.SysfsReader{Root: hostTrees.sysfs}).Qualification(card)
		if e != nil {
			return qualification.Attachment{}, e
		}
		b, e := os.ReadFile(filepath.Join(hostTrees.proc, "sys/kernel/random/boot_id"))
		if e != nil {
			return qualification.Attachment{}, e
		}
		return qualification.Attachment{BootID: strings.TrimSpace(string(b)), Port: q.Port, Manufacturer: q.Manufacturer, Product: q.Product, BCDDevice: q.BCDDevice, Card: card, DevNum: q.DevNum, Interface: q.Interface}, nil
	}
	return runOutputDSDTest(ctx, args, env, out, errout)
}
func resolveNativeTest(ctx context.Context, file string) (nativeEndpoint, func(alsapcm.NodeInfo) error, error) {
	cfg, _, e := loadCLIConfig(file)
	if e != nil {
		return nativeEndpoint{}, nil, e
	}
	host, _, e := net.SplitHostPort(cfg.ListenAddress)
	if e != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() || cfg.Output.Player.OpenHome {
		return nativeEndpoint{}, nil, errors.New("dsd-test requires loopback config with OpenHome off")
	}
	inv, e := hostTrees.observer().ObserveContext(ctx)
	if e != nil {
		return nativeEndpoint{}, nil, e
	}
	r := binding.Resolve(cfg.Output.Player.Device, inv)
	if r.State != binding.StateAvailable {
		return nativeEndpoint{}, nil, fmt.Errorf("native binding unavailable: %s", r.State)
	}
	checker := binding.Checker{Want: r, Proc: alsa.Reader{ProcRoot: hostTrees.proc}, Sysfs: alsa.SysfsReader{Root: hostTrees.sysfs}}
	return nativeEndpoint{Endpoint: alsapcm.Endpoint{Card: r.Card, Device: r.Endpoint.Device}, Subdevice: r.Endpoint.Subdevice}, checker.Check, nil
}

type nativeOptions struct {
	config, manifest, fixtures, contact, report, mixer, scenario, ack, format, pause string
	rate, seconds, tail, prefill                                                     int
	list                                                                             []string
}

func parseNativeOptions(args []string, w io.Writer) (nativeOptions, error) {
	var o nativeOptions
	fs := flag.NewFlagSet("dsd-test", flag.ContinueOnError)
	fs.SetOutput(w)
	fs.StringVar(&o.ack, "ack-level", "", "off-ear or contact-position (required)")
	// Neutral parser values keep downstream diagnostics defined; Visit still
	// requires explicit flags before any evidence or opener is reached.
	fs.StringVar(&o.format, "format", "DSD_U32_BE", "native ALSA format (required)")
	fs.IntVar(&o.rate, "bit-rate", 2822400, "logical bits/channel/s (required)")
	fs.IntVar(&o.seconds, "seconds", 0, "wall-clock budget from first open (required)")
	fs.StringVar(&o.pause, "pause-method", "kernel", "kernel or silence (required)")
	fs.IntVar(&o.tail, "tail-ms", 0, "outgoing silence, 0..100 (required)")
	fs.IntVar(&o.prefill, "prefill-ms", 0, "activation silence, 0..500 (required)")
	fs.StringVar(&o.config, "config", "", "loopback config")
	fs.StringVar(&o.manifest, "manifest", "", "version 1 owner manifest JSON")
	fs.StringVar(&o.fixtures, "fixtures", "", "listening asset root")
	fs.StringVar(&o.contact, "contact-report", "", "version 2 contact receipt")
	fs.StringVar(&o.report, "report", "", "new report path, never an approval")
	fs.StringVar(&o.mixer, "mixer-snapshot", "", "fresh amixer annex")
	fs.StringVar(&o.scenario, "scenario", "", "level-check, matrix or interactive")
	if e := fs.Parse(args); e != nil {
		return o, e
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case !set["ack-level"] || (o.ack != "off-ear" && o.ack != "contact-position"):
		return o, errors.New("--ack-level must be off-ear or contact-position")
	case !set["format"] || format.DSDWordBytes(format.ALSAFormat(o.format)) == 0:
		return o, errors.New("--format must be DSD_U32_LE, DSD_U32_BE or DSD_U16_LE")
	case !set["bit-rate"] || (o.rate != 2822400 && o.rate != 5644800 && o.rate != 11289600):
		return o, errors.New("--bit-rate must be 2822400, 5644800 or 11289600")
	case !set["seconds"] || o.seconds < 1 || o.seconds > 600:
		return o, errors.New("--seconds must be 1..600")
	case !set["pause-method"] || (o.pause != "kernel" && o.pause != "silence"):
		return o, errors.New("--pause-method must be kernel or silence")
	case !set["tail-ms"] || o.tail < 0 || o.tail > 100:
		return o, errors.New("--tail-ms must be 0..100")
	case !set["prefill-ms"] || o.prefill < 0 || o.prefill > 500:
		return o, errors.New("--prefill-ms must be 0..500")
	}
	o.list = fs.Args()
	if len(o.list) == 0 {
		return o, errors.New("fixture-list arguments (manifest IDs) required after flags")
	}
	if o.config == "" || o.manifest == "" || o.fixtures == "" || o.report == "" || o.mixer == "" {
		return o, errors.New("--config, --manifest, --fixtures, --mixer-snapshot and --report required")
	}
	if o.scenario != "level-check" && o.scenario != "matrix" && o.scenario != "interactive" {
		return o, errors.New("--scenario must be level-check, matrix or interactive")
	}
	if o.scenario == "level-check" && o.ack != "off-ear" {
		return o, errors.New("--ack-level must be off-ear for level-check")
	}
	return o, nil
}

type nativeReport struct {
	pendingContact                                           *qualification.Contact
	Completed                                                []bool
	IgnoredKeys                                              []string
	Navigated                                                bool
	Device, Subdevice                                        int
	BuildSHA, GoVersion, Platform                            string
	Modified                                                 bool
	SilenceFrames                                            [2]int
	SchemaVersion, QualificationVersion                      int
	Args                                                     []string
	Tuple                                                    qualification.Tuple
	Attachment                                               qualification.Attachment
	Before, After                                            alsactl.Snapshot
	ControlObservations                                      []alsactl.Snapshot
	ExcludedBefore, ExcludedAfter                            []alsactl.Control
	ManifestHash, AmixerAnnex, End, Error, OwnerConfirmation string
	PauseReleaseSeconds                                      int
	PCMTimePolicy                                            [2]int
	Opened                                                   []alsapcm.Params
	Contact                                                  *qualification.Contact
}

func runOutputDSDTest(ctx context.Context, args []string, env outputDSDTestEnv, out, errout io.Writer) int {
	o, e := parseNativeOptions(args, errout)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 2
	}
	if env.goos != "linux" {
		fmt.Fprintln(errout, "native harness requires Linux")
		return 1
	}
	resolved, check, e := env.resolve(ctx, o.config)
	ep := resolved.Endpoint
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	b, e := qualification.ReadEvidenceFile(o.manifest)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	m, e := qualification.ReadManifest(strings.NewReader(string(b)))
	if e == nil {
		e = m.CheckAssets(o.fixtures)
	}
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	byID := map[string]qualification.Fixture{}
	for _, f := range m.Fixtures {
		byID[f.ID] = f
	}
	for _, id := range o.list {
		f, ok := byID[id]
		if !ok || f.Rate != o.rate || (f.Container != "DSF" && f.Container != "DFF") {
			fmt.Fprintln(errout, "fixture-list tuple mismatch")
			return 1
		}
	}
	a, e := env.attachment(ep.Card)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	controls, e := env.controls(ep.Card)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	annex, e := qualification.ReadEvidenceFile(o.mixer)
	if e != nil || len(annex) == 0 || len(annex) > 1<<20 {
		fmt.Fprintln(errout, "missing/bounded --mixer-snapshot")
		return 1
	}
	if !strings.HasPrefix(string(annex), fmt.Sprintf("# card=%d\n", ep.Card)) {
		fmt.Fprintln(errout, "--mixer-snapshot card differs from live binding")
		return 1
	}
	tuple := qualification.Tuple{Format: o.format, BitRate: o.rate, Pause: o.pause, TailMS: o.tail, PrefillMS: o.prefill}
	mh := fmt.Sprintf("%x", sha256.Sum256(b))
	if o.scenario != "level-check" {
		var contact qualification.Contact
		b, e := qualification.ReadEvidenceFile(o.contact)
		if e == nil {
			var envelope struct {
				Contact    *qualification.Contact
				End, Error string
			}
			e = json.Unmarshal(b, &envelope)
			if e == nil && envelope.Contact != nil {
				if envelope.End != "complete" || envelope.Error != "" {
					e = errors.New("contact report incomplete or errored")
				} else {
					contact = *envelope.Contact
				}
			} else if e == nil {
				e = json.Unmarshal(b, &contact)
			}
		}
		if e == nil {
			e = contact.Validate(tuple, a, ep.Device, resolved.Subdevice, controls.Hash(), mh)
		}
		if e != nil {
			fmt.Fprintln(errout, "--contact-report:", e)
			return 1
		}
	}
	report := nativeReport{Device: ep.Device, Subdevice: resolved.Subdevice, SchemaVersion: 1, QualificationVersion: qualification.Version, Args: args, Tuple: tuple, Attachment: a, Before: controls, ManifestHash: mh, AmixerAnnex: string(annex), PauseReleaseSeconds: 30, End: "incomplete"}
	report.Platform = runtime.GOOS + "/" + runtime.GOARCH
	report.GoVersion = runtime.Version()
	report.BuildSHA = "unverified"
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				report.BuildSHA = s.Value
			}
			if s.Key == "vcs.modified" {
				report.Modified = s.Value == "true"
			}
		}
	}
	carrier := o.rate / (8 * format.DSDWordBytes(format.ALSAFormat(o.format)))
	report.SilenceFrames = [2]int{(carrier*o.tail + 999) / 1000, (carrier*o.prefill + 999) / 1000}
	file, e := os.OpenFile(o.report, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		fmt.Fprintln(errout, e)
		return 1
	}
	defer file.Close()
	runCtx, cancelRun := context.WithCancelCause(ctx)
	defer cancelRun(nil)
	// Capture every admission snapshot, including excluded values, without
	// sharing the report with opening workers that may outlive a timed-out call.
	var observationsMu sync.Mutex
	var observations []alsactl.Snapshot
	// Capture the reference once, compare it before every activation/reopen.
	admit := func(n alsapcm.NodeInfo) error {
		if e := runCtx.Err(); e != nil {
			return e
		}
		if n.Card != ep.Card || n.Device != ep.Device || n.Subdevice != resolved.Subdevice {
			return errors.New("opened endpoint differs from contacted endpoint")
		}
		if check != nil {
			if e := check(n); e != nil {
				return e
			}
		}
		if e := runCtx.Err(); e != nil {
			return e
		}
		aa, e := env.attachment(ep.Card)
		if e != nil {
			return e
		}
		cc, e := env.controls(ep.Card)
		if e != nil {
			return e
		}
		observationsMu.Lock()
		observations = append(observations, cc)
		observationsMu.Unlock()
		if aa != a || cc.Hash() != controls.Hash() {
			return errors.New("native binding/mixer reference changed")
		}
		return nil
	}
	out = &nativeConsoleOutput{out: out, raw: env.tty}
	reader := env.stdin
	e = executeNativeTest(runCtx, cancelRun, o, env, ep, admit, m, byID, reader, out, &report)
	after := report.After
	observationsMu.Lock()
	report.ControlObservations = append([]alsactl.Snapshot(nil), observations...)
	observationsMu.Unlock()
	for _, c := range controls.Controls {
		if c.Excluded() {
			report.ExcludedBefore = append(report.ExcludedBefore, c)
		}
	}
	for _, c := range after.Controls {
		if c.Excluded() {
			report.ExcludedAfter = append(report.ExcludedAfter, c)
		}
	}
	if e != nil {
		report.Contact = nil
		report.Error = e.Error()
		if report.End != "deadline" && report.End != "stopped" && report.End != "quit" {
			report.End = "incomplete"
		}
		fmt.Fprintln(errout, e)
	}
	if je := json.NewEncoder(file).Encode(report); je != nil {
		fmt.Fprintln(errout, je)
		return 1
	}
	if e != nil {
		return 1
	}
	return 0
}
