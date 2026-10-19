package conf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseVolumioFixture(t *testing.T) {
	text := string(readFixture(t, "volumio-mpd.conf"))
	f, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Problems) != 0 {
		t.Fatalf("problems: %q", f.Problems)
	}
	if f.MusicDirectory != "/var/lib/mpd/music" {
		t.Errorf("music directory %q", f.MusicDirectory)
	}
	if strings.Join(f.BindAddresses, ",") != "any" || f.Port != nil {
		t.Errorf("bind %q port %v; the commented port is not a setting", f.BindAddresses, f.Port)
	}
	if f.PasswordSet || f.PasswordCount != 0 || len(f.passwordValues()) != 0 {
		t.Fatalf("the commented proxy_password was taken as a password: %+v", f.passwordValues())
	}
	if f.VolumeNormalization != "no" || f.ReplayGain != "" || f.ZeroconfEnabled != "" || f.SamplerateConverter != "" || f.AudioOutputFormat != "" {
		t.Errorf("scalars = replay %q norm %q zero %q sr %q format %q",
			f.ReplayGain, f.VolumeNormalization, f.ZeroconfEnabled, f.SamplerateConverter, f.AudioOutputFormat)
	}
	if f.Resampler.Plugin != "soxr" || f.Resampler.Quality != "high" {
		t.Errorf("resampler %+v", f.Resampler)
	}
	if len(f.Filters) != 0 {
		t.Errorf("filters %+v", f.Filters)
	}
	if len(f.Outputs) != 2 {
		t.Fatalf("outputs %+v", f.Outputs)
	}
	alsa, fifo := f.Outputs[0], f.Outputs[1]
	if alsa.Type != "alsa" || alsa.Name != "alsa" || alsa.Device != "volumio" || alsa.DOP != "no" || alsa.Enabled != "" || alsa.MixerType != "" {
		t.Errorf("alsa output %+v; enabled is omitted, not a guessed yes", alsa)
	}
	if fifo.Type != "fifo" || fifo.Name != "multiroom" || fifo.Enabled != "no" || fifo.Path != "/tmp/snapfifo" || fifo.Format != "44100:16:2" || fifo.Device != "" {
		t.Errorf("fifo output %+v", fifo)
	}
	addr, network := f.Endpoint("")
	if addr != "127.0.0.1:6600" || network != "tcp" {
		t.Errorf("endpoint %s %s; bind \"any\" dials loopback on the default port", addr, network)
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if bytesContains(raw, "proxy_password") {
		t.Errorf("json kept a comment: %s", raw)
	}
}

func TestParseQuotingEscapesCommentsAndUnknownKeys(t *testing.T) {
	const text = "" +
		"# music_directory \"hidden\"\n" +
		"\t# indented\n" +
		"music_directory \"a\\\"b\\\\c\" # trailing\n" +
		"music_directory \"hello\\nworld\"\n" +
		"unknown_setting \"zzz\"\n" +
		"bind_to_address \"127.0.0.1\"\n" +
		"not a quote\n" +
		"port \"nope\"\n" +
		"port \"6601\"\n" +
		"replaygain \"album\" extra\n"
	f, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if f.MusicDirectory != `hellonworld` {
		t.Errorf("music directory %q; a backslash escapes the next byte, and the second assignment wins", f.MusicDirectory)
	}
	if strings.Join(f.BindAddresses, ",") != "127.0.0.1" {
		t.Errorf("bind %q", f.BindAddresses)
	}
	if f.Port == nil || *f.Port != 6601 {
		t.Errorf("port %v", f.Port)
	}
	if f.ReplayGain != "" {
		t.Errorf("a value with trailing tokens was kept: %q", f.ReplayGain)
	}
	joined := strings.Join(f.Problems, "\n")
	for _, want := range []string{"quoted value expected", "port \"nope\"", "trailing tokens"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "hidden") || strings.Contains(f.String(), "zzz") {
		t.Errorf("a comment or an unknown key leaked:\n%s\n%s", joined, f.String())
	}
}

