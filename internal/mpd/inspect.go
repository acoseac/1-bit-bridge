package mpd

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/mpd/conf"
)

// Software is the player the box is running. Name is "Volumio", "moOde"
// or "MPD". Version, Build and BuildDate come from that player's own
// files: Volumio's /etc/os-release, moOde's footer, or the MPD greeting
// when the box is neither.
type Software struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Build     string `json:"build"`
	BuildDate string `json:"buildDate"`
}

// Daemon is the MPD this report dialled.
type Daemon struct {
	Version  string `json:"version"`
	Endpoint string `json:"endpoint"`
}

// Output is one audio output as the configuration and the live outputs
// command describe it together. An empty string is a setting neither
// source gave. Enabled is the effective value: the live outputenabled
// when the daemon answered, otherwise the file, otherwise "yes", which
// is what MPD does with an output that omits enabled.
type Output struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Device         string `json:"device"`
	Path           string `json:"path"`
	DOP            string `json:"dop"`
	MixerType      string `json:"mixerType"`
	MixerControl   string `json:"mixerControl"`
	MixerDevice    string `json:"mixerDevice"`
	Enabled        string `json:"enabled"`
	Format         string `json:"format"`
	AllowedFormats string `json:"allowedFormats"`
}

// NamedFilter is one filter block.
type NamedFilter struct {
	Name   string `json:"name"`
	Plugin string `json:"plugin"`
}

// NamedResampler is the resampler block.
type NamedResampler struct {
	Plugin  string `json:"plugin"`
	Quality string `json:"quality"`
}

// Report is what bridge output player prints. Every field is present in
// JSON; a slice is empty rather than null, and ConfigPermitted is null
// when the config command was not attempted. Password values are not a
// field: PasswordSet and PasswordCount are the only trace of them.
type Report struct {
	Software            Software       `json:"software"`
	MPD                 Daemon         `json:"mpd"`
	ConfPath            string         `json:"confPath"`
	MusicDirectory      string         `json:"musicDirectory"`
	PasswordSet         bool           `json:"passwordSet"`
	PasswordCount       int            `json:"passwordCount"`
	ConfigPermitted     *bool          `json:"configPermitted"`
	Outputs             []Output       `json:"outputs"`
	ReplayGain          string         `json:"replayGain"`
	VolumeNormalization string         `json:"volumeNormalization"`
	Resampler           NamedResampler `json:"resampler"`
	SamplerateConverter string         `json:"samplerateConverter"`
	AudioOutputFormat   string         `json:"audioOutputFormat"`
	Filters             []NamedFilter  `json:"filters"`
	ZeroconfEnabled     string         `json:"zeroconfEnabled"`
	Problems            []string       `json:"problems"`
}

// Found reports whether anything about a player was read: a known
// player, a configuration file, or a daemon that answered.
func (r Report) Found() bool {
	if r.Software.Name == "Volumio" || r.Software.Name == "moOde" {
		return true
	}
	return r.MPD.Version != "" || r.ConfPath != "" || r.MusicDirectory != "" || len(r.Outputs) > 0
}

// Options selects what Inspect reads. Root is a tree to detect the
// player in and to search for mpd.conf; empty means do not look at a
// tree. Conf is a configuration file to read instead of searching.
// Endpoint, when set, is dialled instead of the address the file names.
// Home and XDGConfigHome are the values MPD's search uses, and only
// when the caller is reading the host rather than a captured tree.
// Password is sent once if set, and is not retained.
type Options struct {
	Root          string
	Conf          string
	Endpoint      string
	Home          string
	XDGConfigHome string
	XDGRuntimeDir string
	Password      string
}

// Inspect detects the player, reads its mpd.conf and asks the daemon
// the read commands. A daemon that is down is a problem on the report,
// not a failure of the call: the file is still worth printing.
func Inspect(ctx context.Context, opt Options) Report {
	var rep Report
	var file *conf.File
	if opt.Root != "" {
		sw, probs := detectSoftware(opt.Root)
		rep.Software = sw
		rep.Problems = append(rep.Problems, probs...)
	}
	if opt.Conf != "" {
		f, err := conf.ReadHost(opt.Root, opt.Conf)
		if err != nil {
			rep.Problems = append(rep.Problems, "conf: "+err.Error())
		} else {
			file = &f
		}
	} else if opt.Root != "" {
		loc := conf.Locate{Root: opt.Root, Home: opt.Home, XDGConfigHome: opt.XDGConfigHome}
		if logical, host, ok := loc.Find(); ok {
			f, err := conf.ReadFile(opt.Root, logical, host)
			if err != nil {
				rep.Problems = append(rep.Problems, "conf: "+err.Error())
			} else {
				file = &f
			}
		}
	}
	if file != nil {
		applyConf(&rep, *file)
	}
	endpoint := opt.Endpoint
	if endpoint == "" && file != nil {
		endpoint, _ = file.Endpoint(opt.Home)
	}
	endpoint = expandEndpoint(endpoint, opt.Home, opt.XDGRuntimeDir, opt.XDGConfigHome)
	if endpoint != "" {
		askDaemon(ctx, &rep, endpoint, opt.Password)
	}
	if rep.Software.Name == "" && (rep.MPD.Version != "" || rep.ConfPath != "") {
		rep.Software.Name = "MPD"
		if rep.Software.Version == "" {
			rep.Software.Version = rep.MPD.Version
		}
	}
	rep.normalize()
	return rep
}

