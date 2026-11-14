package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/doctor"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/outputcheck"
)

// The player checklist (backlog B473): Player, DAC, Music and Phone. The
// lines are internal/outputcheck's, and this file is the one place that
// gathers what they judge, for both surfaces: the console's panel, from
// the serving process's own GET /v1/outputs builder (its caches and its
// lazy MPD session), and `bridge doctor`, from a one-shot builder over the
// config that it closes at once.

// outputChecklistSource is what the checklist reads.
type outputChecklistSource struct {
	// Active is the MPD gate: output.mpd resolves on and the host is Linux.
	// When it is false the bridge is no MPD player and there is no
	// checklist, whatever the bridge's own player does: the checklist stays
	// MPD's, on that gate alone (B470 PR 5b).
	Active func(context.Context) bool
	// Outputs is the body GET /v1/outputs serves.
	Outputs func(context.Context) (api.OutputsResponse, error)
	// Roots are the configured library roots: the resolver's live roots in
	// serve, cfg.LibraryRoots in the doctor. When the body cannot be read,
	// the Music line judges them, each one unchecked.
	Roots func() []string
	// LoopbackBaseURL is the loopback track route's base URL while it
	// serves, "" otherwise: what the body carries, read here when the body
	// cannot be.
	LoopbackBaseURL func() string
	// LoopbackErr is why the loopback track route is not serving, or nil:
	// errLoopbackNotServing when it answers and refuses tracks.
	LoopbackErr func() error
	// Tokens are the bridge's device tokens: the auth store's list in
	// serve, tokens.json in the doctor. The Phone line counts the ones a
	// phone can use (usablePhones).
	Tokens func() []auth.Token
	// Reads, where it is not nil, is the table Outputs is read through, and
	// ReadKey the identity of the config the source reads the box over in
	// it (outputsReadKey): bridge doctor's, whose every gather builds a
	// builder of its own (cliOutputsReads). Serve's source has none: its
	// builder is the serving process's own, whose one flight every read
	// joins (api's localOutputs.Response), and a read there leaves that
	// flight with its request's context, which ends with the request.
	Reads   *outputsReads
	ReadKey string
}

// usablePhones is the number of device tokens a phone can use now: not
// expired (Validate refuses one past its ExpiresAt), and used by a phone
// since it was issued or last rotated, since a rotation replaces the secret
// the phone held. LastUsedAt is zero for a token never used, which is
// after nothing. The demo's static token is no phone either: List does not
// return it. Serve and the doctor both count with this.
func usablePhones(tokens []auth.Token, now time.Time) int {
	n := 0
	for _, tok := range tokens {
		expired := tok.ExpiresAt != nil && !tok.ExpiresAt.IsZero() && now.After(*tok.ExpiresAt)
		if !expired && tok.LastUsedAt.After(tok.RotatedAt) {
			n++
		}
	}
	return n
}

// outputChecklist returns the four lines, or false when the bridge is no
// player.
func outputChecklist(ctx context.Context, src outputChecklistSource) ([]outputcheck.Line, bool) {
	if src.Active == nil || !src.Active(ctx) {
		return nil, false
	}
	var in outputcheck.Input
	if resp, err := readOutputs(ctx, src); err != nil {
		in = unreadInput(err, src)
	} else {
		in = checklistInput(resp)
	}
	if src.LoopbackErr != nil {
		switch err := src.LoopbackErr(); {
		case errors.Is(err, errLoopbackNotServing):
			in.Loopback.NotServing = true
		case err != nil:
			in.Loopback.Err = err.Error()
		}
	}
	if src.Tokens != nil {
		tokens := src.Tokens()
		in.Phones, in.Tokens = usablePhones(tokens, time.Now()), len(tokens)
	}
	return outputcheck.Lines(in), true
}

// errOutputsNotWired is a checklist source with no outputs body to read.
var errOutputsNotWired = errors.New("the outputs are not wired")

