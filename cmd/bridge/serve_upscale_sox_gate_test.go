package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
)

// TestServeWithoutSoxReportsUpscalingOffOnEverySurface boots the real
// serve with upscaling, the CarPlay kind and its pre-generation all
// switched on, on a PATH that holds no sox, and asks each surface that
// answers "is upscaling doing anything": /v1/health, the console's upscale
// stats (the Settings tile, and the chip beside the switch that takes its
// verdict from them), /v1/upscale/stats, and the Jobs card after a
// pre-generation sweep.
//
// /v1/health read the live gate (the flag AND a usable sox) and said off,
// while the other three read the flag alone. The console and
// /v1/upscale/stats answered `enabled: true` beside `soxAvailable: false`,
// and the card said "on" while every sweep queued jobs that could only
// fail. Measured with the real binary on six hi-res tracks: three sweeps
// queued 18 jobs, all 18 failed with a WARN each, and the third strike
// suppressed all six from pre-generation for 30 days. Installing sox did
// not clear it, and the card then read "all caught up".
//
// Every surface must answer as health does, and the card must say why:
// switched on, not active, degraded for want of sox, and a sweep that
// queued nothing.
func TestServeWithoutSoxReportsUpscalingOffOnEverySurface(t *testing.T) {
	withoutSoxOnPath(t)
	// Registered before startServedBridge registers its drain, so it runs
	// after serve has returned: the loop reads the delay once, when it
	// starts.
	prev := autoOptimizeSettleDelay
	autoOptimizeSettleDelay = time.Millisecond
	t.Cleanup(func() { autoOptimizeSettleDelay = prev })

	// minFreeBytes 1: the sweep's free-space floor must not be what keeps
	// it from queueing on a small CI volume.
	const upscaleYAML = "upscale:\n  enabled: true\n  autoOptimize:\n    enabled: true\n    minFreeBytes: 1\n"
	b := startServedBridge(t, upscaleYAML, func(lib string) {
		// Two tracks the CarPlay kind takes (96 kHz / 24-bit FLAC), so a
		// sweep that runs has something to queue.
		for _, name := range []string{"01 One.flac", "02 Two.flac"} {
			writeHiResFLACFixture(t, filepath.Join(lib, "Artist", "Album", name))
		}
	})

	if healthUpscaleEnabled(t, b.phone, b.apiBase) {
		t.Fatalf("fixture broken: /v1/health says upscaling is on, so a usable sox was found "+
			"after all, and nothing below would show what it is meant to; stderr=%s", b.stderr.String())
	}

	// The console's tile, and the Settings chip that reads it.
	stats := consoleUpscaleStats(t, b.console, b.adminBase)
	if stats.SoxAvailable == nil || *stats.SoxAvailable {
		t.Fatalf("fixture broken: the console's sox probe says soxAvailable=%v on a PATH with no sox",
			stats.SoxAvailable)
	}
	if stats.Enabled || stats.Pool != nil {
		t.Errorf("GET /api/upscale/stats says enabled=%t (pool present: %t) while /v1/health says "+
			"upscaling is off: the Settings tile, and the chip beside the switch, call the feature "+
			"active on a bridge with no sox", stats.Enabled, stats.Pool != nil)
	}

	// What a paired device, or third-party tooling, reads.
	mint := pairViaAdmin(t, t.Context(), b.console, b.adminBase+"/api/tokens",
		`{"name":"sox gate test"}`, http.StatusCreated, b.stderr)
	token := linkQueryItems(t, mint.PairURL)["token"]
	if token == "" {
		t.Fatalf("the console's pairing link carries no token: %s", mint.PairURL)
	}
	if v1UpscaleStatsEnabled(t, b.phone, b.apiBase, token) {
		t.Errorf("GET /v1/upscale/stats says enabled=true while /v1/health says upscaling is off; " +
			"PROTOCOL.md documents the two as one live state")
	}

	// The pre-generation sweeper and its card. Wait for the startup scan,
	// so the sweep has the two tracks to offer, then ask for one and wait
	// until one more sweep has finished than had before the nudge.
	waitForTracksIndexed(t, b.console, b.adminBase, 2, b.stderr)
	before := b.autoOptimizeSweeps.Load()
	nudgeAutoOptimize(t, b.console, b.adminBase)
	card := waitForAutoOptimizeSweep(t, b, before)
	if !card.Enabled {
		t.Fatalf("fixture broken: the card says the pre-generation switches are off: %+v", card)
	}
	if card.Active {
		t.Errorf("the Jobs card calls pre-generation active on a bridge with no sox, where every job " +
			"a sweep queues fails and strikes its file")
	}
	if card.DegradedReason != "sox_missing" {
		t.Errorf("the Jobs card's degradedReason = %q, want \"sox_missing\": switched on and not "+
			"running must say why, not read as switched off", card.DegradedReason)
	}
	if card.Last == nil || !card.Last.Disabled || card.Last.Enqueued != 0 {
		t.Errorf("the sweep after the scan recorded %+v, want one the gate refused, that queued nothing",
			card.Last)
	}
}

