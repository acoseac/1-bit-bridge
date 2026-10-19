package api

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"os"
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
)

// OutputsResponse is GET /v1/outputs. Outputs is never null. A client
// ignores an element whose kind it does not know; kind is open.
type OutputsResponse struct {
	Outputs []OutputInfo `json:"outputs"`
}

// OutputInfo is one local output. Every key is present. DAC is null
// when this element names no card.
type OutputInfo struct {
	ID        string           `json:"id"`
	Kind      string           `json:"kind"`
	Player    OutputPlayer     `json:"player"`
	MPD       OutputMPD        `json:"mpd"`
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
type OutputMPD struct {
	Port             int      `json:"port"`
	Addresses        []string `json:"addresses"`
	Version          string   `json:"version"`
	State            string   `json:"state"`
	ReachableFromLAN bool     `json:"reachableFromLAN"`
}

// OutputDAC is one sound card. IsMPDsOutput is true when this card is
// the device MPD's alsa output names, and null when that device did
// not resolve to one card. The bridge does not emit false: it does not
// list the other cards once one resolved.
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

// outputSession is the read-only MPD connection the route keeps. *mpd.Client
// is one. A caller never supplies protocol text.
type outputSession interface {
	Status(ctx context.Context) (mpd.Reply, error)
	Find(ctx context.Context, path string) (mpd.Reply, error)
	Version() string
	Close() error
}

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

	mu       sync.Mutex
	client   outputSession
	stopIdle func()
	last     time.Time
	flight   *outputFlight
	cached   outputCache
	probed   outputProbe
	// joined, when set, runs after a caller has found a build in flight
	// and before it waits. Production leaves it nil. Tests use it to
	// release that build only once the second caller is waiting on it.
	joined func()
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

type outputProbe struct {
	at    time.Time
	until time.Time
	found bool
	rep   mpd.Report
	opt   mpd.Options
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
func (o *localOutputs) Active(ctx context.Context) bool {
	if o == nil || o.goos != "linux" {
		return false
	}
	switch o.output().Mode() {
	case "on":
		return true
	case "auto":
		found, _, _ := o.probe(ctx)
		return found
	default:
		return false
	}
}

func (o *localOutputs) probe(ctx context.Context) (bool, mpd.Report, mpd.Options) {
	o.mu.Lock()
	if !o.probed.until.IsZero() && o.now().Before(o.probed.until) {
		p := o.probed
		o.mu.Unlock()
		return p.found, p.rep, p.opt
	}
	o.mu.Unlock()
	opt, refuse := mpd.OptionsForHost(o.goos, "", o.output().Conf, o.output().Endpoint, o.output().Password, o.getenv)
	var rep mpd.Report
	found := false
	if !refuse {
		pctx, cancel := context.WithTimeout(ctx, outputProbeLimit)
		rep = o.inspect(pctx, opt)
		cancel()
		found = rep.Found()
	}
	ttl := outputProbeMiss
	if found {
		ttl = outputProbeHit
	}
	o.mu.Lock()
	o.probed = outputProbe{at: o.now(), until: o.now().Add(ttl), found: found, rep: rep, opt: opt}
	o.mu.Unlock()
	return found, rep, opt
}

// Response builds the route body. One build runs at a time. A success
// is reused for outputCacheTTL. The MPD connection opens on the first
// build that can dial and closes outputIdle after the last call.
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

	body, err := o.build(ctx)
	o.mu.Lock()
	if err == nil {
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
	if o.now().Sub(o.last) < outputIdle {
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
	if o.client != nil {
		c := o.client
		o.mu.Unlock()
		return c, nil
	}
	o.mu.Unlock()
	c, err := o.dial(ctx, endpoint, password)
	if err != nil {
		return nil, err
	}
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
	o.mu.Unlock()
	return c, nil
}

func (o *localOutputs) build(ctx context.Context) (OutputsResponse, error) {
	cfg := o.output()
	_, rep, opt := o.probe(ctx)
	endpoint := rep.MPD.Endpoint
	if endpoint == "" {
		endpoint = opt.Endpoint
	}
	sess, _ := o.session(ctx, endpoint, cfg.Password)
	state := "unknown"
	version := rep.MPD.Version
	if sess != nil {
		if v := sess.Version(); v != "" {
			version = v
		}
		if reply, err := sess.Status(ctx); err == nil {
			state = playbackState(reply)
		}
	}
	port := listenPort(endpoint, rep, opt)
	addrs := o.lanAddresses(ctx, port)
	player := OutputPlayer{Software: playerSoftware(rep.Software.Name), Version: rep.Software.Version}
	if player.Version == "" && player.Software == "mpd" {
		player.Version = version
	}
	rules := o.pathRules(ctx, rep.MusicDirectory, sess)
	mpdInfo := OutputMPD{
		Port:             port,
		Addresses:        addrs,
		Version:          version,
		State:            state,
		ReachableFromLAN: len(addrs) > 0,
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
		if o.MPD.Addresses == nil {
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

func (o *localOutputs) pathRules(ctx context.Context, musicDir string, sess outputSession) []OutputPathRule {
	roots := o.roots()
	var tracks []string
	if o.tracks != nil {
		if got, err := o.tracks(ctx); err == nil {
			tracks = got
		}
	}
	multi := len(roots) > 1
	libs := make([]mpd.LibraryRoot, len(roots))
	for i, path := range roots {
		id := pathBase(path)
		var own []string
		for _, stored := range tracks {
			if !multi || strings.HasPrefix(stored, id+"/") {
				own = append(own, stored)
			}
		}
		libs[i] = mpd.LibraryRoot{ID: id, Path: path, Multi: multi, Tracks: own}
	}
	var lookup mpd.PathLookup
	if sess != nil && musicDir != "" {
		lookup = func(ctx context.Context, mpdPath string) (bool, error) {
			reply, err := sess.Find(ctx, mpdPath)
			if err != nil {
				return false, err
			}
			_, ok := reply.Get("file")
			return ok, nil
		}
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
	return out
}

func (o *localOutputs) dacs(rep mpd.Report) []OutputDAC {
	snap, err := o.cards()
	if err != nil {
		return nil
	}
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
		idx, pcm, ok := alsa.ResolveALSADevice(device, cards)
		if !ok || seen[idx] {
			continue
		}
		seen[idx] = true
		hits = append(hits, hit{idx, pcm})
	}
	if len(hits) == 1 {
		if dac, ok := dacAt(snap, hits[0].index, hits[0].pcm, true); ok {
			return []OutputDAC{dac}
		}
	}
	if len(hits) > 1 {
		var out []OutputDAC
		for _, h := range hits {
			if dac, ok := dacAt(snap, h.index, h.pcm, false); ok {
				out = append(out, dac)
			}
		}
		return out
	}
	var usb []OutputDAC
	for _, c := range snap.Cards {
		if c.USBID == nil {
			continue
		}
		if dac, ok := dacAt(snap, c.Index, 0, false); ok {
			usb = append(usb, dac)
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
	var out []string
	seen := map[string]struct{}{}
	for _, raw := range o.advertised() {
		host := hostOf(raw)
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() {
			continue
		}
		s := ip.String()
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		addr := net.JoinHostPort(s, strconv.Itoa(port))
		if o.greet(ctx, addr) {
			out = append(out, s)
		}
	}
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

// outputID is what the app stores for an output. The card index is not
// an input: it changes when a DAC is replugged or the box reboots.
// alsaID is the id string from /proc/asound/cards, and usbID is
// vendor:product. Both are empty when the element names no card.
func outputID(identity, endpoint, alsaID, usbID string) string {
	sum := sha256.Sum256([]byte(identity + "\n" + endpoint + "\n" + alsaID + "\n" + usbID))
	return hex.EncodeToString(sum[:])
}

func pathBase(path string) string {
	path = strings.TrimRight(path, `/\`)
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
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
	_ = conn.SetDeadline(time.Now().Add(outputGreetLimit))
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
	if cfg.Identity == "" {
		cfg.Identity = s.fingerprint
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
