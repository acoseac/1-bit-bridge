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
// Home and the XDG directories are the values MPD's search and its path
// expansion use, and only when the caller is reading the host rather
// than a captured tree. Password is sent once if set, and is not retained.
type Options struct {
	Root          string
	Conf          string
	Endpoint      string
	Home          string
	XDGConfigHome string
	XDGRuntimeDir string
	XDGCacheHome  string
	XDGMusicDir   string
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
	addr, _, err := resolveDial(endpoint, pathEnv{
		Home: opt.Home, XDGConfigHome: opt.XDGConfigHome,
		XDGRuntimeDir: opt.XDGRuntimeDir, XDGCacheHome: opt.XDGCacheHome,
		XDGMusicDir: opt.XDGMusicDir,
	})
	if endpoint != "" {
		if err != nil {
			rep.Problems = append(rep.Problems, redactConf(file, err.Error()))
			rep.MPD.Endpoint = redactConf(file, endpoint)
		} else {
			askDaemon(ctx, &rep, addr, opt.Password)
			rep.MPD.Endpoint = redactConf(file, rep.MPD.Endpoint)
			for i := range rep.Problems {
				rep.Problems[i] = redactConf(file, rep.Problems[i])
			}
		}
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

// parseOSRelease reads an os-release(5) file. A line whose first
// non-space character is '#' is a comment. A double-quoted value
// unescapes \\ \" \$ and \`, which is the set os-release names as
// shell special characters; a '#' inside the quotes is part of the
// value. A single-quoted value is literal. An unquoted value ends at
// whitespace or at '#'.
func parseOSRelease(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		line = strings.TrimLeft(line, " \t")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, raw, ok := strings.Cut(line, "=")
		if !ok || k == "" || strings.ContainsAny(k, " \t#") {
			continue
		}
		out[k] = osReleaseValue(raw)
	}
	return out
}

func osReleaseValue(raw string) string {
	raw = strings.TrimLeft(raw, " \t")
	if raw == "" {
		return ""
	}
	switch raw[0] {
	case '"':
		return unquoteOSDouble(raw[1:])
	case '\'':
		return unquoteOSSingle(raw[1:])
	default:
		return unquoteOSBare(raw)
	}
}

func unquoteOSDouble(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			return b.String()
		case '\\':
			if i+1 >= len(s) {
				b.WriteByte('\\')
				return b.String()
			}
			i++
			switch s[i] {
			case '\\', '"', '$', '`':
				b.WriteByte(s[i])
			default:
				// POSIX double quotes leave a backslash that does not
				// escape $, `, " or \ in place.
				b.WriteByte('\\')
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func unquoteOSSingle(s string) string {
	if i := strings.IndexByte(s, '\''); i >= 0 {
		return s[:i]
	}
	return s
}

func unquoteOSBare(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '#', ' ', '\t':
			return strings.TrimRight(b.String(), " \t")
		case '\\':
			if i+1 >= len(s) {
				b.WriteByte('\\')
				return b.String()
			}
			i++
			b.WriteByte(s[i])
		default:
			b.WriteByte(s[i])
		}
	}
	return strings.TrimRight(b.String(), " \t")
}

// moodeRelease reads the release moOde prints in its footer. The line
// getMoodeRel parses is `<li>Release: 10.2.2 2026-05-31</li>` in
// www/footer.php (the same text survives into footer.min.php). A
// "Release:" earlier in a comment or a script string is not that line.
// The value ends at the markup that closes it: '<', '"', ';', ')' or
// the end of the line.
func moodeRelease(text string) (version, date string) {
	text = stripHTMLComments(text)
	rest := text
	for {
		i := strings.Index(rest, "<li>")
		if i < 0 {
			return "", ""
		}
		rest = rest[i+len("<li>"):]
		rest = strings.TrimLeft(rest, " \t")
		const key = "Release:"
		if !strings.HasPrefix(rest, key) {
			continue
		}
		after := rest[len(key):]
		if after == "" || (after[0] != ' ' && after[0] != '\t') {
			continue
		}
		after = strings.TrimLeft(after, " \t")
		if j := strings.IndexAny(after, "<\";)\n\r"); j >= 0 {
			after = after[:j]
		}
		fields := strings.Fields(after)
		if len(fields) > 0 {
			version = fields[0]
		}
		if len(fields) > 1 {
			date = fields[1]
		}
		return version, date
	}
}

func stripHTMLComments(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "<!--")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		s = s[i+4:]
		j := strings.Index(s, "-->")
		if j < 0 {
			return b.String()
		}
		s = s[j+3:]
	}
}

