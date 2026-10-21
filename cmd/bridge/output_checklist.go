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
	"syscall"
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
	// LoopbackErr is why the loopback track route is not serving, or nil.
	LoopbackErr func() error
	// Phones is the number of paired devices.
	Phones func() int
}

// outputChecklist returns the four lines, or false when the bridge is no
// player.
func outputChecklist(ctx context.Context, src outputChecklistSource) ([]outputcheck.Line, bool) {
	if src.Active == nil || !src.Active(ctx) {
		return nil, false
	}
	var in outputcheck.Input
	if src.Outputs != nil {
		if resp, err := src.Outputs(ctx); err == nil {
			in = checklistInput(resp)
		}
	}
	if src.LoopbackErr != nil {
		if err := src.LoopbackErr(); err != nil {
			in.Loopback.Err = err.Error()
		}
	}
	if src.Phones != nil {
		in.Phones = src.Phones()
	}
	return outputcheck.Lines(in), true
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
		port := cfg.Output.EffectiveLoopbackPort()
		base, probeErr := probeLoopbackRoute(ctx, port)
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
			LoopbackBaseURL: func() string { return base },
		}
		if seam != nil {
			seam(&lcfg)
		}
		lo := api.NewLocalOutputs(lcfg)
		defer lo.Close()
		lines, ok := outputChecklist(ctx, outputChecklistSource{
			Active:      lo.Active,
			Outputs:     lo.Response,
			LoopbackErr: func() error { return probeErr },
			Phones:      func() int { return countPairedDevices(cfg.DataDir) },
		})
		if !ok {
			return nil
		}
		return doctorChecks(lines)
	}
}

// loopbackProbeLimit bounds the doctor's question to the loopback route.
const loopbackProbeLimit = 2 * time.Second

// probeLoopbackRoute asks 127.0.0.1:port whether this bridge's loopback
// route serves there: the route answers a request for no track with its
// typed 404, which nothing else on the port would. Nothing listening is
// ("", nil): the bridge is not running, or its route did not start. A
// port that answers otherwise is an error: something else holds it.
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
		if errors.Is(err, syscall.ECONNREFUSED) {
			return "", nil
		}
		return "", fmt.Errorf("port %d did not answer as the loopback route: %w", port, err)
	}
	defer resp.Body.Close()
	var refusal struct {
		Error string `json:"error"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode == http.StatusNotFound && json.Unmarshal(body, &refusal) == nil && refusal.Error == "not_found" {
		return base, nil
	}
	return "", fmt.Errorf("port %d answers, but not as this bridge's loopback route", port)
}

// countPairedDevices is the number of tokens in the data dir's token file,
// read and not written. A file that cannot be read counts none.
func countPairedDevices(dataDir string) int {
	store, err := auth.OpenStore(filepath.Join(dataDir, tokensFileName))
	if err != nil {
		return 0
	}
	return len(store.List())
}
