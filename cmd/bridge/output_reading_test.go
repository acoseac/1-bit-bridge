package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/auth"
	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/flactest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/signalpath"
)

// TestServeReadsTheOutputAndNamesItsOwnTrack boots the real serve with
// output.mpd on and an MPD on a loopback socket that is on one of this
// bridge's tracks through the loopback track route. The reading names that
// song as the bridge's row, with the index's format and the file's own
// stat: the wiring of the route, the index lookup and the row lookup, which
// only a boot shows. The suite's /proc is empty, so the card is an honest
// unknown.
func TestServeReadsTheOutputAndNamesItsOwnTrack(t *testing.T) {
	root := t.TempDir()
	music := filepath.Join(root, "Music")
	dataDir := filepath.Join(root, "data")
	for _, d := range []string{music, dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := flactest.Stream(44100, 2, 16, 4096, 7)
	file := filepath.Join(music, "a.flac")
	if err := os.WriteFile(file, body, 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	store, err := manifest.OpenStore(manifest.DefaultDBPath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	rate, bits, dsd := 44100.0, 16, false
	row := &manifest.Track{Path: "a.flac", Size: fi.Size(), ModTime: fi.ModTime(), Codec: "FLAC", SampleRate: &rate, BitsPerSample: &bits, IsDSD: &dsd}
	if err := store.UpsertTrack(context.Background(), row); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.OpenStore(filepath.Join(dataDir, tokensFileName))
	if err != nil {
		t.Fatal(err)
	}
	bearer, _, err := tokens.Mint("reading")
	if err != nil {
		t.Fatal(err)
	}
	var song atomic.Value
	song.Store("")
	mpdAddr := serveFakeMPD(t, &song)
	confPath := filepath.Join(root, "mpd.conf")
	conf := "music_directory \"" + filepath.ToSlash(music) + "\"\naudio_output {\n\ttype \"alsa\"\n\tname \"alsa\"\n\tdevice \"hw:0,0\"\n}\n"
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}

	s := bootLoopbackServe(t, music, dataDir, "  mpd: on\n  loopbackPort: 0\n  endpoint: \""+mpdAddr+"\"\n  conf: \""+filepath.ToSlash(confPath)+"\"\n")
	if s.base == "" {
		t.Fatalf("no loopback route; stderr=%s", s.stderr.String())
	}
	uri := s.base + dlna.FilePathPrefix + dlna.TrackID("", "a.flac")
	song.Store(uri)

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true},
	}
	get := func(path string, out any) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, "https://"+s.api+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v; stderr=%s", path, err, s.stderr.String())
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
		}
		return resp.StatusCode
	}
	var health api.HealthResponse
	get("/v1/health", &health)
	if !slices.Contains(health.Features, "outputReading") {
		t.Fatalf("features %v", health.Features)
	}
	var outputs api.OutputsResponse
	if code := get("/v1/outputs", &outputs); code != http.StatusOK || len(outputs.Outputs) != 1 {
		t.Fatalf("outputs %d %+v", code, outputs)
	}
	var reading api.OutputReading
	if code := get("/v1/outputs/"+outputs.Outputs[0].ID+"/reading", &reading); code != http.StatusOK {
		t.Fatalf("reading %d", code)
	}
	if reading.State != "unknown" || !slices.Equal(reading.StateReasons, []string{signalpath.NoCard}) {
		t.Fatalf("state %s %v", reading.State, reading.StateReasons)
	}
	if reading.MPD == nil || reading.MPD.State != "play" || reading.MPD.Audio != "44100:16:2" {
		t.Fatalf("mpd %+v", reading.MPD)
	}
	if reading.Song == nil || reading.Song.URI != uri || reading.Song.Row == nil {
		t.Fatalf("song %+v", reading.Song)
	}
	if r := reading.Song.Row; r.Path != "a.flac" || r.Via != "loopback" || !r.Current || r.Codec != "FLAC" ||
		r.SampleRate == nil || *r.SampleRate != 44100 || r.BitsPerSample == nil || *r.BitsPerSample != 16 {
		t.Fatalf("row %+v", r)
	}
}

