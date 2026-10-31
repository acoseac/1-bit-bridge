package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/hotplug"
	"github.com/acoseac/1-bit-bridge/internal/player/openhome"
)

// The console's Player page (B601) reads serve's own player through
// admin.PlayerOutput, which consolePlayer is: one view per serve, over the
// boot plan, the bridgePlayer serve started, GET /v1/outputs' own view of it
// (bridgeOutput.Facts), the engine's snapshot and the live config the console
// hands it. It writes nothing (the console writes the config, through the live
// holder), starts nothing and dials nothing. Its reads of the box are the
// bridge's ALSA readers over hostTrees, bounded, one at a time.

// The selection tokens a DAC listing hands out: how long one is good for, and
// how many the view keeps.
const (
	consoleSelectionTTL = 10 * time.Minute
	consoleSelectionCap = 64
)

// The bounds of the view's reads: a reading of the box for a listing or a
// choice, and the lookup of the track heard in the library index.
const (
	consoleReadBound   = 3 * time.Second
	consoleLookupBound = 500 * time.Millisecond
)

// consoleTrackStore is what the view reads of the library index: the row of
// the track heard.
type consoleTrackStore interface {
	GetTrack(ctx context.Context, path string) (*manifest.Track, error)
}

// consolePlayerWiring is what serve hands the console's view of its player.
type consolePlayerWiring struct {
	// Plan is serve's boot decision about its player, and Player the player
	// it started (startServePlayer's, whether or not the plan starts it).
	Plan   servePlayerPlan
	Player *bridgePlayer
	// View is GET /v1/outputs' view of the player (newBridgeOutput).
	View *bridgeOutput
	// Boot is the boot snapshot of the config.
	Boot *config.Config
	// GOOS is the host the player runs on; "" is this one.
	GOOS string
	// LockHeld is serve holding its data directory's lock, and StateErr the
	// refusal of serve's preflight of the player's files at boot (nil for
	// none): with planServePlayer's other input, the live config, they are
	// what the next start decides from.
	LockHeld bool
	StateErr error
	// Trees are what a DAC listing reads; the zero value is hostTrees.
	Trees outputTrees
	// Store is the library index.
	Store consoleTrackStore
	// Now is the clock tokens age by; nil is time.Now.
	Now func() time.Time
}

// consolePlayer is the console's view of serve's player.
type consolePlayer struct {
	plan       servePlayerPlan
	p          *bridgePlayer
	view       *bridgeOutput
	bootDevice config.PlayerDevice
	// bootOpenHome is the boot snapshot's output.player.openhome, which
	// serve decided its OpenHome surface from: a live opt-in that differs
	// waits for the next start (pending).
	bootOpenHome bool
	goos         string
	lockHeld     bool
	stateErr     error
	trees        outputTrees
	store        consoleTrackStore
	now          func() time.Time
	readBound    time.Duration
	// observer reads the box: trees' own, or a test's with its seam.
	observer func() binding.Observer

	// slot is the one reading of the box under way: taken before a reading
	// starts and let go when it returns, which a read stuck in the kernel
	// may do after its caller has gone.
	slot chan struct{}

	mu         sync.Mutex
	selections map[string]consoleSelection
	// named is the last track heard the view named, by its occurrence.
	named consoleNamed
}

// consoleSelection is what a token stands for: the card a listing showed,
// its endpoint, its unit as that reading read it (its devnum included, kept
// in memory alone), and the binding of each kind that reading built for it.
// The serial stays here: the page sees its label.
type consoleSelection struct {
	card     int
	endpoint binding.Endpoint
	identity alsa.USBIdentity
	devices  map[string]config.PlayerDevice
	at       time.Time
}

// consoleNamed is the library's row for one occurrence heard.
type consoleNamed struct {
	occ                        engine.OccurrenceID
	path, title, artist, album string
	row                        bool
}

var _ admin.PlayerOutput = (*consolePlayer)(nil)

// newConsolePlayer is the console's view of serve's player.
func newConsolePlayer(w consolePlayerWiring) *consolePlayer {
	goos := w.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	now := w.Now
	if now == nil {
		now = time.Now
	}
	c := &consolePlayer{plan: w.Plan, p: w.Player, view: w.View, goos: goos, lockHeld: w.LockHeld, stateErr: w.StateErr,
		trees: w.Trees.or(hostTrees), store: w.Store, now: now, readBound: consoleReadBound,
		slot: make(chan struct{}, 1), selections: map[string]consoleSelection{}}
	c.observer = c.trees.observer
	if w.Boot != nil {
		c.bootDevice = w.Boot.Output.Player.Device
		c.bootOpenHome = w.Boot.Output.Player.OpenHome
	}
	return c
}