func TestParseBlocks(t *testing.T) {
	const text = "" +
		"audio_output {\n" +
		"  type \"alsa\"\n" +
		"  name \"out\"\n" +
		"  type \"oss\"\n" +
		"  device \"hw:0,0\"\n" +
		"  dop \"yes\"\n" +
		"  mixer_type \"hardware\"\n" +
		"  mixer_control \"PCM\"\n" +
		"  mixer_device \"hw:0\"\n" +
		"  enabled \"yes\"\n" +
		"  format \"44100:16:2\"\n" +
		"  allowed_formats \"96000:*:*\"\n" +
		"  password \"shout-secret-value\"\n" +
		"}\n" +
		"resampler {\n" +
		"  plugin \"soxr\"\n" +
		"  quality \"very high\"\n" +
		"  threads \"2\"\n" +
		"}\n" +
		"resampler {\n  plugin \"src\"\n  quality \"low\"\n}\n" +
		"filter {\n" +
		"  plugin \"ffmpeg\"\n" +
		"  name \"eq\"\n" +
		"}\n" +
		"decoder {\n" +
		"  plugin \"faad\"\n" +
		"  enabled \"no\"\n" +
		"}\n" +
		"audio_output {\n" +
		"  type \"fifo\"\n"
	f, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Outputs) != 1 || f.Outputs[0].Type != "alsa" || f.Outputs[0].Name != "out" {
		t.Fatalf("outputs %+v; a repeated type keeps the first", f.Outputs)
	}
	o := f.Outputs[0]
	if o.Device != "hw:0,0" || o.DOP != "yes" || o.MixerType != "hardware" || o.MixerControl != "PCM" || o.MixerDevice != "hw:0" || o.Enabled != "yes" || o.Format != "44100:16:2" || o.AllowedFormats != "96000:*:*" {
		t.Errorf("output %+v", o)
	}
	if f.PasswordSet || strings.Contains(f.String(), "shout-secret-value") || bytesContains(mustJSON(t, f), "shout-secret-value") {
		t.Error("a password inside an output was taken as MPD's password")
	}
	if f.Resampler.Plugin != "soxr" || f.Resampler.Quality != "very high" {
		t.Errorf("resampler %+v", f.Resampler)
	}
	if len(f.Filters) != 1 || f.Filters[0].Name != "eq" || f.Filters[0].Plugin != "ffmpeg" {
		t.Errorf("filters %+v", f.Filters)
	}
	joined := strings.Join(f.Problems, "\n")
	if !strings.Contains(joined, "repeats type") || !strings.Contains(joined, "second resampler") || !strings.Contains(joined, "not closed") {
		t.Errorf("problems:\n%s", joined)
	}
	if strings.Contains(joined, "shout-secret-value") {
		t.Errorf("a problem quoted the output password:\n%s", joined)
	}
}

func TestPasswordIsRedacted(t *testing.T) {
	const secret = "super-secret-mpd-password"
	text := "password \"" + secret + "@read,control\"\nmusic_directory \"/m\"\npassword \"" + secret + "-2@admin\"\n"
	f, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if !f.PasswordSet || f.PasswordCount != 2 || len(f.passwordValues()) != 2 {
		t.Fatalf("password set %+v count %d", f.passwordValues(), f.PasswordCount)
	}
	assertNoSecret(t, secret, f)
	assertNoSecret(t, secret+"-2", f)

	broken, err := Parse("password \"" + secret + "\nmusic_directory \"/m\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if broken.PasswordSet || broken.MusicDirectory != "/m" {
		t.Fatalf("a broken password line consumed the file: %+v", broken.passwordValues())
	}
	assertNoSecret(t, secret, broken)
	if !strings.Contains(strings.Join(broken.Problems, "\n"), "password: missing closing quote") {
		t.Errorf("problems %q", broken.Problems)
	}

	trailed, err := Parse("password \"" + secret + "\" junk\n")
	if err != nil {
		t.Fatal(err)
	}
	if trailed.PasswordSet {
		t.Fatal("trailing tokens still stored the password")
	}
	assertNoSecret(t, secret, trailed)
}

