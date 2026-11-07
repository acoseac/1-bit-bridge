package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/acoustid"
	"github.com/acoseac/1-bit-bridge/internal/enrichgate"
)

// `bridge fingerprint --config` and its switch while it runs (backlog B617,
// round 3, point 2): the switch is the variable, then the file --config
// names, then the file at the default path, then on; a file --config names
// that is not there refuses the lookup; and the run re-reads its switch once
// a second, so a switch-off, or a config found at the start that goes away or
// stops loading, stops the run within a second, with the refusal's exit.

// fingerprintOnLine begins what a lookup run says of its switch as it starts.
const fingerprintOnLine = "Enrichment is on ("

// TestBridgeFingerprintReadsTheConfigItIsGiven: --config names the file the
// switch is read from, after the variable and before the file at the default
// path; one that is not there refuses a lookup, never reads as on; and
// --no-lookup asks nothing of it.
func TestBridgeFingerprintReadsTheConfigItIsGiven(t *testing.T) {
	t.Run("a config that says off", func(t *testing.T) {
		fingerprintSwitchCase{name: "flag"}.setUp(t)
		cfg := filepath.Join(t.TempDir(), "off.yaml")
		writeSwitchConfig(t, cfg, "  enabled: false\n")
		code, stderr := fingerprintCLI(t, "--config", cfg, "--key", "k", "missing.flac")
		want := fingerprintOffRefusal + "enrich.enabled in " + cfg + ")"
		if code != 1 || !strings.Contains(stderr, want) {
			t.Errorf("exit %d, stderr:\n%s\nwant exit 1 and %q", code, stderr, want)
		}
		if strings.Contains(stderr, fingerprintProbed) {
			t.Errorf("a refused run probed fpcalc:\n%s", stderr)
		}
	})
	t.Run("a config that is not there", func(t *testing.T) {
		fingerprintSwitchCase{name: "flag"}.setUp(t)
		cfg := filepath.Join(t.TempDir(), "missing.yaml")
		code, stderr := fingerprintCLI(t, "--config", cfg, "--key", "k", "missing.flac")
		if code != 1 || !strings.Contains(stderr, "The config at "+cfg+" is not there") || !strings.Contains(stderr, "--no-lookup") {
			t.Errorf("exit %d, stderr:\n%s\nwant exit 1 naming the config that is not there", code, stderr)
		}
		if strings.Contains(stderr, fingerprintProbed) || strings.Contains(stderr, fingerprintOnLine) {
			t.Errorf("a config that is not there read as on:\n%s", stderr)
		}
	})
	t.Run("the flag's config over the one here", func(t *testing.T) {
		fingerprintSwitchCase{name: "flag", local: "  enabled: false\n"}.setUp(t)
		cfg := filepath.Join(t.TempDir(), "on.yaml")
		writeSwitchConfig(t, cfg, "  enabled: true\n")
		code, stderr := fingerprintCLI(t, "--config", cfg, "--key", "k", "missing.flac")
		if code != 1 || !strings.Contains(stderr, fingerprintProbed) || !strings.Contains(stderr, fingerprintOnLine+"enrich.enabled in "+cfg+")") {
			t.Errorf("exit %d, stderr:\n%s\nwant the run to reach the probe, saying the flag's config decided", code, stderr)
		}
	})
	t.Run("the variable over the flag's config", func(t *testing.T) {
		fingerprintSwitchCase{name: "flag", env: "off"}.setUp(t)
		cfg := filepath.Join(t.TempDir(), "on.yaml")
		writeSwitchConfig(t, cfg, "  enabled: true\n")
		code, stderr := fingerprintCLI(t, "--config", cfg, "--key", "k", "missing.flac")
		if code != 1 || !strings.Contains(stderr, fingerprintOffRefusal+"BRIDGE_ENRICH_ENABLED)") {
			t.Errorf("exit %d, stderr:\n%s\nwant the variable's refusal", code, stderr)
		}
	})
	t.Run("--no-lookup with a config that is not there", func(t *testing.T) {
		fingerprintSwitchCase{name: "flag"}.setUp(t)
		code, stderr := fingerprintCLI(t, "--config", filepath.Join(t.TempDir(), "missing.yaml"), "--no-lookup", "missing.flac")
		if code != 1 || !strings.Contains(stderr, fingerprintProbed) {
			t.Errorf("exit %d, stderr:\n%s\nwant the run to reach the probe: --no-lookup sends nothing", code, stderr)
		}
	})
}

// lookupRun is `bridge fingerprint` over two tracks of fpcalcLookupSeconds,
// lookup-1.flac and lookup-2.flac, with the fpcalc stand-in on PATH and an
// AcoustID of the test's own, which answers each lookup with answer.
type lookupRun struct {
	dir    string
	asked  atomic.Int64
	answer func(n int64, w http.ResponseWriter, r *http.Request)
}