// Platform is the host the player runs on.
func (c *consolePlayer) Platform() string { return c.goos }

// Report is the player now: running, configured, pending and its transports.
func (c *consolePlayer) Report(ctx context.Context, live *config.Config) admin.PlayerReport {
	run, facts, snap := c.running(ctx, live)
	return admin.PlayerReport{
		Running:    run,
		Configured: c.configured(live),
		Pending:    c.pending(live, run.OpenHome),
		Transports: c.transports(run, facts, snap),
	}
}

// nextPlan is what the next start decides on live: planServePlayer's answer,
// or, after a boot whose preflight of the player's files refused, that
// refusal again, which only a start can find gone.
func (c *consolePlayer) nextPlan(live *config.Config) servePlayerPlan {
	if c.stateErr != nil {
		return servePlayerPlan{off: c.stateErr.Error(), told: true}
	}
	return planServePlayer(live, c.goos, c.lockHeld)
}

// configured is what the live config asks for: the player, its binding, and
// its OpenHome surface, by the port it would take and the name a controller
// would list it by (openhome.FriendlyName, the description's own).
func (c *consolePlayer) configured(live *config.Config) admin.PlayerConfigured {
	player := live.Output.Player
	conf := admin.PlayerConfigured{Enabled: player.Enabled, Binding: playerBindingView(player.Device),
		OpenHome: player.OpenHome, OpenHomePort: player.EffectiveOpenHomePort(), OpenHomeName: openhome.FriendlyName(player.Room)}
	next := c.nextPlan(live)
	switch {
	case next.start:
		conf.Starts = true
	case next.refused == refusedNotConfigured:
		conf.Reason = "no DAC is bound: choose one, and the player starts when the bridge restarts"
	case next.off != "":
		conf.Reason = next.off
	case !live.Output.Player.Enabled:
		conf.Reason = "the player is turned off"
	}
	return conf
}

// pending is what waits for the next start: the next plan beside the boot
// plan, and the OpenHome opt-in written beside the boot snapshot's, which
// serve decided its surface from (serving: the surface is on the LAN now).
func (c *consolePlayer) pending(live *config.Config, serving bool) []admin.PlayerPending {
	out := []admin.PlayerPending{}
	next := c.nextPlan(live)
	switch {
	case !c.plan.start && next.start:
		out = append(out, admin.PlayerPending{Change: "start",
			Text: "The player starts when the bridge restarts."})
	case c.plan.start && !next.start:
		text := "The player stops when the bridge restarts."
		if c.withheld(live) == string(binding.StateDisabled) {
			text = "The player is turned off: it plays nothing now, and it stops when the bridge restarts."
		}
		out = append(out, admin.PlayerPending{Change: "stop", Text: text})
	case c.plan.start && next.start && !hotplug.SameBinding(live.Output.Player.Device, c.bootDevice):
		out = append(out, admin.PlayerPending{Change: "dac",
			Text: "The player opens the DAC chosen here when the bridge restarts. Until then it plays nothing: the DAC it started with is no longer the one the config names."})
	}
	if player := live.Output.Player; player.OpenHome != c.bootOpenHome {
		var text string
		switch {
		case player.OpenHome && next.start:
			text = "Casting from the 1-bit app is turned on: the bridge puts the player on the LAN as " + openhome.FriendlyName(player.Room) + " when it restarts."
		case player.OpenHome:
			text = "Casting from the 1-bit app is turned on: the bridge puts the player on the LAN as " + openhome.FriendlyName(player.Room) + " when it restarts with the player on."
		case serving:
			text = "Casting from the 1-bit app is turned off: the bridge takes the player off the LAN when it restarts. Until then the 1-bit app can still cast to it."
		default:
			text = "Casting from the 1-bit app is turned off: the bridge leaves the player off the LAN when it restarts."
		}
		out = append(out, admin.PlayerPending{Change: "openHome", Text: text})
	}
	return out
}