// withoutSoxOnPath leaves PATH holding only the directories with no sox
// in them, for the rest of the test, and fails the test if a sox is still
// found there.
func withoutSoxOnPath(t *testing.T) {
	t.Helper()
	names := []string{"sox"}
	if runtime.GOOS == "windows" {
		names = []string{"sox.exe", "sox.bat", "sox.cmd", "sox.com"}
	}
	var keep []string
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		held := false
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(d, name)); err == nil {
				held = true
				break
			}
		}
		if !held {
			keep = append(keep, d)
		}
	}
	t.Setenv("PATH", strings.Join(keep, string(os.PathListSeparator)))
	if p, err := exec.LookPath("sox"); err == nil {
		t.Fatalf("fixture broken: sox is still found on PATH, at %s", p)
	}
}

// writeHiResFLACFixture writes a FLAC file the scanner indexes as 96 kHz,
// 24-bit stereo, five seconds long: a STREAMINFO block and a Vorbis
// comment, and no audio frames, since nothing here decodes it. Bytes by
// hand rather than through mewkiz/flac, whose top-level constructors leak
// the file handle (TestNoLeakyFlacConstructors).
func writeHiResFLACFixture(t *testing.T, path string) {
	t.Helper()
	const rate, bits, channels = 96000, 24, 2
	var buf bytes.Buffer
	buf.WriteString("fLaC")

	// STREAMINFO: not the last block, type 0, 34 bytes.
	buf.Write([]byte{0x00, 0x00, 0x00, 34})
	var si [34]byte
	binary.BigEndian.PutUint16(si[0:2], 4096) // min block size
	binary.BigEndian.PutUint16(si[2:4], 4096) // max block size
	// Bytes 4-9, the frame sizes, stay zero ("unknown"). Then 20 bits of
	// sample rate, 3 of channels-1, 5 of bits-1 and 36 of total samples.
	packed := uint64(rate)<<44 | uint64(channels-1)<<41 | uint64(bits-1)<<36 | uint64(rate*5)
	binary.BigEndian.PutUint64(si[10:18], packed)
	buf.Write(si[:]) // bytes 18-33, the audio MD5, stay zero ("not computed")

	// VORBIS_COMMENT: the last block, type 4. Little-endian lengths.
	var vc bytes.Buffer
	putString := func(s string) {
		_ = binary.Write(&vc, binary.LittleEndian, uint32(len(s)))
		vc.WriteString(s)
	}
	putString("1-bit-bridge test fixture")
	comments := []string{"ARTIST=Artist", "ALBUM=Album", "TITLE=" + strings.TrimSuffix(filepath.Base(path), ".flac")}
	_ = binary.Write(&vc, binary.LittleEndian, uint32(len(comments)))
	for _, c := range comments {
		putString(c)
	}
	n := vc.Len()
	buf.Write([]byte{0x80 | 4, byte(n >> 16), byte(n >> 8), byte(n)})
	buf.Write(vc.Bytes())

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// consoleUpscaleView is the part of GET /api/upscale/stats this test reads.
type consoleUpscaleView struct {
	Enabled      bool           `json:"enabled"`
	SoxAvailable *bool          `json:"soxAvailable"`
	Pool         map[string]any `json:"pool"`
}

// consoleUpscaleStats reads the console's upscale stats.
func consoleUpscaleStats(t *testing.T, client *http.Client, adminBase string) consoleUpscaleView {
	t.Helper()
	var out consoleUpscaleView
	getJSON(t, client, adminBase+"/api/upscale/stats", "", &out)
	return out
}

// v1UpscaleStatsEnabled is GET /v1/upscale/stats' `enabled`, read with a
// paired device's bearer token.
func v1UpscaleStatsEnabled(t *testing.T, client *http.Client, apiBase, token string) bool {
	t.Helper()
	var out api.UpscaleStats
	getJSON(t, client, apiBase+"/v1/upscale/stats", token, &out)
	return out.Enabled
}

// autoOptimizeCardView is the Jobs card this test reads from GET /api/jobs.
type autoOptimizeCardView struct {
	Enabled        bool   `json:"enabled"`
	Active         bool   `json:"active"`
	DegradedReason string `json:"degradedReason"`
	Last           *struct {
		Disabled bool `json:"disabled"`
		Enqueued int  `json:"enqueued"`
	} `json:"last"`
}

// nudgeAutoOptimize asks for a pre-generation sweep as the card's
// "Sweep now" button does.
func nudgeAutoOptimize(t *testing.T, client *http.Client, adminBase string) {
	t.Helper()
	resp, err := client.Post(adminBase+"/api/upscale/auto-optimize/sweep", "", nil)
	if err != nil {
		t.Fatalf("POST /api/upscale/auto-optimize/sweep: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/upscale/auto-optimize/sweep = %d: %s", resp.StatusCode, raw)
	}
}

// waitForAutoOptimizeSweep waits until more than `before` pre-generation
// sweeps have finished, by the count serve keeps for the test
// (servedBridge.autoOptimizeSweeps), and returns the Jobs card read after
// that. The count goes up once a sweep's result is on the card, so the
// card read here shows that sweep or a later one.
//
// It counts rather than comparing the card's lastFinishedAt with the time
// of the nudge. A sweep the gate refuses finishes within a millisecond,
// which on Windows is inside one tick of the wall clock (about 15.6 ms), and
// a time decoded from JSON carries no monotonic reading, so the two compare
// by wall clock: the finish equals the nudge's instant, "after" never
// holds, and the wait ran out on test (windows-latest) (2026-09-28).
func waitForAutoOptimizeSweep(t *testing.T, b *servedBridge, before int64) autoOptimizeCardView {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for b.autoOptimizeSweeps.Load() <= before {
		if time.Now().After(deadline) {
			t.Fatalf("no pre-generation sweep finished within 30 s of the nudge (%d finished before it, %d now); stderr=%s",
				before, b.autoOptimizeSweeps.Load(), b.stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	var jobs struct {
		AutoOptimize *autoOptimizeCardView `json:"autoOptimize"`
	}
	getJSON(t, b.console, b.adminBase+"/api/jobs", "", &jobs)
	if jobs.AutoOptimize == nil {
		t.Fatalf("GET /api/jobs carries no pre-generation card after a sweep finished; stderr=%s", b.stderr.String())
	}
	return *jobs.AutoOptimize
}

// waitForTracksIndexed polls the console's stats until the library holds
// n tracks and no scan is running.
func waitForTracksIndexed(t *testing.T, client *http.Client, adminBase string, n int, stderr *safeBuffer) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var stats struct {
			TracksIndexed int  `json:"tracksIndexed"`
			IsScanning    bool `json:"isScanning"`
		}
		getJSON(t, client, adminBase+"/api/stats", "", &stats)
		if stats.TracksIndexed >= n && !stats.IsScanning {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the library holds %d tracks after 30 s, want %d; stderr=%s",
				stats.TracksIndexed, n, stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// getJSON GETs url, with a bearer token when one is given, requires a 200,
// and decodes the body into out.
func getJSON(t *testing.T, client *http.Client, url, token string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode GET %s: %v: %s", url, err, raw)
	}
}
