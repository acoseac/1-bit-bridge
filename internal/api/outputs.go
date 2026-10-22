package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
	"github.com/acoseac/1-bit-bridge/internal/mpd/conf"
)

const (
	outputIdle       = 60 * time.Second
	outputCacheTTL   = 500 * time.Millisecond
	outputProbeHit   = 30 * time.Second
	outputProbeMiss  = 5 * time.Second
	outputProbeLimit = 750 * time.Millisecond
	outputGreetLimit = 300 * time.Millisecond
	outputBuildLimit = 15 * time.Second
)

// OutputsResponse is GET /v1/outputs. Outputs is never null. A client
// ignores an element whose kind it does not know; kind is open.
type OutputsResponse struct {
	Outputs []OutputInfo `json:"outputs"`
}

// OutputInfo is one local output. DAC is null when this element names
// no card. MPD is present for kind mpd and omitted for any other kind,
// so a client decodes it as optional.
type OutputInfo struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind"`
	Player    OutputPlayer     `json:"player"`
	MPD       *OutputMPD       `json:"mpd,omitempty"`
	DAC       *OutputDAC       `json:"dac"`
	PathRules []OutputPathRule `json:"pathRules"`
}

// OutputPlayer is the software driving MPD.
type OutputPlayer struct {
	Software string `json:"software"`
	Version  string `json:"version"`
}

// OutputMPD is the daemon beside the bridge. Addresses are the LAN
// addresses that greeted, never loopback, and never null.
// LoopbackBaseURL is the base URL of the bridge's loopback track route,
// http://127.0.0.1:<port>, which only this MPD can reach: the app hands
// it <base>/dlna/file/<trackID> for a bridge track its database lacks.
// Additive, and absent while the route is not serving.
type OutputMPD struct {
	Port             int      `json:"port"`
	Addresses        []string `json:"addresses"`
	Version          string   `json:"version"`
	State            string   `json:"state"`
	ReachableFromLAN bool     `json:"reachableFromLAN"`
	LoopbackBaseURL  string   `json:"loopbackBaseURL,omitempty"`
}

// OutputDAC is one sound card. IsMPDsOutput is true or null, never
// false. It is true when this card is the one device MPD's alsa output
// names, and null when that device did not resolve to one card. The
// bridge does not list the other cards once one resolved, so it has no
// card to mark false.
type OutputDAC struct {
	Card         int                `json:"card"`
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	USBID        string             `json:"usbId"`
	IsMPDsOutput *bool              `json:"isMPDsOutput"`
	Capabilities OutputCapabilities `json:"capabilities"`
}

// OutputCapabilities is every playback altset of the card's stream.
// Lists are never null.
type OutputCapabilities struct {
	MaxPCMRate     int      `json:"maxPCMRate"`
	PCMBitDepths   []int    `json:"pcmBitDepths"`
	NativeDSD      bool     `json:"nativeDSD"`
	NativeDSDRates []int    `json:"nativeDSDRates"`
	DSDFormats     []string `json:"dsdFormats"`
}

// OutputPathRule is one library root. The app swaps prefixes and does
// not re-derive them. It applies the swap only when state is proven.
type OutputPathRule struct {
	Root              string `json:"root"`
	StripBridgePrefix string `json:"stripBridgePrefix"`
	PrependMpdPrefix  string `json:"prependMpdPrefix"`
	State             string `json:"state"`
	Checked           int    `json:"checked"`
	Found             int    `json:"found"`
}

// outputSession is the read-only MPD connection the routes keep. *mpd.Client
// is one. A caller never supplies protocol text.
type outputSession interface {
	Status(ctx context.Context) (mpd.Reply, error)
	Find(ctx context.Context, path string) (mpd.Reply, error)
	CurrentSong(ctx context.Context) (mpd.Reply, error)
	ReplayGainStatus(ctx context.Context) (mpd.Reply, error)
	Outputs(ctx context.Context) (mpd.Reply, error)
	Version() string
	Close() error
}

// serialSession runs one command at a time on the session GET /v1/outputs
// and the reading share. MPD answers one command at a time on a
// connection, so two written together would each read the other's reply.
// The wait for the turn ends with the caller's context, so a reading keeps
// its own deadline behind the outputs body's finds. Close does not wait:
// closing the connection is what ends a command that hangs.
type serialSession struct {
	turn  chan struct{}
	inner outputSession
}

func newSerialSession(inner outputSession) *serialSession {
	return &serialSession{turn: make(chan struct{}, 1), inner: inner}
}