// openHomeDetail is why the OpenHome Source the player started with is not
// on the LAN now, "" while it is (serving: GET /v1/outputs' view lists it)
// and for a player serve started without its opt-in, or did not start (its
// own status says why). It reads ready before eng, and the surface under the
// player's mu, as GET /v1/outputs' view does, and the surface's status, which
// copies. None of its reasons is one a restart cures, and the page offers no
// restart for it.
func (c *consolePlayer) openHomeDetail(serving bool) string {
	if serving || !c.plan.openhome || c.p == nil {
		return ""
	}
	if !signalled(c.p.ready) {
		return "The player is starting, and its OpenHome source starts with it."
	}
	if c.p.eng == nil {
		return "The player did not start, so neither did its OpenHome source."
	}
	c.p.mu.Lock()
	s := c.p.surface
	c.p.mu.Unlock()
	if s == nil {
		return "Its OpenHome source did not start, and the player runs without it: the bridge's log says why."
	}
	st := s.Status()
	switch {
	case st.Stopped:
		return "Its OpenHome source has stopped: the bridge is stopping."
	case len(st.Failed) > 0:
		return "Its OpenHome source could not listen on the box's LAN address: the bridge's log says why, and it tries again every few seconds."
	case st.ListFailure != "":
		return "Its OpenHome source could not list the box's addresses: the bridge's log says why, and it tries again every few seconds."
	case st.Reconciles == 0:
		return "Its OpenHome source is looking for the box's LAN addresses."
	}
	return "The box has no LAN address for its OpenHome source yet (an interface that is up, with a private IPv4 address): it goes on the LAN once one comes up."
}

// withheld is why the live config withholds the running player's output
// now, as serve's player reads it (playerOutput.withheld): turned off first,
// then another binding than the one it started with; "" for neither, and for
// a player serve did not start.
func (c *consolePlayer) withheld(live *config.Config) string {
	if !c.plan.start || live == nil {
		return ""
	}
	set := hotplug.SettingsFrom(live, c.goos)
	switch {
	case !set.Enabled:
		return string(binding.StateDisabled)
	case !hotplug.SameBinding(set.Device, c.bootDevice):
		return reasonBindingChanged
	}
	return ""
}

// engine is the running player's engine, nil before it runs and for a player
// that did not start: read after ready, as GET /v1/outputs' view reads it.
func (c *consolePlayer) engine() *engine.Engine {
	if c.p == nil || !c.plan.start || !signalled(c.p.ready) {
		return nil
	}
	return c.p.eng
}

// running is the player serve runs, and the facts and the engine snapshot it
// was read from (zero and nil for a player that did not start).
func (c *consolePlayer) running(ctx context.Context, live *config.Config) (admin.PlayerRunning, api.BridgeOutputFacts, *engine.Snapshot) {
	run := admin.PlayerRunning{Started: c.plan.start, Binding: playerBindingView(c.bootDevice)}
	if !c.plan.start {
		run.State, run.Reason = "off", c.plan.refused
		switch {
		case c.plan.refused == refusedNotConfigured:
			run.Detail = "No DAC was bound when the bridge started."
		case c.plan.off != "":
			run.Detail = c.plan.off
		default:
			run.Detail = "The player was turned off when the bridge started."
		}
		return run, api.BridgeOutputFacts{}, nil
	}
	run.Withheld = c.withheld(live)
	var facts api.BridgeOutputFacts
	if c.view != nil {
		facts = c.view.Facts(ctx)
	}
	run.State, run.Reason, run.LastError = facts.State, facts.Reason, facts.LastError
	if !facts.Listed {
		// No identity read yet: the start has not made it, or failed first.
		run.State, run.Reason = bridgeStateUnavailable, reasonStarting
		if signalled(c.p.ready) && c.p.eng == nil {
			run.Reason = reasonStartFailed
		}
	}
	run.OpenHome = facts.OpenHome != nil
	run.OpenHomeDetail = c.openHomeDetail(run.OpenHome)
	run.Detail = c.reasonText(run.Reason)
	run.DAC = c.dac(facts)
	run.Signal = playerSignalView(facts.Playback)
	eng := c.engine()
	if eng == nil {
		return run, facts, nil
	}
	snap := eng.Snapshot()
	run.Transport = engineTransportWord(snap)
	run.Track = c.track(ctx, snap)
	return run, facts, snap
}

// reasonText is the sentence beside an unavailable player's word.
func (c *consolePlayer) reasonText(reason string) string {
	switch reason {
	case "":
		return ""
	case reasonStarting:
		return "The player is starting."
	case reasonStartFailed:
		if signalled(c.p.ready) && c.p.failed != "" {
			return "The player did not start: " + c.p.failed
		}
		return "The player did not start; the bridge's log says why."
	case reasonClosed:
		return "The player's engine has stopped; it runs again when the bridge restarts."
	case reasonOutputUnknown:
		return "The player has not heard yet whether its DAC is there."
	case string(binding.StateDisabled):
		return "The player is turned off: it plays nothing now, and it stops when the bridge restarts."
	case reasonBindingChanged:
		return "The config names another DAC than the one the player started with: it plays nothing until the bridge restarts, or until that DAC is chosen again."
	case reasonMaintenance:
		return "The player is held for maintenance."
	case reasonReleaseUnconfirmed:
		return "The DAC's last release was never confirmed, so nothing opens it until it is."
	}
	return "The DAC is not available (" + reason + ")."
}

