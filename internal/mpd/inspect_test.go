package mpd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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
	writeFile(t, filepath.Join(root, "var", "www", "footer.php"), "Release: 9.0.0 2024-01-02\n")
	sw, _ := detectSoftware(root)
	if sw.Version != "9.0.0" || sw.BuildDate != "2024-01-02" {
		t.Fatalf("footer.php %#v", sw)
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