// pathEnv is the set of directories MPD's ParsePath expands
// (src/config/Path.cxx GetVariable): HOME, XDG_CONFIG_HOME,
// XDG_RUNTIME_DIR, XDG_CACHE_HOME and XDG_MUSIC_DIR.
type pathEnv struct {
	Home          string
	XDGConfigHome string
	XDGRuntimeDir string
	XDGCacheHome  string
	XDGMusicDir   string
}

// resolveDial expands a bind or a --mpd flag the way MPD expands a
// socket (src/config/Net.cxx ServerSocketAddGeneric and ParsePath).
// A leading '/' is already a socket. A leading '~' is the home
// directory (only '~' and '~/', not '~user'). A leading '$' is one of
// the five variables, then an optional '/rest'. Anything that does not
// expand to an absolute path is an error and is not a TCP address.
// The error does not quote the endpoint: the path can hold a password.
func resolveDial(endpoint string, env pathEnv) (addr, network string, err error) {
	if endpoint == "" {
		return "", "", nil
	}
	switch endpoint[0] {
	case '@':
		return endpoint, "unix", nil
	case '/', '~', '$':
		expanded, err := expandSocket(endpoint, env)
		if err != nil {
			return "", "", err
		}
		if !strings.HasPrefix(expanded, "/") {
			return "", "", errRelativeSocket
		}
		return expanded, "unix", nil
	}
	if strings.Contains(endpoint, "/") {
		return "", "", errRelativeSocket
	}
	return endpoint, "tcp", nil
}

var (
	errRelativeSocket = errors.New("mpd: relative socket path")
	errEndpointHome   = errors.New("mpd: socket home is unset")
	errEndpointUser   = errors.New("mpd: socket ~user is not expanded")
	errEndpointUnset  = errors.New("mpd: socket variable is unset")
	errEndpointVar    = errors.New("mpd: unknown socket variable")
)

func expandSocket(endpoint string, env pathEnv) (string, error) {
	switch {
	case strings.HasPrefix(endpoint, "/"):
		return endpoint, nil
	case endpoint == "~" || strings.HasPrefix(endpoint, "~/"):
		if !strings.HasPrefix(env.Home, "/") {
			return "", errEndpointHome
		}
		if endpoint == "~" {
			return env.Home, nil
		}
		return strings.TrimRight(env.Home, "/") + "/" + endpoint[2:], nil
	case strings.HasPrefix(endpoint, "~"):
		return "", errEndpointUser
	case strings.HasPrefix(endpoint, "$"):
		name, rest, _ := strings.Cut(endpoint[1:], "/")
		if name == "" {
			return "", errEndpointUnset
		}
		val, err := lookupPathVar(name, env)
		if err != nil {
			return "", err
		}
		if rest == "" {
			return val, nil
		}
		return strings.TrimRight(val, "/") + "/" + rest, nil
	default:
		return "", errRelativeSocket
	}
}

func lookupPathVar(name string, env pathEnv) (string, error) {
	switch name {
	case "HOME":
		if strings.HasPrefix(env.Home, "/") {
			return env.Home, nil
		}
	case "XDG_RUNTIME_DIR":
		if absDir(env.XDGRuntimeDir) {
			return env.XDGRuntimeDir, nil
		}
	case "XDG_CONFIG_HOME":
		if absDir(env.XDGConfigHome) {
			return env.XDGConfigHome, nil
		}
		if strings.HasPrefix(env.Home, "/") && absDir(strings.TrimRight(env.Home, "/")+"/.config") {
			return strings.TrimRight(env.Home, "/") + "/.config", nil
		}
	case "XDG_CACHE_HOME":
		if absDir(env.XDGCacheHome) {
			return env.XDGCacheHome, nil
		}
		if strings.HasPrefix(env.Home, "/") && absDir(strings.TrimRight(env.Home, "/")+"/.cache") {
			return strings.TrimRight(env.Home, "/") + "/.cache", nil
		}
	case "XDG_MUSIC_DIR":
		if strings.HasPrefix(env.XDGMusicDir, "/") {
			return env.XDGMusicDir, nil
		}
	default:
		return "", errEndpointVar
	}
	return "", errEndpointUnset
}

func absDir(p string) bool {
	if !strings.HasPrefix(p, "/") {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func redactConf(file *conf.File, s string) string {
	if file == nil {
		return s
	}
	return file.Redact(s)
}