// dac is the running player's DAC: how the watcher last resolved the boot
// binding, and the card GET /v1/outputs projects while it is available.
func (c *consolePlayer) dac(facts api.BridgeOutputFacts) *admin.PlayerDAC {
	d := &admin.PlayerDAC{PCMBitDepths: []int{}, NativeDSDRates: []int{}, DSDFormats: []string{}}
	if c.p != nil && c.p.feed != nil {
		if s, ok := c.p.feed.Latest(); ok && s.Valid() && hotplug.SameBinding(s.Binding(), c.bootDevice) {
			res := s.Resolution()
			d.State, d.Reason = string(res.State), res.Reason
			_, d.Stale = s.Stale()
			if res.State == binding.StateAvailable || res.State == binding.StateEndpointMissing {
				d.USBID = res.Identity.ID.String()
			}
		}
	}
	if d.State == "" {
		d.Reason = "The player's DAC watcher has not read the box yet"
	}
	if dac := facts.DAC; dac != nil {
		d.Name, d.USBID = dac.Name, dac.USBID
		caps := dac.Capabilities
		d.MaxPCMRate = caps.MaxPCMRate
		d.PCMBitDepths = append(d.PCMBitDepths, caps.PCMBitDepths...)
		d.NativeDSDRates = append(d.NativeDSDRates, caps.NativeDSDRates...)
		d.DSDFormats = append(d.DSDFormats, caps.DSDFormats...)
	}
	return d
}

// track is the occurrence heard: its entry, the decoder's report, what the
// DAC has played of it, and the library's row for its reference, looked up
// once an occurrence; nil while nothing is heard.
func (c *consolePlayer) track(ctx context.Context, snap *engine.Snapshot) *admin.PlayerTrack {
	t, ok := snap.Track()
	if !ok {
		return nil
	}
	out := &admin.PlayerTrack{Entry: t.Entry}
	if t.HasDetails {
		d := t.Details
		out.Codec, out.Bits, out.RateHz, out.Channels = d.Codec, d.Bits, d.RateHz, d.Channels
		if d.RateHz > 0 && d.Frames > 0 {
			out.Duration = float64(d.Frames) / float64(d.RateHz)
		}
	}
	if pos := snap.Position(); pos.Occurrence == t.Occurrence {
		out.Position = pos.Seconds()
	}
	if n, ok := c.name(ctx, t); ok {
		out.Named, out.Path, out.Title, out.Artist, out.Album = n.row, n.path, n.title, n.artist, n.album
	}
	return out
}

// name is the library's row for the track heard, through the player's own
// resolver's index (library.Resolver.StoredPath, which opens nothing) and the
// store, looked up once an occurrence and within consoleLookupBound; false
// when it cannot be had now (a lookup that failed is asked again next time).
// The controller's metadata is never read for it.
func (c *consolePlayer) name(ctx context.Context, t engine.Track) (consoleNamed, bool) {
	c.mu.Lock()
	if c.named.occ == t.Occurrence && c.named.occ != 0 {
		n := c.named
		c.mu.Unlock()
		return n, true
	}
	c.mu.Unlock()
	res := c.p.res
	if res == nil || c.store == nil {
		return consoleNamed{}, false
	}
	lctx, cancel := context.WithTimeout(ctx, consoleLookupBound)
	defer cancel()
	path, err := res.StoredPath(lctx, t.Ref)
	if err != nil {
		return consoleNamed{}, false
	}
	n := consoleNamed{occ: t.Occurrence, path: path}
	row, err := c.store.GetTrack(lctx, path)
	if err != nil {
		return consoleNamed{}, false
	}
	if row != nil {
		n.row, n.title, n.artist, n.album = true, row.Title, row.Artist, row.Album
	}
	c.mu.Lock()
	c.named = n
	c.mu.Unlock()
	return n, true
}

