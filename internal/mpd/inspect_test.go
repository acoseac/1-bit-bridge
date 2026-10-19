package mpd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInspectVolumioFixture(t *testing.T) {
	fx := loadReplies(t)
	srv := startFake(t, fakeOpts{greeting: fx.Greeting, replies: fx.replies()})
	root := volumioTree(t)
	rep := Inspect(context.Background(), Options{Root: root, Endpoint: srv.addr})
	if rep.Software.Name != "Volumio" || rep.Software.Version != "4.119" ||
		rep.Software.Build != "18952480e8d8c63f22208e9007a0f47a9563eae6" ||
		rep.Software.BuildDate != "Tue Mar 24 17:20:52 UTC 2026" {
		t.Fatalf("software %#v", rep.Software)
	}
	if rep.MPD.Version != "0.24.0" || rep.MPD.Endpoint != srv.addr {
		t.Fatalf("mpd %#v", rep.MPD)
	}
	if rep.ConfPath != "/etc/mpd.conf" || rep.MusicDirectory != "/var/lib/mpd/music" {
		t.Fatalf("conf %q music %q", rep.ConfPath, rep.MusicDirectory)
	}
	if rep.PasswordSet || rep.PasswordCount != 0 {
		t.Fatalf("password set %v count %d", rep.PasswordSet, rep.PasswordCount)
	}
	if rep.ConfigPermitted == nil || *rep.ConfigPermitted {
		t.Fatalf("config permitted %#v", rep.ConfigPermitted)
	}
	if len(rep.Outputs) != 2 {
		t.Fatalf("outputs %#v", rep.Outputs)
	}
	alsa := rep.Outputs[0]
	if alsa.Name != "alsa" || alsa.Type != "alsa" || alsa.Device != "volumio" ||
		alsa.DOP != "no" || alsa.Enabled != "yes" || alsa.MixerType != "" {
		t.Fatalf("alsa %#v", alsa)
	}
	fifo := rep.Outputs[1]
	if fifo.Name != "multiroom" || fifo.Type != "fifo" || fifo.Path != "/tmp/snapfifo" ||
		fifo.Enabled != "no" || fifo.DOP != "" {
		t.Fatalf("fifo %#v", fifo)
	}
	if rep.ReplayGain != "off" || rep.VolumeNormalization != "no" ||
		rep.Resampler.Plugin != "soxr" || rep.Resampler.Quality != "high" ||
		len(rep.Filters) != 0 || len(rep.Problems) != 0 {
		t.Fatalf("settings gain %q norm %q resampler %#v filters %#v problems %v",
			rep.ReplayGain, rep.VolumeNormalization, rep.Resampler, rep.Filters, rep.Problems)
	}
	if !rep.Found() {
		t.Fatal("the fixture was not found")
	}
}

func TestDetectMoodeAndVolumioWins(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "usr", "local", "bin", "moodeutl"), "")
	writeFile(t, filepath.Join(root, "var", "www", "footer.min.php"),
		"<li>Release: 10.2.2 2026-05-31</li> <!-- NOTE: getMoodeRel() parses this -->\n")
	sw, probs := detectSoftware(root)
	if sw.Name != "moOde" || sw.Version != "10.2.2" || sw.BuildDate != "2026-05-31" || len(probs) != 0 {
		t.Fatalf("moode %#v %v", sw, probs)
	}
	writeFile(t, filepath.Join(root, "volumio", ".keep"), "")
	writeFile(t, filepath.Join(root, "data", "configuration", ".keep"), "")
	writeFile(t, filepath.Join(root, "etc", "os-release"), "VOLUMIO_VERSION=4.119\n")
	sw, _ = detectSoftware(root)
	if sw.Name != "Volumio" || sw.Version != "4.119" {
		t.Fatalf("volumio should win, got %#v", sw)
	}
}

func TestMoodeFooterFallback(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "usr", "local", "bin", "moodeutl"), "")
	writeFile(t, filepath.Join(root, "var", "www", "footer.php"),
		"<!-- Release: 1.0.0 1999-01-01 -->\n"+
			"<script>var x = \"Release: 2.0.0 2000-01-01\";</script>\n"+
			"<li>Release: 9.0.0 2024-01-02</li> <!-- NOTE: getMoodeRel() parses this -->\n")
	sw, _ := detectSoftware(root)
	if sw.Version != "9.0.0" || sw.BuildDate != "2024-01-02" {
		t.Fatalf("footer.php %#v", sw)
	}
	version, date := moodeRelease("<li>Release: 8.3.1 2024-08-21)</li>\n")
	if version != "8.3.1" || date != "2024-08-21" {
		t.Fatalf("delimiter stayed in the value: %q %q", version, date)
	}
	version, date = moodeRelease("<li>Release: 8.3.1;</li>")
	if version != "8.3.1" || date != "" {
		t.Fatalf("semicolon stayed in the value: %q %q", version, date)
	}
}

