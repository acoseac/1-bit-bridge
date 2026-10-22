package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/mpd"
	"github.com/acoseac/1-bit-bridge/internal/signalpath"
)

// piCaptures are B1's captures from the Volumio Pi 5 with the iFi GO bar
// (internal/alsa/testdata, 2026-10-19), each with the file MPD was on and
// what reached card 5.
var piCaptures = []struct {
	name   string
	file   string
	track  OutputTrack
	format string
	rate   int
}{
	{"pcm-44k1-16", "Album/01 Forty-four.flac", flacRow(44100, 16), "S16_LE", 44100},
	{"pcm-96k-24", "Album/02 Ninety-six.flac", flacRow(96000, 24), "S24_3LE", 96000},
	{"pcm-192k-24", "Album/03 One-ninety-two.flac", flacRow(192000, 24), "S24_3LE", 192000},
	{"pcm-384k-24", "Album/04 Three-eighty-four.flac", flacRow(384000, 24), "S24_3LE", 384000},
	{"dsd256-native", "Album/05 DSD256.dsf", dsfRow(11289600), "DSD_U32_BE", 352800},
}

func flacRow(rate float64, bits int) OutputTrack {
	dsd := false
	return OutputTrack{Codec: "FLAC", SampleRate: &rate, BitsPerSample: &bits, IsDSD: &dsd}
}

func dsfRow(rate float64) OutputTrack {
	one, two, dsd := 1, 2, true
	return OutputTrack{Codec: "DSF", SampleRate: &rate, BitsPerSample: &one, Channels: &two, IsDSD: &dsd}
}

// piCapture copies one capture's /proc tree under a temp directory, with
// the Volumio box's own etc/asound.conf and etc/alsa/conf.d beside it, so
// MPD's device "volumio" reaches card 5 through empty, plug and hw as it
// does on the box. The captured files are copied, never edited.
func piCapture(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	copyTree(t, filepath.Join("..", "alsa", "testdata", name, "proc"), filepath.Join(root, "proc"))
	copyTree(t, filepath.Join("..", "alsa", "testdata", "volumio-asound-chain", "etc"), filepath.Join(root, "etc"))
	return filepath.Join(root, "proc")
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// capturedStatus is MPD's own status reply kept beside a playing capture.
func capturedStatus(t *testing.T, name string) mpd.Reply {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "alsa", "testdata", name, "mpd_status"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	reply, err := mpd.ParseReply(text + "OK\n")
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

// volumioReplies is the box's own replies to the read commands
// (internal/mpd/testdata/volumio-mpd-replies.json).
func volumioReplies(t *testing.T) map[string]mpd.Reply {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "mpd", "testdata", "volumio-mpd-replies.json"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	out := map[string]mpd.Reply{}
	for _, name := range []string{"status", "outputs", "replay_gain_status", "currentsong"} {
		reply, err := mpd.ParseReply(raw[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = reply
	}
	return out
}

// volumioConf is the box's own mpd.conf with music_directory pointed at
// dir: the device "volumio", dop "no", no mixer_type, normalization off.
func volumioConf(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "mpd", "testdata", "volumio-mpd.conf"))
	if err != nil {
		t.Fatal(err)
	}
	music := regexp.MustCompile(`(?m)^music_directory.*$`)
	return music.ReplaceAllLiteralString(string(b), `music_directory "`+filepath.ToSlash(dir)+`"`)
}

func replyOf(kv ...string) mpd.Reply {
	r := mpd.Reply{Pairs: []mpd.Pair{}}
	for i := 0; i+1 < len(kv); i += 2 {
		r.Pairs = append(r.Pairs, mpd.Pair{Key: kv[i], Value: kv[i+1]})
	}
	return r
}

// withPair returns r with key set to value, replacing the first such line
// or adding one.
func withPair(r mpd.Reply, key, value string) mpd.Reply {
	out := mpd.Reply{Pairs: append([]mpd.Pair(nil), r.Pairs...)}
	for i := range out.Pairs {
		if out.Pairs[i].Key == key {
			out.Pairs[i].Value = value
			return out
		}
	}
	out.Pairs = append(out.Pairs, mpd.Pair{Key: key, Value: value})
	return out
}

// scriptedMPD is the MPD the reading tests reach through the session
// interface. status answers from a list whose last element repeats, and
// the other commands from fixed replies. A command can be made to fail, or
// to hold until released or until its context ends.
type scriptedMPD struct {
	mu          sync.Mutex
	statuses    []mpd.Reply
	nStatus     int
	current     mpd.Reply
	gain        mpd.Reply
	outputs     mpd.Reply
	errs        map[string]error
	hold        map[string]chan struct{}
	calls       []string
	inFlight    int
	maxInFlight int
	closed      atomic.Int32
}

func (m *scriptedMPD) run(ctx context.Context, name string, answer func() mpd.Reply) (mpd.Reply, error) {
	m.mu.Lock()
	m.calls = append(m.calls, name)
	m.inFlight++
	if m.inFlight > m.maxInFlight {
		m.maxInFlight = m.inFlight
	}
	hold, err := m.hold[name], m.errs[name]
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.inFlight--
		m.mu.Unlock()
	}()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return mpd.Reply{}, ctx.Err()
		}
	}
	if err != nil {
		return mpd.Reply{}, err
	}
	return answer(), nil
}

func (m *scriptedMPD) Status(ctx context.Context) (mpd.Reply, error) {
	return m.run(ctx, "status", func() mpd.Reply {
		m.mu.Lock()
		defer m.mu.Unlock()
		i := min(m.nStatus, len(m.statuses)-1)
		m.nStatus++
		return m.statuses[i]
	})
}

func (m *scriptedMPD) Find(ctx context.Context, path string) (mpd.Reply, error) {
	return m.run(ctx, "find", func() mpd.Reply { return replyOf("file", path) })
}

func (m *scriptedMPD) CurrentSong(ctx context.Context) (mpd.Reply, error) {
	return m.run(ctx, "currentsong", func() mpd.Reply { return m.current })
}

func (m *scriptedMPD) ReplayGainStatus(ctx context.Context) (mpd.Reply, error) {
	return m.run(ctx, "replay_gain_status", func() mpd.Reply { return m.gain })
}

func (m *scriptedMPD) Outputs(ctx context.Context) (mpd.Reply, error) {
	return m.run(ctx, "outputs", func() mpd.Reply { return m.outputs })
}

func (m *scriptedMPD) Version() string { return "0.24.0" }

func (m *scriptedMPD) Close() error {
	m.closed.Add(1)
	return nil
}

func (m *scriptedMPD) commands() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

func (m *scriptedMPD) statusCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nStatus
}

// restart answers the next status from the first of the list again: the
// GET /v1/outputs a test makes for the id reads one.
func (m *scriptedMPD) restart(statuses ...mpd.Reply) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statuses, m.nStatus = statuses, 0
}

// set changes the script under its lock, for a test that changes it once
// the server has run.
func (m *scriptedMPD) set(change func(*scriptedMPD)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	change(m)
}

// readingRig is one box: a capture's /proc with Volumio's asound.conf,
// Volumio's mpd.conf, an MPD music_directory holding the library root at
// INTERNAL/Demo Library as Volumio lays it out, one song in it that is one
// of the bridge's rows, and MPD on that song.
type readingRig struct {
	t        *testing.T
	procRoot string
	music    string
	root     string
	confPath string
	file     string
	uri      string
	tracks   map[string]OutputTrack
	loop     map[string][2]string
	askedMu  sync.Mutex
	asked    []string
	mpd      *scriptedMPD
	dials    atomic.Int32
	reads    atomic.Int32
	cards    func() (alsa.Snapshot, error)
	cfg      LocalOutputsConfig
}