func TestRepeatedParameterKeepsTheLast(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "mpd.conf"), "volume_normalization \"yes\"\ninclude \"drop.conf\"\n")
	mustWrite(t, filepath.Join(root, "drop.conf"), "volume_normalization \"no\"\n")
	f, err := ReadFile(root, "/mpd.conf", filepath.Join(root, "mpd.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if f.VolumeNormalization != "no" {
		t.Fatalf("normalization %q; MPD ReadConfigParam keeps the last value", f.VolumeNormalization)
	}
}

func TestPasswordSecretIsRedactedInAnIncludePath(t *testing.T) {
	root := t.TempDir()
	const secret = "secret123"
	mustWrite(t, filepath.Join(root, "mpd.conf"),
		"password \""+secret+"@read,control\"\ninclude \"/home/"+secret+"/missing.conf\"\n")
	f, err := ReadFile(root, "/mpd.conf", filepath.Join(root, "mpd.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.PasswordSet || f.PasswordCount != 1 {
		t.Fatalf("password count %d", f.PasswordCount)
	}
	assertNoSecret(t, secret, f)
	assertNoSecret(t, secret+"@read,control", f)
	if !strings.Contains(strings.Join(f.Problems, "\n"), "[redacted]") {
		t.Fatalf("the include path was not redacted: %q", f.Problems)
	}
}

func TestShortPasswordRedactsWholeTokensOnly(t *testing.T) {
	f, err := Parse("password \"a@read\"\nmusic_directory \"/admin/a/music\"\nbind_to_address \"/var/run/a\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if f.MusicDirectory != "/admin/a/music" {
		t.Fatalf("stored directory %q", f.MusicDirectory)
	}
	got := f.String()
	if !strings.Contains(got, "/admin/[redacted]/music") || !strings.Contains(got, "/var/run/[redacted]") {
		t.Fatalf("short secret was not redacted as a token:\n%s", got)
	}
	if strings.Contains(got, "/admin/a/") || strings.Contains(got, "run/a") {
		t.Fatalf("token survived:\n%s", got)
	}
	if !strings.Contains(got, "admin") {
		t.Fatalf("a letter inside a word was redacted:\n%s", got)
	}
	b := mustJSON(t, f)
	if bytes.Contains(b, []byte(`"/admin/a/music"`)) || bytes.Contains(b, []byte("a@read")) {
		t.Fatalf("json kept the secret: %s", b)
	}
}

func TestAnUnclosedBlockDoesNotAbsorbTheNext(t *testing.T) {
	const text = "" +
		"audio_output {\n" +
		"type \"alsa\"\n" +
		"name \"DAC\"\n" +
		"audio_output {\n" +
		"type \"fifo\"\n" +
		"name \"Snapcast\"\n" +
		"format \"44100:16:2\"\n" +
		"}\n"
	f, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Outputs) != 2 {
		t.Fatalf("outputs %+v problems %v", f.Outputs, f.Problems)
	}
	if f.Outputs[0].Name != "DAC" || f.Outputs[0].Type != "alsa" || f.Outputs[0].Format != "" {
		t.Fatalf("DAC absorbed the next block: %+v", f.Outputs[0])
	}
	if f.Outputs[1].Name != "Snapcast" || f.Outputs[1].Type != "fifo" || f.Outputs[1].Format != "44100:16:2" {
		t.Fatalf("fifo %+v", f.Outputs[1])
	}
	if !strings.Contains(strings.Join(f.Problems, "\n"), "not closed") {
		t.Fatalf("problems %v", f.Problems)
	}
}

func TestARepeatedKeyInsideABlockIsAProblem(t *testing.T) {
	f, err := Parse("resampler {\nplugin \"soxr\"\nthreads \"1\"\nthreads \"2\"\n}\n")
	if err != nil {
		t.Fatal(err)
	}
	if f.Resampler.Plugin != "soxr" || f.Resampler.Quality != "" {
		t.Fatalf("resampler %+v", f.Resampler)
	}
	if !strings.Contains(strings.Join(f.Problems, "\n"), "repeats threads") {
		t.Fatalf("problems %v", f.Problems)
	}
}