func (s *serialSession) take(ctx context.Context) error {
	select {
	case s.turn <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *serialSession) give() { <-s.turn }

func (s *serialSession) Status(ctx context.Context) (mpd.Reply, error) {
	if err := s.take(ctx); err != nil {
		return mpd.Reply{}, err
	}
	defer s.give()
	return s.inner.Status(ctx)
}

func (s *serialSession) Find(ctx context.Context, path string) (mpd.Reply, error) {
	if err := s.take(ctx); err != nil {
		return mpd.Reply{}, err
	}
	defer s.give()
	return s.inner.Find(ctx, path)
}

func (s *serialSession) CurrentSong(ctx context.Context) (mpd.Reply, error) {
	if err := s.take(ctx); err != nil {
		return mpd.Reply{}, err
	}
	defer s.give()
	return s.inner.CurrentSong(ctx)
}

func (s *serialSession) ReplayGainStatus(ctx context.Context) (mpd.Reply, error) {
	if err := s.take(ctx); err != nil {
		return mpd.Reply{}, err
	}
	defer s.give()
	return s.inner.ReplayGainStatus(ctx)
}

func (s *serialSession) Outputs(ctx context.Context) (mpd.Reply, error) {
	if err := s.take(ctx); err != nil {
		return mpd.Reply{}, err
	}
	defer s.give()
	return s.inner.Outputs(ctx)
}

func (s *serialSession) Version() string { return s.inner.Version() }

func (s *serialSession) Close() error { return s.inner.Close() }

// LocalOutputsConfig is the injected half of GET /v1/outputs. A nil
// function takes the production one. GOOS empty is this process.
// Output, Roots, Tracks and Advertised empty means the server's own.
type LocalOutputsConfig struct {
	GOOS       string
	Now        func() time.Time
	AfterFunc  func(time.Duration, func()) func()
	Identity   string
	Advertised func() []string
	Output     func() config.OutputConfig
	Roots      func() []string
	Tracks     func(context.Context) ([]string, error)
	Inspect    func(context.Context, mpd.Options) mpd.Report
	Dial       func(context.Context, string, string) (outputSession, error)
	Greet      func(context.Context, string) bool
	Cards      func() (alsa.Snapshot, error)
	Getenv     func(string) string
	BuildLimit time.Duration

	// LoopbackBaseURL is the base URL of the loopback track route while it
	// serves, "" otherwise; mpd.loopbackBaseURL carries it. Nil is "".
	LoopbackBaseURL func() string
	// OnActive runs each time Active answers true: the loopback track
	// route starts the first time output.mpd resolves on. It must be
	// cheap and idempotent. Nil does nothing.
	OnActive func()

	// Track is this bridge's row at a manifest path, for the reading's
	// song: ok is false when there is none. Nil is no lookup: no song is
	// then one of this bridge's rows.
	Track func(ctx context.Context, path string) (track OutputTrack, ok bool, err error)
	// LoopbackTrack is the row a loopback track route id names (the DLNA
	// track id) and the file it serves, for a song MPD fetches from that
	// route. Nil is no lookup.
	LoopbackTrack func(trackID string) (path, absPath string, ok bool)
}

type localOutputs struct {
	goos       string
	now        func() time.Time
	after      func(time.Duration, func()) func()
	identity   string
	advertised func() []string
	output     func() config.OutputConfig
	roots      func() []string
	tracks     func(context.Context) ([]string, error)
	inspect    func(context.Context, mpd.Options) mpd.Report
	dial       func(context.Context, string, string) (outputSession, error)
	greet      func(context.Context, string) bool
	cards      func() (alsa.Snapshot, error)
	getenv     func(string) string
	buildLimit time.Duration
	loopback   func() string
	onActive   func()
	track      func(context.Context, string) (OutputTrack, bool, error)
	loopTrack  func(string) (string, string, bool)
	limits     readingLimits
	// confFile and alsaConfig are the reading's mpd.conf and ALSA
	// configuration reads: conf.ReadFile and readALSAContext, which a test
	// replaces with one that hangs or panics.
	confFile   func(root, logical, host string) (conf.File, error)
	alsaConfig func(root string, uids []int) alsaContext

	mu           sync.Mutex
	client       outputSession
	sessEndpoint string
	sessPassword string
	stopIdle     func()
	last         time.Time
	flight       *outputFlight
	cached       outputCache
	probed       outputProbe
	// inspecting is an inspection still running past its probe's deadline
	// (inspectBounded): a probe does not start a second while it runs.
	inspecting bool
	// answered is auto's newest verdict that a caller of Active recorded,
	// whichever way the gate reached it: a probe it stored, a probe past its
	// own deadline it did not store, the cache, or another caller's probe
	// it waited on. activeNow returns it, so the loopback route follows
	// exactly what the gate says. Every verdict is numbered as it is
	// produced under mu (verdictSeq), answeredSeq is the number of the one
	// answered holds, and a caller records only a newer one.
	answered     bool
	answeredSeq  uint64
	verdictSeq   uint64
	probeFlights map[string]*probeFlight
	paths        outputPathCache
	// readings counts the readings taking place: the idle close waits them
	// out as it waits out a build (outputs_reading.go).
	readings int
	reading  readingState
	// joined, when set, runs after a caller has found a build in flight
	// and before it waits. Production leaves it nil. Tests use it to
	// release that build only once the second caller is waiting on it.
	joined func()
	// probeWait, when set, runs after a caller has found a probe in
	// flight and before it waits. Production leaves it nil.
	probeWait func()
	// beforeRecord, when set, runs after a caller of Active has its
	// verdict and before it records it, with that caller's context.
	// Production leaves it nil. Tests park one caller there.
	beforeRecord func(context.Context)
}

type outputFlight struct {
	done chan struct{}
	body OutputsResponse
	err  error
}

type outputCache struct {
	at   time.Time
	body OutputsResponse
	ok   bool
}

type outputPathCache struct {
	at    time.Time
	key   string
	rules []OutputPathRule
	ok    bool
}

type outputProbe struct {
	at    time.Time
	until time.Time
	key   string
	found bool
	rep   mpd.Report
	opt   mpd.Options
}

// probeFlight is one inspection shared by every caller that asks for
// the same configuration while it is running. seq numbers its verdict,
// set under mu before done closes, so every waiter carries the number the
// probe was given.
type probeFlight struct {
	done    chan struct{}
	found   bool
	rep     mpd.Report
	opt     mpd.Options
	seq     uint64
	expired bool
}

func newLocalOutputs(cfg LocalOutputsConfig) *localOutputs {
	o := &localOutputs{
		goos:       cfg.GOOS,
		now:        cfg.Now,
		after:      cfg.AfterFunc,
		identity:   cfg.Identity,
		advertised: cfg.Advertised,
		output:     cfg.Output,
		roots:      cfg.Roots,
		tracks:     cfg.Tracks,
		inspect:    cfg.Inspect,
		dial:       cfg.Dial,
		greet:      cfg.Greet,
		cards:      cfg.Cards,
		getenv:     cfg.Getenv,
		buildLimit: cfg.BuildLimit,
		loopback:   cfg.LoopbackBaseURL,
		onActive:   cfg.OnActive,
		track:      cfg.Track,
		loopTrack:  cfg.LoopbackTrack,
		limits:     defaultReadingLimits,
		confFile:   conf.ReadFile,
		alsaConfig: readALSAContext,
	}
	if o.goos == "" {
		o.goos = runtime.GOOS
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.after == nil {
		o.after = func(d time.Duration, fn func()) func() {
			t := time.AfterFunc(d, fn)
			return func() { t.Stop() }
		}
	}
	if o.inspect == nil {
		o.inspect = func(ctx context.Context, opt mpd.Options) mpd.Report { return mpd.Inspect(ctx, opt) }
	}
	if o.dial == nil {
		o.dial = func(ctx context.Context, endpoint, password string) (outputSession, error) {
			return mpd.Dial(ctx, endpoint, password)
		}
	}
	if o.greet == nil {
		o.greet = greetMPD
	}
	if o.cards == nil {
		o.cards = func() (alsa.Snapshot, error) { return (alsa.Reader{}).Read() }
	}
	if o.getenv == nil {
		o.getenv = os.Getenv
	}
	if o.buildLimit <= 0 {
		o.buildLimit = outputBuildLimit
	}
	if o.output == nil {
		o.output = func() config.OutputConfig { return config.OutputConfig{} }
	}
	if o.roots == nil {
		o.roots = func() []string { return nil }
	}
	if o.advertised == nil {
		o.advertised = func() []string { return nil }
	}
	return o
}

// Active reports whether /v1/health advertises localOutputs and the
// route answers. Off this host's Linux it is false, including when the
// config says on: the readers refuse the host the way bridge output dac
// does. auto is on only when a configuration was read or a daemon
// greeted, and that answer is cached so health does not dial every time.
// The same answer starts the loopback track route: OnActive runs each
// time it is true. auto's answer is also recorded, and the route asks it
// on every request (activeNow) until the gate answers again.
func (o *localOutputs) Active(ctx context.Context) bool {
	if o == nil || o.goos != "linux" {
		return false
	}
	on := false
	switch o.outputConfig().Mode() {
	case "on":
		on = true
	case "auto":
		on = o.answerAuto(ctx)
	}
	if on && o.onActive != nil {
		o.onActive()
	}
	return on
}

// answerAuto is auto's answer: the probe's verdict, recorded under mu as
// the gate's answer for activeNow when it is newer than the one recorded,
// and the gate's newest recorded verdict is what the caller gets. The
// verdict was numbered when it was produced, and this caller took it out
// of mu to get here, so another caller may have recorded a newer one
// meanwhile: the older one is not recorded over it. A caller that stopped
// waiting on another caller's probe has no verdict of its own (0): it
// records nothing, so a request that ends while a probe runs cannot turn
// the loopback route off.
func (o *localOutputs) answerAuto(ctx context.Context) bool {
	found, _, _, seq, _ := o.probeVerdict(ctx)
	if o.beforeRecord != nil {
		o.beforeRecord(ctx)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if seq > o.answeredSeq {
		o.answered, o.answeredSeq = found, seq
	}
	return o.answered
}

// activeNow is the gate's answer as it stands, without asking MPD
// anything: false off Linux and for off, true for on, and for auto the
// newest verdict a caller of Active recorded (answered), whatever its age
// and whatever configuration it was for, false before the first. It never
// probes, dials or greets, and never runs OnActive, so the loopback track
// route asks it on every request. It waits only on mu, which probe, like
// every holder of mu, releases before any I/O, so that read never waits
// on MPD.
func (o *localOutputs) activeNow() bool {
	if o == nil || o.goos != "linux" {
		return false
	}
	switch o.outputConfig().Mode() {
	case "on":
		return true
	case "auto":
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.answered
	}
	return false
}

func (o *localOutputs) probe(ctx context.Context) (bool, mpd.Report, mpd.Options) {
	found, rep, opt, _, _ := o.probeVerdict(ctx)
	return found, rep, opt
}

// probeVerdict is probe, the number of its verdict, and whether the
// inspection it answers from ran past its deadline (expired), which a
// caller that stopped waiting has not had either. Every answer is a
// verdict, numbered under mu as it is produced: a cache read when it is
// read, a probe when it completes (stored or past its own deadline),
// whichever caller ran it. The false of a caller that stopped waiting on
// another caller's probe is none, and carries 0.
func (o *localOutputs) probeVerdict(ctx context.Context) (bool, mpd.Report, mpd.Options, uint64, bool) {
	cfg := o.outputConfig()
	key := probeKey(cfg)
	o.mu.Lock()
	if o.probed.key == key && !o.probed.until.IsZero() && o.now().Before(o.probed.until) {
		p := o.probed
		seq := o.nextVerdictLocked()
		o.mu.Unlock()
		return p.found, p.rep, p.opt, seq, false
	}
	if f := o.probeFlights[key]; f != nil {
		o.mu.Unlock()
		if o.probeWait != nil {
			o.probeWait()
		}
		select {
		case <-f.done:
			return f.found, f.rep, f.opt, f.seq, f.expired
		case <-ctx.Done():
			return false, mpd.Report{}, mpd.Options{}, 0, true
		}
	}
	f := &probeFlight{done: make(chan struct{})}
	if o.probeFlights == nil {
		o.probeFlights = map[string]*probeFlight{}
	}
	o.probeFlights[key] = f
	o.mu.Unlock()

	opt, refuse := mpd.OptionsForHost(o.goos, "", cfg.Conf, cfg.Endpoint, cfg.Password, o.getenv)
	var rep mpd.Report
	found := false
	expired := false
	if !refuse {
		// Detached from the caller: a cancelled /v1/health must not
		// store a miss that hides the output for the miss TTL.
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outputProbeLimit)
		rep, expired = o.inspectBounded(pctx, opt)
		cancel()
		found = rep.Found()
	}
	o.mu.Lock()
	if !expired {
		ttl := outputProbeMiss
		if found {
			ttl = outputProbeHit
		}
		o.probed = outputProbe{at: o.now(), until: o.now().Add(ttl), key: key, found: found, rep: rep, opt: opt}
	}
	delete(o.probeFlights, key)
	f.found, f.rep, f.opt, f.seq, f.expired = found, rep, opt, o.nextVerdictLocked(), expired
	close(f.done)
	o.mu.Unlock()
	return found, rep, opt, f.seq, expired
}

// outputProbeGrace is how long past its deadline a probe waits for its
// inspection to return: the dial and the greeting end with the context, a
// read of a file on a dead mount does not.
const outputProbeGrace = 100 * time.Millisecond

// inspectBounded runs the inspection on a goroutine of its own and waits for
// it until ctx ends and outputProbeGrace more. One that has not returned by
// then is expired with no report, and while it still runs no probe starts
// another: a file that holds a read for good holds one goroutine, not one
// per probe. A panic in it is an expired probe, logged. expired is whether
// ctx had ended when the report came.
func (o *localOutputs) inspectBounded(ctx context.Context, opt mpd.Options) (rep mpd.Report, expired bool) {
	o.mu.Lock()
	if o.inspecting {
		o.mu.Unlock()
		return mpd.Report{}, true
	}
	o.inspecting = true
	o.mu.Unlock()
	done := make(chan struct{})
	var out mpd.Report
	var panicked bool
	go func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				logOutputPanic("the probe", r)
			}
			o.mu.Lock()
			o.inspecting = false
			o.mu.Unlock()
			close(done)
		}()
		out = o.inspect(ctx, opt)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		timer := time.NewTimer(outputProbeGrace)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			return mpd.Report{}, true
		}
	}
	if panicked {
		return mpd.Report{}, true
	}
	return out, ctx.Err() != nil
}