// outputsReadBound is how long the checklist waits for the outputs body
// (backlog B650): the builder's own limit, 15 s (api's outputBuildLimit), by
// which a build that can answer has answered, and five seconds more. The
// build proves each library root's path rule (mpd.ProveRoots), whose stat of
// a root whose mount does not answer holds it in the kernel with no end, on
// the goroutine of the call that began it, so bridge doctor on a player box
// printed nothing and the console's doctor never answered. A body not read in
// time is the input of a box that could not be read, whose Player line says
// so. The read left holding the stat is waited on by every later read of the
// same source: in serve the builder's one flight, which a later call joins
// and leaves with its own context, and in bridge doctor the read its table
// keeps for the config (outputsReads). A test shortens it.
var outputsReadBound = 20 * time.Second

func readOutputs(ctx context.Context, src outputChecklistSource) (api.OutputsResponse, error) {
	if src.Outputs == nil {
		return api.OutputsResponse{}, errOutputsNotWired
	}
	r := src.Reads.begin(ctx, src.ReadKey, src.Outputs)
	bound := outputsReadBound
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-r.done:
		return r.resp, r.err
	case <-t.C:
	case <-ctx.Done():
	}
	// An answer ready as the wait ended is still the answer.
	select {
	case <-r.done:
		return r.resp, r.err
	default:
	}
	if err := ctx.Err(); err != nil {
		return api.OutputsResponse{}, err
	}
	return api.OutputsResponse{}, fmt.Errorf("it did not answer within %s: a library root, or MPD's music directory, may be on a mount that does not answer", bound)
}

// outputsRead is one read of the outputs body, on a goroutine of its own:
// resp and err are its answer once done is closed, which a read that answers
// after every wait on it has ended keeps there and blocks no one.
type outputsRead struct {
	done chan struct{}
	resp api.OutputsResponse
	err  error
}

// outputsReads is the reads of the outputs body one process's bridge doctor
// gathers make (backlog B650's second round): at most one in flight for a
// config (outputsReadKey), which every gather over that config waits on, each
// for its own bound (outputsReadBound) and on its own context, a gather that
// stops waiting ending neither the read nor another's wait. A gather builds a
// one-shot builder of its own (doctorOutputChecklist), so a read that a root
// whose mount does not answer holds stays in its builder's flight with no
// end, which no later gather's builder joins: the launcher's doctor row
// gathers in one process for as long as its menu runs, and until this began
// a read, and stranded its goroutine, at every use. A read leaves the table
// as it answers, so a gather after that reads the config it loaded then.
type outputsReads struct {
	mu  sync.Mutex
	out map[string]*outputsRead
	// joined, when not nil, is told each time a gather waits on a read
	// another gather began: a test's hook.
	joined func()
}

// begin is the read a wait for the outputs body waits on: the read in flight
// over key where rs is a table that holds one, or one it begins there on a
// context no gather's end cancels; without a table, or a key, one of this
// call's own, on its context.
func (rs *outputsReads) begin(ctx context.Context, key string, read func(context.Context) (api.OutputsResponse, error)) *outputsRead {
	r := &outputsRead{done: make(chan struct{})}
	if rs == nil || key == "" {
		go func() {
			r.resp, r.err = read(ctx)
			close(r.done)
		}()
		return r
	}
	rs.mu.Lock()
	if out := rs.out[key]; out != nil {
		joined := rs.joined
		rs.mu.Unlock()
		if joined != nil {
			joined()
		}
		return out
	}
	if rs.out == nil {
		rs.out = map[string]*outputsRead{}
	}
	rs.out[key] = r
	rs.mu.Unlock()
	go func() {
		resp, err := read(context.WithoutCancel(ctx))
		rs.mu.Lock()
		delete(rs.out, key)
		r.resp, r.err = resp, err
		close(r.done)
		rs.mu.Unlock()
	}()
	return r
}

// inFlight is the number of reads rs holds that have not answered.
func (rs *outputsReads) inFlight() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return len(rs.out)
}

// outputsReadKey is the identity of the config a doctor gather reads the box
// over: the file it was loaded from and every setting it holds, as yaml.v3
// encodes them, the encoding a save of bridge.yaml writes (MPD's password
// included, which encoding/json leaves out), so two gathers share a read only
// over the same file and the same settings, and a config edited since reads
// the box afresh. "" where the settings do not encode: that gather reads
// alone.
func outputsReadKey(cfgFile string, cfg *config.Config) string {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(append([]byte(cfgFile+"\x00"), data...))
	return hex.EncodeToString(sum[:])
}