func (r *Report) normalize() {
	if r.Outputs == nil {
		r.Outputs = []Output{}
	}
	if r.Filters == nil {
		r.Filters = []NamedFilter{}
	}
	if r.Problems == nil {
		r.Problems = []string{}
	}
}

func applyConf(rep *Report, f conf.File) {
	f = f.Display()
	rep.ConfPath = f.Path
	rep.MusicDirectory = f.MusicDirectory
	rep.PasswordSet = f.PasswordSet
	rep.PasswordCount = f.PasswordCount
	rep.ReplayGain = f.ReplayGain
	rep.VolumeNormalization = f.VolumeNormalization
	rep.Resampler = NamedResampler{Plugin: f.Resampler.Plugin, Quality: f.Resampler.Quality}
	rep.SamplerateConverter = f.SamplerateConverter
	rep.AudioOutputFormat = f.AudioOutputFormat
	rep.ZeroconfEnabled = f.ZeroconfEnabled
	for _, flt := range f.Filters {
		rep.Filters = append(rep.Filters, NamedFilter{Name: flt.Name, Plugin: flt.Plugin})
	}
	for _, o := range f.Outputs {
		rep.Outputs = append(rep.Outputs, Output{
			Name: o.Name, Type: o.Type, Device: o.Device, Path: o.Path,
			DOP: o.DOP, MixerType: o.MixerType, MixerControl: o.MixerControl,
			MixerDevice: o.MixerDevice, Enabled: effectiveEnabled(o.Enabled, nil),
			Format: o.Format, AllowedFormats: o.AllowedFormats,
		})
	}
	rep.Problems = append(rep.Problems, f.Problems...)
}

// effectiveEnabled applies MPD's default. An omitted enabled is "yes".
// A live answer, when the pointer is non-nil, replaces the file.
func effectiveEnabled(file string, live *bool) string {
	if live != nil {
		if *live {
			return "yes"
		}
		return "no"
	}
	if file == "" {
		return "yes"
	}
	return file
}

func askDaemon(ctx context.Context, rep *Report, endpoint, password string) {
	c, err := Dial(ctx, endpoint, password)
	if err != nil {
		rep.Problems = append(rep.Problems, redact(err, password).Error())
		rep.MPD.Endpoint = endpoint
		return
	}
	defer func() { _ = c.Close() }()
	rep.MPD.Version = c.Version()
	rep.MPD.Endpoint = endpoint

	if outs, err := c.Outputs(ctx); err != nil {
		note(rep, err, password)
	} else {
		mergeOutputs(rep, ServerOutputs(outs))
	}
	if gain, err := c.ReplayGainStatus(ctx); err != nil {
		note(rep, err, password)
	} else if mode, ok := gain.Get("replay_gain_mode"); ok && mode != "" {
		rep.ReplayGain = mode
	}
	cfg, err := c.Config(ctx)
	switch {
	case err == nil:
		yes := true
		rep.ConfigPermitted = &yes
		if rep.MusicDirectory == "" {
			if md, ok := cfg.Get("music_directory"); ok {
				rep.MusicDirectory = md
			}
		}
	default:
		var ack *ACKError
		if errors.As(err, &ack) && ack.Code == ACKPermission {
			no := false
			rep.ConfigPermitted = &no
		} else {
			note(rep, err, password)
		}
	}
}

func note(rep *Report, err error, password string) {
	rep.Problems = append(rep.Problems, redact(err, password).Error())
}

