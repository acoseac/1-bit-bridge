package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOutputPlayerOnTheVolumioFixture(t *testing.T) {
	fx := playerReplies(t)
	ln := playerListen(t, fx)
	root := playerVolumioRoot(t, false)
	stdout, stderr, code := runCapture(t, "output", "player", "--root", root, "--mpd", ln.Addr().String())
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d\nstderr %q\nstdout %s", code, stderr, stdout)
	}
	want := "player: Volumio 4.119\n" +
		"  build: 18952480e8d8c63f22208e9007a0f47a9563eae6\n" +
		"  built: Tue Mar 24 17:20:52 UTC 2026\n" +
		"mpd: 0.24.0 at " + ln.Addr().String() + "\n" +
		"  config: not permitted\n" +
		"conf: /etc/mpd.conf\n" +
		"music directory: /var/lib/mpd/music\n" +
		"password: not set\n" +
		"outputs:\n" +
		"  alsa: type alsa, device volumio, dop no, mixer unset, enabled yes\n" +
		"  multiroom: type fifo, device unset, path /tmp/snapfifo, dop unset, mixer unset, enabled no\n" +
		"replay gain: off\n" +
		"volume normalization: no\n" +
		"resampler: soxr (quality high)\n" +
		"samplerate converter: unset\n" +
		"audio output format: unset\n" +
		"filters: none\n" +
		"zeroconf: unset\n"
	if stdout != want {
		t.Fatalf("text:\n%s\nwant:\n%s", stdout, want)
	}

	stdout, stderr, code = runCapture(t, "output", "player", "--json", "--root", root, "--mpd", ln.Addr().String())
	if code != 0 || stderr != "" {
		t.Fatalf("json exit %d stderr %q", code, stderr)
	}
	assertPlayerJSON(t, stdout, ln.Addr().String())
}

