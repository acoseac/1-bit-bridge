package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/mpd"
)

// outputPlayerCmd implements `bridge output player` on this host.
func outputPlayerCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runOutputPlayer(ctx, args, runtime.GOOS, stdout, stderr)
}

// runOutputPlayer is `bridge output player`.
//
// Exit codes: 0 a player, a configuration or a daemon was read, 1 nothing
// was (including this host off Linux when no flag names a tree, a file or
// an endpoint), 2 usage error. A daemon that does not answer does not by
// itself fail the command when the player or its configuration was read:
// the failure is a problem on the report.
func runOutputPlayer(ctx context.Context, args []string, goos string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("output player", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the report as JSON")
	confPath := fs.String("conf", "", "read this mpd.conf instead of searching")
	endpoint := fs.String("mpd", "", "dial this host:port or unix-socket path instead of the address the configuration names")
	root := fs.String("root", "", "read the player and its configuration from this directory instead of the host's /")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "output player: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	opt, refuse := playerOptions(goos, *root, *confPath, *endpoint, os.Getenv)
	if refuse {
		fmt.Fprintf(stderr, "output player: the player is read from a Linux box, and this is %s\n", goos)
		fmt.Fprintln(stderr, "  --root reads a tree captured from one, --conf a configuration file, --mpd an endpoint.")
		return 1
	}
	ctx, cancel := context.WithTimeout(ctx, mpd.DefaultTimeout)
	defer cancel()
	rep := mpd.Inspect(ctx, opt)
	if *asJSON {
		if writeJSONIndent(stdout, stderr, "output player", rep) != 0 {
			return 1
		}
	} else {
		renderPlayer(stdout, rep)
	}
	if !rep.Found() {
		return 1
	}
	return 0
}

// playerOptions decides what Inspect is allowed to read. The rules live
// in mpd.OptionsForHost, which GET /v1/outputs uses as well, so the
// command and the route cannot drift. The command has no password flag.
func playerOptions(goos, root, confPath, endpoint string, getenv func(string) string) (mpd.Options, bool) {
	return mpd.OptionsForHost(goos, root, confPath, endpoint, "", getenv)
}

func renderPlayer(w io.Writer, r mpd.Report) {
	for _, p := range r.Problems {
		fmt.Fprintf(w, "problem: %s\n", p)
	}
	switch {
	case r.Software.Name != "" && r.Software.Version != "":
		fmt.Fprintf(w, "player: %s %s\n", r.Software.Name, r.Software.Version)
	case r.Software.Name != "":
		fmt.Fprintf(w, "player: %s\n", r.Software.Name)
	default:
		fmt.Fprintf(w, "player: unknown\n")
	}
	if r.Software.Build != "" {
		fmt.Fprintf(w, "  build: %s\n", r.Software.Build)
	}
	if r.Software.BuildDate != "" {
		fmt.Fprintf(w, "  built: %s\n", r.Software.BuildDate)
	}
	switch {
	case r.MPD.Version != "" || r.MPD.Endpoint != "":
		fmt.Fprintf(w, "mpd: %s at %s\n", orWord(r.MPD.Version), orWord(r.MPD.Endpoint))
	default:
		fmt.Fprintf(w, "mpd: unset\n")
	}
	switch {
	case r.ConfigPermitted == nil:
		fmt.Fprintf(w, "  config: not attempted\n")
	case *r.ConfigPermitted:
		fmt.Fprintf(w, "  config: permitted\n")
	default:
		fmt.Fprintf(w, "  config: not permitted\n")
	}
	fmt.Fprintf(w, "conf: %s\n", orWord(r.ConfPath))
	fmt.Fprintf(w, "music directory: %s\n", orWord(r.MusicDirectory))
	switch {
	case !r.PasswordSet:
		fmt.Fprintf(w, "password: not set\n")
	case r.PasswordCount == 1:
		fmt.Fprintf(w, "password: set\n")
	default:
		fmt.Fprintf(w, "password: %d set\n", r.PasswordCount)
	}
	fmt.Fprintf(w, "outputs:\n")
	if len(r.Outputs) == 0 {
		fmt.Fprintf(w, "  none\n")
	}
	for _, o := range r.Outputs {
		fmt.Fprintf(w, "  %s: type %s, device %s", orWord(o.Name), orWord(o.Type), orWord(o.Device))
		if o.Path != "" {
			fmt.Fprintf(w, ", path %s", o.Path)
		}
		fmt.Fprintf(w, ", dop %s, mixer %s, enabled %s\n", orWord(o.DOP), mixerPhrase(o), orWord(o.Enabled))
	}
	fmt.Fprintf(w, "replay gain: %s\n", orWord(r.ReplayGain))
	fmt.Fprintf(w, "volume normalization: %s\n", orWord(r.VolumeNormalization))
	switch {
	case r.Resampler.Plugin == "" && r.Resampler.Quality == "":
		fmt.Fprintf(w, "resampler: unset\n")
	case r.Resampler.Quality != "":
		fmt.Fprintf(w, "resampler: %s (quality %s)\n", orWord(r.Resampler.Plugin), r.Resampler.Quality)
	default:
		fmt.Fprintf(w, "resampler: %s\n", r.Resampler.Plugin)
	}
	fmt.Fprintf(w, "samplerate converter: %s\n", orWord(r.SamplerateConverter))
	fmt.Fprintf(w, "audio output format: %s\n", orWord(r.AudioOutputFormat))
	if len(r.Filters) == 0 {
		fmt.Fprintf(w, "filters: none\n")
	} else {
		names := make([]string, 0, len(r.Filters))
		for _, f := range r.Filters {
			switch {
			case f.Name != "" && f.Plugin != "":
				names = append(names, f.Name+" ("+f.Plugin+")")
			case f.Name != "":
				names = append(names, f.Name)
			default:
				names = append(names, orWord(f.Plugin))
			}
		}
		fmt.Fprintf(w, "filters: %s\n", strings.Join(names, ", "))
	}
	fmt.Fprintf(w, "zeroconf: %s\n", orWord(r.ZeroconfEnabled))
}

func mixerPhrase(o mpd.Output) string {
	var parts []string
	if o.MixerType != "" {
		parts = append(parts, o.MixerType)
	}
	if o.MixerControl != "" {
		parts = append(parts, o.MixerControl)
	}
	if o.MixerDevice != "" {
		parts = append(parts, o.MixerDevice)
	}
	if len(parts) == 0 {
		return "unset"
	}
	return strings.Join(parts, " ")
}

func orWord(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}
