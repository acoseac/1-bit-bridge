package main

import (
	"context"
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
	// Active is the localOutputs gate: output.mpd resolves on and the host
	// is Linux. When it is false the bridge is no player and there is no
	// checklist.
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

func readOutputs(ctx context.Context, src outputChecklistSource) (api.OutputsResponse, error) {
	if src.Outputs == nil {
		return api.OutputsResponse{}, errOutputsNotWired
	}
	return src.Outputs(ctx)
}

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
// every element shares, and each element's card.
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
// route, asked on its port; and the paired devices in tokens.json.
//
// seam, when not nil, edits the builder's config before it is built: a
// test stands in for Linux and for the box's MPD and cards there.
// Production passes nil.
func doctorOutputChecklist(cfg *config.Config, seam func(*api.LocalOutputsConfig)) func(context.Context) []doctor.Check {
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
				// a missing database is not created by a diagnostic.
				if _, err := os.Stat(dbPath); err != nil {
					return nil, err
				}
				st, err := manifest.OpenStore(dbPath)
				if err != nil {
					return nil, err
				}
				defer func() { _ = st.Close() }()
				return st.TrackPathsLocal(ctx)
			},
			Advertised: func() []string {
				listen, _ := configuredPort(cfg.ListenAddress)
				return advertise.URLs(advertise.Params{Port: listen, CustomEndpoints: cfg.CustomEndpoints})
			},
			LoopbackBaseURL: route.BaseURL,
		}
		if seam != nil {
			seam(&lcfg)
		}
		lo := api.NewLocalOutputs(lcfg)
		defer lo.Close()
		lines, ok := outputChecklist(ctx, doctorChecklistSource(cfg, lo, route))
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