// cliOutputsReads is the reads of the outputs body this process's bridge
// doctor gathers share (outputsReads). A test stands in a table of its own
// for it before it builds a gather, and puts this one back once its reads
// have answered.
var cliOutputsReads = &outputsReads{}

// cliMPDInspections is the MPD inspections this process's bridge doctor
// gathers share (api.MPDInspections, backlog B650's third round): the probe
// output.mpd's auto asks at the gate and the one every build makes inspect
// mpd.conf and the player's files, which a dead mount holds with no end,
// and each gather's builder is a new one. A test stands in a table of its
// own for it before it builds a gather, and puts this one back once its
// inspections have returned.
var cliMPDInspections = &api.MPDInspections{}

// unreadInput is the input when the outputs body could not be read: the
// error, and what does not come from that body. The configured roots are
// judged unchecked, each by the name its path rule would carry, so the
// Music line still says whether those tracks play over the loopback route,
// whose base URL comes from the route, since the body that carries it is
// what failed.
func unreadInput(err error, src outputChecklistSource) outputcheck.Input {
	in := outputcheck.Input{Unread: err.Error()}
	if src.Roots != nil {
		for _, root := range src.Roots() {
			in.Roots = append(in.Roots, outputcheck.RootRule{Root: filepath.Base(root), State: "unchecked"})
		}
	}
	if src.LoopbackBaseURL != nil {
		in.Loopback.BaseURL = src.LoopbackBaseURL()
	}
	return in
}

// checklistInput reads the outputs body: the player, MPD and path rules
// every element shares, and each element's card. It reads the elements of
// kind mpd alone: the bridge's own player's (kind bridge, B470 PR 5b), which
// serve's body carries after them and which has no mpd object, is no MPD
// output, so the checklist stays MPD's.
func checklistInput(resp api.OutputsResponse) outputcheck.Input {
	var in outputcheck.Input
	for i, o := range resp.Outputs {
		if o.Kind != "mpd" || o.MPD == nil {
			continue
		}
		if i == 0 || in.Player.Port == 0 {
			in.Player = outputcheck.Player{
				Software:   o.Player.Software,
				Version:    o.Player.Version,
				MPDVersion: o.MPD.Version,
				Port:       o.MPD.Port,
				Addresses:  o.MPD.Addresses,
			}
			in.Loopback.BaseURL = o.MPD.LoopbackBaseURL
			in.Roots = nil
			for _, r := range o.PathRules {
				in.Roots = append(in.Roots, outputcheck.RootRule{Root: r.Root, State: r.State, Checked: r.Checked, Found: r.Found})
			}
		}
		if o.DAC != nil {
			in.DACs = append(in.DACs, outputcheck.DAC{
				ID:             o.DAC.ID,
				Name:           o.DAC.Name,
				USBID:          o.DAC.USBID,
				IsMPDsOutput:   o.DAC.IsMPDsOutput != nil && *o.DAC.IsMPDsOutput,
				MaxPCMRate:     o.DAC.Capabilities.MaxPCMRate,
				NativeDSDRates: o.DAC.Capabilities.NativeDSDRates,
			})
		}
	}
	return in
}

// consoleOutputChecklist is admin.Deps.OutputChecklist: the lines as the
// console's DTO.
func consoleOutputChecklist(src outputChecklistSource) func(context.Context) ([]admin.OutputCheckLine, bool) {
	return func(ctx context.Context) ([]admin.OutputCheckLine, bool) {
		lines, ok := outputChecklist(ctx, src)
		if !ok {
			return nil, false
		}
		out := make([]admin.OutputCheckLine, 0, len(lines))
		for _, l := range lines {
			out = append(out, admin.OutputCheckLine{Key: l.Key, Title: l.Title, Status: string(l.Status), Summary: l.Summary, Fix: l.Fix})
		}
		return out, true
	}
}

// checklistDoctorChecks is the checklist as doctor checks, for the
// console's own doctor run: the serving process's source, so the
// Diagnostics panel and the dashboard say the same thing, and a settings
// render does not dial MPD through a builder of its own.
func checklistDoctorChecks(src outputChecklistSource) func(context.Context) []doctor.Check {
	return func(ctx context.Context) []doctor.Check {
		lines, ok := outputChecklist(ctx, src)
		if !ok {
			return nil
		}
		return doctorChecks(lines)
	}
}

