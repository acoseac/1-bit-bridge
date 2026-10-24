package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// outputBindEnv is what `bridge output dac inspect` and `bridge output dac
// bind` take from the host, so a test can stand in for each: the OS they
// refuse the host's trees off, the clock a report is stamped with, and the
// interface lister the player's LAN state is read from.
type outputBindEnv struct {
	goos string
	now  func() time.Time
	lan  lan.Lister
	// beforeLock, when set, runs between bind's first read of the config
	// and its taking the data dir's lock: the window a test lands another
	// writer in.
	beforeLock func()
	// beforeSave, when set, runs between bind's read of the config under
	// the locks and its save: the window a writer that takes no lock (an
	// edit, or a bridge older than bridge.yaml's lock) can still save in,
	// and one that takes bridge.yaml's waits through.
	beforeSave func()
}

// outputDACBindingCmd is inspect and bind on this host.
func outputDACBindingCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runOutputDACBinding(ctx, args, outputBindEnv{goos: runtime.GOOS, now: time.Now, lan: lan.HostInterfaces}, stdout, stderr)
}

// isOutputDACBinding reports whether `bridge output dac` was given one of
// the player-binding verbs, inspect or bind, rather than its own flags.
func isOutputDACBinding(args []string) bool {
	return len(args) > 0 && (args[0] == "inspect" || args[0] == "bind")
}

// runOutputDACBinding is `bridge output dac inspect` and `bridge output dac
// bind`: the player's DAC binding (B470 PR 4a). Neither opens a PCM, and a
// serial reaches the terminal as its label alone.
func runOutputDACBinding(ctx context.Context, args []string, env outputBindEnv, stdout, stderr io.Writer) int {
	switch args[0] {
	case "inspect":
		return runDACInspect(args[1:], env, stdout, stderr)
	case "bind":
		return runDACBind(ctx, args[1:], env, stdout, stderr)
	}
	fmt.Fprintf(stderr, "output dac: unknown verb %q\n", args[0])
	return 2
}

// dacTreeFlags are the trees inspect and bind read, named or the host's.
type dacTreeFlags struct {
	proc, sysfs *string
}

func addDACTreeFlags(fs *flag.FlagSet) dacTreeFlags {
	return dacTreeFlags{
		proc: fs.String("proc-root", "", "read the /proc tree at this directory instead of the host's (default /proc)"),
		sysfs: fs.String("sysfs-root", "", "read the sysfs tree at this directory instead of the host's (default /sys); "+
			"a tree laid out from another box reads anywhere"),
	}
}

// observer resolves the trees: the flags, or the host's own on Linux. Off
// Linux, unless both trees are named, it refuses before reading anything.
func (f dacTreeFlags) observer(env outputBindEnv, cmd string, stderr io.Writer) (binding.Observer, bool) {
	proc, sysfs := *f.proc, *f.sysfs
	if proc == "" || sysfs == "" {
		if env.goos != "linux" {
			fmt.Fprintf(stderr, "%s: ALSA is Linux only, and this is %s\n", cmd, env.goos)
			fmt.Fprintln(stderr, "  --proc-root and --sysfs-root read trees laid out from a Linux box.")
			return binding.Observer{}, false
		}
		if proc == "" {
			proc = hostTrees.proc
		}
		if sysfs == "" {
			sysfs = hostTrees.sysfs
		}
	}
	return binding.Observer{Proc: alsa.Reader{ProcRoot: proc}, Sysfs: alsa.SysfsReader{Root: sysfs}}, true
}

// dacInspectReport is inspect's --json shape, a diagnostic and not a wire
// contract: every key always present, a serial as its label alone.
type dacInspectReport struct {
	ReadAt time.Time `json:"readAt"`
	// Config is the config file read, and ConfigProblem why none was.
	Config        string             `json:"config"`
	ConfigProblem string             `json:"configProblem"`
	Player        dacPlayerView      `json:"player"`
	LAN           dacLANView         `json:"lan"`
	Binding       *dacBindingView    `json:"binding"`
	Resolution    dacResolutionView  `json:"resolution"`
	Cards         []dacCardView      `json:"cards"`
	Problems      []string           `json:"problems"`
	Verify        *dacVerifyView     `json:"verify"`
	resolved      binding.Resolution // what Verify checks against
}