func TestOSReleaseFollowsTheShellQuoting(t *testing.T) {
	text := "" +
		"# VOLUMIO_VERSION=hidden\n" +
		"\n" +
		"VOLUMIO_VERSION=\"4.119\"\n" +
		"PRETTY_NAME=\"say \\\"hi\\\" \\$x `y`\"\n" +
		"ID=raspbian # not part of the value\n" +
		"NAME=\"foo # bar\"\n" +
		"VOLUMIO_BUILD_DATE='Tue Mar 24'\n"
	kv := parseOSRelease(text)
	if kv["VOLUMIO_VERSION"] != "4.119" {
		t.Fatalf("version %q", kv["VOLUMIO_VERSION"])
	}
	if kv["PRETTY_NAME"] != "say \"hi\" $x `y`" {
		t.Fatalf("pretty %q", kv["PRETTY_NAME"])
	}
	tick := parseOSRelease("NOTE=\"tick \\` end\"\n")
	if tick["NOTE"] != "tick ` end" {
		t.Fatalf("backtick %q", tick["NOTE"])
	}
	if kv["ID"] != "raspbian" {
		t.Fatalf("unquoted comment stayed: %q", kv["ID"])
	}
	if kv["NAME"] != "foo # bar" {
		t.Fatalf("quoted comment was stripped: %q", kv["NAME"])
	}
	if kv["VOLUMIO_BUILD_DATE"] != "Tue Mar 24" {
		t.Fatalf("single quotes %q", kv["VOLUMIO_BUILD_DATE"])
	}
	if _, ok := kv["# VOLUMIO_VERSION"]; ok {
		t.Fatal("a comment line was a key")
	}
}

func TestUnsetRuntimeDirIsNotDialled(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "etc", "mpd.conf"), "bind_to_address \"$XDG_RUNTIME_DIR/socket\"\nmusic_directory \"/music\"\n")
	rep := Inspect(context.Background(), Options{Root: root, Home: root})
	if rep.MPD.Version != "" || rep.MusicDirectory != "/music" {
		t.Fatalf("mpd %#v music %q", rep.MPD, rep.MusicDirectory)
	}
	joined := strings.Join(rep.Problems, "\n")
	if strings.Contains(joined, "dial") || strings.Contains(joined, "$XDG_RUNTIME_DIR") {
		t.Fatalf("unexpanded endpoint was dialled or echoed: %q", joined)
	}
	if joined == "" {
		t.Fatal("no problem for an unexpanded endpoint")
	}
}

// TestUndiallableEndpointIsAbsentFromTheReport pins that a path
// resolveDial refuses is not stored on the report. The problem says
// why; the path, and a short secret inside it, are nowhere in the JSON.
func TestUndiallableEndpointIsAbsentFromTheReport(t *testing.T) {
	home := t.TempDir()
	cases := []struct {
		name     string
		conf     string
		endpoint string
		home     string
		leaks    []string
	}{
		{
			name:  "unset runtime dir",
			conf:  "password \"abc@read\"\nbind_to_address \"$XDG_RUNTIME_DIR/socket_abc\"\nmusic_directory \"/music\"\n",
			home:  home,
			leaks: []string{"$XDG_RUNTIME_DIR/socket_abc", "XDG_RUNTIME_DIR", "socket_abc", "abc@read", "abc"},
		},
		{
			name:  "tilde without home",
			conf:  "bind_to_address \"~/mpd/socket\"\nmusic_directory \"/music\"\n",
			leaks: []string{"~/mpd/socket"},
		},
		{
			name:     "flag",
			endpoint: "$HOME/socket_abc",
			leaks:    []string{"$HOME/socket_abc", "socket_abc"},
		},
		{
			name:     "relative",
			endpoint: "relative/socket",
			leaks:    []string{"relative/socket"},
		},
		{
			name:     "tilde user",
			endpoint: "~nobody/socket",
			leaks:    []string{"~nobody/socket"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opt := Options{Endpoint: tc.endpoint, Home: tc.home}
			if tc.conf != "" {
				dir := t.TempDir()
				writeFile(t, filepath.Join(dir, "etc", "mpd.conf"), tc.conf)
				opt.Root = dir
			}
			rep := Inspect(context.Background(), opt)
			if rep.MPD.Endpoint != "" || rep.MPD.Version != "" {
				t.Fatalf("dialled %#v", rep.MPD)
			}
			if len(rep.Problems) == 0 {
				t.Fatal("no problem for an undiallable endpoint")
			}
			body, err := json.Marshal(rep)
			if err != nil {
				t.Fatal(err)
			}
			text := string(body) + "\n" + strings.Join(rep.Problems, "\n")
			for _, leak := range tc.leaks {
				if strings.Contains(text, leak) {
					t.Fatalf("report contains %q:\n%s", leak, text)
				}
			}
		})
	}
}