func newReadingRig(t *testing.T, capture, file string, track OutputTrack, status mpd.Reply) *readingRig {
	t.Helper()
	r := &readingRig{t: t, file: file, loop: map[string][2]string{}}
	r.procRoot = piCapture(t, capture)
	r.music = t.TempDir()
	r.root = filepath.Join(r.music, "INTERNAL", "Demo Library")
	abs := filepath.Join(r.root, filepath.FromSlash(file))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("the bytes of "+file), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		t.Fatal(err)
	}
	track.Size, track.MTimeNS = fi.Size(), fi.ModTime().UnixNano()
	r.tracks = map[string]OutputTrack{file: track}
	r.uri = "INTERNAL/Demo Library/" + file
	r.confPath = filepath.Join(t.TempDir(), "mpd.conf")
	r.writeConf(volumioConf(t, r.music))
	songID, _ := status.Get("songid")
	audio, _ := status.Get("audio")
	replies := volumioReplies(t)
	r.mpd = &scriptedMPD{
		statuses: []mpd.Reply{status},
		current:  replyOf("file", r.uri, "Format", audio, "Id", songID),
		gain:     replies["replay_gain_status"],
		outputs:  replies["outputs"],
	}
	r.cards = func() (alsa.Snapshot, error) { return (alsa.Reader{ProcRoot: r.procRoot}).Read() }
	r.cfg = LocalOutputsConfig{
		GOOS: "linux",
		Output: func() config.OutputConfig {
			return config.OutputConfig{MPD: "on"}
		},
		Inspect: func(context.Context, mpd.Options) mpd.Report {
			return mpd.Report{
				Software:       mpd.Software{Name: "Volumio", Version: "4.119"},
				MPD:            mpd.Daemon{Version: "0.24.0", Endpoint: "127.0.0.1:6600"},
				ConfPath:       r.confPath,
				MusicDirectory: r.music,
				Outputs: []mpd.Output{
					{Name: "alsa", Type: "alsa", Device: "volumio", Enabled: "yes"},
					{Name: "multiroom", Type: "fifo", Enabled: "no"},
				},
			}
		},
		Dial: func(context.Context, string, string) (outputSession, error) {
			r.dials.Add(1)
			return r.mpd, nil
		},
		Greet:      func(context.Context, string) bool { return false },
		Advertised: func() []string { return nil },
		Cards: func() (alsa.Snapshot, error) {
			r.reads.Add(1)
			return r.cards()
		},
		Roots: func() []string { return []string{r.root} },
		Tracks: func(context.Context) ([]string, error) {
			var out []string
			for p := range r.tracks {
				out = append(out, p)
			}
			sort.Strings(out)
			return out, nil
		},
		Track: func(_ context.Context, path string) (OutputTrack, bool, error) {
			r.askedMu.Lock()
			r.asked = append(r.asked, path)
			r.askedMu.Unlock()
			tr, ok := r.tracks[path]
			return tr, ok, nil
		},
		LoopbackTrack: func(id string) (string, string, bool) {
			v, ok := r.loop[id]
			return v[0], v[1], ok
		},
		LoopbackBaseURL: func() string { return "http://127.0.0.1:7791" },
	}
	return r
}

// askedPaths are the manifest paths the reading looked a row up at.
func (r *readingRig) askedPaths() []string {
	r.askedMu.Lock()
	defer r.askedMu.Unlock()
	return append([]string(nil), r.asked...)
}