// dacPlayerView is the player's state: whether this config runs it, why
// not, and its UUID once made.
type dacPlayerView struct {
	Enabled bool    `json:"enabled"`
	Reason  string  `json:"reason"`
	UUID    *string `json:"uuid"`
	UUIDNot string  `json:"uuidProblem"`
}

// dacLANView is the player's LAN state (internal/player/lan).
type dacLANView struct {
	State      string            `json:"state"`
	Reason     string            `json:"reason"`
	Interfaces []dacLANInterface `json:"interfaces"`
}

type dacLANInterface struct {
	Name  string   `json:"name"`
	Addrs []string `json:"addrs"`
}

// dacBindingView is output.player.device with the serial as its label, and
// every other field as written only when it holds a value that field
// takes: anything else is "that value" (an endpoint number, null), as in
// the config's refusal, since a serial pasted into the wrong field must not
// be printed.
type dacBindingView struct {
	Binding     string `json:"binding"`
	VendorID    string `json:"vendorId"`
	ProductID   string `json:"productId"`
	SerialLabel string `json:"serial"`
	Port        string `json:"port"`
	PCMDevice   *int   `json:"pcmDevice"`
	Subdevice   *int   `json:"subdevice"`
}

// dacBindingOf is d's view.
func dacBindingOf(d config.PlayerDevice) *dacBindingView {
	const refused = "that value"
	v := &dacBindingView{Binding: refused, SerialLabel: d.Serial.Label(), VendorID: d.VendorID, ProductID: d.ProductID, Port: d.Port}
	switch d.Binding {
	case config.BindingSerial, config.BindingUniqueModel, config.BindingPort:
		v.Binding = d.Binding
	}
	for _, id := range []*string{&v.VendorID, &v.ProductID} {
		if n, err := usbid.ParseHex16(*id); err == nil {
			*id = usbid.Hex16(n)
		} else if *id != "" {
			*id = refused
		}
	}
	if _, err := usbid.ParsePort(v.Port); err != nil && v.Port != "" {
		v.Port = refused
	}
	endpoint := func(n int) *int {
		if n < 0 || n > 255 {
			return nil
		}
		return &n
	}
	v.PCMDevice, v.Subdevice = endpoint(d.PCMDevice), endpoint(d.Subdevice)
	return v
}

// endpoint words the view's endpoint for a person.
func (v dacBindingView) endpoint() string {
	if v.PCMDevice == nil || v.Subdevice == nil {
		return "an endpoint out of range"
	}
	return binding.Endpoint{Device: *v.PCMDevice, Subdevice: *v.Subdevice}.String()
}

// dacResolutionView is how the binding resolves.
type dacResolutionView struct {
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Card    *int   `json:"card"`
	Matches []int  `json:"matches"`
}