// transports is each transport's standing on the running player's DAC: what
// it plays now, from the three facts it rests on (the DAC's altsets, the mode
// the player runs with, and the backend's published epoch of approved
// tuples), never one of them alone, and only while the player's output takes
// playback (outputOpen).
func (c *consolePlayer) transports(run admin.PlayerRunning, facts api.BridgeOutputFacts, snap *engine.Snapshot) []admin.PlayerTransport {
	running := snap != nil
	open, closedBy := outputOpen(run, snap)
	dac := run.DAC
	known := dac != nil && dac.Name != ""
	capable := func(ok bool) string {
		switch {
		case !known:
			return "unknown"
		case ok:
			return "yes"
		}
		return "no"
	}
	mode := c.bootDSDMode()
	allowDoP := c.bootAllowDoP()
	if facts.Listed {
		mode, allowDoP = facts.DSDMode, facts.AllowDoP
	}
	pcm := admin.PlayerTransport{Kind: "pcm", Plays: open, Approved: []string{}, Mode: "always on: PCM needs no approval",
		Note: "PCM plays bit-exact, as the file holds it: nothing is resampled, converted or mixed."}
	dop := admin.PlayerTransport{Kind: "dop", Approved: []string{}, Mode: "dsdMode " + mode + ", allowDoP " + fmt.Sprint(allowDoP)}
	native := admin.PlayerTransport{Kind: "native", Approved: []string{}, Mode: "dsdMode " + mode}
	if known {
		pcm.Capable = capable(dac.MaxPCMRate > 0)
		deep := false
		for _, b := range dac.PCMBitDepths {
			deep = deep || b >= 24
		}
		dop.Capable = capable(dac.MaxPCMRate >= 176400 && deep)
		native.Capable = capable(len(dac.NativeDSDRates) > 0)
	} else {
		pcm.Capable, dop.Capable, native.Capable = "unknown", "unknown", "unknown"
	}
	dop.Note = "DoP travels in 24-bit PCM at 176.4 kHz or more; whether the DAC decodes it is what an owner's listening qualification establishes."
	native.Note = "Native DSD is the DAC's own DSD format, at the rates its altsets list."
	if p := facts.DSDPlayback; running && p != nil {
		for _, t := range p.Tuples {
			if t.Transport == "dop" {
				dop.Approved = append(dop.Approved, dsdTupleText(t))
			} else {
				native.Approved = append(native.Approved, dsdTupleText(t))
			}
		}
		if len(p.Tuples) == 0 && p.Reason != "" {
			dop.Note += " No approval is in force (" + p.Reason + ")."
			native.Note += " No approval is in force (" + p.Reason + ")."
		}
	}
	dop.Plays, native.Plays = open && len(dop.Approved) > 0, open && len(native.Approved) > 0
	prefix := ""
	switch {
	case !running:
		prefix = "The player is not running, so nothing plays. "
	case !open:
		prefix = "The player's output is not available now (" + closedBy + "), so nothing plays. "
	}
	for _, t := range []*admin.PlayerTransport{&pcm, &dop, &native} {
		t.Note = prefix + t.Note
	}
	return []admin.PlayerTransport{pcm, dop, native}
}

// outputOpen reports whether the running player's output takes playback now,
// and the word for why not: the live config withholds nothing (the player
// turned off, another DAC chosen), GET /v1/outputs' element is not
// unavailable, and the engine's output report is available. An engine runs on
// while its output is withheld or its DAC is gone, and plays nothing then.
func outputOpen(run admin.PlayerRunning, snap *engine.Snapshot) (bool, string) {
	switch {
	case snap == nil:
		return false, ""
	case run.Withheld != "":
		return false, run.Withheld
	case run.State == bridgeStateUnavailable:
		return false, run.Reason
	case !snap.Output().Known:
		return false, reasonOutputUnknown
	case !snap.Output().Available:
		if r := snap.Output().Reason; r != "" {
			return false, r
		}
		return false, reasonOutputUnavailable
	}
	return true, ""
}

// bootDSDMode and bootAllowDoP are the boot snapshot's DSD settings, which
// the player runs with.
func (c *consolePlayer) bootDSDMode() string {
	if c.view != nil {
		return c.view.dsdMode
	}
	return "off"
}

func (c *consolePlayer) bootAllowDoP() bool { return c.view != nil && c.view.allowDoP }

// dsdTupleText words an approved tuple for the page: its rate by the DSD
// family's name, and its format.
func dsdTupleText(t api.OutputDSDTuple) string {
	name := fmt.Sprintf("%d bit/s", t.BitRate)
	switch {
	case t.BitRate > 0 && t.BitRate%44100 == 0:
		name = fmt.Sprintf("DSD%d", t.BitRate/44100)
	case t.BitRate > 0 && t.BitRate%48000 == 0:
		name = fmt.Sprintf("DSD%d (48 kHz family)", t.BitRate/48000)
	}
	return name + " " + t.Format
}