func newLookupRun(t *testing.T, answer func(n int64, w http.ResponseWriter, r *http.Request)) *lookupRun {
	t.Helper()
	f := &lookupRun{dir: t.TempDir(), answer: answer}
	for _, name := range []string{"lookup-1.flac", "lookup-2.flac"} {
		writeFLACFixtureLasting(t, filepath.Join(f.dir, name), fpcalcLookupSeconds, "ARTIST=Some Artist", "TITLE=T")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.answer(f.asked.Add(1), w, r)
	}))
	t.Cleanup(srv.Close)
	prev := fingerprintLookupBase
	fingerprintLookupBase = srv.URL
	t.Cleanup(func() { fingerprintLookupBase = prev })
	installFpcalcStandIn(t, "", "")
	return f
}

// run runs `bridge fingerprint` with args and the two tracks, and returns
// its exit, its stderr and when it returned.
func (f *lookupRun) run(args ...string) (int, string, time.Time) {
	var stdout, stderr bytes.Buffer
	args = append(append([]string{"fingerprint"}, args...),
		filepath.Join(f.dir, "lookup-1.flac"), filepath.Join(f.dir, "lookup-2.flac"))
	code := run(context.Background(), args, &stdout, &stderr)
	return code, stderr.String(), time.Now()
}

// noMatch is AcoustID's answer that it knows nothing.
func noMatch(w http.ResponseWriter) {
	_, _ = w.Write([]byte(`{"status":"ok","results":[]}`))
}

// replaceFile puts data at path in one rename, so a reader finds the old
// bytes or the new, never a file half written.
func replaceFile(path string, data []byte) {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, path)
	}
}

// configBytes is the config writeSwitchConfig writes with enrich.
func configBytes(t *testing.T, enrich string) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.yaml")
	writeSwitchConfig(t, path, enrich)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// flipAt records when a test changed the switch.
type flipAt struct {
	mu sync.Mutex
	at time.Time
}

func (f *flipAt) mark() {
	f.mu.Lock()
	f.at = time.Now()
	f.mu.Unlock()
}

func (f *flipAt) get(t *testing.T) time.Time {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.at.IsZero() {
		t.Fatal("the switch was never changed: the case tests nothing")
	}
	return f.at
}

// TestBridgeFingerprintFollowsItsSwitchWhileItRuns: a run that started with
// its switch on stops once the switch is off, or once the config it read at
// the start is gone or no longer loads, within a second: between two tracks
// it sends the second track no lookup, and in the middle of a lookup it cuts
// the lookup off; either way it exits 1 saying why, as a run refused at its
// start does. The config is the one found here, or the one --config names.
func TestBridgeFingerprintFollowsItsSwitchWhileItRuns(t *testing.T) {
	// The run re-reads its switch once a second (Interval); the rest is for
	// a loaded host, and stays well below lookupHold, which a run that never
	// noticed would wait out before sending its second lookup.
	const bound = 2*enrichgate.Interval + time.Second
	off := func(t *testing.T) func(string) {
		b := configBytes(t, "  enabled: false\n")
		return func(path string) { replaceFile(path, b) }
	}
	gone := func(*testing.T) func(string) { return func(path string) { _ = os.Remove(path) } }
	broken := func(*testing.T) func(string) {
		return func(path string) { replaceFile(path, []byte("enrich: [\n")) }
	}
	for _, c := range []struct {
		name string
		// flag has the run read --config's file; else the one found here.
		flag bool
		// middle changes the config while the first lookup is on the wire,
		// which AcoustID then holds; else as its answer goes back.
		middle bool
		change func(t *testing.T) func(path string)
		// says is what the run must say, with <config> for the config's
		// name as the run reads it.
		says string
	}{
		{name: "switched off between two tracks", change: off, says: fingerprintOffRefusal + "enrich.enabled in <config>)"},
		{name: "switched off in the middle of a lookup", middle: true, change: off, says: fingerprintOffRefusal + "enrich.enabled in <config>)"},
		{name: "the flag's config switched off between two tracks", flag: true, change: off, says: fingerprintOffRefusal + "enrich.enabled in <config>)"},
		{name: "its config gone between two tracks", change: gone, says: "The config at <config> is gone"},
		{name: "its config no longer loading in the middle of a lookup", middle: true, change: broken, says: "The config at <config> no longer loads"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cwd, _ := isolateConfigEnv(t)
			clearEnrichSwitchEnv(t)
			name, path := "bridge.yaml", filepath.Join(cwd, "bridge.yaml")
			var args []string
			if c.flag {
				path = filepath.Join(t.TempDir(), "flag.yaml")
				name, args = path, []string{"--config", path}
			}
			writeSwitchConfig(t, path, "  enabled: true\n")
			change := c.change(t)
			var flip flipAt
			cut := make(chan time.Time, 1)
			f := newLookupRun(t, func(n int64, w http.ResponseWriter, r *http.Request) {
				if n != 1 {
					noMatch(w)
					return
				}
				change(path)
				flip.mark()
				if !c.middle {
					noMatch(w)
					return
				}
				<-r.Context().Done()
				cut <- time.Now()
				panic(http.ErrAbortHandler)
			})
			code, stderr, ended := f.run(append(args, "--key", "k")...)
			stopped := ended
			if c.middle {
				// The held handler sees the cut once the connection closes,
				// which can be after the run has returned.
				select {
				case stopped = <-cut:
				case <-time.After(10 * time.Second):
					t.Fatalf("the held lookup was never cut off; exit %d, stderr:\n%s", code, stderr)
				}
			}
			took := stopped.Sub(flip.get(t))
			if took > bound {
				t.Errorf("the run stopped %v after its config changed, past %v", took, bound)
			}
			t.Logf("stopped %v after the config changed", took)
			if n := f.asked.Load(); n != 1 {
				t.Errorf("AcoustID was asked %d time(s), want the first track's lookup alone", n)
			}
			says := strings.ReplaceAll(c.says, "<config>", name)
			if code != 1 || !strings.Contains(stderr, says) || !strings.Contains(stderr, "--no-lookup") {
				t.Errorf("exit %d, stderr:\n%s\nwant exit 1, %q and the way to --no-lookup", code, stderr, says)
			}
			if !strings.Contains(stderr, fingerprintOnLine+"enrich.enabled in "+name+")") {
				t.Errorf("the run did not say what decided its switch as it started:\n%s", stderr)
			}
		})
	}
}