// dacCardView is one card's identity, the serial as its label.
type dacCardView struct {
	Index       int      `json:"index"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	State       string   `json:"state"`
	Why         string   `json:"why"`
	USBID       *string  `json:"usbId"`
	SerialLabel *string  `json:"serial"`
	Port        string   `json:"port"`
	PortProblem string   `json:"portProblem"`
	Interface   *int     `json:"interface"`
	Playback    []string `json:"playback"`
}

// dacVerifyView is --verify-card's answer.
type dacVerifyView struct {
	Card   int    `json:"card"`
	Passes bool   `json:"passes"`
	Says   string `json:"says"`
}

// runDACInspect is `bridge output dac inspect`: each card's identity, how
// output.player.device resolves or why it does not, the player's state, its
// UUID and its LAN state. It opens nothing. --verify-card N runs the check
// after open against card N's sysfs reading without opening its PCM.
//
// Exit codes: 0 the report was printed (and --verify-card passed), 1 there
// is nothing to read or --verify-card refused, 2 usage error.
func runDACInspect(args []string, env outputBindEnv, stdout, stderr io.Writer) int {
	const cmd = "output dac inspect"
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", configFlagUsage)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	verify := fs.Int("verify-card", -1, "run the check after open against card `N`'s sysfs reading, without opening its PCM")
	trees := addDACTreeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", cmd, fs.Arg(0))
		return 2
	}
	obs, ok := trees.observer(env, cmd, stderr)
	if !ok {
		return 1
	}
	inv, err := obs.Observe()
	if err != nil {
		reportDACReadError(stderr, err)
		return 1
	}
	rep := dacInspect(env, *configPath, inv)
	code := 0
	if *verify >= 0 {
		rep.Verify = dacVerify(obs, rep.resolved, *verify, rep.Binding != nil)
		if !rep.Verify.Passes {
			code = 1
		}
	}
	if *asJSON {
		if c := writeJSONIndent(stdout, stderr, cmd, rep); c != 0 {
			return c
		}
		return code
	}
	renderDACInspect(stdout, rep)
	return code
}

// dacInspect builds inspect's report over inv.
func dacInspect(env outputBindEnv, configPath string, inv binding.Inventory) dacInspectReport {
	rep := dacInspectReport{ReadAt: stampOf(env.now()), Cards: []dacCardView{}, Problems: append([]string{}, inv.Problems...)}
	rep.Resolution = dacResolutionView{Matches: []int{}}
	cfg, path, err := loadCLIConfig(configPath)
	rep.Config = path
	if err != nil {
		rep.ConfigProblem = err.Error()
		rep.Player.Reason = "no config is read, so there is no player"
		rep.Resolution.State, rep.Resolution.Reason = string(binding.StateNotConfigured), "no config is read: bridge output dac bind needs one"
	} else {
		p := cfg.Output.Player
		on, why := cfg.PlayerEnabled(env.goos)
		rep.Player.Enabled = on
		switch {
		case why != nil:
			rep.Player.Reason = why.Error()
		case !on:
			rep.Player.Reason = "output.player.enabled is not true"
		}
		if id, err := config.ReadPlayerUUID(cfg.DataDir); err == nil {
			rep.Player.UUID = &id
		} else if errors.Is(err, fs.ErrNotExist) {
			rep.Player.UUIDNot = "not made yet: bind makes it once the player is enabled, and so will the player's first start"
		} else {
			rep.Player.UUIDNot = err.Error()
		}
		d := p.Device
		if d.Configured() {
			rep.Binding = dacBindingOf(d)
		}
		if err := p.Unread(); err != nil {
			rep.Resolution.State, rep.Resolution.Reason = string(binding.StateBindingInvalid), err.Error()
		} else {
			r := binding.Resolve(d, inv)
			rep.resolved = r
			rep.Resolution.State, rep.Resolution.Reason = string(r.State), r.Reason
			if r.Card >= 0 {
				c := r.Card
				rep.Resolution.Card = &c
			}
			if r.Matches != nil {
				rep.Resolution.Matches = r.Matches
			}
		}
	}
	rl := lan.Check(env.lan)
	rep.LAN = dacLANView{State: rl.State, Reason: rl.Reason, Interfaces: []dacLANInterface{}}
	for _, e := range rl.Interfaces {
		li := dacLANInterface{Name: e.Name, Addrs: []string{}}
		for _, a := range e.Addrs {
			li.Addrs = append(li.Addrs, a.String())
		}
		rep.LAN.Interfaces = append(rep.LAN.Interfaces, li)
	}
	for _, c := range inv.Cards {
		v := dacCardView{Index: c.Index, ID: c.ID, Name: c.Name, State: string(c.State), Why: c.Why, Playback: []string{}}
		if u := c.USB; u != nil {
			id, label, iface := u.ID.String(), u.Serial.Label(), u.Interface
			v.USBID, v.SerialLabel, v.Interface = &id, &label, &iface
			v.Port, v.PortProblem = u.Port, u.PortProblem
		}
		for _, e := range c.Endpoints {
			v.Playback = append(v.Playback, e.String())
		}
		rep.Cards = append(rep.Cards, v)
	}
	return rep
}

// dacVerify is --verify-card: the check after open, run against the trees
// obs reads (card n's sysfs reading, and under a port binding /proc/asound's
// cards and the cards beside it) as if the kernel had opened card n's bound
// endpoint, with nothing opened.
func dacVerify(obs binding.Observer, r binding.Resolution, n int, bound bool) *dacVerifyView {
	v := &dacVerifyView{Card: n}
	switch {
	case !bound:
		v.Says = "there is no binding to check against: bridge output dac bind <card> --binding serial|uniqueModel|port"
		return v
	case r.State != binding.StateAvailable:
		v.Says = fmt.Sprintf("the binding does not resolve (%s): %s", r.State, r.Reason)
		return v
	}
	node := alsapcm.NodeInfo{Card: n, Device: r.Endpoint.Device, Subdevice: r.Endpoint.Subdevice, Playback: true}
	if err := (binding.Checker{Proc: obs.Proc, Sysfs: obs.Sysfs, Want: r}).Check(node); err != nil {
		v.Says = "refused: " + err.Error()
		return v
	}
	v.Passes = true
	is := "the bound unit"
	if r.Binding == config.BindingPort {
		is = "at the bound port"
	}
	v.Says = fmt.Sprintf("passes: card %d is %s, %s", n, is, binding.Describe(r.Identity))
	return v
}

// renderDACInspect prints inspect's report for a person.
func renderDACInspect(w io.Writer, rep dacInspectReport) {
	if rep.ConfigProblem != "" {
		fmt.Fprintf(w, "config: %s\n", rep.ConfigProblem)
	} else {
		fmt.Fprintf(w, "config: %s\n", rep.Config)
	}
	switch {
	case rep.Player.Enabled:
		fmt.Fprintln(w, "player: on")
	case rep.Player.Reason == "output.player.enabled is not true":
		fmt.Fprintln(w, "player: off (output.player.enabled is not true)")
	default:
		fmt.Fprintf(w, "player: off: %s\n", rep.Player.Reason)
	}
	if rep.Player.UUID != nil {
		fmt.Fprintf(w, "player uuid: %s\n", *rep.Player.UUID)
	} else if rep.ConfigProblem == "" {
		fmt.Fprintf(w, "player uuid: %s\n", rep.Player.UUIDNot)
	}
	if rep.LAN.State == lan.StateReady {
		var names []string
		for _, i := range rep.LAN.Interfaces {
			names = append(names, i.Name+" ("+strings.Join(i.Addrs, ", ")+")")
		}
		fmt.Fprintf(w, "LAN: ready on %s\n", strings.Join(names, ", "))
	} else {
		fmt.Fprintf(w, "LAN: %s: %s\n", rep.LAN.State, rep.LAN.Reason)
	}
	if b := rep.Binding; b != nil {
		ep := b.endpoint()
		switch b.Binding {
		case config.BindingSerial:
			fmt.Fprintf(w, "binding: serial %s:%s (%s), %s\n", b.VendorID, b.ProductID, b.SerialLabel, ep)
		case config.BindingUniqueModel:
			fmt.Fprintf(w, "binding: uniqueModel %s:%s, %s\n", b.VendorID, b.ProductID, ep)
		default:
			fmt.Fprintf(w, "binding: %s %s, %s\n", b.Binding, b.Port, ep)
		}
	} else {
		fmt.Fprintln(w, "binding: none")
	}
	fmt.Fprintf(w, "resolves: %s: %s\n", rep.Resolution.State, rep.Resolution.Reason)
	if v := rep.Verify; v != nil {
		fmt.Fprintf(w, "verify card %d: %s\n", v.Card, v.Says)
	}
	for _, p := range rep.Problems {
		fmt.Fprintf(w, "problem: %s\n", p)
	}
	for _, c := range rep.Cards {
		fmt.Fprintln(w)
		if c.ID == "" && c.Name == "" {
			fmt.Fprintf(w, "card %d: (not in /proc/asound)\n", c.Index)
		} else {
			fmt.Fprintf(w, "card %d: %s [%s]\n", c.Index, c.ID, c.Name)
		}
		if c.USBID != nil {
			fmt.Fprintf(w, "  identity:  usb %s, %s\n", *c.USBID, serialPhrase(*c.SerialLabel))
			port := c.Port
			if port == "" {
				port = "none: " + c.PortProblem
			}
			fmt.Fprintf(w, "  port:      %s, interface %d\n", port, *c.Interface)
		} else {
			fmt.Fprintf(w, "  identity:  %s: %s\n", c.State, c.Why)
		}
		if len(c.Playback) == 0 {
			fmt.Fprintln(w, "  playback:  none")
		} else {
			fmt.Fprintf(w, "  playback:  %s\n", strings.Join(c.Playback, ", "))
		}
	}
}

// serialPhrase words a serial's label for a card's identity line: its tail
// after "serial", or what the label says when there is no tail to show.
func serialPhrase(label string) string {
	if strings.HasPrefix(label, "…") {
		return "serial " + label
	}
	return label
}

// errBindConfigChanged is bind's save refused because the config file no
// longer holds the bytes bind read under the lock.
var errBindConfigChanged = errors.New("the config changed after bind read it")

// runDACBind is `bridge output dac bind <card> --binding
// serial|uniqueModel|port`: it writes the binding for card's unit into the
// config file's output.player.device, which Save writes 0600, keeping the
// block's other fields. The serial goes from sysfs to the file and is never
// printed. It refuses a binding that would not resolve to that card now,
// an ambiguous one above all, a player block that does not read (Save
// would write its old text back), and a data dir a running bridge holds,
// which reads output.player only when it starts, and whose console would
// then refuse every settings save over the file bind changed until it
// restarted. Once it holds the data dir's lock and then bridge.yaml's
// (config.LockFile), it reads the config again and saves that copy,
// refusing a data dir that moved in between. When the player is enabled it
// makes the player's UUID, the first time only.
//
// Exit codes: 0 the binding was written, 1 refused, 2 usage error.
func runDACBind(ctx context.Context, args []string, env outputBindEnv, stdout, stderr io.Writer) int {
	const cmd = "output dac bind"
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(stderr, "%s: the card comes first: bridge output dac bind <card> --binding serial|uniqueModel|port\n", cmd)
		return 2
	}
	card, err := strconv.Atoi(args[0])
	if err != nil || card < 0 || card > 255 {
		fmt.Fprintf(stderr, "%s: %q is not a card number (the N of /proc/asound/cardN)\n", cmd, args[0])
		return 2
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	kind := fs.String("binding", "", "what the binding names: serial (the unit), uniqueModel (the one unit of its model) or port (the port, whatever is in it)")
	pcm := fs.Int("pcm-device", 0, "the playback PCM device on the card: the D of pcmC<card>D<D>p (default: the card's first)")
	sub := fs.Int("subdevice", 0, "the playback substream of that device (default: the card's first)")
	configPath := fs.String("config", "", configFlagUsage)
	trees := addDACTreeFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", cmd, fs.Arg(0))
		return 2
	}
	switch *kind {
	case config.BindingSerial, config.BindingUniqueModel, config.BindingPort:
	default:
		fmt.Fprintf(stderr, "%s: --binding is serial, uniqueModel or port\n", cmd)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	obs, ok := trees.observer(env, cmd, stderr)
	if !ok {
		return 1
	}
	cfg, cfgPath, err := loadCLIConfig(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: config load: %v\n", cmd, err)
		return 2
	}
	unread := func(c *config.Config) bool {
		err := c.Output.Player.Unread()
		if err != nil {
			fmt.Fprintf(stderr, "%s: output.player in %s does not read: %v\n", cmd, cfgPath, err)
			fmt.Fprintln(stderr, "  Fix the block first: a bind would leave the file's own text in place of the binding.")
		}
		return err != nil
	}
	if unread(cfg) {
		return 1
	}
	if env.beforeLock != nil {
		env.beforeLock()
	}
	dataDir := cfg.DataDir
	release, fallback, refused := holdDataDirForMutation(dataDir, cmd, stderr)
	defer release()
	if refused {
		return 1
	}
	// bridge.yaml's own lock (config.LockFile, backlog B506), inside the
	// data dir's, the order every command that takes both keeps, and held
	// from the read below to the save, so no writer that takes it lands
	// between them.
	lock, err := config.LockFile(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
		return 1
	}
	defer lock.Unlock()
	// The binding is saved into the config as it stands under the locks,
	// not as it was read before them: a restore, a library add, or a
	// serve's last settings save before it stopped, that committed in
	// between would otherwise be written over with the older copy. The path
	// is the one resolved above, so this reads that file again. read is the
	// bytes this read parses, or older ones: a writer that takes no lock (an
	// edit, or a bridge older than the lock) can still save, so the save
	// asks just before its rename whether the file still holds them
	// (SaveIf), the second guard, which narrows that window to the instant
	// before the rename and does not close it.
	read, err := os.ReadFile(cfgPath)
	if err == nil {
		cfg, _, err = loadCLIConfig(cfgPath)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: config load: %v\n", cmd, err)
		return 1
	}
	if cfg.DataDir != dataDir {
		fmt.Fprintf(stderr, "%s: the config's data dir changed from %s to %s while bind took the lock on the first\n", cmd, dataDir, cfg.DataDir)
		fmt.Fprintln(stderr, "  Run bind again: it locks the data dir the config names.")
		return 1
	}
	if unread(cfg) {
		return 1
	}
	if probeAdminBesideTheLock(fallback, cfg) && refuseIfBridgeMayBeRunning(ctx, cfg, cmd, stderr) {
		return 1
	}
	inv, err := obs.Observe()
	if err != nil {
		reportDACReadError(stderr, err)
		return 1
	}
	ep := binding.Endpoint{Device: *pcm, Subdevice: *sub}
	if c, ok := inv.Card(card); ok && !set["pcm-device"] && !set["subdevice"] && len(c.Endpoints) > 0 {
		ep = c.Endpoints[0]
	}
	dev, err := binding.For(inv, card, *kind, ep)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmd, err)
		return 1
	}
	cfg.Output.Player.Device = dev
	if env.beforeSave != nil {
		env.beforeSave()
	}
	err = cfg.SaveIf(lock, func(ctx context.Context) error {
		if now, err := config.ReadFile(ctx, cfgPath); err != nil || !bytes.Equal(now, read) {
			return errBindConfigChanged
		}
		return nil
	})
	// The turn is over: what follows writes the player's identity, never the
	// config.
	lock.Unlock()
	switch {
	case errors.Is(err, errBindConfigChanged) || errors.Is(err, config.ErrChangedInTurn):
		// Bind's own comparison, or the one every save in a turn makes first
		// (against the file as the turn found it), which differ only by the
		// instant each read.
		fmt.Fprintf(stderr, "%s: %s changed after bind read it, so the binding was not written: run bind again\n", cmd, cfgPath)
		fmt.Fprintln(stderr, "  something saved it without bridge.yaml's lock (an edit, or a bridge older than the lock), and bind does not write over what it saved.")
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "%s: save config: %v\n", cmd, err)
		return 1
	}
	r := binding.Resolve(dev, inv)
	if dev.Binding == config.BindingPort {
		fmt.Fprintf(stdout, "bound card %d by port %s, whatever unit is in it: now %s, %s\n", card, dev.Port, binding.Describe(r.Identity), r.Endpoint)
		if r.Port.HasInterface() {
			fmt.Fprintf(stdout, "the unit in that port carries more than one card, so the binding records this card's configuration and interface, %d.%d, which choose it among them\n",
				r.Port.Config, r.Port.Interface)
		}
	} else {
		fmt.Fprintf(stdout, "bound card %d by %s: %s, %s\n", card, *kind, binding.Describe(r.Identity), r.Endpoint)
	}
	if dev.Serial.Present() {
		fmt.Fprintf(stdout, "wrote output.player.device to %s (0600); the serial is in that file and nowhere else\n", cfgPath)
	} else {
		fmt.Fprintf(stdout, "wrote output.player.device to %s (0600)\n", cfgPath)
	}
	// bind wrote the file with no serve on the data dir, and a serve reads
	// output.player when it starts: nothing reloads the file.
	fmt.Fprintln(stdout, "the binding applies when bridge serve next starts")
	if !cfg.Output.Player.Enabled {
		fmt.Fprintln(stdout, "output.player.enabled is not true: the binding waits for the player to be turned on")
		return 0
	}
	id, created, err := config.EnsurePlayerUUID(cfg.DataDir)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "%s: the player's UUID: %v\n", cmd, err)
		return 1
	case created:
		fmt.Fprintf(stdout, "player uuid: %s (made now, kept in %s)\n", id, config.PlayerUUIDFile)
	default:
		fmt.Fprintf(stdout, "player uuid: %s (kept)\n", id)
	}
	return 0
}