// playerSignalView is what the DAC receives, from the backend's observation.
func playerSignalView(p *api.OutputPlayback) *admin.PlayerSignal {
	if p == nil {
		return nil
	}
	transport := p.Transport
	if transport == "" {
		transport = "native"
	}
	return &admin.PlayerSignal{Transport: transport, Format: p.Format, RateHz: p.RateHz, Channels: p.Channels, ValidBits: p.ValidBits, State: p.State}
}

// engineTransportWord is the engine's state as the page words it: starting
// for a session nothing of which is audible yet.
func engineTransportWord(s *engine.Snapshot) string {
	if s.State() == engine.Playing && s.Starting() {
		return "starting"
	}
	return s.State().String()
}

// sounding reports whether the engine plays, starts, or holds a pause, or is
// releasing on its way to one: what a withheld output stops.
func sounding(s *engine.Snapshot) bool {
	switch s.State() {
	case engine.Playing, engine.Paused:
		return true
	case engine.Releasing:
		return s.Pending() == engine.IntentPlay || s.Pending() == engine.IntentPause
	}
	return false
}

// playerBindingView is a binding as the page shows it, the serial as its
// label; nil for no binding.
func playerBindingView(d config.PlayerDevice) *admin.PlayerBinding {
	if !d.Configured() {
		return nil
	}
	b := dacBindingOf(d)
	v := &admin.PlayerBinding{Kind: b.Binding, Port: b.Port, Endpoint: b.endpoint()}
	if b.VendorID != "" || b.ProductID != "" {
		v.USBID = b.VendorID + ":" + b.ProductID
	}
	if d.Serial.Present() {
		v.SerialLabel = d.Serial.Label()
	}
	return v
}

// SameBinding reports whether a and b name the same thing, the transport
// usb whether or not written: a choice of what the config names already
// writes nothing.
func (c *consolePlayer) SameBinding(a, b config.PlayerDevice) bool {
	return hotplug.SameBinding(usbTransport(a), usbTransport(b))
}

// usbTransport is d with its transport written: "" is usb.
func usbTransport(d config.PlayerDevice) config.PlayerDevice {
	if d.Transport == "" && d.Configured() {
		d.Transport = "usb"
	}
	return d
}

// Sounding reports whether the running player's engine sounds now (sounding),
// false for a player that does not run. The console's writes ask it in their
// config turn, before the save that serve's player withholds its output on.
func (c *consolePlayer) Sounding() bool {
	eng := c.engine()
	return eng != nil && sounding(eng.Snapshot())
}

// Withholds reports whether a change of the live config from before to after
// withholds the running player's output where before did not, as serve's
// player hears it (withheld).
func (c *consolePlayer) Withholds(before, after *config.Config) bool {
	return c.withheld(before) == "" && c.withheld(after) != ""
}