func TestOutputPlayerNeverPrintsAPassword(t *testing.T) {
	const secret = "player-report-secret-value"
	fx := playerReplies(t)
	ln := playerListen(t, fx)
	root := playerVolumioRoot(t, false)
	conf := filepath.Join(root, "etc", "mpd.conf")
	b, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(conf, append(b, []byte("password \""+secret+"\"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"output", "player", "--root", root, "--mpd", ln.Addr().String()},
		{"output", "player", "--json", "--root", root, "--mpd", ln.Addr().String()},
	} {
		stdout, stderr, code := runCapture(t, args...)
		if code != 0 {
			t.Fatalf("%v exit %d stderr %q", args, code, strings.ReplaceAll(stderr, secret, "[redacted]"))
		}
		if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
			t.Fatalf("%v printed the password", args[:3])
		}
	}
	stdout, _, _ := runCapture(t, "output", "player", "--json", "--root", root, "--mpd", ln.Addr().String())
	var rep struct {
		PasswordSet   bool `json:"passwordSet"`
		PasswordCount int  `json:"passwordCount"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.PasswordSet || rep.PasswordCount != 1 {
		t.Fatalf("password presence %+v", rep)
	}
}

func TestOutputPlayerUsageAndOffLinux(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runOutputPlayer(context.Background(), nil, "darwin", &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "darwin") {
		t.Fatalf("off linux: exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	if runtime.GOOS != "linux" {
		out, errOut, code := runCapture(t, "output", "player")
		if code != 1 || out != "" || !strings.Contains(errOut, runtime.GOOS) {
			t.Fatalf("host refuse: exit %d stdout %q stderr %q", code, out, errOut)
		}
	}
	for _, args := range [][]string{
		{"output", "player", "nope"},
		{"output", "player", "--bogus"},
	} {
		out, errOut, code := runCapture(t, args...)
		if code != 2 || out != "" {
			t.Fatalf("%v: exit %d stdout %q stderr %q", args, code, out, errOut)
		}
	}
	out, _, code := runCapture(t, "output", "help")
	if code != 0 || !strings.Contains(out, "  player ") {
		t.Fatalf("help: exit %d %q", code, out)
	}
	empty := t.TempDir()
	out, _, code = runCapture(t, "output", "player", "--root", empty)
	if code != 1 || !strings.Contains(out, "player: unknown\n") {
		t.Fatalf("empty tree: exit %d %q", code, out)
	}
}

func assertPlayerJSON(t *testing.T, stdout, endpoint string) {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"software", "mpd", "confPath", "musicDirectory", "passwordSet", "passwordCount",
		"configPermitted", "outputs", "replayGain", "volumeNormalization", "resampler",
		"samplerateConverter", "audioOutputFormat", "filters", "zeroconfEnabled", "problems",
	}
	if len(doc) != len(want) {
		t.Fatalf("json keys %v", keysOf(doc))
	}
	for _, k := range want {
		if _, ok := doc[k]; !ok {
			t.Fatalf("missing %s", k)
		}
	}
	var rep struct {
		Software struct {
			Name, Version, Build, BuildDate string
		} `json:"software"`
		MPD struct {
			Version, Endpoint string
		} `json:"mpd"`
		ConfPath            string
		MusicDirectory      string
		PasswordSet         bool
		PasswordCount       int
		ConfigPermitted     *bool
		ReplayGain          string
		VolumeNormalization string
		Resampler           struct{ Plugin, Quality string }
		Filters             []struct{ Name, Plugin string }
		Outputs             []struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Device  string `json:"device"`
			Path    string `json:"path"`
			DOP     string `json:"dop"`
			Enabled string `json:"enabled"`
			Format  string `json:"format"`
		}
		Problems []string
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Software.Name != "Volumio" || rep.Software.Version != "4.119" || rep.MPD.Version != "0.24.0" || rep.MPD.Endpoint != endpoint {
		t.Fatalf("identity %+v %+v", rep.Software, rep.MPD)
	}
	if rep.ConfigPermitted == nil || *rep.ConfigPermitted || rep.PasswordSet || rep.MusicDirectory != "/var/lib/mpd/music" {
		t.Fatalf("flags permitted %v password %v music %q", rep.ConfigPermitted, rep.PasswordSet, rep.MusicDirectory)
	}
	if len(rep.Outputs) != 2 || rep.Outputs[0].Device != "volumio" || rep.Outputs[0].DOP != "no" || rep.Outputs[0].Enabled != "yes" {
		t.Fatalf("outputs %+v", rep.Outputs)
	}
	if rep.Outputs[1].Path != "/tmp/snapfifo" || rep.Outputs[1].Format != "44100:16:2" || rep.Outputs[1].Enabled != "no" {
		t.Fatalf("fifo %+v", rep.Outputs[1])
	}
	if rep.ReplayGain != "off" || rep.VolumeNormalization != "no" || rep.Resampler.Plugin != "soxr" || len(rep.Filters) != 0 || len(rep.Problems) != 0 {
		t.Fatalf("settings %+v", rep)
	}
	var software, daemon, output, resampler map[string]json.RawMessage
	mustJSON(t, doc["software"], &software)
	mustJSON(t, doc["mpd"], &daemon)
	for _, k := range []string{"name", "version", "build", "buildDate"} {
		if _, ok := software[k]; !ok {
			t.Fatalf("software missing %s", k)
		}
	}
	for _, k := range []string{"version", "endpoint"} {
		if _, ok := daemon[k]; !ok {
			t.Fatalf("mpd missing %s", k)
		}
	}
	var outputs []json.RawMessage
	mustJSON(t, doc["outputs"], &outputs)
	mustJSON(t, outputs[0], &output)
	for _, k := range []string{"name", "type", "device", "path", "dop", "mixerType", "mixerControl", "mixerDevice", "enabled", "format", "allowedFormats"} {
		if _, ok := output[k]; !ok {
			t.Fatalf("output missing %s", k)
		}
	}
	mustJSON(t, doc["resampler"], &resampler)
	for _, k := range []string{"plugin", "quality"} {
		if _, ok := resampler[k]; !ok {
			t.Fatalf("resampler missing %s", k)
		}
	}
	var filters []json.RawMessage
	mustJSON(t, doc["filters"], &filters)
	if filters == nil {
		t.Fatal("filters is null")
	}
	var problems []json.RawMessage
	mustJSON(t, doc["problems"], &problems)
	if problems == nil {
		t.Fatal("problems is null")
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustJSON(t *testing.T, raw json.RawMessage, dest any) {
	t.Helper()
	if err := json.Unmarshal(raw, dest); err != nil {
		t.Fatal(err)
	}
}

func playerVolumioRoot(t *testing.T, _ bool) string {
	t.Helper()
	root := t.TempDir()
	base := filepath.Join(repoRootForCitations(t), "internal", "mpd", "testdata")
	copyBytes(t, filepath.Join(base, "volumio-os-release"), filepath.Join(root, "etc", "os-release"))
	copyBytes(t, filepath.Join(base, "volumio-mpd.conf"), filepath.Join(root, "etc", "mpd.conf"))
	if err := os.MkdirAll(filepath.Join(root, "volumio"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "data", "configuration"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func copyBytes(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

type playerFixture struct {
	Greeting         string `json:"greeting"`
	Status           string `json:"status"`
	Stats            string `json:"stats"`
	Outputs          string `json:"outputs"`
	ReplayGainStatus string `json:"replay_gain_status"`
	Config           string `json:"config"`
	CurrentSong      string `json:"currentsong"`
	Commands         string `json:"commands"`
}

func (f playerFixture) body(cmd string) (string, bool) {
	switch cmd {
	case "status":
		return f.Status, true
	case "stats":
		return f.Stats, true
	case "outputs":
		return f.Outputs, true
	case "replay_gain_status":
		return f.ReplayGainStatus, true
	case "config":
		return f.Config, true
	case "currentsong":
		return f.CurrentSong, true
	case "commands":
		return f.Commands, true
	default:
		return "", false
	}
}

func playerReplies(t *testing.T) playerFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRootForCitations(t), "internal", "mpd", "testdata", "volumio-mpd-replies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx playerFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	return fx
}

func playerListen(t *testing.T, fx playerFixture) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go servePlayer(conn, fx)
		}
	}()
	return ln
}

func servePlayer(conn net.Conn, fx playerFixture) {
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(fx.Greeting)); err != nil {
		return
	}
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		name := strings.TrimRight(line, "\r\n")
		body, ok := fx.body(name)
		if !ok {
			_, _ = conn.Write([]byte("ACK [5@0] {} unknown command\n"))
			continue
		}
		if _, err := conn.Write([]byte(body)); err != nil {
			return
		}
	}
}