// serveFakeMPD is an MPD on a loopback socket on the song song holds, for
// the boot test: it greets, answers the read commands, and refuses config
// over TCP as MPD 0.24 does.
func serveFakeMPD(t *testing.T, song *atomic.Value) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	replies := map[string]func() string{
		"status": func() string {
			return "state: play\nsongid: 7\nelapsed: 12.500\naudio: 44100:16:2\nmixrampdb: 0\nOK\n"
		},
		"currentsong":        func() string { return "file: " + song.Load().(string) + "\nId: 7\nOK\n" },
		"replay_gain_status": func() string { return "replay_gain_mode: off\nOK\n" },
		"outputs": func() string {
			return "outputid: 0\noutputname: alsa\nplugin: alsa\noutputenabled: 1\nattribute: dop=0\nOK\n"
		},
		"find":   func() string { return "OK\n" },
		"config": func() string { return "ACK [4@0] {config} Command only permitted to local clients\n" },
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if _, err := conn.Write([]byte("OK MPD 0.24.0\n")); err != nil {
					return
				}
				br := bufio.NewReader(conn)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
					reply := "ACK [5@0] {} unknown command\n"
					if f, ok := replies[name]; ok {
						reply = f()
					}
					if _, err := conn.Write([]byte(reply)); err != nil {
						return
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// TestOutputTrackLookupReadsTheRowAndItsStat: the reading's row lookup
// carries the track's format and the size and time the scanner's skip
// gate compares, and a path with no row is none.
func TestOutputTrackLookupReadsTheRowAndItsStat(t *testing.T) {
	store, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rate, bits, dsd, two := 2822400.0, 1, true, 2
	when := time.Date(2026, 10, 19, 12, 0, 0, 123456789, time.UTC)
	if err := store.UpsertTrack(context.Background(), &manifest.Track{
		Path: "Album/a.dsf", Size: 1234, ModTime: when, Codec: "DSF",
		SampleRate: &rate, BitsPerSample: &bits, IsDSD: &dsd, Channels: &two,
	}); err != nil {
		t.Fatal(err)
	}
	lookup := outputTrackLookup(store)
	got, ok, err := lookup(context.Background(), "Album/a.dsf")
	if err != nil || !ok {
		t.Fatalf("ok %v err %v", ok, err)
	}
	if got.Codec != "DSF" || got.SampleRate == nil || *got.SampleRate != rate || got.BitsPerSample == nil || *got.BitsPerSample != 1 ||
		got.IsDSD == nil || !*got.IsDSD || got.Channels == nil || *got.Channels != 2 || got.Size != 1234 || got.MTimeNS != when.UnixNano() {
		t.Fatalf("track %+v", got)
	}
	if _, ok, err := lookup(context.Background(), "Album/missing.dsf"); ok || err != nil {
		t.Fatalf("a missing row: ok %v err %v", ok, err)
	}
}

// countedRows is an outputRowReader that answers one row and counts the
// reads it is asked for.
type countedRows struct {
	track       manifest.Track
	size, mtime int64
	reads       []string
}

func (r *countedRows) GetTrackWithStat(_ context.Context, path string) (*manifest.Track, int64, int64, error) {
	r.reads = append(r.reads, path)
	if path != r.track.Path {
		return nil, 0, 0, nil
	}
	t := r.track
	return &t, r.size, r.mtime, nil
}

// TestOutputTrackLookupReadsOneVersionOfTheRow: the format and the size and
// time the lookup returns are one version of the row only while one
// statement reads them all, since a scan can commit between two (an ALAC
// file replaced by an AAC one at the same path would pair the old codec with
// the new file's size and time, which the file on disk then matches). The
// lookup's reader has GetTrackWithStat alone, which the manifest pins to one
// statement (TestGetTrackWithStatIsOneStatement), and it is called once a
// look, for the row and its stat together.
func TestOutputTrackLookupReadsOneVersionOfTheRow(t *testing.T) {
	rate, bits := 44100.0, 16
	rows := &countedRows{track: manifest.Track{Path: "Album/a.m4a", Codec: "ALAC", SampleRate: &rate, BitsPerSample: &bits}, size: 1000, mtime: 42}
	lookup := outputTrackLookup(rows)
	got, ok, err := lookup(context.Background(), "Album/a.m4a")
	if err != nil || !ok || got.Codec != "ALAC" || got.Size != 1000 || got.MTimeNS != 42 {
		t.Fatalf("ok %v err %v track %+v", ok, err, got)
	}
	if !slices.Equal(rows.reads, []string{"Album/a.m4a"}) {
		t.Fatalf("the lookup read the row %d times: %v", len(rows.reads), rows.reads)
	}
}

// TestLoopbackTrackLookupIsTheRoutesIndex: the id resolves through the
// index the loopback route reads; a routed track, which the route does not
// serve, is none.
func TestLoopbackTrackLookupIsTheRoutesIndex(t *testing.T) {
	lib := &dlna.StaticLibrary{Tracks: []dlna.TrackInfo{
		{TrackID: "aaaaaaaaaaaaaaaa", RelativePath: "Album/a.flac", AbsolutePath: "/music/Album/a.flac"},
		{TrackID: "bbbbbbbbbbbbbbbb", RelativePath: "Routed/b.flac"},
	}}
	lookup := loopbackTrackLookup(lib)
	if path, abs, ok := lookup("aaaaaaaaaaaaaaaa"); !ok || path != "Album/a.flac" || abs != "/music/Album/a.flac" {
		t.Fatalf("a track: %q %q %v", path, abs, ok)
	}
	for _, id := range []string{"bbbbbbbbbbbbbbbb", "cccccccccccccccc"} {
		if _, _, ok := lookup(id); ok {
			t.Fatalf("%s resolved", id)
		}
	}
}
