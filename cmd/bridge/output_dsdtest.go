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
	forceS32   bool
	kernel     func() (string, error)
	identity   func(int, int, int) (qualification.Identity, error)
	gain       func(int) (bool, error)
	openPCM    func(context.Context, alsapcm.SinkConfig) (nativePCMSink, error)
	open       func(context.Context, alsapcm.DSDConfig) (nativeTestSink, error)
	controls   func(int) (alsactl.Snapshot, error)
	attachment func(int) (qualification.Attachment, error)
	deadline   time.Time
	now        func() time.Time
	at         func(time.Time) (<-chan time.Time, func())
	stdin      io.Reader
	tty        bool
	makeRaw    func() (restore func() error, err error)
	goos       string
	resolve    func(context.Context, string) (nativeEndpoint, func(alsapcm.NodeInfo) error, error)
	// build is the binary's VCS revision and whether its tree was modified,
	// which a report records as BuildSHA and Modified; nil is vcsBuild.
	build func() (sha string, modified bool)
	// player is what the real engine and backend of the backend-lifecycle
	// and transition scenarios read and open.
	player nativePlayerSeams
}

// nativePlayerSeams are what the backend a backend-lifecycle or transition
// scenario builds reads and opens: the sysfs and control readers its
// authorization identifies the DAC and checks constraints with, and its
// sink openers, nil for the backend's own (alsapcm's).
type nativePlayerSeams struct {
	sysfs      alsa.SysfsReader
	controls   alsactl.Reader
	openSink   func(context.Context, alsapcm.SinkConfig) (*alsapcm.Sink, error)
	openNative func(context.Context, alsapcm.DSDConfig) (*alsapcm.DSDSink, error)
	// observe, when set, is told what each turn of the scenario's loop saw:
	// a test waits on it for a state before it sends a key.
	observe func(lifecycleView)
}

// vcsBuild is the binary's VCS revision, "" when the build carries none,
// and whether its tree was modified.
func vcsBuild() (sha string, modified bool) {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			sha = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	return sha, modified
}

// nativePlatform is the platform and Go version a report records.
func nativePlatform() (platform, goVersion string) {
	return runtime.GOOS + "/" + runtime.GOARCH, runtime.Version()
}

// nativeEnvironment is a report's Environment on this platform and Go,
// running kernel: what an approval and its evidence are bound to.
func nativeEnvironment(kernel string) string {
	platform, goVersion := nativePlatform()
	return qualification.Digest([]string{platform, goVersion, kernel})
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
	env := outputDSDTestEnv{goos: runtime.GOOS, open: productionNativeOpen, controls: (alsactl.Reader{}).Read, now: time.Now, stdin: os.Stdin, tty: term.IsTerminal(fd), resolve: resolveNativeTest, openPCM: productionNativePCMOpen,
		build: vcsBuild, player: nativePlayerSeams{sysfs: alsa.SysfsReader{Root: hostTrees.sysfs}}}
	env.kernel = func() (string, error) {
		b, e := os.ReadFile(filepath.Join(hostTrees.proc, "sys/kernel/osrelease"))
		return strings.TrimSpace(string(b)), e
	}
	env.gain = (alsa.SysfsReader{Root: hostTrees.sysfs}).QualificationGain
	env.identity = func(card, device, sub int) (qualification.Identity, error) {
		sys := alsa.SysfsReader{Root: hostTrees.sysfs}
		q, e := sys.Qualification(card)
		if e != nil {
			return qualification.Identity{}, e
		}
		id, ok := sys.Card(card)
		if !ok || id.USB == nil {
			return qualification.Identity{}, errors.New("USB identity unavailable")
		}
		return qualification.Identity{Vendor: fmt.Sprintf("%04x", id.USB.ID.Vendor), ProductID: fmt.Sprintf("%04x", id.USB.ID.Product), Manufacturer: q.Manufacturer, Product: q.Product, BCDDevice: q.BCDDevice, Port: q.Port, Interface: q.Interface, Device: device, Subdevice: sub}, nil
	}
	env.makeRaw = func() (func() error, error) {
		state, e := term.MakeRaw(fd)
		if e != nil {
			return nil, e
		}
		return func() error { return term.Restore(fd, state) }, nil
	}
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
	experiments, experimentState                                                     string
	transport                                                                        string
	ackDoPNoise                                                                      bool
	provisional                                                                      string
	qualification, nativeTupleID                                                     string
	config, manifest, fixtures, contact, report, mixer, scenario, ack, format, pause string
	rate, seconds, tail, prefill                                                     int
	list                                                                             []string
}