// DACs lists the box's playback cards from one reading: each USB card's
// playback endpoints, with the kinds of binding that name it now and a
// selection token where one does, and every other card with why it cannot be
// bound.
func (c *consolePlayer) DACs(ctx context.Context, live *config.Config) admin.PlayerDACList {
	list := admin.PlayerDACList{Cards: []admin.PlayerDACCard{}}
	inv, err := c.read(ctx)
	if err != nil {
		list.Problem = err.Error()
		return list
	}
	list.Read = true
	var liveRes, bootRes binding.Resolution
	if d := live.Output.Player.Device; d.Configured() && live.Output.Player.Unread() == nil {
		liveRes = binding.Resolve(d, inv)
	}
	if c.plan.start {
		bootRes = binding.Resolve(c.bootDevice, inv)
	}
	names := func(r binding.Resolution, card int, ep binding.Endpoint) bool {
		return r.State == binding.StateAvailable && r.Card == card && r.Endpoint == ep
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	for _, card := range inv.Cards {
		row := admin.PlayerDACCard{Card: card.Index, ID: card.ID, Name: card.Name, Kinds: []admin.PlayerDACKind{}}
		if card.State != binding.CardUSB {
			row.Why = card.Why
			list.Cards = append(list.Cards, row)
			continue
		}
		u := *card.USB
		row.ID, row.Name = playerDACText(card.ID, u), playerDACText(card.Name, u)
		row.USBID, row.SerialLabel, row.Port = u.ID.String(), u.Serial.Label(), u.Port
		if len(card.Endpoints) == 0 {
			row.Why = "it has no playback stream in /proc/asound"
			list.Cards = append(list.Cards, row)
			continue
		}
		for _, ep := range card.Endpoints {
			r := row
			r.Endpoint = ep.String()
			r.Kinds = []admin.PlayerDACKind{}
			devices := map[string]config.PlayerDevice{}
			for _, kind := range []string{config.BindingSerial, config.BindingUniqueModel, config.BindingPort} {
				dev, err := binding.For(inv, card.Index, kind, ep)
				k := admin.PlayerDACKind{Kind: kind, Available: err == nil}
				if err != nil {
					k.Why = err.Error()
				} else {
					devices[kind] = dev
					if r.Recommended == "" {
						r.Recommended = kind
					}
				}
				r.Kinds = append(r.Kinds, k)
			}
			r.Configured, r.Running = names(liveRes, card.Index, ep), names(bootRes, card.Index, ep)
			switch {
			case len(devices) == 0:
				r.Why = "no binding names it now"
			default:
				token := newSelectionToken()
				c.selections[token] = consoleSelection{card: card.Index, endpoint: ep, identity: u, devices: devices, at: now}
				r.Selection = token
			}
			list.Cards = append(list.Cards, r)
		}
	}
	c.trimLocked()
	return list
}

// pruneLocked drops the tokens past their time.
func (c *consolePlayer) pruneLocked(now time.Time) {
	for t, s := range c.selections {
		if now.Sub(s.at) > consoleSelectionTTL || now.Before(s.at) {
			delete(c.selections, t)
		}
	}
}

// trimLocked keeps the newest consoleSelectionCap tokens.
func (c *consolePlayer) trimLocked() {
	if len(c.selections) <= consoleSelectionCap {
		return
	}
	type aged struct {
		token string
		at    time.Time
	}
	all := make([]aged, 0, len(c.selections))
	for t, s := range c.selections {
		all = append(all, aged{t, s.at})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.After(all[j].at) })
	for _, a := range all[consoleSelectionCap:] {
		delete(c.selections, a.token)
	}
}

// newSelectionToken is 128 random bits, in hexadecimal.
func newSelectionToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on the platforms the bridge builds for
	}
	return hex.EncodeToString(b[:])
}

// Chosen is the binding a token and a kind name now: the token's card, read
// again, must be the same unit at the same enumeration, and the binding of
// that kind a fresh reading builds for it the one the listing offered. A
// choice of the binding the player started with is that binding as the boot
// snapshot writes it, so serve's player hears its own binding again.
func (c *consolePlayer) Chosen(ctx context.Context, selection, kind string) (config.PlayerDevice, error) {
	c.mu.Lock()
	sel, ok := c.selections[selection]
	if ok && (c.now().Sub(sel.at) > consoleSelectionTTL || c.now().Before(sel.at)) {
		delete(c.selections, selection)
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return config.PlayerDevice{}, fmt.Errorf("%w: the bridge holds no such choice, or it is older than %v", admin.ErrPlayerSelectionStale, consoleSelectionTTL)
	}
	want, ok := sel.devices[kind]
	if !ok {
		return config.PlayerDevice{}, fmt.Errorf("%w: a %s binding did not name card %d when the DACs were listed", admin.ErrPlayerBindingRefused, kind, sel.card)
	}
	inv, err := c.read(ctx)
	if err != nil {
		return config.PlayerDevice{}, err
	}
	card, ok := inv.Card(sel.card)
	if !ok || card.State != binding.CardUSB || !sameEnumeration(*card.USB, sel.identity) {
		return config.PlayerDevice{}, fmt.Errorf("%w: card %d is no longer the DAC the list showed (it was unplugged, plugged in again, or another took its number)",
			admin.ErrPlayerSelectionStale, sel.card)
	}
	dev, err := binding.For(inv, sel.card, kind, sel.endpoint)
	if err != nil {
		return config.PlayerDevice{}, fmt.Errorf("%w: %v", admin.ErrPlayerBindingRefused, err)
	}
	if !hotplug.SameBinding(dev, want) {
		return config.PlayerDevice{}, fmt.Errorf("%w: the %s binding the box gives card %d now is not the one the list showed", admin.ErrPlayerSelectionStale, kind, sel.card)
	}
	if c.plan.start && c.SameBinding(dev, c.bootDevice) {
		dev = c.bootDevice
	}
	return dev, nil
}

