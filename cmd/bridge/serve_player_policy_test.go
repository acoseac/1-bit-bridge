package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// Serve's and output play's player since the cutover (B728 B7): the backend
// each builds decides DSD by the effective policy over the player's DSD
// state and the box, and reads no approval record; the legacy keys reach it
// only through the migration (B6).

// goBarDSD lays out in trees what the effective policy reads of the GO bar
// beside its USB strings and firmware (goBarIdentityFiles): descriptors
// with no gain stage on the playback path, the kernel's table of its
// controls holding none, and its stream file listing S32_LE with 32 bits
// and DSD_U32_BE at DSD64's, DSD128's and DSD256's frame rates.
func goBarDSD(t *testing.T, trees outputTrees) {
	t.Helper()
	goBarIdentityFiles(t, trees)
	stream := "Playback:\n  Status: Stop\n  Interface 1\n    Altset 1\n    Format: S32_LE\n    Channels: 2\n" +
		"    Rates: 44100, 48000, 88200, 96000, 176400, 192000, 352800, 384000\n    Bits: 32\n" +
		"  Interface 1\n    Altset 2\n    Format: DSD_U32_BE\n    Channels: 2\n    Rates: 88200, 176400, 352800\n    Bits: 32\n"
	for path, data := range map[string][]byte{
		filepath.Join(trees.sysfs, qualifyGoBar, "descriptors"):  playbacktest.CleanDescriptors(),
		filepath.Join(trees.proc, "asound", "card5", "stream0"):  []byte(stream),
		filepath.Join(trees.proc, "asound", "card5", "usbmixer"): []byte("USB Mixer: usb_id=0x20b13009, ctrlif=0, ctlerr=0\nCard: iFi GO bar\n"),
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// wiredPolicyOf is the effective DSD policy cfg, a backend's config serve or
// output play wired, decides DSD by: it fails the test when the wiring
// configured the harness's approval admission, or no policy.
func wiredPolicyOf(t *testing.T, cfg playback.Config) *playback.DSDPolicy {
	t.Helper()
	if cfg.Authorization != nil {
		t.Fatalf("the wiring configured an approval admission (%+v): production reads no record since the cutover", *cfg.Authorization)
	}
	if cfg.DSD == nil {
		t.Fatal("the wiring configured no effective DSD policy")
	}
	return cfg.DSD
}

// policyBackendOf is a backend built on the policy cfg was wired with, as
// serve's and output play's is, but for its card's controls, read through a
// reader standing in for the box, which has no control node, and its opens,
// which opens counts and refuses; its set computed once.
func policyBackendOf(t *testing.T, cfg playback.Config, content playback.Resolver, opens *dopOpens) *playback.Backend {
	t.Helper()
	pol := *wiredPolicyOf(t, cfg)
	box, ok := pol.Box.(playback.HostBox)
	if !ok {
		t.Fatalf("the policy reads the box through %T, want the box's trees", pol.Box)
	}
	box.Controls = alsactl.Reader{Open: func(int) (alsactl.Device, error) { return qualifyNoControls{}, nil }}
	pol.Box = box
	b, err := playback.New(playback.Config{DSD: &pol, Locator: cfg.Locator, Resolver: content, OpenSink: opens.openSink, OpenNative: opens.openNative})
	if err != nil {
		t.Fatal(err)
	}
	b.RefreshDSD(true)
	return b
}

// unplayable is err's refusal word, "" for none.
func unplayable(err error) engine.Reason {
	var u *engine.Unplayable
	if errors.As(err, &u) {
		return u.Reason
	}
	return ""
}

// ratesPublished are the rates p publishes a tuple at, each with its
// transport ("" native).
func ratesPublished(p playback.PlayerPolicy) string {
	var b bytes.Buffer
	for _, tu := range p.Tuples {
		fmt.Fprintf(&b, "%d/%q ", tu.Tuple.BitRate, tu.Tuple.Transport)
	}
	return b.String()
}

// TestServeReadsNoApprovalRecord (B728 B7, the cutover; binding note 11):
// once the migration took an owner's record, serve's player decides DSD by
// the effective policy alone: the backend serve wires configures no approval
// admission, and the set its policy publishes, native at the rates the record
// approved, stays the same with the record and its reports gone from the data
// directory: nothing the backend admits by reads them.
func TestServeReadsNoApprovalRecord(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	trees := goBarTrees(t)
	goBarDSD(t, trees)
	standIn := newPlayerStandIn()
	var record qualification.Approval
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer + "    dsdMode: native\n    dsdQualification: approved.json\n", standIn: standIn,
		before: func(dataDir string) {
			record = legacyRecordIn(t, dataDir, goBarLiveIdentity(t, trees), qualification.Version, nativeAt(dsdstate.DSD64), nativeAt(dsdstate.DSD128))
		},
		with: func(o *serveOpts) { o.playerTrees = trees }})
	eng := ps.runningEngine(t)
	availableOutput(t, eng)
	standIn.mu.Lock()
	wired := standIn.cfg
	standIn.mu.Unlock()
	if st := wiredPolicyOf(t, wired).Store.State(); st.MigratedFrom == nil || st.MigratedFrom.RecordSHA256 == "" {
		t.Fatalf("the state serve's player holds %+v: the record was not migrated", st.MigratedFrom)
	}
	before := policyBackendOf(t, wired, dopContents{}, &dopOpens{}).Policy()
	if got := ratesPublished(before); got != `2822400/"" 5644800/"" ` || before.Reason != "" {
		t.Fatalf("the migrated owner's set publishes %s(reason %q), want native at DSD64 and DSD128, the record's rates", got, before.Reason)
	}
	// The record and the reports it names, and nothing else: serve's own
	// files (its DSD state, its queue) stay where they are (round 2).
	gone := []string{"approved.json"}
	for _, p := range record.Body.Policies {
		gone = append(gone, p.Matrix.Path, p.Lifecycle.Path)
	}
	for _, name := range gone {
		if err := os.Remove(filepath.Join(ps.dataDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(ps.dataDir, "approved.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the record is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ps.dataDir, dsdstate.FileName)); err != nil {
		t.Fatalf("the player's DSD state after the record went: %v; the test takes away the record and its reports alone", err)
	}
	after := policyBackendOf(t, wired, dopContents{}, &dopOpens{}).Policy()
	if got := ratesPublished(after); got != ratesPublished(before) || after.Reason != before.Reason {
		t.Errorf("with the record gone the set publishes %s(reason %q), want %s as before: production read the record", got, after.Reason, ratesPublished(before))
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("exit %d; stderr=%s", code, ps.stderr.String())
	}
}

// TestOutputPlayKeepsItsOwnDSDState (B728 B7; binding note 9): bridge
// output play decides DSD by the effective policy over a DSD state of its
// own, in its own state directory, which it migrates from the legacy keys
// itself, reading the owner's record where the keys name it, in serve's data
// directory; it never opens serve's DSD state. Beside a running serve, which
// holds its data directory and a state of its own saying DSD is off, output
// play's state holds the record's rows and serve's file is as it was; beside
// a serve that never migrated, serve's data directory still holds no state.
func TestOutputPlayKeepsItsOwnDSDState(t *testing.T) {
	persistentMedium(t)
	for _, serveRuns := range []bool{true, false} {
		t.Run(map[bool]string{true: "beside a running serve", false: "beside a serve that never migrated"}[serveRuns], func(t *testing.T) {
			c := newPlayCase(t, goBarPlayer+"    dsdMode: native\n    dsdQualification: approved.json\n")
			goBarDSD(c.t, c.trees)
			legacyRecordIn(t, c.dataDir, goBarLiveIdentity(t, c.trees), qualification.Version, nativeAt(dsdstate.DSD64))
			servesState := filepath.Join(c.dataDir, dsdstate.FileName)
			var served []byte
			if serveRuns {
				served = []byte(`{"schemaVersion":1,"defaultMode":"off"}` + "\n")
				if err := os.WriteFile(servesState, served, 0o600); err != nil {
					t.Fatal(err)
				}
				holdDataDirLock(t, c.dataDir)
			}
			wired, quit := playWired(t, c)
			st := wiredPolicyOf(t, wired).Store.State()
			quit()
			if st.MigratedFrom == nil || st.MigratedFrom.RecordSHA256 == "" || st.DefaultMode != dsdstate.ModeAuto {
				t.Errorf("output play's state %+v (default %q): want its own migration of the keys and the record", st.MigratedFrom, st.DefaultMode)
			}
			if d, ok := st.DAC(goBarKey(t)); !ok || d.Choice == nil || d.Choice.Mode != dsdstate.ModeNative {
				t.Errorf("output play's state for the GO bar %+v, want the record's native choice", d)
			}
			got, err := os.ReadFile(servesState)
			switch {
			case serveRuns && (err != nil || !bytes.Equal(got, served)):
				t.Errorf("serve's state after output play: %q, %v; want it as it was", got, err)
			case !serveRuns && !errors.Is(err, os.ErrNotExist):
				t.Errorf("output play left serve's data directory a DSD state: %q, %v", got, err)
			}
		})
	}
}

// TestOutputsPublishTheEffectiveSet (B728 B7, within today's wire shape):
// GET /v1/outputs' dsdPlayback is the effective set the backend admits by:
// a tuple for each rate that plays, DoP's carrying "transport": "dop" and a
// native one no transport member; the model and firmware of the unit the set
// was computed for; no reason while a rate plays, and the lowest rate's word
// when none does; and neither qualifiedOn nor qualificationRevision, since no
// record is read.
func TestOutputsPublishTheEffectiveSet(t *testing.T) {
	for _, tc := range []struct {
		name   string
		st     dsdstate.State
		want   string
		reason string
	}{
		{"native", playbacktest.ChoiceState(dsdstate.ModeNative), `2822400/"" 5644800/"" 11289600/"" `, ""},
		{"dop", playbacktest.ChoiceState(dsdstate.ModeDoP), `2822400/"dop" 5644800/"dop" 11289600/"dop" `, ""},
		{"off", playbacktest.ChoiceState(dsdstate.ModeOff), ``, "dsd_off_by_choice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := playbacktest.Policy(5, playbacktest.NewBox(5), playbacktest.NewMemStore(tc.st))
			b, err := playback.New(playback.Config{DSD: p, Locator: refusedNativeLocator{new(int)}, Resolver: dopContents{}})
			if err != nil {
				t.Fatal(err)
			}
			b.RefreshDSD(true)
			if got := ratesPublished(b.Policy()); got != tc.want {
				t.Fatalf("the set publishes %s, want %s", got, tc.want)
			}
			f := dsdFactsOf(t, b)
			if f.DSDPlayback == nil {
				t.Fatal("no dsdPlayback")
			}
			raw, err := json.Marshal(f.DSDPlayback)
			if err != nil {
				t.Fatal(err)
			}
			m := rawKeys(t, raw)
			for key, want := range map[string]string{"model": `"Test Test DAC"`, "bcdDevice": `"0100"`} {
				if string(m[key]) != want {
					t.Errorf("dsdPlayback.%s is %s, want %s", key, m[key], want)
				}
			}
			for _, key := range []string{"qualifiedOn", "qualificationRevision"} {
				if v, ok := m[key]; ok {
					t.Errorf("dsdPlayback.%s is sent (%s): no record is read", key, v)
				}
			}
			if r, ok := m["reason"]; tc.reason == "" && ok || tc.reason != "" && string(r) != `"`+tc.reason+`"` {
				t.Errorf("dsdPlayback.reason is %s (sent %v), want %q", r, ok, tc.reason)
			}
			var tuples []json.RawMessage
			if err := json.Unmarshal(m["tuples"], &tuples); err != nil {
				t.Fatal(err)
			}
			for _, tuple := range tuples {
				tm := rawKeys(t, tuple)
				transport, present := tm["transport"]
				switch string(tm["format"]) {
				case `"S32_LE"`:
					if string(transport) != `"dop"` {
						t.Errorf("the DoP tuple %s carries transport %s, want \"dop\"", tuple, transport)
					}
				default:
					if present {
						t.Errorf("the native tuple %s carries a transport member: absent is native", tuple)
					}
				}
			}
		})
	}
}

// playWired runs output play on c's config and files until its first track
// is heard, and answers the backend config it wired, read while the run
// plays; quit ends the run.
func playWired(t *testing.T, c *playCase) (playback.Config, func()) {
	t.Helper()
	c.standIn.hold = true
	pr, pw := io.Pipe()
	c.stdin = pr
	env := c.env("linux")
	var mu sync.Mutex
	var wired playback.Config
	env.backend = func(cfg playback.Config) (playBackend, error) {
		mu.Lock()
		wired = cfg
		mu.Unlock()
		c.standIn.notify = cfg.Notify
		return c.standIn, nil
	}
	stdout, stderr := newPlayBuf(), newPlayBuf()
	done := make(chan int, 1)
	go func() {
		done <- runOutputPlay(context.Background(), append([]string{"--config", c.configPath, "--keys"}, c.files...), env, stdout, stderr)
	}()
	stdout.waitFor(t, "a.flac) is audible")
	mu.Lock()
	defer mu.Unlock()
	return wired, func() {
		_, _ = pw.Write([]byte("q"))
		select {
		case <-done:
		case <-time.After(playWatch):
			t.Errorf("output play did not end after q:\n%s\n%s", stdout.String(), stderr.String())
		}
		_ = pw.Close()
	}
}