func TestTildeWithoutHomeIsNotDialled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "etc", "mpd.conf"), "bind_to_address \"~/mpd/socket\"\nmusic_directory \"/music\"\n")
	rep := Inspect(context.Background(), Options{Root: root})
	if rep.MPD.Version != "" {
		t.Fatalf("dialled %#v", rep.MPD)
	}
	joined := strings.Join(rep.Problems, "\n")
	if strings.Contains(joined, "dial") || strings.Contains(joined, "~/") {
		t.Fatalf("relative tilde path was dialled or echoed: %q", joined)
	}
}

func TestTildeFlagExpandsBeforeDial(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	dir, err := os.MkdirTemp("/tmp", "mpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "socket")
	fx := loadReplies(t)
	startFake(t, fakeOpts{network: "unix", addr: sock, greeting: fx.Greeting, replies: fx.replies()})
	rep := Inspect(context.Background(), Options{Endpoint: "~/socket", Home: dir})
	if rep.MPD.Version != "0.24.0" || rep.MPD.Endpoint != sock {
		t.Fatalf("mpd %#v problems %v", rep.MPD, rep.Problems)
	}
}

func TestPlainMPDWhenNothingElseIsInstalled(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "etc", "mpd.conf"), "music_directory \"/music\"\n")
	rep := Inspect(context.Background(), Options{Root: root})
	if rep.Software.Name != "MPD" || rep.MusicDirectory != "/music" || rep.MPD.Version != "" {
		t.Fatalf("%#v", rep)
	}
	if !rep.Found() {
		t.Fatal("a configuration is a player")
	}
}

func TestConfigOverASocketFillsMusicDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	dir, err := os.MkdirTemp("/tmp", "mpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	fx := loadReplies(t)
	replies := fx.replies()
	replies["config"] = "music_directory: /from-socket\nOK\n"
	srv := startFake(t, fakeOpts{network: "unix", addr: sock, greeting: fx.Greeting, replies: replies})
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "etc", "mpd.conf"), "bind_to_address \""+sock+"\"\n")
	rep := Inspect(context.Background(), Options{Root: root, Endpoint: srv.addr})
	if rep.ConfigPermitted == nil || !*rep.ConfigPermitted || rep.MusicDirectory != "/from-socket" {
		t.Fatalf("permitted %#v music %q problems %v", rep.ConfigPermitted, rep.MusicDirectory, rep.Problems)
	}
}

func TestRuntimeDirSocketIsDialled(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets")
	}
	dir, err := os.MkdirTemp("/tmp", "mpd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "socket")
	fx := loadReplies(t)
	startFake(t, fakeOpts{network: "unix", addr: sock, greeting: fx.Greeting, replies: fx.replies()})
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "etc", "mpd.conf"), "bind_to_address \"$XDG_RUNTIME_DIR/socket\"\nmusic_directory \"/music\"\n")
	rep := Inspect(context.Background(), Options{Root: root, XDGRuntimeDir: dir})
	if rep.MPD.Version != "0.24.0" || rep.MPD.Endpoint != sock || rep.MusicDirectory != "/music" {
		t.Fatalf("mpd %#v music %q problems %v", rep.MPD, rep.MusicDirectory, rep.Problems)
	}
}

func TestNothingFound(t *testing.T) {
	rep := Inspect(context.Background(), Options{Root: t.TempDir()})
	if rep.Found() || rep.Software.Name != "" || rep.Outputs == nil || rep.Problems == nil {
		t.Fatalf("%#v", rep)
	}
}

func volumioTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	copyFile(t, "testdata/volumio-os-release", filepath.Join(root, "etc", "os-release"))
	copyFile(t, "testdata/volumio-mpd.conf", filepath.Join(root, "etc", "mpd.conf"))
	writeFile(t, filepath.Join(root, "volumio", ".keep"), "")
	writeFile(t, filepath.Join(root, "data", "configuration", ".keep"), "")
	return root
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, string(b))
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