// nextVerdictLocked numbers a verdict as it is produced. Caller holds mu.
func (o *localOutputs) nextVerdictLocked() uint64 {
	o.verdictSeq++
	return o.verdictSeq
}

// outputConfig is the output section as the config holds it now, and the
// empty one when no reader is wired: every reader of it reads it this way.
func (o *localOutputs) outputConfig() config.OutputConfig {
	if o.output == nil {
		return config.OutputConfig{}
	}
	return o.output()
}

func probeKey(cfg config.OutputConfig) string {
	return strings.Join([]string{cfg.MPD, cfg.Endpoint, cfg.Conf, cfg.Password}, "\n")
}

// Response builds the route body. One build runs at a time, on a context
// detached from the caller with its own deadline. A success that finished
// inside that deadline is reused for outputCacheTTL. The MPD connection
// opens on the first build that can dial and closes outputIdle after the
// last call.
func (o *localOutputs) Response(ctx context.Context) (OutputsResponse, error) {
	o.armIdle()
	o.mu.Lock()
	if o.cached.ok && o.now().Sub(o.cached.at) < outputCacheTTL {
		body := o.cached.body
		o.mu.Unlock()
		return body, nil
	}
	if o.flight != nil {
		f := o.flight
		o.mu.Unlock()
		if o.joined != nil {
			o.joined()
		}
		select {
		case <-f.done:
			return f.body, f.err
		case <-ctx.Done():
			return OutputsResponse{}, ctx.Err()
		}
	}
	f := &outputFlight{done: make(chan struct{})}
	o.flight = f
	o.mu.Unlock()

	buildCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.buildLimit)
	defer cancel()
	body, err := o.build(buildCtx)
	o.mu.Lock()
	if err == nil && buildCtx.Err() == nil {
		o.cached = outputCache{at: o.now(), body: body, ok: true}
	}
	o.flight = nil
	f.body, f.err = body, err
	close(f.done)
	o.mu.Unlock()
	o.armIdle()
	return body, err
}