func parseNativeOptions(args []string, w io.Writer) (nativeOptions, error) {
	var o nativeOptions
	fs := flag.NewFlagSet("dsd-test", flag.ContinueOnError)
	fs.SetOutput(w)
	fs.StringVar(&o.experiments, "mixer-experiments", "", "JSON list of hashed changed-state contact reports")
	fs.StringVar(&o.experimentState, "mixer-experiment-state", "", "nonzero-attenuation or restored (off-ear contact)")
	fs.StringVar(&o.transport, "transport", "native", "native or dop")
	fs.BoolVar(&o.ackDoPNoise, "ack-dop-noise", false, "acknowledge potential full-scale DoP noise")
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
	fs.StringVar(&o.contact, "contact-report", "", "contact receipt: version 2 native, version 3 DoP")
	fs.StringVar(&o.report, "report", "", "new report path, never an approval")
	fs.StringVar(&o.mixer, "mixer-snapshot", "", "fresh amixer annex")
	fs.StringVar(&o.provisional, "provisional-record", "", "CLI candidate for backend-lifecycle only")
	fs.StringVar(&o.qualification, "qualification", "", "owner-approved record, for approved-native-dop-native only")
	fs.StringVar(&o.nativeTupleID, "native-tuple-id", "", "the approved native policy's ID, for approved-native-dop-native only")
	fs.StringVar(&o.scenario, "scenario", "", "level-check, matrix, interactive, backend-lifecycle or approved-native-dop-native")
	if e := fs.Parse(args); e != nil {
		return o, e
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	switch {
	case !set["ack-level"] || (o.ack != "off-ear" && o.ack != "contact-position"):
		return o, errors.New("--ack-level must be off-ear or contact-position")
	case o.transport != "native" && o.transport != "dop":
		return o, errors.New("--transport must be native or dop")
	case !set["format"] || !(qualification.Tuple{Transport: o.transport, Format: o.format}).ValidTransport():
		return o, errors.New("--format must be DSD_U32_LE, DSD_U32_BE or DSD_U16_LE, or S32_LE with --transport dop")
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
	if o.transport == "dop" && !o.ackDoPNoise {
		return o, errors.New("--ack-dop-noise required for DoP")
	}
	if o.experimentState != "" && (o.transport != "dop" || o.scenario != "level-check" || o.ack != "off-ear" || (o.experimentState != "nonzero-attenuation" && o.experimentState != "restored")) {
		return o, errors.New("mixer experiment state requires an off-ear DoP contact")
	}
	o.list = fs.Args()
	if len(o.list) == 0 {
		return o, errors.New("fixture-list arguments (manifest IDs) required after flags")
	}
	if o.config == "" || o.manifest == "" || o.fixtures == "" || o.report == "" || o.mixer == "" {
		return o, errors.New("--config, --manifest, --fixtures, --mixer-snapshot and --report required")
	}
	transition := o.scenario == qualification.TransitionScenario
	if o.scenario != "level-check" && o.scenario != "matrix" && o.scenario != "interactive" && o.scenario != "backend-lifecycle" && !transition {
		return o, errors.New("--scenario must be level-check, matrix, interactive, backend-lifecycle or approved-native-dop-native")
	}
	if (o.scenario == "backend-lifecycle") != (o.provisional != "") {
		return o, errors.New("--provisional-record is required only for backend-lifecycle")
	}
	if transition != (o.qualification != "") || transition != (o.nativeTupleID != "") {
		return o, errors.New("--qualification and --native-tuple-id are required only for approved-native-dop-native")
	}
	if transition && o.transport != "dop" {
		return o, errors.New("approved-native-dop-native names its DoP policy by --transport dop and the policy flags")
	}
	if o.scenario == "level-check" && o.ack != "off-ear" {
		return o, errors.New("--ack-level must be off-ear for level-check")
	}
	if o.transport == "dop" && o.scenario == "level-check" && len(o.list) != 1 {
		return o, errors.New("DoP contact requires exactly one fixture")
	}
	return o, nil
}

type nativeReport struct {
	ExperimentState                                          string                         `json:",omitempty"`
	Experiments                                              []qualification.Evidence       `json:",omitempty"`
	DoP                                                      *qualification.DoPObservations `json:",omitempty"`
	Kernel                                                   string                         `json:",omitempty"`
	PackingVersion, SourceRate, CarrierRate                  int                            `json:",omitempty"`
	NativeTuple                                              qualification.Tuple
	NativeCompleted                                          int
	Identity                                                 qualification.Identity
	FeatureUnit                                              *bool
	Scenario, Environment                                    string
	LifecycleActions                                         []string
	PassedEdges                                              []qualification.Edge
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
	// EdgeOpened is every open of another transport than the qualified
	// tuple's: a PCM edge's, or a transition's native endpoint's.
	EdgeOpened []alsapcm.Params `json:",omitempty"`
	// Endpoints and Qualification are a transition report's: the approved
	// native and DoP policies it played, native first, each by its ID and
	// the digest of the whole policy, and the approved record it read them
	// from, by its name and the digest the record carries.
	Endpoints     []qualification.PolicyRef `json:",omitempty"`
	Qualification *qualification.Evidence   `json:",omitempty"`
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
	transition := o.scenario == qualification.TransitionScenario
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
	anyNative := false
	// The transition scenario's fixtures are at its two policies' rates,
	// which the approved record names: they are checked with its endpoints.
	for _, id := range o.list {
		f, ok := byID[id]
		anyNative = anyNative || f.Container == "DSF" || f.Container == "DFF"
		if !transition && (!ok || ((f.Rate != o.rate || (f.Container != "DSF" && f.Container != "DFF")) && !(o.scenario == "backend-lifecycle" && f.Container == "WAV"))) {
			fmt.Fprintln(errout, "fixture-list tuple mismatch")
			return 1
		}
	}
	if o.scenario == "backend-lifecycle" && !anyNative {
		fmt.Fprintln(errout, "backend lifecycle requires native execution")
		return 1
	}
	// A DoP fixture's PCM reference plays at its carrier, where a run plays
	// one: a PCM edge's own fixture has none, and the transition plays none.
	if o.transport == "dop" && !transition {
		for _, id := range o.list {
			f := byID[id]
			if f.Container == "WAV" {
				continue
			}
			ref := byID[f.PCMReference]
			if ref.Rate != o.rate/16 {
				fmt.Fprintln(errout, "DoP requires same-carrier PCM reference")
				return 1
			}
			carrier := o.rate / 16
			if o.scenario == "level-check" && (f.Frames+15)/16+int64((carrier*o.prefill+999)/1000)+int64((carrier*o.tail+999)/1000) > int64(carrier)*10 {
				fmt.Fprintln(errout, "DoP contact exceeds ten seconds including silence")
				return 1
			}
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
	if o.transport == "dop" {
		tuple.Transport = "dop"
		env.forceS32 = true
	}
	mh := fmt.Sprintf("%x", sha256.Sum256(b))
	var approved *approvedTransition
	if transition {
		t, e := readApprovedTransition(o, env, resolved, byID, tuple, mh, a, controls)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		approved = &t
	} else if o.scenario != "level-check" {
		var contact qualification.Contact
		b, e := qualification.ReadEvidenceFile(o.contact)
		if e == nil {
			var envelope struct {
				SchemaVersion, QualificationVersion int
				DoP                                 *qualification.DoPObservations
				Contact                             *qualification.Contact
				End, Error                          string
			}
			e = json.Unmarshal(b, &envelope)
			if e == nil && envelope.Contact != nil {
				if envelope.End != "complete" || envelope.Error != "" {
					e = errors.New("contact report incomplete or errored")
				} else if o.transport == "dop" && (envelope.SchemaVersion != 2 || envelope.QualificationVersion != qualification.Version) {
					e = errors.New("DoP contact report version mismatch")
				} else if o.transport == "dop" && envelope.DoP.Validate() != nil {
					e = errors.New("DoP contact report observations incomplete")
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
	report.Scenario = o.scenario
	report.ExperimentState = o.experimentState
	if env.gain != nil {
		gain, e := env.gain(ep.Card)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
		report.FeatureUnit = &gain
	}
	switch {
	case approved != nil:
		// The identity and the kernel the approved record was read against.
		report.Identity, report.Kernel = approved.identity, approved.kernel
		report.Endpoints = []qualification.PolicyRef{{ID: approved.native.ID, SHA256: qualification.Digest(approved.native)}, {ID: approved.dop.ID, SHA256: qualification.Digest(approved.dop)}}
		report.Qualification = &qualification.Evidence{Path: filepath.Base(o.qualification), SHA256: approved.record.SHA256}
	case env.identity != nil:
		report.Identity, e = env.identity(ep.Card, ep.Device, resolved.Subdevice)
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
	}
	if o.transport == "dop" {
		report.SchemaVersion = 2
		report.PackingVersion = 1
		report.SourceRate = o.rate
		report.CarrierRate = o.rate / 16
		if report.FeatureUnit == nil {
			fmt.Fprintln(errout, "DoP requires USB gain descriptor evidence")
			return 1
		}
		for _, c := range controls.Controls {
			if c.ReadError != "" {
				fmt.Fprintln(errout, "DoP controls unreadable")
				return 1
			}
		}
	}
	if env.kernel != nil && approved == nil {
		report.Kernel, e = env.kernel()
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
	}
	report.Platform, report.GoVersion = nativePlatform()
	report.BuildSHA = "unverified"
	build := env.build
	if build == nil {
		build = vcsBuild
	}
	sha, modified := build()
	if sha != "" {
		report.BuildSHA = sha
	}
	report.Modified = modified
	report.Environment = nativeEnvironment(report.Kernel)
	w := format.DSDWordBytes(format.ALSAFormat(o.format))
	if o.transport == "dop" {
		w = 2
	}
	carrier := o.rate / (8 * w)
	report.SilenceFrames = [2]int{(carrier*o.tail + 999) / 1000, (carrier*o.prefill + 999) / 1000}
	if o.experiments != "" {
		b, e := qualification.ReadEvidenceFile(o.experiments)
		if e == nil {
			e = json.Unmarshal(b, &report.Experiments)
		}
		if e == nil {
			e = qualification.CheckDoPExperiments(filepath.Dir(o.experiments), report.Experiments, tuple, report.Identity, mh, report.Environment)
		}
		if e != nil {
			fmt.Fprintln(errout, e)
			return 1
		}
	}
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
		if e := runCtx.Err(); e != nil {
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
	// Raw mode belongs only to the console: validation keeps ISIG/OPOST,
	// and executeNativeTest joins its reader before restoration and diagnostics.
	e = func() (runErr error) {
		if env.tty && env.makeRaw != nil {
			restore, err := env.makeRaw()
			if err != nil {
				return err
			}
			defer func() { runErr = errors.Join(runErr, restore()) }()
		}
		return executeNativeTest(runCtx, cancelRun, o, env, ep, admit, m, byID, approved, reader, out, &report)
	}()
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