func TestReadHostResolvesBothSidesBeforeTheLogicalPath(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	mustWrite(t, filepath.Join(real, "etc", "mpd.conf"), "music_directory \"/m\"\n")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink: " + err.Error())
	}
	f, err := ReadHost(real, filepath.Join(link, "etc", "mpd.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Path != "/etc/mpd.conf" || f.MusicDirectory != "/m" {
		t.Fatalf("path %q music %q", f.Path, f.MusicDirectory)
	}
	other, err := ReadHost(link, filepath.Join(real, "etc", "mpd.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if other.Path != "/etc/mpd.conf" {
		t.Fatalf("path from the link root %q", other.Path)
	}
}

func TestPasswordWordMarshals(t *testing.T) {
	f, err := Parse("password \"password\"\nmusic_directory \"/secret-password/music\"\n")
	if err != nil {
		t.Fatal(err)
	}
	if f.MusicDirectory != "/secret-password/music" || !f.PasswordSet {
		t.Fatalf("parse lost the setting or the password: %#v", f.MusicDirectory)
	}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(`"password"`)) || strings.Contains(f.String(), "secret-password") {
		t.Fatalf("printed the password: %s\n%s", b, f.String())
	}
	if f.MusicDirectory != "/secret-password/music" {
		t.Fatal("printing redacted the stored directory")
	}
}