func mergeOutputs(rep *Report, live []ServerOutput) {
	if len(rep.Outputs) == 0 {
		for _, o := range live {
			en := o.Enabled
			rep.Outputs = append(rep.Outputs, Output{
				Name: o.Name, Type: o.Plugin, Enabled: effectiveEnabled("", &en),
				DOP: liveDOP(o), AllowedFormats: liveFormats(o),
			})
		}
		return
	}
	byName := map[string]*ServerOutput{}
	for i := range live {
		byName[live[i].Name] = &live[i]
	}
	seen := map[string]bool{}
	for i := range rep.Outputs {
		o := &rep.Outputs[i]
		liveOut, ok := byName[o.Name]
		if !ok {
			continue
		}
		seen[o.Name] = true
		if liveOut.Plugin != "" {
			o.Type = liveOut.Plugin
		}
		en := liveOut.Enabled
		o.Enabled = effectiveEnabled(o.Enabled, &en)
		if o.DOP == "" {
			o.DOP = liveDOP(*liveOut)
		}
		if o.AllowedFormats == "" {
			o.AllowedFormats = liveFormats(*liveOut)
		}
	}
	for _, liveOut := range live {
		if seen[liveOut.Name] {
			continue
		}
		en := liveOut.Enabled
		rep.Outputs = append(rep.Outputs, Output{
			Name: liveOut.Name, Type: liveOut.Plugin, Enabled: effectiveEnabled("", &en),
			DOP: liveDOP(liveOut), AllowedFormats: liveFormats(liveOut),
		})
	}
}

func liveDOP(o ServerOutput) string {
	v, ok := o.Attribute("dop")
	if !ok || v == "" {
		return ""
	}
	switch v {
	case "1", "yes":
		return "yes"
	case "0", "no":
		return "no"
	default:
		return v
	}
}

func liveFormats(o ServerOutput) string {
	v, _ := o.Attribute("allowed_formats")
	return v
}

// detectSoftware names the player from the files a distribution
// installs. Volumio is both /volumio and /data/configuration, the pair
// a plain mpd or moOde image does not have; its version is the
// VOLUMIO_* keys in /etc/os-release. moOde installs
// /usr/local/bin/moodeutl (moode-player/moode, the utility the image
// uses for its own version) and writes the release into
// /var/www/footer.min.php, which moodeutl --mooderel reads; the
// unminified footer.php is the fallback. The string is "Release: 10.2.2
// 2026-05-31". The reader splits that instead of executing moodeutl.
// Volumio wins when both sets of files are present.
func detectSoftware(root string) (Software, []string) {
	if conf.Exists(root, "/volumio") && conf.Exists(root, "/data/configuration") {
		sw := Software{Name: "Volumio"}
		b, err := conf.ReadLogical(root, "/etc/os-release", 64<<10)
		if err != nil {
			if !os.IsNotExist(err) {
				return sw, []string{"os-release: " + err.Error()}
			}
			return sw, nil
		}
		kv := parseOSRelease(string(b))
		sw.Version = kv["VOLUMIO_VERSION"]
		sw.Build = kv["VOLUMIO_BUILD_VERSION"]
		sw.BuildDate = kv["VOLUMIO_BUILD_DATE"]
		return sw, nil
	}
	if conf.Exists(root, "/usr/local/bin/moodeutl") {
		sw := Software{Name: "moOde"}
		for _, p := range []string{"/var/www/footer.min.php", "/var/www/footer.php"} {
			b, err := conf.ReadLogical(root, p, 256<<10)
			if err != nil {
				continue
			}
			sw.Version, sw.BuildDate = moodeRelease(string(b))
			break
		}
		return sw, nil
	}
	return Software{}, nil
}

func parseOSRelease(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		out[k] = v
	}
	return out
}

func moodeRelease(text string) (version, date string) {
	const key = "Release: "
	i := strings.Index(text, key)
	if i < 0 {
		return "", ""
	}
	rest := text[i+len(key):]
	if j := strings.IndexAny(rest, "<\n\r"); j >= 0 {
		rest = rest[:j]
	}
	fields := strings.Fields(rest)
	if len(fields) > 0 {
		version = fields[0]
	}
	if len(fields) > 1 {
		date = fields[1]
	}
	return version, date
}

func expandEndpoint(addr, home, xdgRuntime, xdgConfig string) string {
	if addr == "" || !strings.Contains(addr, "$") {
		return addr
	}
	return os.Expand(addr, func(k string) string {
		switch k {
		case "HOME":
			if home == "" {
				return "$HOME"
			}
			return home
		case "XDG_RUNTIME_DIR":
			if xdgRuntime == "" {
				return "$XDG_RUNTIME_DIR"
			}
			return xdgRuntime
		case "XDG_CONFIG_HOME":
			if xdgConfig == "" {
				return "$XDG_CONFIG_HOME"
			}
			return xdgConfig
		default:
			return "$" + k
		}
	})
}