func (r *readingRig) writeConf(text string) {
	r.t.Helper()
	if err := os.WriteFile(r.confPath, []byte(text), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

// serve starts the route over the rig and returns the server, a token and
// the output's id as GET /v1/outputs lists it.
func (r *readingRig) serve() (*httptest.Server, string, string) {
	r.t.Helper()
	hs, token := newOutputsHTTP(r.t, "fp", r.cfg)
	return hs, token, firstOutputID(r.t, hs, token)
}

func firstOutputID(t *testing.T, hs *httptest.Server, token string) string {
	t.Helper()
	resp := outputsGet(t, hs, token, "/v1/outputs")
	body, raw := decodeOutputs(t, resp)
	if len(body.Outputs) == 0 {
		t.Fatalf("no output: %s", raw)
	}
	return body.Outputs[0].ID
}

func getReading(t *testing.T, hs *httptest.Server, token, id string) (int, OutputReading, []byte) {
	t.Helper()
	resp := outputsGet(t, hs, token, "/v1/outputs/"+id+"/reading")
	defer resp.Body.Close()
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var got OutputReading
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
	}
	return resp.StatusCode, got, raw
}

func wantVerdict(t *testing.T, got OutputReading, raw []byte, value string, reasons ...string) {
	t.Helper()
	want := signalpath.Sorted(reasons)
	if got.Verdict.Value != value || !slices.Equal(got.Verdict.Reasons, want) {
		t.Fatalf("verdict %+v, want %s %v\n%s", got.Verdict, value, want, raw)
	}
}

func str(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestReadingTheGoBarOnEveryCapture drives the route over each of the Pi's
// captures: what card 5 received, its owner resolved through the thread's
// Tgid to mpd, MPD's own status from the same minute, Volumio's chain, and
// the song as one of the bridge's rows. Each is bit-exact, which is what the
// Pi measured: the formats reached the DAC unchanged.
func TestReadingTheGoBarOnEveryCapture(t *testing.T) {
	for _, c := range piCaptures {
		t.Run(c.name, func(t *testing.T) {
			status := capturedStatus(t, c.name)
			rig := newReadingRig(t, c.name, c.file, c.track, status)
			hs, token, id := rig.serve()
			code, got, raw := getReading(t, hs, token, id)
			if code != http.StatusOK {
				t.Fatalf("status %d: %s", code, raw)
			}
			if got.OutputID != id || got.State != "whole" || len(got.StateReasons) != 0 {
				t.Fatalf("id %s state %s %v\n%s", got.OutputID, got.State, got.StateReasons, raw)
			}
			wantVerdict(t, got, raw, signalpath.BitExact)
			dac := got.DAC
			if dac == nil || dac.Card != 5 || dac.ID != "Audio" || dac.Device != 0 || dac.Subdevice == nil || *dac.Subdevice != 0 {
				t.Fatalf("dac %+v", dac)
			}
			if dac.State != "RUNNING" || dac.Format != c.format || dac.Rate == nil || *dac.Rate != c.rate || dac.Channels == nil || *dac.Channels != 2 {
				t.Fatalf("hw %+v", dac)
			}
			o := dac.Owner
			if o == nil || o.TID != 6584 || str(o.Thread) != "output:alsa" || o.PID == nil || *o.PID != 6551 || str(o.Process) != "mpd" {
				t.Fatalf("owner %+v", o)
			}
			songID, _ := status.Get("songid")
			audio, _ := status.Get("audio")
			m := got.MPD
			if m == nil || m.State != "play" || m.SongID == nil || itoa(*m.SongID) != songID || m.Audio != audio || m.Elapsed == nil {
				t.Fatalf("mpd %+v", m)
			}
			if m.ReplayGainMode != "off" || m.Volume != nil || m.Xfade == nil || *m.Xfade != 0 || m.MixRampDB == nil || *m.MixRampDB != 0 || m.MixRampDelay != nil {
				t.Fatalf("mpd settings %+v", m)
			}
			if len(m.Outputs) != 2 || m.Outputs[0].Name != "alsa" || !m.Outputs[0].Enabled || m.Outputs[0].Attributes["dop"] != "0" || m.Outputs[1].Enabled {
				t.Fatalf("outputs %+v", m.Outputs)
			}
			cfgOut := m.Config.Output
			if cfgOut == nil || cfgOut.Name != "alsa" || cfgOut.Device != "volumio" || !slices.Equal(cfgOut.Chain, []string{"empty", "plug", "hw"}) || cfgOut.MixerType != "hardware" || cfgOut.DoP {
				t.Fatalf("config output %+v", cfgOut)
			}
			if dac.Mixer == nil || dac.Mixer.Control != "PCM" || dac.Mixer.Volume != nil {
				t.Fatalf("the DAC's mixer: %+v", dac.Mixer)
			}
			song := got.Song
			if song == nil || song.URI != rig.uri || song.Format != audio || song.Row == nil {
				t.Fatalf("song %+v", song)
			}
			if row := song.Row; row.Path != c.file || row.Via != "path" || !row.Current || row.Codec != c.track.Codec {
				t.Fatalf("row %+v", row)
			}
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// newReadingServer is newOutputsHTTP that also returns the Server, for the
// tests that set a reading's deadlines or call it directly.
func newReadingServer(t *testing.T, cfg LocalOutputsConfig) (*Server, *httptest.Server, string) {
	t.Helper()
	store := outputsToken(t)
	raw, _, err := store.Mint("phone")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if cfg.Getenv == nil {
		cfg.Getenv = func(string) string { return "" }
	}
	srv := New(&config.Config{
		ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789",
		ScanIntervalSec: 3600, LibraryName: "T", LibraryRoots: []string{t.TempDir()},
	}, store, nil, "fp").WithLocalOutputs(cfg)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return srv, hs, raw
}

// The mpd.conf edits the tests make, on Volumio's own file whatever its
// indentation.
var (
	volumioDeviceLine = regexp.MustCompile(`(?m)^(\s*)device(\s+)"volumio"[ \t]*$`)
	normalizationLine = regexp.MustCompile(`(?m)^volume_normalization\s+"no"`)
)

// withOutputSetting adds key "value" to Volumio's alsa output, after its
// device line.
func withOutputSetting(conf, key, value string) string {
	return volumioDeviceLine.ReplaceAllString(conf, `${1}device${2}"volumio"`+"\n"+`${1}`+key+` "`+value+`"`)
}

// withDevice points Volumio's alsa output at another device.
func withDevice(conf, device string) string {
	return volumioDeviceLine.ReplaceAllString(conf, `${1}device${2}"`+device+`"`)
}

// TestReadingAnIdleBox reads the idle capture: MPD stopped, card 5 closed.
// The reading is whole and the verdict says nothing plays.
func TestReadingAnIdleBox(t *testing.T) {
	replies := volumioReplies(t)
	rig := newReadingRig(t, "idle", "Album/01.flac", flacRow(44100, 16), replies["status"])
	rig.mpd.current = replies["currentsong"]
	hs, token, id := rig.serve()
	code, got, raw := getReading(t, hs, token, id)
	if code != http.StatusOK || got.State != "whole" {
		t.Fatalf("status %d state %s\n%s", code, got.State, raw)
	}
	wantVerdict(t, got, raw, signalpath.Withheld, signalpath.NotPlaying)
	if got.Song != nil || got.MPD == nil || got.MPD.State != "stop" || got.MPD.SongID != nil || got.MPD.Elapsed != nil {
		t.Fatalf("song %+v mpd %+v", got.Song, got.MPD)
	}
	d := got.DAC
	if d == nil || d.Subdevice != nil || d.State != "" || d.Format != "" || d.Rate != nil || d.Channels != nil || d.Owner != nil {
		t.Fatalf("a closed card: %+v", d)
	}
}

// TestReadingJSONKeepsEveryKey: every key is present in every state, a
// list is [] and a map {}, and what was not read is null.
func TestReadingJSONKeepsEveryKey(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	hs, token, id := rig.serve()
	_, _, raw := getReading(t, hs, token, id)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"outputId", "takenAt", "ageMs", "state", "stateReasons", "mpd", "song", "dac", "verdict"} {
		if _, ok := top[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
	if string(top["stateReasons"]) != "[]" {
		t.Errorf("stateReasons %s", top["stateReasons"])
	}
	for _, want := range []string{
		`"reasons":[]`, `"volume":null`, `"mixrampDelay":null`, `"attributes":{"allowed_formats":"","dop":"0"}`,
		`"attributes":{}`, `"channels":null`, `"chain":["empty","plug","hw"]`,
	} {
		if !jsonContains(raw, want) {
			t.Errorf("body lacks %s:\n%s", want, raw)
		}
	}
	nested := map[string][]string{
		"mpd":     {"state", "songId", "elapsed", "audio", "replayGainMode", "volume", "xfade", "mixrampDb", "mixrampDelay", "outputs", "config"},
		"song":    {"uri", "format", "row"},
		"dac":     {"card", "id", "device", "subdevice", "state", "format", "rate", "channels", "owner", "mixer"},
		"verdict": {"value", "reasons"},
	}
	for obj, keys := range nested {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(top[obj], &m); err != nil {
			t.Fatalf("%s: %v", obj, err)
		}
		for _, k := range keys {
			if _, ok := m[k]; !ok {
				t.Errorf("%s missing %s", obj, k)
			}
		}
	}

	// MPD that cannot be dialled: mpd, song and the MPD half are null, the
	// lists stay lists.
	rig.cfg.Dial = func(context.Context, string, string) (outputSession, error) {
		return nil, errors.New("mpd: dial 127.0.0.1:6600: connection refused")
	}
	hs2, token2, id2 := rig.serve()
	_, got, raw2 := getReading(t, hs2, token2, id2)
	if got.State != "unknown" || !slices.Equal(got.StateReasons, []string{signalpath.MPDUnreachable}) {
		t.Fatalf("state %s %v", got.State, got.StateReasons)
	}
	for _, want := range []string{`"mpd":null`, `"song":null`, `"stateReasons":["mpd_unreachable"]`} {
		if !jsonContains(raw2, want) {
			t.Errorf("body lacks %s:\n%s", want, raw2)
		}
	}
	if got.DAC == nil || got.DAC.Format != "S16_LE" {
		t.Errorf("the DAC is still read when MPD is not: %+v", got.DAC)
	}
}

// TestTheVerdictReadsMPDsOwnSettings goes through the route for the
// settings the verdict reads from each of its sources: mpd.conf read at
// the reading, status, replay_gain_status, the live outputs attributes,
// and the chain from MPD's device to the card.
func TestTheVerdictReadsMPDsOwnSettings(t *testing.T) {
	c := piCaptures[0]
	cases := []struct {
		name   string
		change func(t *testing.T, r *readingRig)
		value  string
		codes  []string
	}{
		{"the box as it is", func(*testing.T, *readingRig) {}, signalpath.BitExact, nil},
		{"software volume in the file", func(t *testing.T, r *readingRig) {
			r.writeConf(withOutputSetting(volumioConf(t, r.music), "mixer_type", "software"))
		}, signalpath.Withheld, []string{signalpath.MPDSoftwareVolume}},
		{"the deprecated top-level software mixer", func(t *testing.T, r *readingRig) {
			r.writeConf("mixer_type \"software\"\n" + volumioConf(t, r.music))
		}, signalpath.Withheld, []string{signalpath.MPDSoftwareVolume}},
		{"the DAC's own mixer", func(t *testing.T, r *readingRig) {
			r.writeConf(withOutputSetting(volumioConf(t, r.music), "mixer_control", "Speaker"))
			r.mpd.restart(withPair(capturedStatus(t, c.name), "volume", "62"))
		}, signalpath.BitExact, nil},
		{"normalization", func(t *testing.T, r *readingRig) {
			r.writeConf(normalizationLine.ReplaceAllString(volumioConf(t, r.music), `volume_normalization "yes"`))
		}, signalpath.Withheld, []string{signalpath.MPDNormalization}},
		{"audio_output_format", func(t *testing.T, r *readingRig) {
			r.writeConf(volumioConf(t, r.music) + "audio_output_format \"48000:24:2\"\n")
		}, signalpath.Withheld, []string{signalpath.MPDFormatOverride}},
		{"filters on the output", func(t *testing.T, r *readingRig) {
			r.writeConf(withOutputSetting(volumioConf(t, r.music), "filters", "eq"))
		}, signalpath.Withheld, []string{signalpath.MPDFilters}},
		{"allowed_formats set live", func(t *testing.T, r *readingRig) {
			r.mpd.set(func(m *scriptedMPD) {
				m.outputs = replyOf("outputid", "0", "outputname", "alsa", "plugin", "alsa", "outputenabled", "1",
					"attribute", "allowed_formats=96000:16:*", "attribute", "dop=0")
			})
		}, signalpath.Withheld, []string{signalpath.MPDFormatOverride}},
		{"replay gain", func(t *testing.T, r *readingRig) {
			r.mpd.set(func(m *scriptedMPD) { m.gain = replyOf("replay_gain_mode", "album") })
		}, signalpath.Withheld, []string{signalpath.MPDReplayGain}},
		{"crossfade", func(t *testing.T, r *readingRig) {
			r.mpd.restart(withPair(capturedStatus(t, c.name), "xfade", "4"))
		}, signalpath.Withheld, []string{signalpath.MPDCrossfade}},
		{"mixramp", func(t *testing.T, r *readingRig) {
			r.mpd.restart(withPair(capturedStatus(t, c.name), "mixrampdelay", "2"))
		}, signalpath.Withheld, []string{signalpath.MPDMixRamp}},
		{"hw: straight to the card", func(t *testing.T, r *readingRig) {
			r.writeConf(withDevice(volumioConf(t, r.music), "hw:CARD=Audio,DEV=0"))
		}, signalpath.BitExact, nil},
		{"a dmix in front of the card", func(t *testing.T, r *readingRig) {
			etc := filepath.Join(filepath.Dir(r.procRoot), "etc", "asound.conf")
			b, err := os.ReadFile(etc)
			if err != nil {
				t.Fatal(err)
			}
			mixed := string(b) + "\npcm.mixed {\n    type dmix\n    slave.pcm \"volumioHw\"\n}\n"
			if err := os.WriteFile(etc, []byte(mixed), 0o644); err != nil {
				t.Fatal(err)
			}
			r.writeConf(withDevice(volumioConf(t, r.music), "mixed"))
		}, signalpath.Withheld, []string{signalpath.ALSAChainUnproven}},
		{"the output switched off live", func(t *testing.T, r *readingRig) {
			r.mpd.set(func(m *scriptedMPD) {
				m.outputs = replyOf("outputid", "0", "outputname", "alsa", "plugin", "alsa", "outputenabled", "0")
			})
		}, signalpath.Withheld, []string{signalpath.ALSAChainUnproven}},
		{"mpd.conf gone", func(t *testing.T, r *readingRig) {
			if err := os.Remove(r.confPath); err != nil {
				t.Fatal(err)
			}
		}, signalpath.Withheld, []string{signalpath.MPDConfigUnread, signalpath.ALSAChainUnproven}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
			// The id comes from GET /v1/outputs before the change, as the app
			// has it; a change to MPD's device does not change the card.
			hs, token, id := rig.serve()
			tc.change(t, rig)
			code, got, raw := getReading(t, hs, token, id)
			if code != http.StatusOK || got.State != "whole" {
				t.Fatalf("status %d state %s %v\n%s", code, got.State, got.StateReasons, raw)
			}
			wantVerdict(t, got, raw, tc.value, tc.codes...)
		})
	}
}

// TestTheReadingReadsMPDConfAgain: the player rewrites mpd.conf and
// restarts MPD when its settings change (Volumio's volume options do), so
// every reading reads the file again rather than keep the one it read
// before.
func TestTheReadingReadsMPDConfAgain(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	clock := &manualClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	rig.cfg.Now = clock.Now
	rig.cfg.AfterFunc = clock.AfterFunc
	hs, token, id := rig.serve()
	_, first, raw := getReading(t, hs, token, id)
	wantVerdict(t, first, raw, signalpath.BitExact)
	rig.writeConf(withOutputSetting(volumioConf(t, rig.music), "mixer_type", "software"))
	clock.Advance(outputCacheTTL)
	_, second, raw2 := getReading(t, hs, token, id)
	wantVerdict(t, second, raw2, signalpath.Withheld, signalpath.MPDSoftwareVolume)
	if second.MPD.Config.Output == nil || second.MPD.Config.Output.MixerType != "software" {
		t.Fatalf("config %+v", second.MPD.Config)
	}
}

// TestMPDConfIsReadUnderItsOwnVolume: the probe's root is /, which on
// Windows is the working directory's drive, and a test's temp directory
// sits on another one there (GitHub's runner: D: and C:). A path that
// names a volume is read under that volume's root; a Linux path names
// none and keeps the probe's.
func TestMPDConfIsReadUnderItsOwnVolume(t *testing.T) {
	if got := confRoot("/", "/etc/mpd.conf"); got != "/" {
		t.Fatalf("a Linux path: %q", got)
	}
	if runtime.GOOS != "windows" {
		return
	}
	for path, want := range map[string]string{
		`C:\Users\runner\AppData\Local\Temp\mpd.conf`: `C:\`,
		`\\server\share\mpd\mpd.conf`:                 `\\server\share\`,
	} {
		if got := confRoot("/", path); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

// doPRig is a DSD64 file an output with dop on sends as DoP: 24-bit PCM at
// 176.4 kHz on card 5 (synthetic, from the 96 kHz capture with its rate
// changed in a copy), MPD's audio dsd64:2.
func doPRig(t *testing.T, dop string) *readingRig {
	t.Helper()
	status := withPair(withPair(capturedStatus(t, "pcm-96k-24"), "audio", "dsd64:2"), "bitrate", "5645")
	rate, one, two, yes := 2822400.0, 1, 2, true
	rig := newReadingRig(t, "pcm-96k-24", "Album/06 DSD64.dff", OutputTrack{
		Codec: "DFF", SampleRate: &rate, BitsPerSample: &one, Channels: &two, IsDSD: &yes,
	}, status)
	hw := filepath.Join(rig.procRoot, "asound", "card5", "pcm0p", "sub0", "hw_params")
	b, err := os.ReadFile(hw)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(b), "rate: 96000 (96000/1)", "rate: 176400 (176400/1)", 1)
	if err := os.WriteFile(hw, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	rig.writeConf(withDevice(volumioConf(t, rig.music), "hw:CARD=Audio,DEV=0"))
	rig.mpd.outputs = replyOf("outputid", "0", "outputname", "alsa", "plugin", "alsa", "outputenabled", "1",
		"attribute", "allowed_formats=", "attribute", "dop="+dop)
	return rig
}

func TestReadingDoPIsBitExactDSDAsDoP(t *testing.T) {
	rig := doPRig(t, "1")
	hs, token, id := rig.serve()
	_, got, raw := getReading(t, hs, token, id)
	wantVerdict(t, got, raw, signalpath.BitExactDoP)
	if got.MPD.Config.Output == nil || !got.MPD.Config.Output.DoP {
		t.Fatalf("dop not read from the live attribute: %+v", got.MPD.Config.Output)
	}

	off := doPRig(t, "0")
	hs2, token2, id2 := off.serve()
	_, got2, raw2 := getReading(t, hs2, token2, id2)
	wantVerdict(t, got2, raw2, signalpath.Withheld, signalpath.DSDConvertedToPCM)
}

// TestReadingNamesTheBridgesOwnRow: a song MPD names by database path is
// the bridge's row when a root's prefix rule and os.SameFile say so, and a
// song it fetches from the loopback track route is the row its id names.
// A file changed since the scan, a URL elsewhere, a path outside every
// root and a lossy row are named as such.
func TestReadingNamesTheBridgesOwnRow(t *testing.T) {
	c := piCaptures[0]
	t.Run("by the loopback route", func(t *testing.T) {
		rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
		const trackID = "0123456789abcdef"
		rig.loop[trackID] = [2]string{c.file, filepath.Join(rig.root, filepath.FromSlash(c.file))}
		rig.mpd.current = replyOf("file", "http://127.0.0.1:7791/dlna/file/"+trackID, "Id", "13")
		hs, token, id := rig.serve()
		_, got, raw := getReading(t, hs, token, id)
		wantVerdict(t, got, raw, signalpath.BitExact)
		if got.Song.Row == nil || got.Song.Row.Via != "loopback" || got.Song.Row.Path != c.file || !got.Song.Row.Current {
			t.Fatalf("row %+v", got.Song.Row)
		}
	})
	notOurs := map[string]string{
		"the loopback route on another port": "http://127.0.0.1:9999/dlna/file/0123456789abcdef",
		"another host":                       "http://192.168.1.9:7790/dlna/file/0123456789abcdef",
		"a path outside every root":          "USB/Other/01.flac",
		"a path that climbs out of the root": "INTERNAL/Demo Library/../Demo Library/" + c.file,
		"an internet radio":                  "https://radio.example/stream.mp3",
	}
	for name, uri := range notOurs {
		t.Run(name, func(t *testing.T) {
			rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
			rig.loop["0123456789abcdef"] = [2]string{c.file, filepath.Join(rig.root, filepath.FromSlash(c.file))}
			rig.mpd.current = replyOf("file", uri, "Id", "13")
			hs, token, id := rig.serve()
			_, got, raw := getReading(t, hs, token, id)
			wantVerdict(t, got, raw, signalpath.Withheld, signalpath.SourceUnknown)
			if got.Song == nil || got.Song.Row != nil || got.Song.URI != uri {
				t.Fatalf("song %+v", got.Song)
			}
			// The bridge never asks its index for a path outside its roots.
			for _, p := range rig.askedPaths() {
				if strings.Contains(p, "..") || filepath.IsAbs(p) {
					t.Fatalf("looked a row up at %q", p)
				}
			}
		})
	}
	t.Run("a file changed since the scan", func(t *testing.T) {
		rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
		if err := os.WriteFile(filepath.Join(rig.root, filepath.FromSlash(c.file)), []byte("re-ripped, and longer than before"), 0o644); err != nil {
			t.Fatal(err)
		}
		hs, token, id := rig.serve()
		_, got, raw := getReading(t, hs, token, id)
		wantVerdict(t, got, raw, signalpath.Withheld, signalpath.SourceChanged)
		if got.Song.Row == nil || got.Song.Row.Current {
			t.Fatalf("row %+v", got.Song.Row)
		}
	})
	t.Run("a lossy row", func(t *testing.T) {
		rate, sixteen := 44100.0, 16
		rig := newReadingRig(t, c.name, "Album/01.m4a", OutputTrack{Codec: "AAC", SampleRate: &rate, BitsPerSample: &sixteen}, capturedStatus(t, c.name))
		hs, token, id := rig.serve()
		_, got, raw := getReading(t, hs, token, id)
		wantVerdict(t, got, raw, signalpath.Withheld, signalpath.SourceLossy)
	})
}

// TestReadingSettles: MPD's status read before the /proc read and the one
// after disagree, or the song has only just started, and the reading
// settles instead of judging.
func TestReadingSettles(t *testing.T) {
	c := piCaptures[0]
	captured := capturedStatus(t, c.name)
	at := func(songID, elapsed string) mpd.Reply {
		return withPair(withPair(captured, "songid", songID), "elapsed", elapsed)
	}
	cases := []struct {
		name     string
		statuses []mpd.Reply
		current  string
		codes    []string
	}{
		{"a song change mid-read", []mpd.Reply{at("13", "181.4"), at("14", "0.010")}, "13", []string{signalpath.SongChanged}},
		// Two changes between the status reads, back to the song the first
		// one named, leave only currentsong to say so.
		{"currentsong on another song", []mpd.Reply{at("13", "4.722"), at("13", "4.725")}, "14", []string{signalpath.SongChanged}},
		{"a song that has only started", []mpd.Reply{at("13", "0.20"), at("13", "0.21")}, "13", []string{signalpath.SongStarting}},
		{"a seek mid-read", []mpd.Reply{at("13", "4.722"), at("13", "64.722")}, "13", []string{signalpath.Seeked}},
		{"a pause mid-read", []mpd.Reply{at("13", "4.722"), withPair(at("13", "4.722"), "state", "pause")}, "13", []string{signalpath.StateChanged}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := newReadingRig(t, c.name, c.file, c.track, captured)
			hs, token, id := rig.serve()
			rig.mpd.restart(tc.statuses...)
			rig.mpd.set(func(m *scriptedMPD) { m.current = withPair(m.current, "Id", tc.current) })
			_, got, raw := getReading(t, hs, token, id)
			if got.State != "settling" || !slices.Equal(got.StateReasons, signalpath.Sorted(tc.codes)) {
				t.Fatalf("state %s %v\n%s", got.State, got.StateReasons, raw)
			}
			wantVerdict(t, got, raw, signalpath.Withheld, tc.codes...)
			if got.DAC == nil || got.MPD == nil {
				t.Fatalf("a settling reading still reports what it read:\n%s", raw)
			}
		})
	}
}

// TestTheReadingIsReadThroughForHalfASecond: one reading serves every call
// for 500 ms (ageMs says how old), then the next call reads again; no
// background sampler reads in between.
func TestTheReadingIsReadThroughForHalfASecond(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	clock := &manualClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	rig.cfg.Now = clock.Now
	rig.cfg.AfterFunc = clock.AfterFunc
	hs, token := newOutputsHTTP(t, "fp", rig.cfg)
	id := firstOutputID(t, hs, token)
	statusesBefore, readsBefore := rig.mpd.statusCalls(), rig.reads.Load()

	_, first, raw := getReading(t, hs, token, id)
	if first.AgeMs != 0 || !first.TakenAt.Equal(clock.now) {
		t.Fatalf("first: age %d taken %s\n%s", first.AgeMs, first.TakenAt, raw)
	}
	if n := rig.mpd.statusCalls() - statusesBefore; n != 2 {
		t.Fatalf("a reading reads status %d times, want before and after the /proc read", n)
	}
	if n := rig.reads.Load() - readsBefore; n != 1 {
		t.Fatalf("/proc read %d times", n)
	}
	clock.Advance(outputCacheTTL - time.Millisecond)
	_, second, _ := getReading(t, hs, token, id)
	if rig.mpd.statusCalls()-statusesBefore != 2 || rig.reads.Load()-readsBefore != 1 {
		t.Fatalf("a call inside 500ms read again")
	}
	if second.AgeMs != 499 || !second.TakenAt.Equal(first.TakenAt) {
		t.Fatalf("second: age %d taken %s", second.AgeMs, second.TakenAt)
	}
	clock.Advance(time.Millisecond)
	_, third, _ := getReading(t, hs, token, id)
	if rig.mpd.statusCalls()-statusesBefore != 4 || rig.reads.Load()-readsBefore != 2 {
		t.Fatalf("at 500ms the reading was not taken again")
	}
	if third.AgeMs != 0 || !third.TakenAt.After(first.TakenAt) {
		t.Fatalf("third: age %d taken %s", third.AgeMs, third.TakenAt)
	}
	clock.Advance(10 * outputCacheTTL)
	if rig.mpd.statusCalls()-statusesBefore != 4 {
		t.Fatalf("something read MPD with no call")
	}
}

// TestTheReadingsConnectionIsLazyAndShared: no session until the first
// reading; GET /v1/outputs and the reading share it; it closes about 60 s
// after the last call, never while a reading is taken; the next call dials
// again.
func TestTheReadingsConnectionIsLazyAndShared(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	clock := &manualClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	rig.cfg.Now = clock.Now
	rig.cfg.AfterFunc = clock.AfterFunc
	store := outputsToken(t)
	raw, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(&config.Config{
		ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789",
		ScanIntervalSec: 3600, LibraryName: "T", LibraryRoots: []string{rig.root},
	}, store, nil, "fp").WithLocalOutputs(rig.cfg)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	health := outputsGet(t, hs, "", "/v1/health")
	health.Body.Close()
	if rig.dials.Load() != 0 {
		t.Fatalf("the gate dialled %d", rig.dials.Load())
	}
	id := firstOutputID(t, hs, raw)
	_, got, body := getReading(t, hs, raw, id)
	if got.State != "whole" {
		t.Fatalf("%s", body)
	}
	if rig.dials.Load() != 1 {
		t.Fatalf("outputs and the reading dialled %d sessions, want one", rig.dials.Load())
	}
	clock.Advance(outputIdle)
	if rig.mpd.closed.Load() != 1 {
		t.Fatalf("idle close: closed %d", rig.mpd.closed.Load())
	}
	clock.Advance(outputCacheTTL)
	if _, again, _ := getReading(t, hs, raw, id); again.State != "whole" || rig.dials.Load() != 2 {
		t.Fatalf("after the idle close: state %s dials %d", again.State, rig.dials.Load())
	}

	// A reading in flight keeps the session past the idle minute.
	hold := make(chan struct{})
	rig.mpd.mu.Lock()
	rig.mpd.hold = map[string]chan struct{}{"currentsong": hold}
	rig.mpd.mu.Unlock()
	srv.localOutputs.limits.command = time.Minute
	srv.localOutputs.limits.build = time.Minute
	clock.Advance(outputCacheTTL)
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/outputs/"+id+"/reading", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	waitForCall(t, rig.mpd, "currentsong", 3)
	closedBefore := rig.mpd.closed.Load()
	clock.Advance(outputIdle)
	if rig.mpd.closed.Load() != closedBefore {
		t.Fatalf("the idle close ran during a reading")
	}
	close(hold)
	<-done
	clock.Advance(outputIdle)
	if rig.mpd.closed.Load() != closedBefore+1 {
		t.Fatalf("no idle close after the reading: closed %d", rig.mpd.closed.Load())
	}
}

// waitForCall returns once the session has been sent name n times. The
// session's own log is the event: a command is recorded when it is sent.
func waitForCall(t *testing.T, m *scriptedMPD, name string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		for _, c := range m.commands() {
			if c == name {
				count++
			}
		}
		if count >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s was not sent %d times: %v", name, n, m.commands())
}

// TestAProcReadPastItsDeadlineIsAnHonestUnknown: the /proc read has a
// deadline, a reading past it is unknown with proc_timeout and still
// reports MPD, and a read that is still running is not joined by another:
// the next reading says so at once rather than pile up readers.
func TestAProcReadPastItsDeadlineIsAnHonestUnknown(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	clock := &manualClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	rig.cfg.Now = clock.Now
	rig.cfg.AfterFunc = clock.AfterFunc
	srv, hs, token := newReadingServer(t, rig.cfg)
	id := firstOutputID(t, hs, token)
	stuck := make(chan struct{})
	var blocked atomic.Int32
	read := rig.cards
	rig.cards = func() (alsa.Snapshot, error) {
		blocked.Add(1)
		<-stuck
		return read()
	}
	srv.localOutputs.limits.proc = 20 * time.Millisecond
	// The whole reading's deadline is far off, so only the /proc read's own
	// deadline can end this one in time.
	srv.localOutputs.limits.build = 30 * time.Second
	t.Cleanup(func() { close(stuck) })

	start := time.Now()
	_, got, raw := getReading(t, hs, token, id)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the reading waited %s for a /proc read with a 20ms deadline", took)
	}
	if got.State != "unknown" || !slices.Equal(got.StateReasons, []string{signalpath.ProcTimeout}) {
		t.Fatalf("state %s %v\n%s", got.State, got.StateReasons, raw)
	}
	wantVerdict(t, got, raw, signalpath.Withheld, signalpath.ProcTimeout)
	if got.DAC != nil || got.MPD == nil || got.MPD.State != "play" {
		t.Fatalf("dac %+v mpd %+v", got.DAC, got.MPD)
	}
	clock.Advance(outputCacheTTL)
	_, again, _ := getReading(t, hs, token, id)
	if !slices.Equal(again.StateReasons, []string{signalpath.ProcTimeout}) || blocked.Load() != 1 {
		t.Fatalf("a second reading started another /proc read: %v, %d reads", again.StateReasons, blocked.Load())
	}
}

// TestAnMPDCommandPastItsDeadlineIsAnHonestUnknown: every MPD command has
// a deadline; past it the reading is unknown with mpd_timeout, still
// reports the card, and the connection the command hangs on is dropped, so
// the next reading dials a new one rather than read a late reply.
func TestAnMPDCommandPastItsDeadlineIsAnHonestUnknown(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	status := "repeat: 0\nrandom: 0\nsingle: 0\nconsume: 0\nplaylist: 26\nplaylistlength: 1\nmixrampdb: 0\nstate: play\n" +
		"song: 0\nsongid: 13\nelapsed: 4.722\naudio: 44100:16:2\nOK\n"
	fake := startTCPMPD(t, "", map[string]string{
		"status":             status,
		"replay_gain_status": "replay_gain_mode: off\nOK\n",
		"outputs":            "outputid: 0\noutputname: alsa\nplugin: alsa\noutputenabled: 1\nattribute: dop=0\nOK\n",
		"find":               "OK\n",
	}, map[string]bool{"currentsong": true})
	rig.cfg.Dial = nil
	inspect := rig.cfg.Inspect
	rig.cfg.Inspect = func(ctx context.Context, opt mpd.Options) mpd.Report {
		rep := inspect(ctx, opt)
		rep.MPD.Endpoint = fake.addr()
		return rep
	}
	clock := &manualClock{now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	rig.cfg.Now = clock.Now
	rig.cfg.AfterFunc = clock.AfterFunc
	srv, hs, token := newReadingServer(t, rig.cfg)
	srv.localOutputs.limits.command = 100 * time.Millisecond
	// Only the command's own deadline can end this reading in time.
	srv.localOutputs.limits.build = 30 * time.Second
	id := firstOutputID(t, hs, token)
	start := time.Now()
	_, got, raw := getReading(t, hs, token, id)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("the reading waited %s for a command with a 100ms deadline", took)
	}
	if got.State != "unknown" || !slices.Equal(got.StateReasons, []string{signalpath.MPDTimeout}) {
		t.Fatalf("state %s %v\n%s", got.State, got.StateReasons, raw)
	}
	if got.DAC == nil || got.DAC.Format != "S16_LE" || got.MPD != nil {
		t.Fatalf("dac %+v mpd %+v", got.DAC, got.MPD)
	}
	if fake.accepted() != 1 {
		t.Fatalf("connections %d before the second reading", fake.accepted())
	}
	clock.Advance(outputCacheTTL)
	getReading(t, hs, token, id)
	if fake.accepted() != 2 {
		t.Fatalf("the hung connection was reused: %d connections", fake.accepted())
	}
	for _, line := range fake.lines() {
		name, _, _ := strings.Cut(line, " ")
		if !slices.Contains([]string{"status", "currentsong", "replay_gain_status", "outputs", "find"}, name) {
			t.Fatalf("the bridge sent %q", line)
		}
	}
}

// TestAForeignOwnerIsNotMPDs: a process other than mpd holding card 5, and
// an owner /proc does not name, are said as such, with no verdict.
func TestAForeignOwnerIsNotMPDs(t *testing.T) {
	c := piCaptures[0]
	t.Run("aplay", func(t *testing.T) {
		rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
		if err := os.RemoveAll(filepath.Join(rig.procRoot, "6551")); err != nil {
			t.Fatal(err)
		}
		status := "Name:\taplay\nUmask:\t0022\nState:\tS (sleeping)\nTgid:\t6584\nPid:\t6584\n"
		if err := os.WriteFile(filepath.Join(rig.procRoot, "6584", "status"), []byte(status), 0o644); err != nil {
			t.Fatal(err)
		}
		hs, token, id := rig.serve()
		_, got, raw := getReading(t, hs, token, id)
		wantVerdict(t, got, raw, signalpath.Withheld, signalpath.OwnerNotMPD)
		if got.DAC.Owner == nil || str(got.DAC.Owner.Process) != "aplay" {
			t.Fatalf("owner %+v", got.DAC.Owner)
		}
	})
	t.Run("an owner /proc does not name", func(t *testing.T) {
		rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
		for _, pid := range []string{"6551", "6584"} {
			if err := os.RemoveAll(filepath.Join(rig.procRoot, pid)); err != nil {
				t.Fatal(err)
			}
		}
		hs, token, id := rig.serve()
		_, got, raw := getReading(t, hs, token, id)
		wantVerdict(t, got, raw, signalpath.Withheld, signalpath.OwnerUnknown)
		if got.DAC.Owner == nil || got.DAC.Owner.TID != 6584 || got.DAC.Owner.Process != nil {
			t.Fatalf("owner %+v", got.DAC.Owner)
		}
	})
}

// TestTheMPDPasswordReachesNoReadingAndNoLog: the password the bridge
// sends (output.password) and the ones mpd.conf holds are in no reading
// and no log line, even when MPD's own replies carry them, and a password
// MPD refuses is mpd_refused.
func TestTheMPDPasswordReachesNoReadingAndNoLog(t *testing.T) {
	const sent, kept = "bridge-sent-secret", "conf-kept-secret"
	rec := loggingtest.Record(t)
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	rig.writeConf("password \"" + kept + "@read,add\"\n" + volumioConf(t, rig.music))
	status := "state: play\nsongid: 13\nelapsed: 4.722\naudio: 44100:16:2\nmixrampdb: 0\nOK\n"
	fake := startTCPMPD(t, sent, map[string]string{
		"status":             status,
		"currentsong":        "file: Album/" + sent + " " + kept + ".flac\nId: 13\nOK\n",
		"replay_gain_status": "replay_gain_mode: off\nOK\n",
		"outputs":            "outputid: 0\noutputname: alsa\nplugin: alsa\noutputenabled: 1\nattribute: name=" + sent + "\nOK\n",
	}, nil)
	rig.cfg.Dial = nil
	inspect := rig.cfg.Inspect
	rig.cfg.Inspect = func(ctx context.Context, opt mpd.Options) mpd.Report {
		rep := inspect(ctx, opt)
		rep.MPD.Endpoint = fake.addr()
		return rep
	}
	password := sent
	rig.cfg.Output = func() config.OutputConfig {
		return config.OutputConfig{MPD: "on", Password: password}
	}
	hs, token := newOutputsHTTP(t, "fp", rig.cfg)
	id := firstOutputID(t, hs, token)
	_, got, raw := getReading(t, hs, token, id)
	if got.State != "whole" || got.Song == nil {
		t.Fatalf("state %s %v\n%s", got.State, got.StateReasons, raw)
	}
	for _, secret := range []string{sent, kept} {
		if jsonContains(raw, secret) {
			t.Fatalf("the reading carries %q:\n%s", secret, raw)
		}
		for _, line := range rec.All() {
			if strings.Contains(line, secret) {
				t.Fatalf("a log line carries %q: %s", secret, line)
			}
		}
	}
	if !strings.Contains(got.Song.URI, "[redacted]") {
		t.Fatalf("uri %q", got.Song.URI)
	}

	const wrong = "a-wrong-secret-value"
	password = wrong
	hs2, token2 := newOutputsHTTP(t, "fp", rig.cfg)
	id2 := firstOutputID(t, hs2, token2)
	_, refused, raw2 := getReading(t, hs2, token2, id2)
	if refused.State != "unknown" || !slices.Equal(refused.StateReasons, []string{signalpath.MPDRefused}) {
		t.Fatalf("state %s %v\n%s", refused.State, refused.StateReasons, raw2)
	}
	if jsonContains(raw2, wrong) {
		t.Fatalf("the reading carries the refused password:\n%s", raw2)
	}
	for _, line := range rec.All() {
		if strings.Contains(line, wrong) {
			t.Fatalf("a log line carries the refused password: %s", line)
		}
	}
}

// TestTheReadingSendsOnlyReads holds the reading to its commands, in
// order: status, then currentsong, replay_gain_status and outputs, the
// /proc read, then status again. Nothing is written, update least of all.
func TestTheReadingSendsOnlyReads(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	hs, token := newOutputsHTTP(t, "fp", rig.cfg)
	id := firstOutputID(t, hs, token)
	before := len(rig.mpd.commands())
	getReading(t, hs, token, id)
	got := rig.mpd.commands()[before:]
	want := []string{"status", "currentsong", "replay_gain_status", "outputs", "status"}
	if !slices.Equal(got, want) {
		t.Fatalf("commands %v, want %v", got, want)
	}
}

// TestOneMPDCommandAtATime: the outputs body and the reading share one
// session, and MPD answers one command at a time on it. A reading behind
// a find that hangs waits for its turn no longer than its own deadline.
func TestOneMPDCommandAtATime(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	hold := make(chan struct{})
	rig.mpd.hold = map[string]chan struct{}{"find": hold}
	store := outputsToken(t)
	raw, _, err := store.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(&config.Config{
		ListenAddress: ":7788", AdminAddress: "127.0.0.1:7789",
		ScanIntervalSec: 3600, LibraryName: "T", LibraryRoots: []string{rig.root},
	}, store, nil, "fp").WithLocalOutputs(rig.cfg)
	srv.localOutputs.limits.command = 50 * time.Millisecond
	done := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/outputs", nil)
		req.Header.Set("Authorization", "Bearer "+raw)
		srv.Handler().ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()
	waitForCall(t, rig.mpd, "find", 1)
	snap, _ := rig.cards()
	picks := dacsFrom(snap, rig.cfg.Inspect(context.Background(), mpd.Options{}))
	id := outputID(OutputIdentity(""), "127.0.0.1:6600", picks[0].dac.ID, picks[0].dac.USBID)
	got, found, err := srv.localOutputs.Reading(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("found %v err %v", found, err)
	}
	if got.State != "unknown" || !slices.Equal(got.StateReasons, []string{signalpath.MPDTimeout}) {
		t.Fatalf("state %s %v", got.State, got.StateReasons)
	}
	close(hold)
	<-done
	rig.mpd.mu.Lock()
	most := rig.mpd.maxInFlight
	rig.mpd.mu.Unlock()
	if most != 1 {
		t.Fatalf("%d commands ran at once on one session", most)
	}
}

// TestAReadingNamesItsOutputByID: the id is the one GET /v1/outputs gives;
// another id is 404 output_not_found, a card that is not there is unknown
// with no_card, a /proc that cannot be read is an honest unknown for any
// id the cards could have named, and the gate is the outputs route's.
func TestAReadingNamesItsOutputByID(t *testing.T) {
	c := piCaptures[0]
	rig := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	hs, token, id := rig.serve()
	other := strings.Repeat("ab", 32)
	for _, bad := range []string{other, "abc", strings.ToUpper(id), id + "0"} {
		code, _, raw := getReading(t, hs, token, bad)
		if code != http.StatusNotFound || !jsonContains(raw, `"output_not_found"`) {
			t.Fatalf("%s: status %d %s", bad, code, raw)
		}
	}
	bare := outputsGet(t, hs, "", "/v1/outputs/"+id+"/reading")
	bare.Body.Close()
	if bare.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", bare.StatusCode)
	}

	// A box with no card, and one with no /proc/asound at all: no card,
	// not a /proc that could not be read.
	for name, read := range map[string]func() (alsa.Snapshot, error){
		"no card":         func() (alsa.Snapshot, error) { return alsa.Snapshot{}, nil },
		"no /proc/asound": func() (alsa.Snapshot, error) { return alsa.Snapshot{}, fs.ErrNotExist },
	} {
		noCards := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
		noCards.cards = read
		hs2, token2, id2 := noCards.serve()
		_, got, raw := getReading(t, hs2, token2, id2)
		if got.State != "unknown" || !slices.Equal(got.StateReasons, []string{signalpath.NoCard}) || got.DAC != nil {
			t.Fatalf("%s: %s", name, raw)
		}
	}

	unreadable := newReadingRig(t, c.name, c.file, c.track, capturedStatus(t, c.name))
	hs3, token3, _ := unreadable.serve()
	unreadable.cards = func() (alsa.Snapshot, error) { return alsa.Snapshot{}, fs.ErrPermission }
	_, got3, raw3 := getReading(t, hs3, token3, id)
	if got3.State != "unknown" || !slices.Equal(got3.StateReasons, []string{signalpath.ProcUnreadable}) {
		t.Fatalf("unreadable /proc: %s", raw3)
	}
	// Only an id of the right form is answered when the cards cannot say.
	for _, bad := range []string{"abc", strings.ToUpper(id)} {
		if code, _, raw := getReading(t, hs3, token3, bad); code != http.StatusNotFound || !jsonContains(raw, `"output_not_found"`) {
			t.Fatalf("%s with /proc unreadable: status %d %s", bad, code, raw)
		}
	}

	for _, gate := range []struct{ goos, mode string }{{"linux", "off"}, {"darwin", "on"}} {
		cfg := rig.cfg
		cfg.GOOS = gate.goos
		cfg.Output = func() config.OutputConfig { return config.OutputConfig{MPD: gate.mode} }
		hsOff, tokenOff := newOutputsHTTP(t, "fp", cfg)
		code, _, raw := getReading(t, hsOff, tokenOff, id)
		if code != http.StatusNotFound || !jsonContains(raw, `"local_outputs_not_supported"`) {
			t.Fatalf("%v: %d %s", gate, code, raw)
		}
	}
}

// TestHealthAdvertisesOutputReadingWithLocalOutputs: the reading's flag is
// on exactly when the outputs route is.
func TestHealthAdvertisesOutputReadingWithLocalOutputs(t *testing.T) {
	for _, tc := range []struct {
		goos, mode string
		want       bool
	}{{"linux", "on", true}, {"linux", "off", false}, {"darwin", "on", false}} {
		cfg := quietOutputs(tc.goos, tc.mode)
		hs, _ := newOutputsHTTP(t, "fp", cfg)
		resp := outputsGet(t, hs, "", "/v1/health")
		var h HealthResponse
		if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if slices.Contains(h.Features, "outputReading") != tc.want || slices.Contains(h.Features, "localOutputs") != tc.want {
			t.Errorf("%s %s: features %v, want outputReading %v", tc.goos, tc.mode, h.Features, tc.want)
		}
	}
}

// tcpMPD is an MPD on a loopback socket that speaks the protocol, for the
// tests that need mpd.Client itself: a command that never answers, the
// password, and the lines the bridge writes.
type tcpMPD struct {
	ln       net.Listener
	password string
	replies  map[string]string
	stall    map[string]bool
	mu       sync.Mutex
	got      []string
	conns    []net.Conn
}

func startTCPMPD(t *testing.T, password string, replies map[string]string, stall map[string]bool) *tcpMPD {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &tcpMPD{ln: ln, password: password, replies: replies, stall: stall}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			m.mu.Lock()
			m.conns = append(m.conns, conn)
			m.mu.Unlock()
			go m.serve(conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		m.mu.Lock()
		for _, c := range m.conns {
			_ = c.Close()
		}
		m.mu.Unlock()
	})
	return m
}

func (m *tcpMPD) addr() string { return m.ln.Addr().String() }

func (m *tcpMPD) accepted() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.conns)
}

func (m *tcpMPD) lines() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.got...)
}

func (m *tcpMPD) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("OK MPD 0.24.0\n")); err != nil {
		return
	}
	br := bufio.NewReader(conn)
	authed := m.password == ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		name, arg, _ := strings.Cut(line, " ")
		if name != "password" {
			m.mu.Lock()
			m.got = append(m.got, line)
			m.mu.Unlock()
		}
		var reply string
		switch {
		case name == "password" && strings.Trim(arg, `"`) == m.password:
			authed = true
			reply = "OK\n"
		case name == "password":
			reply = "ACK [3@0] {password} incorrect password\n"
		case !authed:
			reply = "ACK [4@0] {" + name + "} you don't have permission for \"" + name + "\"\n"
		case m.stall[name]:
			// Never answer: the client's deadline is what ends it.
			_, _ = br.ReadString(0)
			return
		default:
			var ok bool
			reply, ok = m.replies[name]
			if !ok {
				reply = "ACK [5@0] {} unknown command \"" + name + "\"\n"
			}
		}
		if _, err := conn.Write([]byte(reply)); err != nil {
			return
		}
	}
}