func TestIncludeAndIncludeOptional(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.conf"), "include \"b.conf\"\nbind_to_address \"any\"\n")
	mustWrite(t, filepath.Join(root, "b.conf"), "include \"sub/c.conf\"\ninclude_optional \"missing.conf\"\n")
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "sub", "c.conf"), "include \"d.conf\"\nmusic_directory \"/from-c\"\n")
	mustWrite(t, filepath.Join(root, "sub", "d.conf"), "replaygain \"track\"\nfilter {\nplugin \"ffmpeg\"\nname \"eq\"\n}\n")
	mustWrite(t, filepath.Join(root, "etc", "extra.conf"), "") // created below
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	const absSecret = "absolute-include-secret"
	mustWrite(t, filepath.Join(root, "etc", "extra.conf"), "password \""+absSecret+"@read\"\nvolume_normalization \"yes\"\n")
	mustWrite(t, filepath.Join(root, "top.conf"), "include \"/etc/extra.conf\"\ninclude \"no-such.conf\"\n")

	f, err := ReadFile(root, "/a.conf", filepath.Join(root, "a.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if f.MusicDirectory != "/from-c" || f.ReplayGain != "track" || strings.Join(f.BindAddresses, ",") != "any" {
		t.Errorf("merged file music %q gain %q bind %q", f.MusicDirectory, f.ReplayGain, f.BindAddresses)
	}
	if len(f.Filters) != 1 || f.Filters[0].Name != "eq" {
		t.Errorf("filters %+v", f.Filters)
	}
	if len(f.Problems) != 0 {
		t.Errorf("a present include and a missing include_optional raised %q", f.Problems)
	}

	top, err := ReadFile(root, "/top.conf", filepath.Join(root, "top.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !top.PasswordSet || top.VolumeNormalization != "yes" {
		t.Errorf("absolute include was not read: password %v norm %q", top.PasswordSet, top.VolumeNormalization)
	}
	assertNoSecret(t, absSecret, top)
	if !strings.Contains(strings.Join(top.Problems, "\n"), "no-such.conf") || !strings.Contains(strings.Join(top.Problems, "\n"), "file not found") {
		t.Errorf("missing include problems %q", top.Problems)
	}
}

func TestIncludeCycleAndDepth(t *testing.T) {
	root := t.TempDir()
	const secret = "cycle-secret-value"
	mustWrite(t, filepath.Join(root, "a.conf"), "password \""+secret+"@read\"\ninclude \"b.conf\"\n")
	mustWrite(t, filepath.Join(root, "b.conf"), "include \"a.conf\"\nmusic_directory \"/cycled\"\n")
	f, err := ReadFile(root, "/a.conf", filepath.Join(root, "a.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if f.MusicDirectory != "/cycled" || !f.PasswordSet {
		t.Fatalf("cycle lost the settings around it: music %q password %v", f.MusicDirectory, f.PasswordSet)
	}
	if !strings.Contains(strings.Join(f.Problems, "\n"), "cycle") {
		t.Fatalf("problems %q", f.Problems)
	}
	assertNoSecret(t, secret, f)

	// A chain longer than maxIncludeDepth stops, and the file that named
	// it is still returned.
	prev := "tail.conf"
	mustWrite(t, filepath.Join(root, prev), "samplerate_converter \"internal\"\n")
	for i := maxIncludeDepth + 2; i >= 0; i-- {
		name := fmt.Sprintf("d%d.conf", i)
		mustWrite(t, filepath.Join(root, name), "include \""+prev+"\"\n")
		prev = name
	}
	deep, err := ReadFile(root, "/"+prev, filepath.Join(root, prev))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(deep.Problems, "\n"), "nested more than") {
		t.Fatalf("problems %q", deep.Problems)
	}
}

func TestIncludeStaysInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	const secret = "outside-root-secret"
	// A lexical ".." include never has to exist: joining it onto the root
	// leaves the root, and the file is not opened.
	mustWrite(t, filepath.Join(root, "a.conf"), "include \"../../etc/passwd\"\nmusic_directory \"/inside\"\n")
	f, err := ReadFile(root, "/a.conf", filepath.Join(root, "a.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if f.MusicDirectory != "/inside" || !strings.Contains(strings.Join(f.Problems, "\n"), "path leaves the root") {
		t.Fatalf("music %q problems %q", f.MusicDirectory, f.Problems)
	}

	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.conf"), "music_directory \"/"+secret+"\"\npassword \""+secret+"\"\n")
	link := filepath.Join(root, "escape.conf")
	if err := os.Symlink(filepath.Join(outside, "secret.conf"), link); err != nil {
		t.Skip("symlinks: " + err.Error())
	}
	mustWrite(t, filepath.Join(root, "b.conf"), "include \"escape.conf\"\nmusic_directory \"/inside\"\n")
	f, err = ReadFile(root, "/b.conf", filepath.Join(root, "b.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if f.MusicDirectory != "/inside" {
		t.Errorf("music directory %q; an include replaced it from outside the root", f.MusicDirectory)
	}
	if f.PasswordSet {
		t.Error("a password from outside the root was read")
	}
	assertNoSecret(t, secret, f)
	if !strings.Contains(strings.Join(f.Problems, "\n"), "path leaves the root") {
		t.Errorf("problems %q", f.Problems)
	}
}

func TestSearchOrderMatchesMPD(t *testing.T) {
	root := t.TempDir()
	home := "/home/pi"
	for _, dir := range []string{
		home, home + "/.config", home + "/.config/mpd", home + "/.mpd",
		"/xdg", "/xdg/mpd", "/etc",
	} {
		if err := os.MkdirAll(hostPath(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, hostPath(root, "/etc/mpd.conf"), "music_directory \"/etc-one\"\n")
	mustWrite(t, hostPath(root, home+"/.config/mpd/mpd.conf"), "music_directory \"/xdg-default\"\n")
	mustWrite(t, hostPath(root, home+"/.mpdconf"), "music_directory \"/dot-mpdconf\"\n")
	mustWrite(t, hostPath(root, home+"/.mpd/mpd.conf"), "music_directory \"/dot-mpd\"\n")
	mustWrite(t, hostPath(root, "/xdg/mpd/mpd.conf"), "music_directory \"/xdg-set\"\n")

	// No home and no XDG: the system file, which MPD tries last and which
	// is the only one left.
	if got := findMusic(t, Locate{Root: root}); got != "/etc-one" {
		t.Errorf("no home: music %q", got)
	}
	// XDG unset: ~/.config is the user config dir, and it wins over
	// ~/.mpdconf and /etc.
	if got := findMusic(t, Locate{Root: root, Home: home}); got != "/xdg-default" {
		t.Errorf("home only: music %q, want the ~/.config file", got)
	}
	// XDG set and present: that file wins, and ~/.config is not also tried.
	if got := findMusic(t, Locate{Root: root, Home: home, XDGConfigHome: "/xdg"}); got != "/xdg-set" {
		t.Errorf("xdg set: music %q", got)
	}
	// The XDG directory exists but its file does not: MPD does not fall
	// back to ~/.config. Next are ~/.mpdconf, ~/.mpd/mpd.conf, /etc.
	if err := os.Remove(hostPath(root, "/xdg/mpd/mpd.conf")); err != nil {
		t.Fatal(err)
	}
	if got := findMusic(t, Locate{Root: root, Home: home, XDGConfigHome: "/xdg"}); got != "/dot-mpdconf" {
		t.Errorf("xdg dir without a file: music %q, want ~/.mpdconf and not ~/.config", got)
	}
	if err := os.Remove(hostPath(root, home+"/.mpdconf")); err != nil {
		t.Fatal(err)
	}
	if got := findMusic(t, Locate{Root: root, Home: home, XDGConfigHome: "/xdg"}); got != "/dot-mpd" {
		t.Errorf("after .mpdconf: music %q", got)
	}

	// An XDG directory that does not exist falls through to ~/.config.
	if got := findMusic(t, Locate{Root: root, Home: home, XDGConfigHome: "/no-such-xdg"}); got != "/xdg-default" {
		t.Errorf("xdg missing: music %q, want ~/.config", got)
	}

	cands := Locate{Root: root, Home: home}.Candidates()
	want := []string{home + "/.config/mpd/mpd.conf", home + "/.mpdconf", home + "/.mpd/mpd.conf", SystemPath}
	if strings.Join(cands, "|") != strings.Join(want, "|") {
		t.Errorf("candidates %q, want %q", cands, want)
	}
	withXDG := Locate{Root: root, Home: home, XDGConfigHome: "/xdg"}.Candidates()
	wantXDG := []string{"/xdg/mpd/mpd.conf", home + "/.mpdconf", home + "/.mpd/mpd.conf", SystemPath}
	if strings.Join(withXDG, "|") != strings.Join(wantXDG, "|") {
		t.Errorf("xdg candidates %q, want %q", withXDG, wantXDG)
	}
}

func TestEndpointPrefersASocket(t *testing.T) {
	f, err := Parse("bind_to_address \"any\"\nbind_to_address \"~/mpd/socket\"\nport \"6602\"\n")
	if err != nil {
		t.Fatal(err)
	}
	addr, network := f.Endpoint("/home/pi")
	if addr != "/home/pi/mpd/socket" || network != "unix" {
		t.Errorf("endpoint %s %s", addr, network)
	}
	tcp, err := Parse("bind_to_address \"localhost\"\nport \"6602\"\n")
	if err != nil {
		t.Fatal(err)
	}
	addr, network = tcp.Endpoint("")
	if addr != "127.0.0.1:6602" || network != "tcp" {
		t.Errorf("tcp endpoint %s %s", addr, network)
	}
	abs, err := Parse("bind_to_address \"@mpd\"\n")
	if err != nil {
		t.Fatal(err)
	}
	addr, network = abs.Endpoint("")
	if addr != "@mpd" || network != "unix" {
		t.Errorf("abstract endpoint %s %s", addr, network)
	}
}

func TestParseRejectsAHugeFile(t *testing.T) {
	_, err := Parse(strings.Repeat("a", MaxConfBytes+1))
	if err == nil {
		t.Fatal("a file over the cap was parsed")
	}
}

func findMusic(t *testing.T, l Locate) string {
	t.Helper()
	logical, host, ok := l.Find()
	if !ok {
		t.Fatal("no conf found")
	}
	f, err := ReadFile(l.root(), logical, host)
	if err != nil {
		t.Fatal(err)
	}
	return f.MusicDirectory
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, f File) []byte {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func bytesContains(b []byte, s string) bool {
	return strings.Contains(string(b), s)
}

func assertNoSecret(t *testing.T, secret string, f File) {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil && !strings.Contains(err.Error(), "refusing to serialise") {
		t.Fatal(err)
	}
	for _, got := range []string{
		f.String(),
		fmt.Sprintf("%v", f),
		fmt.Sprintf("%+v", f),
		fmt.Sprintf("%#v", f),
		string(b),
		strings.Join(f.Problems, "\n"),
		errString(err),
	} {
		if strings.Contains(got, secret) {
			t.Errorf("secret %q appeared in %q", secret, got)
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errOutside) {
		return err.Error()
	}
	return err.Error()
}