// TestTheFingerprintSwitchIsReadAtMostOnceASecond: a run's switch is read as
// the run starts, and then once a second by the goroutine that follows it,
// however often the client and the run's context ask it: get gives the last
// read's answer and reads nothing.
func TestTheFingerprintSwitchIsReadAtMostOnceASecond(t *testing.T) {
	fingerprintSwitchCase{name: "reads"}.setUp(t)
	cfg := filepath.Join(t.TempDir(), "bridge.yaml")
	writeSwitchConfig(t, cfg, "  enabled: true\n")
	var reads atomic.Int64
	sw := &fingerprintSwitch{explicit: cfg, stderr: io.Discard, onRead: func() { reads.Add(1) }}
	if st := sw.start(); !st.on {
		t.Fatalf("the switch reads %+v, want on", st)
	}
	ctx, stop := sw.follow(context.Background())
	asked := 0
	for end := time.Now().Add(2*enrichgate.Interval + enrichgate.Interval/2); time.Now().Before(end); asked++ {
		if !sw.get() || enrichgate.Admit(ctx, sw.get) != nil {
			t.Fatal("the switch read off while its config says on")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	// The start's read and one a second over two and a half seconds.
	if n := reads.Load(); n < 2 || n > 3 {
		t.Errorf("the config was read %d time(s) while the switch was asked %d times, want the start's and one a second", n, 2*asked)
	}
}

// TestBridgeFingerprintFollowsItsSwitchFromItsFirstRead (backlog B617, round
// 4): a lookup run follows its switch from the read it starts on, before it
// probes fpcalc, so a switch-off just after that read reaches the run within
// a second however long the probe takes. The probe here (fingerprintProbe)
// turns the config off as it begins and takes 1.6 s; the run sends no lookup
// and exits 1 with the refusal, where a run that followed its switch only
// once the probe was done sent its first lookup on the start's answer.
func TestBridgeFingerprintFollowsItsSwitchFromItsFirstRead(t *testing.T) {
	fingerprintSwitchCase{name: "probe"}.setUp(t)
	cfg := filepath.Join(t.TempDir(), "bridge.yaml")
	writeSwitchConfig(t, cfg, "  enabled: true\n")
	off := configBytes(t, "  enabled: false\n")
	f := newLookupRun(t, func(_ int64, w http.ResponseWriter, _ *http.Request) { noMatch(w) })
	var flip flipAt
	prev := fingerprintProbe
	fingerprintProbe = func(ctx context.Context) (acoustid.Info, error) {
		replaceFile(cfg, off)
		flip.mark()
		select {
		case <-time.After(1600 * time.Millisecond):
		case <-ctx.Done():
			return acoustid.Info{}, ctx.Err()
		}
		return acoustid.Info{Path: "fpcalc", Version: "1.6.1"}, nil
	}
	t.Cleanup(func() { fingerprintProbe = prev })

	code, stderr, ended := f.run("--config", cfg, "--key", "k")
	if n := f.asked.Load(); n != 0 {
		t.Errorf("AcoustID was asked %d time(s) after the switch-off during the probe", n)
	}
	if code != 1 || !strings.Contains(stderr, fingerprintOffRefusal+"enrich.enabled in "+cfg+")") ||
		!strings.Contains(stderr, "--no-lookup") {
		t.Errorf("exit %d, stderr:\n%s\nwant exit 1 and the switch-off's refusal", code, stderr)
	}
	if took := ended.Sub(flip.get(t)); took > 2*enrichgate.Interval+time.Second {
		t.Errorf("the run ended %v after the switch-off", took)
	}
}