// doctorChecks are the lines as doctor checks: the key is the name, the
// fix is the hint.
func doctorChecks(lines []outputcheck.Line) []doctor.Check {
	out := make([]doctor.Check, 0, len(lines))
	for _, l := range lines {
		out = append(out, doctor.Check{Name: l.Key, Status: doctor.Status(l.Status), Summary: l.Summary, Hint: l.Fix})
	}
	return out
}

// doctorOutputChecklist is `bridge doctor`'s source: the GET /v1/outputs
// builder over the loaded config, asked once and closed, so the doctor
// holds no MPD connection past its run; the serving bridge's loopback
// route, asked on its port; and the paired devices in tokens.json. It
// stays MPD's: the one-shot builder lists MPD's elements alone, on the MPD
// gate, and the doctor never reads or makes the bridge's own player's
// identity (B470 PR 5b).
//
// seam, when not nil, edits the builder's config before it is built: a
// test stands in for Linux and for the box's MPD and cards there.
// Production passes nil. cfgFile is the file cfg was loaded from, beside
// which the record of its data dir is ("" for none).
//
// Its body is read through this process's table (cliOutputsReads, as it
// stands when the gather is built), so a gather over a config whose read
// another gather began and a root still holds waits on that read rather
// than beginning one of its own; and its builder inspects MPD through the
// process's inspections (cliMPDInspections, likewise), so a gather whose
// inspection another gather began waits on that one.
func doctorOutputChecklist(cfgFile string, cfg *config.Config, seam func(*api.LocalOutputsConfig)) func(context.Context) []doctor.Check {
	reads, inspections := cliOutputsReads, cliMPDInspections
	return func(ctx context.Context) []doctor.Check {
		route := &loopbackAnswer{ask: func() (string, error) {
			return probeLoopbackRoute(ctx, cfg.Output.EffectiveLoopbackPort())
		}}
		dbPath := manifest.DefaultDBPath(cfg.DataDir)
		lcfg := api.LocalOutputsConfig{
			Identity: api.OutputIdentity(cfg.DataDir),
			Output:   func() config.OutputConfig { return cfg.Output },
			Roots:    func() []string { return append([]string(nil), cfg.LibraryRoots...) },
			Tracks: func(ctx context.Context) ([]string, error) {
				// Opened per call and closed at once, as the other doctor
				// probes open it: `bridge serve` may hold the same store, and
				// a missing database is not created by a diagnostic. Nor is
				// one opened in a data dir the record refuses, which may hold
				// a database that is not this bridge's (B741's round 2, Codex
				// on #1441): MPD need not answer for the path rules to ask.
				if err := doctorStoreGate(cfgFile, cfg.DataDir); err != nil {
					return nil, err
				}
				if _, err := os.Stat(dbPath); err != nil {
					return nil, err
				}
				st, err := doctorOpenStore(dbPath)
				if err != nil {
					return nil, err
				}
				defer func() { _ = st.Close() }()
				return st.TrackPathsLocal(ctx)
			},
			Advertised: func() []string {
				listen, _ := configuredPort(cfg.ListenAddress)
				return advertise.URLs(advertise.Params{Port: listen, CustomEndpoints: cfg.CustomEndpoints,
					MDNS: cfg.EffectiveMDNSEnabled()})
			},
			Cards:           hostTrees.cards,
			LoopbackBaseURL: route.BaseURL,
			Inspections:     inspections,
		}
		if seam != nil {
			seam(&lcfg)
		}
		lo := api.NewLocalOutputs(lcfg)
		// Closed as the gather ends, though a read this builder began may
		// still be inside its Response, held by a root, with a later gather
		// waiting on it. Close waits for none of it: it takes the builder's
		// lock, which no holder keeps across I/O, closes the session
		// without taking its turn, and leaves the flight alone. The build
		// keeps the session it took, whose next command fails and dials
		// once more, and the idle close the flight's end arms closes that
		// one within a minute; so the body it answers reaches every gather
		// still waiting on it. A Close while the build is still dialling
		// closes nothing of that dial: the session it opens is stored once
		// it answers, after the Close, and lives until that idle close, a
		// minute after the build ends.
		defer lo.Close()
		src := doctorChecklistSource(cfg, lo, route)
		src.Reads, src.ReadKey = reads, outputsReadKey(cfgFile, cfg)
		lines, ok := outputChecklist(ctx, src)
		if !ok {
			return nil
		}
		return doctorChecks(lines)
	}
}