// sameEnumeration reports whether two readings of a card are one unit at one
// enumeration: binding.SameUnit, and the same devnum, read in both.
func sameEnumeration(a, b alsa.USBIdentity) bool {
	return binding.SameUnit(a, b) && a.DevNum > 0 && a.DevNum == b.DevNum
}

// consoleReading is one reading's answer.
type consoleReading struct {
	inv binding.Inventory
	err error
}

// read takes one reading of the box within consoleReadBound: one at a time,
// a reading still under way from an earlier call holding the next back, and
// one stuck in the kernel abandoned to return on its own, its slot let go
// then. Its errors are admin.ErrPlayerBoxUnread's.
func (c *consolePlayer) read(ctx context.Context) (binding.Inventory, error) {
	ctx, cancel := context.WithTimeout(ctx, c.readBound)
	defer cancel()
	select {
	case c.slot <- struct{}{}:
	case <-ctx.Done():
		return binding.Inventory{}, fmt.Errorf("%w: an earlier reading of the box has not returned yet: try again", admin.ErrPlayerBoxUnread)
	}
	done := make(chan consoleReading, 1)
	rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), c.readBound)
	go func() {
		defer func() { <-c.slot }()
		defer rcancel()
		defer func() {
			if r := recover(); r != nil {
				logPlayerPanic("the console's reading of the box", r)
				done <- consoleReading{err: fmt.Errorf("%w: the reading failed; the bridge's log has why", admin.ErrPlayerBoxUnread)}
			}
		}()
		inv, err := observeBracketed(rctx, c.observer())
		done <- consoleReading{inv, err}
	}()
	select {
	case r := <-done:
		return r.inv, r.err
	case <-ctx.Done():
		return binding.Inventory{}, fmt.Errorf("%w: the reading did not return within %v", admin.ErrPlayerBoxUnread, c.readBound)
	}
}

// observeBracketed is one reading of both trees, bracketed by a reading of
// sysfs's identities before /proc is read, as GET /v1/outputs' DAC read is
// (readPlayerDACObserved): a card whose unit or enumeration moved between the
// two is set aside as transient, since /proc's endpoints may be another
// unit's, and so is one whose enumeration (devnum) either reading could not
// read, which nothing then shows did not move; DACs offers a transient card
// no token. Its /proc id and name, which the list shows, are redacted of the
// serials and ports of both readings' units. A reading that is not whole (a
// card listing cut short, a tree read in part) is no reading.
func observeBracketed(ctx context.Context, obs binding.Observer) (binding.Inventory, error) {
	before := obs.Sysfs.ReadContext(ctx)
	inv, _, err := obs.ObserveWithProc(ctx)
	switch {
	case err != nil:
		return binding.Inventory{}, fmt.Errorf("%w: %v", admin.ErrPlayerBoxUnread, err)
	case !before.Present || !inv.Sysfs:
		return binding.Inventory{}, fmt.Errorf("%w: sysfs cannot be read, so no card's unit can be told", admin.ErrPlayerBoxUnread)
	case !before.CardsComplete || before.Partial || !inv.CardsComplete || inv.Partial:
		why := strings.Join(append(append([]string{}, before.Problems...), inv.Problems...), "; ")
		if why == "" {
			why = "a listing was cut short"
		}
		return binding.Inventory{}, fmt.Errorf("%w: the reading was not whole (%s): try again", admin.ErrPlayerBoxUnread, why)
	}
	earlier := map[int]alsa.Identity{}
	for _, id := range before.Cards {
		earlier[id.Card] = id
	}
	for i, card := range inv.Cards {
		if card.State != binding.CardUSB {
			continue
		}
		id, ok := earlier[card.Index]
		before := ok && !id.Vanished && id.Kind == alsa.IdentityUSB && id.USB != nil
		if before && sameEnumeration(*id.USB, *card.USB) {
			continue
		}
		why := "it changed while the box was read: list again"
		if before && binding.SameUnit(*id.USB, *card.USB) && (id.USB.DevNum <= 0 || card.USB.DevNum <= 0) {
			why = "its enumeration (sysfs's devnum) cannot be read, so a choice of it could not be checked against the box again"
		}
		text := func(s string) string {
			if before {
				s = playerDACText(s, *id.USB)
			}
			return playerDACText(s, *card.USB)
		}
		inv.Cards[i].ID, inv.Cards[i].Name = text(card.ID), text(card.Name)
		inv.Cards[i].State, inv.Cards[i].Why = binding.CardTransient, why
		inv.Cards[i].SysfsUSB, inv.Cards[i].USB = card.USB, nil
	}
	return inv, nil
}
