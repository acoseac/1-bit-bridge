package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/flactest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// loopbackServe is a `bridge serve` whose loopback route a test reads: the
// base URL serve reported once its boot decision was made, and the
// channels that say serve exited.
type loopbackServe struct {
	base     string
	exited   <-chan struct{}
	exitCode <-chan int
	stderr   *safeBuffer
	stop     context.CancelFunc
}

// bootLoopbackServe writes a library with one indexed FLAC under music,
// starts serve with the given output section, and waits for serve's boot
// decision about the loopback route: an explicit signal, not a poll.
func bootLoopbackServe(t *testing.T, music, dataDir string, output string) loopbackServe {
	t.Helper()
	lan := holdLoopback(t)
	admin := holdLoopback(t)
	configPath := filepath.Join(filepath.Dir(dataDir), "bridge.yaml")
	yamlText := "libraryRoots:\n  - " + music + "\ndataDir: " + dataDir +
		"\nadminAddress: " + admin.addr + "\noutput:\n" + output
	if err := os.WriteFile(configPath, []byte(yamlText), 0o600); err != nil {
		t.Fatal(err)
	}
	boot := make(chan string, 1)
	opts := serveOpts{
		configPath:    configPath,
		addrOverride:  lan.addr,
		lanListener:   lan.ln,
		adminListener: admin.ln,
		// The route runs where /v1/outputs answers, on Linux. This host
		// stands in for one; nothing here reads the host's /proc or MPD.
		localOutputsGOOS:   "linux",
		outputLoopbackBoot: func(base string) { boot <- base },
	}
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	exitCode := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		exitCode <- runServe(ctx, opts, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, exitCode, stderr)
	select {
	case base := <-boot:
		return loopbackServe{base: base, exited: exited, exitCode: exitCode, stderr: stderr, stop: cancel}
	case <-exited:
		t.Fatalf("serve exited with code %d before its loopback decision; stderr=%s", <-exitCode, stderr.String())
		return loopbackServe{}
	}
}

// seedTrack writes body at music/rel and indexes it, as a scan would, so
// the route has a row to serve without waiting for one.
func seedTrack(t *testing.T, dataDir, rel string, size int) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertTrack(context.Background(), &manifest.Track{Path: rel, Size: int64(size)}); err != nil {
		t.Fatal(err)
	}
}

func loopbackFetch(t *testing.T, base, rel string) (int, []byte, string) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get(base + dlna.FilePathPrefix + dlna.TrackID("", rel))
	if err != nil {
		t.Fatalf("get %s: %v", rel, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var refusal struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &refusal)
	return resp.StatusCode, body, refusal.Error
}

// TestServeServesItsTracksOnLoopbackWhileOutputIsOn: with output.mpd on,
// serve starts the loopback route through the real index adapter, serves
// a library track byte for byte, refuses a link out of the root and a row
// whose path leaves it, and closes the port when serve stops.
func TestServeServesItsTracksOnLoopbackWhileOutputIsOn(t *testing.T) {
	root := t.TempDir()
	music := filepath.Join(root, "Music")
	outside := filepath.Join(root, "outside")
	dataDir := filepath.Join(root, "data")
	for _, d := range []string{music, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	track := flactest.Stream(44100, 2, 16, 4096, 7)
	secret := flactest.Stream(44100, 2, 16, 4096, 99)
	if err := os.WriteFile(filepath.Join(music, "a.flac"), track, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.flac"), secret, 0o644); err != nil {
		t.Fatal(err)
	}
	seedTrack(t, dataDir, "a.flac", len(track))
	// A row whose path leaves the root: the index resolver refuses it, so
	// the route has nothing to serve for it.
	seedTrack(t, dataDir, "../outside/secret.flac", len(secret))
	links := runtime.GOOS != "windows"
	if links {
		if err := os.Symlink(filepath.Join(outside, "secret.flac"), filepath.Join(music, "out.flac")); err != nil {
			t.Fatal(err)
		}
		seedTrack(t, dataDir, "out.flac", len(secret))
	}

	s := bootLoopbackServe(t, music, dataDir, "  mpd: on\n  loopbackPort: 0\n")
	if !strings.HasPrefix(s.base, "http://127.0.0.1:") {
		t.Fatalf("loopback route base %q, want http://127.0.0.1:<port>; stderr=%s", s.base, s.stderr.String())
	}

	status, body, _ := loopbackFetch(t, s.base, "a.flac")
	if status != http.StatusOK || string(body) != string(track) {
		t.Fatalf("a.flac: status %d, %d bytes of %d", status, len(body), len(track))
	}
	status, body, code := loopbackFetch(t, s.base, "../outside/secret.flac")
	if status != http.StatusNotFound || code != "not_found" || strings.Contains(string(body), string(secret[:64])) {
		t.Fatalf("a row that leaves the root: status %d, error %q", status, code)
	}
	if links {
		status, body, code := loopbackFetch(t, s.base, "out.flac")
		if status != http.StatusForbidden || code != "outside_library" || strings.Contains(string(body), string(secret[:64])) {
			t.Fatalf("a link out of the root: status %d, error %q", status, code)
		}
	}

	s.stop()
	<-s.exited
	if code := <-s.exitCode; code != 0 {
		t.Fatalf("serve exit code %d; stderr=%s", code, s.stderr.String())
	}
	host := strings.TrimPrefix(s.base, "http://")
	if conn, err := net.Dial("tcp", host); err == nil {
		conn.Close()
		t.Fatalf("%s still accepts after serve stopped", host)
	}
}

// TestServeStartsNoLoopbackRouteWhileOutputIsOff: output.mpd off is the
// explicit opt-out, and the route stays off.
func TestServeStartsNoLoopbackRouteWhileOutputIsOff(t *testing.T) {
	root := t.TempDir()
	music := filepath.Join(root, "Music")
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(music, 0o755); err != nil {
		t.Fatal(err)
	}
	s := bootLoopbackServe(t, music, dataDir, "  mpd: off\n  loopbackPort: 0\n")
	if s.base != "" {
		t.Fatalf("output.mpd off started the loopback route at %s", s.base)
	}
}