// doctorChecklistSource is what `bridge doctor` gathers the checklist
// from: the one-shot outputs builder, the configured roots, the running
// bridge's loopback route as its port answers, and the token file.
func doctorChecklistSource(cfg *config.Config, lo *api.LocalOutputs, route *loopbackAnswer) outputChecklistSource {
	return outputChecklistSource{
		Active:          lo.Active,
		Outputs:         lo.Response,
		Roots:           func() []string { return append([]string(nil), cfg.LibraryRoots...) },
		LoopbackBaseURL: route.BaseURL,
		LoopbackErr:     route.Err,
		Tokens:          func() []auth.Token { return tokenFile(cfg.DataDir) },
	}
}

// loopbackAnswer is the running bridge's loopback route as the doctor
// reads it: asked once, and only when a line needs it, so a bridge that is
// no player dials nothing.
type loopbackAnswer struct {
	ask  func() (string, error)
	once sync.Once
	base string
	err  error
}

func (a *loopbackAnswer) read() { a.once.Do(func() { a.base, a.err = a.ask() }) }

// BaseURL is the route's base URL while it serves, "" otherwise.
func (a *loopbackAnswer) BaseURL() string { a.read(); return a.base }

// Err is why the route is not serving: errLoopbackNotServing when it
// answers and refuses tracks, another error when something else holds its
// port, nil when it serves or nothing answers.
func (a *loopbackAnswer) Err() error { a.read(); return a.err }

// loopbackProbeLimit bounds the doctor's question to the loopback route.
const loopbackProbeLimit = 2 * time.Second

// errLoopbackNotServing is the loopback route answering on its port with
// not_serving: the running bridge has the route up, and output.mpd does
// not resolve on there now (off in the config it loaded, or auto with no
// MPD found at the gate's last look).
var errLoopbackNotServing = errors.New("the running bridge's loopback route answers, but is not serving tracks: output.mpd does not resolve on there")

// probeLoopbackRoute asks 127.0.0.1:port whether this bridge's loopback
// route serves there: the route answers a request for no track with its
// typed 404, which nothing else on the port would: not_found while it
// serves, and not_serving while output.mpd does not resolve on in the
// bridge that runs it, which is errLoopbackNotServing. A dial that fails
// is ("", nil), nothing serving there: the bridge is not running, or its
// route did not start. Any failed dial, not one errno: a refusal is
// ECONNREFUSED on unix and WSAECONNREFUSED on Windows. A port that
// answers otherwise is an error: something else holds it.
func probeLoopbackRoute(ctx context.Context, port int) (string, error) {
	if port <= 0 {
		return "", nil
	}
	base := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ctx, cancel := context.WithTimeout(ctx, loopbackProbeLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+dlna.FilePathPrefix, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" {
			return "", nil
		}
		return "", fmt.Errorf("port %d did not answer as the loopback route: %w", port, err)
	}
	defer resp.Body.Close()
	var refusal struct {
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode == http.StatusNotFound && json.Unmarshal(body, &refusal) == nil {
		switch refusal.Error {
		case "not_found":
			return base, nil
		case "not_serving":
			return "", errLoopbackNotServing
		}
	}
	return "", fmt.Errorf("port %d answers, but not as this bridge's loopback route", port)
}

// tokenFile is the device tokens in the data dir's token file, read and
// not written; a file that cannot be read holds none. LastUsedAt lands in
// that file lazily: the serving bridge writes a phone's use at most once
// per auth's lastUsedFlushInterval (30 s) and at its shutdown, so a phone
// that first connected in the last half minute may not count here yet,
// while the console, which reads the serving bridge's own store, counts it.
func tokenFile(dataDir string) []auth.Token {
	store, err := auth.OpenStore(filepath.Join(dataDir, tokensFileName))
	if err != nil {
		return nil
	}
	return store.List()
}