func (o *localOutputs) armIdle() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.last = o.now()
	if o.stopIdle != nil {
		o.stopIdle()
	}
	o.stopIdle = o.after(outputIdle, o.idleClose)
}

func (o *localOutputs) idleClose() {
	o.mu.Lock()
	if o.flight != nil || o.readings > 0 || o.now().Sub(o.last) < outputIdle {
		o.mu.Unlock()
		return
	}
	c := o.client
	o.client = nil
	o.stopIdle = nil
	o.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (o *localOutputs) session(ctx context.Context, endpoint, password string) (outputSession, error) {
	if endpoint == "" {
		return nil, nil
	}
	o.mu.Lock()
	if o.client != nil && o.sessEndpoint == endpoint && o.sessPassword == password {
		c := o.client
		o.mu.Unlock()
		return c, nil
	}
	old := o.client
	o.client = nil
	o.sessEndpoint = ""
	o.sessPassword = ""
	o.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	dialled, err := o.dial(ctx, endpoint, password)
	if err != nil {
		return nil, err
	}
	if dialled == nil {
		return nil, nil
	}
	var c outputSession = newSerialSession(dialled)
	o.mu.Lock()
	if o.client != nil {
		o.mu.Unlock()
		_ = c.Close()
		o.mu.Lock()
		kept := o.client
		o.mu.Unlock()
		return kept, nil
	}
	o.client = c
	o.sessEndpoint = endpoint
	o.sessPassword = password
	o.mu.Unlock()
	return c, nil
}

// dropSession closes c and forgets it when it is still the cached one.
// A context cancellation is not a dead daemon: the caller went away.
func (o *localOutputs) dropSession(c outputSession) {
	if c == nil {
		return
	}
	o.mu.Lock()
	if o.client == c {
		o.client = nil
		o.sessEndpoint = ""
		o.sessPassword = ""
	}
	o.mu.Unlock()
	_ = c.Close()
}

func sessionBroken(err error) bool {
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (o *localOutputs) build(ctx context.Context) (OutputsResponse, error) {
	cfg := o.outputConfig()
	_, rep, opt := o.probe(ctx)
	endpoint := rep.MPD.Endpoint
	if endpoint == "" {
		endpoint = opt.Endpoint
	}
	sess, _ := o.session(ctx, endpoint, cfg.Password)
	redialed := false
	// again drops a dead session and dials once more in this build.
	again := func(err error) bool {
		if !sessionBroken(err) || redialed {
			return false
		}
		o.dropSession(sess)
		redialed = true
		sess, _ = o.session(ctx, endpoint, cfg.Password)
		return sess != nil
	}
	state := "unknown"
	version := ""
	if sess != nil {
		version = sess.Version()
		if version == "" {
			version = rep.MPD.Version
		}
		reply, err := sess.Status(ctx)
		if again(err) {
			if v := sess.Version(); v != "" {
				version = v
			}
			reply, err = sess.Status(ctx)
		}
		if err == nil {
			state = playbackState(reply)
		}
	}
	port := listenPort(endpoint, rep, opt)
	addrs := o.lanAddresses(ctx, port)
	player := OutputPlayer{Software: playerSoftware(rep.Software.Name), Version: rep.Software.Version}
	if player.Version == "" && player.Software == "mpd" {
		player.Version = version
	}
	var lookup mpd.PathLookup
	if sess != nil && rep.MusicDirectory != "" {
		lookup = func(ctx context.Context, mpdPath string) (bool, error) {
			if sess == nil {
				return false, errors.New("mpd: no session")
			}
			reply, err := sess.Find(ctx, mpdPath)
			if again(err) {
				reply, err = sess.Find(ctx, mpdPath)
			}
			if err != nil {
				return false, err
			}
			_, ok := reply.Get("file")
			return ok, nil
		}
	}
	rules := o.pathRules(ctx, rep.MusicDirectory, lookup)
	mpdInfo := &OutputMPD{
		Port:             port,
		Addresses:        addrs,
		Version:          version,
		State:            state,
		ReachableFromLAN: len(addrs) > 0,
	}
	if o.loopback != nil {
		mpdInfo.LoopbackBaseURL = o.loopback()
	}
	dacs := o.dacs(rep)
	base := OutputInfo{Kind: "mpd", Player: player, MPD: mpdInfo, PathRules: rules}
	var outputs []OutputInfo
	if len(dacs) == 0 {
		one := base
		one.ID = outputID(o.identity, endpoint, "", "")
		outputs = []OutputInfo{one}
	} else {
		outputs = make([]OutputInfo, len(dacs))
		for i, dac := range dacs {
			one := base
			copied := append([]OutputPathRule(nil), rules...)
			one.PathRules = copied
			d := dac
			one.DAC = &d
			one.ID = outputID(o.identity, endpoint, dac.ID, dac.USBID)
			outputs[i] = one
		}
	}
	resp := OutputsResponse{Outputs: outputs}
	resp.normalize()
	return resp, nil
}

func (resp *OutputsResponse) normalize() {
	if resp.Outputs == nil {
		resp.Outputs = []OutputInfo{}
	}
	for i := range resp.Outputs {
		o := &resp.Outputs[i]
		if o.MPD != nil && o.MPD.Addresses == nil {
			o.MPD.Addresses = []string{}
		}
		if o.PathRules == nil {
			o.PathRules = []OutputPathRule{}
		}
		if o.DAC != nil {
			c := &o.DAC.Capabilities
			if c.PCMBitDepths == nil {
				c.PCMBitDepths = []int{}
			}
			if c.NativeDSDRates == nil {
				c.NativeDSDRates = []int{}
			}
			if c.DSDFormats == nil {
				c.DSDFormats = []string{}
			}
		}
	}
}

func (o *localOutputs) pathRules(ctx context.Context, musicDir string, lookup mpd.PathLookup) []OutputPathRule {
	roots := o.roots()
	key := o.pathRuleKey(musicDir, roots)
	o.mu.Lock()
	if o.paths.ok && o.paths.key == key && o.now().Sub(o.paths.at) < outputIdle {
		rules := append([]OutputPathRule(nil), o.paths.rules...)
		o.mu.Unlock()
		return rules
	}
	o.mu.Unlock()
	var tracks []string
	trackErr := false
	if o.tracks != nil {
		got, err := o.tracks(ctx)
		if err != nil {
			trackErr = true
		} else {
			tracks = got
		}
	}
	multi := len(roots) > 1
	libs := make([]mpd.LibraryRoot, len(roots))
	for i, path := range roots {
		id := filepath.Base(path)
		var own []string
		for _, stored := range tracks {
			if !multi || trackUnderRoot(stored, path) {
				own = append(own, stored)
			}
		}
		libs[i] = mpd.LibraryRoot{ID: id, Path: path, Multi: multi, Tracks: own}
	}
	proved := mpd.ProveRoots(ctx, musicDir, libs, lookup)
	out := make([]OutputPathRule, len(proved))
	for i, rule := range proved {
		out[i] = OutputPathRule{
			Root:              rule.Root,
			StripBridgePrefix: rule.StripBridgePrefix,
			PrependMpdPrefix:  rule.PrependMpdPrefix,
			State:             rule.State,
			Checked:           rule.Checked,
			Found:             rule.Found,
		}
	}
	cacheable := lookup != nil && !trackErr
	for _, rule := range proved {
		if rule.State == mpd.PathUnchecked || rule.State == mpd.PathMismatch || rule.Errored {
			cacheable = false
			break
		}
	}
	if cacheable {
		o.mu.Lock()
		o.paths = outputPathCache{at: o.now(), key: key, rules: append([]OutputPathRule(nil), out...), ok: true}
		o.mu.Unlock()
	}
	return out
}

func (o *localOutputs) pathRuleKey(musicDir string, roots []string) string {
	cfg := o.outputConfig()
	return strings.Join([]string{musicDir, cfg.MPD, cfg.Endpoint, cfg.Conf, cfg.Password, strings.Join(roots, "\n")}, "\n")
}

func (o *localOutputs) dacs(rep mpd.Report) []OutputDAC {
	snap, err := o.cards()
	if err != nil {
		return nil
	}
	root := alsa.ConfigRoot(snap.ProcRoot)
	picks := dacsFrom(snap, rep, func(device string, cards []alsa.Card) (int, int, bool) {
		return alsa.ResolveOutputDevice(root, device, cards)
	})
	if picks == nil {
		return nil
	}
	out := make([]OutputDAC, len(picks))
	for i, p := range picks {
		out[i] = p.dac
	}
	return out
}

// dacPick is one card an outputs element names, and the PCM device on it:
// the one MPD's device resolved to, or 0 for a candidate card.
type dacPick struct {
	dac OutputDAC
	pcm int
}

// dacsFrom is the cards GET /v1/outputs lists for one reading of the cards,
// in its order: the one card MPD's enabled alsa device resolves to, else
// each card several devices resolve to, else every USB card. The reading
// lists them from its own snapshot, so an id it matches is the one the
// outputs route gives. resolve maps a device to a card: the route's reads
// the ALSA configuration for a PCM name, and the reading's resolves from
// the configuration it read once, under its deadline.
func dacsFrom(snap alsa.Snapshot, rep mpd.Report, resolve func(device string, cards []alsa.Card) (int, int, bool)) []dacPick {
	cards := make([]alsa.Card, len(snap.Cards))
	for i, c := range snap.Cards {
		cards[i] = c.Card
	}
	type hit struct {
		index int
		pcm   int
	}
	var hits []hit
	seen := map[int]bool{}
	for _, device := range alsaDevices(rep) {
		idx, pcm, ok := resolve(device, cards)
		if !ok || seen[idx] {
			continue
		}
		seen[idx] = true
		hits = append(hits, hit{idx, pcm})
	}
	if len(hits) == 1 {
		if dac, ok := dacAt(snap, hits[0].index, hits[0].pcm, true); ok {
			return []dacPick{{dac: dac, pcm: hits[0].pcm}}
		}
	}
	if len(hits) > 1 {
		var out []dacPick
		for _, h := range hits {
			if dac, ok := dacAt(snap, h.index, h.pcm, false); ok {
				out = append(out, dacPick{dac: dac, pcm: h.pcm})
			}
		}
		return out
	}
	var usb []dacPick
	for _, c := range snap.Cards {
		if c.USBID == nil {
			continue
		}
		if dac, ok := dacAt(snap, c.Index, 0, false); ok {
			usb = append(usb, dacPick{dac: dac})
		}
	}
	return usb
}

func dacAt(snap alsa.Snapshot, index, pcm int, mpdOutput bool) (OutputDAC, bool) {
	var card *alsa.CardReading
	for i := range snap.Cards {
		if snap.Cards[i].Index == index {
			card = &snap.Cards[i]
			break
		}
	}
	if card == nil {
		return OutputDAC{}, false
	}
	var alt []alsa.Altset
	for _, d := range card.Devices {
		if d.Device != pcm || d.Stream == nil || d.Stream.Playback == nil {
			continue
		}
		alt = d.Stream.Playback.Altsets
	}
	caps := alsa.CapabilitiesFromAltsets(alt)
	dac := OutputDAC{
		Card: card.Index,
		ID:   card.ID,
		Name: card.Name,
		Capabilities: OutputCapabilities{
			MaxPCMRate:     caps.MaxPCMRate,
			PCMBitDepths:   caps.PCMBitDepths,
			NativeDSD:      caps.NativeDSD,
			NativeDSDRates: caps.NativeDSDRates,
			DSDFormats:     caps.DSDFormats,
		},
	}
	if card.USBID != nil {
		dac.USBID = card.USBID.String()
	}
	if mpdOutput {
		yes := true
		dac.IsMPDsOutput = &yes
	}
	return dac, true
}

func alsaDevices(rep mpd.Report) []string {
	var enabled, all []string
	for _, o := range rep.Outputs {
		if o.Type != "alsa" || o.Device == "" {
			continue
		}
		all = append(all, o.Device)
		if o.Enabled != "no" {
			enabled = append(enabled, o.Device)
		}
	}
	if len(enabled) > 0 {
		return enabled
	}
	return all
}

func (o *localOutputs) lanAddresses(ctx context.Context, port int) []string {
	if port <= 0 || o.advertised == nil {
		return []string{}
	}
	var cands []string
	seen := map[string]struct{}{}
	for _, raw := range o.advertised() {
		host := hostOf(raw)
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		s := ip.String()
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		cands = append(cands, s)
	}
	var mu sync.Mutex
	var out []string
	var wg sync.WaitGroup
	for _, s := range cands {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			addr := net.JoinHostPort(s, strconv.Itoa(port))
			if o.greet(ctx, addr) {
				mu.Lock()
				out = append(out, s)
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	sort.Strings(out)
	if out == nil {
		return []string{}
	}
	return out
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func listenPort(endpoint string, rep mpd.Report, opt mpd.Options) int {
	if p, ok := tcpPort(endpoint); ok {
		return p
	}
	if rep.ConfPath != "" {
		if f, err := conf.ReadFile(opt.Root, rep.ConfPath, rep.ConfPath); err == nil && f.Port != nil && *f.Port > 0 {
			return *f.Port
		}
	}
	return conf.DefaultPort
}

func tcpPort(endpoint string) (int, bool) {
	if endpoint == "" || strings.Contains(endpoint, "/") || strings.HasPrefix(endpoint, "@") {
		return 0, false
	}
	_, p, err := net.SplitHostPort(endpoint)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func playbackState(reply mpd.Reply) string {
	state, ok := reply.Get("state")
	switch state {
	case "play", "pause", "stop":
		if ok {
			return state
		}
	}
	return "unknown"
}

func playerSoftware(name string) string {
	switch name {
	case "Volumio":
		return "volumio"
	case "moOde":
		return "moode"
	case "MPD":
		return "mpd"
	default:
		return ""
	}
}

// OutputIdentity is the bridge half of an output id: the hex SHA-256 of
// the data directory. A renewing certificate changes the TLS fingerprint,
// and a rename changes the friendly name; neither is an input. This is
// the same directory deriveDLNAUDN hashes, without the friendly name.
func OutputIdentity(dataDir string) string {
	sum := sha256.Sum256([]byte(dataDir))
	return hex.EncodeToString(sum[:])
}

// outputID is what the app stores for an output. The card index is not
// an input: it changes when a DAC is replugged or the box reboots.
// alsaID is the id string from /proc/asound/cards, and usbID is
// vendor:product. Both are empty when the element names no card.
func outputID(identity, endpoint, alsaID, usbID string) string {
	sum := sha256.Sum256([]byte(identity + "\n" + endpoint + "\n" + alsaID + "\n" + usbID))
	return hex.EncodeToString(sum[:])
}

// trackUnderRoot reports whether a stored library path belongs to root.
// Multi-root paths are stored under the exact filepath.Base(root), which
// is also the prefix the client strips. A case-folded first segment is
// not this root's: CutPrefix would miss it and the rule would read
// mismatch.
func trackUnderRoot(stored, root string) bool {
	return strings.HasPrefix(stored, filepath.Base(root)+"/")
}

func greetMPD(ctx context.Context, addr string) bool {
	ctx, cancel := context.WithTimeout(ctx, outputGreetLimit)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return false
	}
	_, err = mpd.ParseGreeting(strings.TrimRight(line, "\r\n"))
	return err == nil
}

// WithLocalOutputs wires GET /v1/outputs. The closures run on the
// request, so they see the live config and the addresses the bridge
// is advertising then.
func (s *Server) WithLocalOutputs(cfg LocalOutputsConfig) *Server {
	if cfg.Identity == "" && s.cfgHolder != nil {
		cfg.Identity = OutputIdentity(s.cfgHolder.Load().DataDir)
	}
	if cfg.Output == nil && s.cfgHolder != nil {
		cfg.Output = func() config.OutputConfig { return s.cfgHolder.Load().Output }
	}
	if cfg.Roots == nil && s.cfgHolder != nil {
		cfg.Roots = func() []string {
			return append([]string(nil), s.cfgHolder.Load().LibraryRoots...)
		}
	}
	if cfg.Advertised == nil {
		cfg.Advertised = func() []string {
			eps := s.ReachableEndpoints()
			out := make([]string, 0, len(eps))
			for _, e := range eps {
				out = append(out, e.URL)
			}
			return out
		}
	}
	s.localOutputs = newLocalOutputs(cfg)
	return s
}

// LocalOutputs is GET /v1/outputs' builder on its own, for a caller with
// no server: `bridge doctor` asks it once for the checklist and closes it.
// The gate and the body are the route's, from the same code.
type LocalOutputs struct{ o *localOutputs }

// NewLocalOutputs builds the outputs body the way WithLocalOutputs does,
// from cfg alone: a nil Output, Roots or Advertised is the empty one, as
// for a server, and nothing is read from a server's config.
func NewLocalOutputs(cfg LocalOutputsConfig) *LocalOutputs {
	return &LocalOutputs{o: newLocalOutputs(cfg)}
}

// Active is the localOutputs gate.
func (l *LocalOutputs) Active(ctx context.Context) bool { return l.o.Active(ctx) }

// Response builds the body GET /v1/outputs would serve.
func (l *LocalOutputs) Response(ctx context.Context) (OutputsResponse, error) {
	return l.o.Response(ctx)
}

// Close ends the MPD session now, rather than after the idle minute, and
// disarms the idle close. A one-shot caller closes as soon as it has its
// body: the connection stays lazy, and MPD's max_connections is shared.
func (l *LocalOutputs) Close() { l.o.close() }

func (o *localOutputs) close() {
	o.mu.Lock()
	c := o.client
	o.client = nil
	o.sessEndpoint = ""
	o.sessPassword = ""
	stop := o.stopIdle
	o.stopIdle = nil
	o.mu.Unlock()
	if stop != nil {
		stop()
	}
	if c != nil {
		_ = c.Close()
	}
}

// errNoLocalOutputs is a server with no outputs builder wired.
var errNoLocalOutputs = errors.New("api: local outputs are not wired")

// LocalOutputsResponse is the body GET /v1/outputs serves, built by the
// route's own builder, with its caches and its lazy session: the console's
// checklist reads the box from here, the way the app does.
func (s *Server) LocalOutputsResponse(ctx context.Context) (OutputsResponse, error) {
	if s.localOutputs == nil {
		return OutputsResponse{}, errNoLocalOutputs
	}
	return s.localOutputs.Response(ctx)
}

// LocalOutputsActive is the gate /v1/health's localOutputs flag reads:
// output.mpd resolves on and the host is Linux. False when the route is
// not wired. Like every call of that gate, a true answer runs OnActive.
func (s *Server) LocalOutputsActive(ctx context.Context) bool {
	return s.localOutputs.Active(ctx)
}

// LocalOutputsActiveNow is that gate's answer as it stands, without a
// probe: on, off, or the answer auto last gave (activeNow). The loopback
// track route asks it on every request, which therefore never dials MPD.
// False when the route is not wired.
func (s *Server) LocalOutputsActiveNow() bool {
	return s.localOutputs.activeNow()
}

func (s *Server) outputs(w http.ResponseWriter, r *http.Request) {
	if s.localOutputs == nil || !s.localOutputs.Active(r.Context()) {
		writeError(w, http.StatusNotFound, "local_outputs_not_supported",
			"local outputs are not enabled on this bridge")
		return
	}
	resp, err := s.localOutputs.Response(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal",
			"the bridge encountered an internal error")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
