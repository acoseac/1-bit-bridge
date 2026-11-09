package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/tsnet"
	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/ipn/store"
	"tailscale.com/tailcfg"
	upstreamtsnet "tailscale.com/tsnet"
	"tailscale.com/types/key"
	"tailscale.com/types/persist"
)

// The embedded node's diagnostic-log uploads and its name, on the paths that
// build it (backlog B749): serve, bridge tsnet auth and bridge tsnet status
// each build the node through newTsnetServer after their mode gate, which
// applies the upload policy before anything starts the node, and the hook
// every node runs just before its upstream start (tsnetBeforeStart,
// tsnet.Config.BeforeStart) reads what that start would see and stops it
// there, so nothing reaches the network.

// errStoppedBeforeTheUpstreamStart is what tsnetBeforeStart answers in these
// tests: the start ends there.
var errStoppedBeforeTheUpstreamStart = errors.New("the test stopped the node before its upstream start")

// tsnetStartSeen is what an upstream start would have seen.
type tsnetStartSeen struct {
	env      string // TS_NO_LOGS_NO_SUPPORT, or noLogsUnset
	hostname string
	dir      string
}

// noLogsUnset is a TS_NO_LOGS_NO_SUPPORT that is not set at all.
const noLogsUnset = "\x00unset"

// noLogsEnvNow is TS_NO_LOGS_NO_SUPPORT as it stands.
func noLogsEnvNow() string {
	v, ok := os.LookupEnv(tsnet.NoLogsEnv)
	if !ok {
		return noLogsUnset
	}
	return v
}

// inheritNoLogsEnv sets TS_NO_LOGS_NO_SUPPORT as the bridge's environment
// holds it when the process starts, in a process that has built no node yet;
// the variable, and what the upload policy keeps of what it did to it, are
// put back as they were when the test ends.
func inheritNoLogsEnv(t *testing.T, v string) {
	t.Helper()
	t.Setenv(tsnet.NoLogsEnv, "")
	t.Cleanup(tsnet.ForgetUploadPolicyForTest())
	if v == noLogsUnset {
		if err := os.Unsetenv(tsnet.NoLogsEnv); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Setenv(tsnet.NoLogsEnv, v)
}

// stopTsnetStarts makes every node a path builds stop just before its
// upstream start, sending what that start would have seen. Call it before
// starting serve, so that its restore runs after serve has been drained.
func stopTsnetStarts(t *testing.T, also func(*upstreamtsnet.Server) error) <-chan tsnetStartSeen {
	t.Helper()
	seen := make(chan tsnetStartSeen, 8)
	prev := tsnetBeforeStart
	tsnetBeforeStart = func(ts *upstreamtsnet.Server) error {
		seen <- tsnetStartSeen{env: noLogsEnvNow(), hostname: ts.Hostname, dir: ts.Dir}
		if also != nil {
			if err := also(ts); err != nil {
				return err
			}
		}
		return errStoppedBeforeTheUpstreamStart
	}
	t.Cleanup(func() { tsnetBeforeStart = prev })
	return seen
}

// startSeen is what the node's upstream start would have seen, once the path
// that started it has returned (its start has ended by then, so the hook has
// run if it was reached), and fails the test where no start reached it.
func startSeen(t *testing.T, seen <-chan tsnetStartSeen, what string) tsnetStartSeen {
	t.Helper()
	select {
	case s := <-seen:
		return s
	default:
		t.Fatalf("%s returned without reaching its node's upstream start", what)
		return tsnetStartSeen{}
	}
}

// tsnetTestConfig is a bridge.yaml in a directory of its own: mode is
// tailscale.mode, key tailscale.uploadDiagnosticLogs as written ("" for no
// key), hostname tailscale.hostname ("" for none) and library libraryName.
type tsnetTestConfig struct {
	mode, key, hostname, library string
}

// write writes c into dir (a fresh temp dir where dir is "") and returns the
// config's path and its data dir.
func (c tsnetTestConfig) write(t *testing.T, dir string) (cfgPath, dataDir string) {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir = filepath.Join(dir, "data")
	body := "libraryRoots:\n  - " + strconv.Quote(lib) + "\ndataDir: " + strconv.Quote(dataDir) +
		"\nlistenAddress: 127.0.0.1:0\nadminAddress: 127.0.0.1:0\ndisableHttp3: true\nmdns:\n    enabled: false\n"
	if c.library != "" {
		body += "libraryName: " + strconv.Quote(c.library) + "\n"
	}
	body += "tailscale:\n    mode: " + c.mode + "\n"
	if c.key != "" {
		body += "    uploadDiagnosticLogs: " + c.key + "\n"
	}
	if c.hostname != "" {
		body += "    hostname: " + strconv.Quote(c.hostname) + "\n"
	}
	cfgPath = filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

// buildAndStartTsnet builds the node from cfgPath as the commands do
// (loadCLIConfig, newTsnetServer) and runs its Start, which the hook stops;
// err is newTsnetServer's.
func buildAndStartTsnet(t *testing.T, cfgPath string) (*tsnet.Server, string, error) {
	t.Helper()
	cfg, _, err := loadCLIConfig(cfgPath)
	if err != nil {
		t.Fatalf("load %s: %v", cfgPath, err)
	}
	var stderr bytes.Buffer
	srv, err := newTsnetServer(cfg, &stderr)
	if err != nil {
		return nil, stderr.String(), err
	}
	t.Cleanup(func() { _ = srv.Close() })
	if err := srv.Start(context.Background()); err == nil {
		t.Fatal("a start the hook stopped succeeded")
	}
	return srv, stderr.String(), nil
}

// TestTheUploadPolicyByKeyAndInheritedValue is the policy's table: the
// config's key absent, false or true, beside an inherited
// TS_NO_LOGS_NO_SUPPORT unset, true, false or one Tailscale's code would stop
// the process on, and the variable the upstream start sees, or the bridge's
// refusal before any node is built.
func TestTheUploadPolicyByKeyAndInheritedValue(t *testing.T) {
	const unreadable = "maybe"
	for _, key := range []string{"", "false", "true"} {
		for _, inherited := range []string{noLogsUnset, "true", "false", unreadable} {
			name := fmt.Sprintf("key=%q inherited=%q", key, strings.ReplaceAll(inherited, noLogsUnset, "unset"))
			t.Run(name, func(t *testing.T) {
				inheritNoLogsEnv(t, inherited)
				seen := stopTsnetStarts(t, nil)
				cfgPath, _ := tsnetTestConfig{mode: "tsnet", key: key}.write(t, "")
				_, _, err := buildAndStartTsnet(t, cfgPath)
				if inherited == unreadable {
					if !errors.Is(err, tsnet.ErrNoLogsEnvUnreadable) {
						t.Fatalf("newTsnetServer = %v, want the bridge's refusal of the variable", err)
					}
					if msg := err.Error(); !strings.Contains(msg, tsnet.NoLogsEnv) || strings.Contains(msg, unreadable) {
						t.Errorf("the refusal must name the variable and not its value: %q", msg)
					}
					if got := noLogsEnvNow(); got != inherited {
						t.Errorf("the refusal left the variable as %q, want it as inherited", got)
					}
					if len(seen) != 0 {
						t.Error("a node was started over a variable the bridge refused")
					}
					return
				}
				if err != nil {
					t.Fatalf("newTsnetServer: %v", err)
				}
				want := "true"
				if key == "true" {
					want = inherited // left as inherited
				}
				if got := startSeen(t, seen, "the node's Start").env; got != want {
					t.Errorf("the upstream start would see TS_NO_LOGS_NO_SUPPORT=%q, want %q",
						strings.ReplaceAll(got, noLogsUnset, "unset"), strings.ReplaceAll(want, noLogsUnset, "unset"))
				}
			})
		}
	}
}

// serveUntilItsTsnetStart boots serve on cfgPath and returns what the
// upstream start of the node serve built would have seen; the test's cleanup
// stops serve. Serve's tsnet start runs in the tailnet goroutine runServe
// starts (tsnetFront.run), so runServe is the narrowest function that runs it.
func serveUntilItsTsnetStart(t *testing.T, cfgPath string, seen <-chan tsnetStartSeen) tsnetStartSeen {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0"}, &safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	select {
	case s := <-seen:
		return s
	case <-exited:
		t.Fatalf("serve exited with code %d before its tsnet start; stderr=%s", <-done, stderr.String())
	case <-serveGiveUp(t):
		t.Fatalf("serve never reached its tsnet start; stderr=%s", stderr.String())
	}
	return tsnetStartSeen{}
}

// TestEveryPathAppliesTheUploadPolicyBeforeTheUpstreamStart: serve, bridge
// tsnet auth and bridge tsnet status each build their node with the policy
// applied by the time its upstream start would run: a config without the key
// overrides an inherited false, and one with the key leaves it.
func TestEveryPathAppliesTheUploadPolicyBeforeTheUpstreamStart(t *testing.T) {
	paths := map[string]func(t *testing.T, cfgPath string, seen <-chan tsnetStartSeen) tsnetStartSeen{
		"serve": serveUntilItsTsnetStart,
		"tsnet auth": func(t *testing.T, cfgPath string, seen <-chan tsnetStartSeen) tsnetStartSeen {
			var stdout, stderr bytes.Buffer
			if code := tsnetCmd(context.Background(), []string{"auth", "--config", cfgPath}, strings.NewReader(""), &stdout, &stderr); code != 1 {
				t.Fatalf("bridge tsnet auth = %d, want 1 (the start the hook stopped); stderr=%s", code, stderr.String())
			}
			return startSeen(t, seen, "bridge tsnet auth (stderr: "+stderr.String()+")")
		},
		"tsnet status": func(t *testing.T, cfgPath string, seen <-chan tsnetStartSeen) tsnetStartSeen {
			var stdout, stderr bytes.Buffer
			if code := tsnetCmd(context.Background(), []string{"status", "--config", cfgPath}, strings.NewReader(""), &stdout, &stderr); code != 1 {
				t.Fatalf("bridge tsnet status = %d, want 1 (the start the hook stopped); stderr=%s", code, stderr.String())
			}
			return startSeen(t, seen, "bridge tsnet status (stderr: "+stderr.String()+")")
		},
	}
	for name, run := range paths {
		for _, c := range []struct{ key, want string }{{"", "true"}, {"true", "false"}} {
			t.Run(fmt.Sprintf("%s key=%q", name, c.key), func(t *testing.T) {
				inheritNoLogsEnv(t, "false")
				seen := stopTsnetStarts(t, nil)
				cfgPath, dataDir := tsnetTestConfig{mode: "tsnet", key: c.key}.write(t, "")
				got := run(t, cfgPath, seen)
				if got.env != c.want {
					t.Errorf("%s: the upstream start would see TS_NO_LOGS_NO_SUPPORT=%q, want %q", name, got.env, c.want)
				}
				if got.dir != filepath.Join(dataDir, "tailscale") {
					t.Errorf("%s: the node's state dir is %q, want the data dir's tailscale folder", name, got.dir)
				}
			})
		}
	}
}

// serveOnceUntilItsTsnetStart is serveUntilItsTsnetStart for a serve the test
// then stops, as the menu's Ctrl+C does before its "Start now" runs serve
// again in the same process: what the node's upstream start would have seen,
// once serve has returned 0.
func serveOnceUntilItsTsnetStart(t *testing.T, cfgPath string, seen <-chan tsnetStartSeen) tsnetStartSeen {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0"}, &safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	var got tsnetStartSeen
	select {
	case got = <-seen:
	case <-exited:
		t.Fatalf("serve exited with code %d before its tsnet start; stderr=%s", <-done, stderr.String())
	case <-serveGiveUp(t):
		t.Fatalf("serve never reached its tsnet start; stderr=%s", stderr.String())
	}
	cancel()
	select {
	case <-exited:
	case <-serveGiveUp(t):
		t.Fatalf("serve did not stop; stderr=%s\nserve's goroutines:\n%s", stderr.String(), serveStacks())
	}
	if code := <-done; code != 0 {
		t.Fatalf("serve exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	return got
}

// readsOff is a TS_NO_LOGS_NO_SUPPORT value as Tailscale's code reads it
// (envknob.NoLogsNoSupport: strconv.ParseBool, empty or unset false).
func readsOff(t *testing.T, v string) bool {
	t.Helper()
	if v == noLogsUnset || v == "" {
		return false
	}
	off, err := strconv.ParseBool(v)
	if err != nil {
		t.Fatalf("TS_NO_LOGS_NO_SUPPORT=%q does not read as Tailscale reads it: %v", v, err)
	}
	return off
}

// TestAServeRunAgainInTheSameProcessKeepsTheUploadsOff: the menu's "Start
// now" (and bridge init's start, before it) runs serve in its own process,
// and runs it again after a stop. Once a serve in the process has built its
// node without tailscale.uploadDiagnosticLogs, every later serve's node
// starts with the uploads off, whatever its config says, until the process
// exits: a consent saved between the two (the console's switch) applies at
// the bridge's next launch. Each inherited value runs through each sequence,
// and the variable is read as Tailscale reads it.
func TestAServeRunAgainInTheSameProcessKeepsTheUploadsOff(t *testing.T) {
	type row struct {
		inherited string
		keys      []string // each serve's tailscale.uploadDiagnosticLogs, "" for none
		off       bool     // the last serve's upstream start reads the uploads off
	}
	var rows []row
	for _, inherited := range []string{noLogsUnset, "false", "true", "1"} {
		rows = append(rows,
			row{inherited, []string{"", "true"}, true},
			row{inherited, []string{"true", "", "true"}, true},
			row{inherited, []string{"", ""}, true},
		)
	}
	rows = append(rows,
		row{noLogsUnset, []string{"true", "true"}, false},
		row{"true", []string{"true", "true"}, true},
	)
	for _, c := range rows {
		name := fmt.Sprintf("inherited=%q keys=%q", strings.ReplaceAll(c.inherited, noLogsUnset, "unset"), c.keys)
		t.Run(name, func(t *testing.T) {
			inheritNoLogsEnv(t, c.inherited)
			seen := stopTsnetStarts(t, nil)
			dir := t.TempDir()
			var got tsnetStartSeen
			for _, key := range c.keys {
				cfgPath, _ := tsnetTestConfig{mode: "tsnet", key: key}.write(t, dir)
				got = serveOnceUntilItsTsnetStart(t, cfgPath, seen)
			}
			if off := readsOff(t, got.env); off != c.off {
				t.Errorf("the last serve's upstream start reads the uploads off: %v (TS_NO_LOGS_NO_SUPPORT=%q), want %v",
					off, strings.ReplaceAll(got.env, noLogsUnset, "unset"), c.off)
			}
		})
	}
}

// TestTheCommandsThatBuildNoNodeLeaveTheEnvironmentAlone: a command its mode
// gate refuses, bridge tsnet logout, and serve in a mode that builds no node
// leave TS_NO_LOGS_NO_SUPPORT as they found it.
func TestTheCommandsThatBuildNoNodeLeaveTheEnvironmentAlone(t *testing.T) {
	for _, inherited := range []string{noLogsUnset, "false"} {
		for _, c := range []struct {
			name string
			mode string
			run  func(t *testing.T, cfgPath, dataDir string)
		}{
			{"tsnet auth in cli mode", "cli", func(t *testing.T, cfgPath, _ string) {
				var stdout, stderr bytes.Buffer
				if code := tsnetCmd(context.Background(), []string{"auth", "--config", cfgPath}, strings.NewReader(""), &stdout, &stderr); code != 1 {
					t.Fatalf("bridge tsnet auth in cli mode = %d, want its refusal", code)
				}
			}},
			{"tsnet status in cli mode", "cli", func(t *testing.T, cfgPath, _ string) {
				var stdout, stderr bytes.Buffer
				if code := tsnetCmd(context.Background(), []string{"status", "--config", cfgPath}, strings.NewReader(""), &stdout, &stderr); code != 1 {
					t.Fatalf("bridge tsnet status in cli mode = %d, want its refusal", code)
				}
			}},
			{"tsnet logout", "tsnet", func(t *testing.T, cfgPath, dataDir string) {
				state := filepath.Join(dataDir, "tailscale")
				if err := os.MkdirAll(state, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(state, "tailscaled.state"), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
				var stdout, stderr bytes.Buffer
				if code := tsnetCmd(context.Background(), []string{"logout", "--config", cfgPath, "--force"},
					strings.NewReader("WIPE\n"), &stdout, &stderr); code != 0 {
					t.Fatalf("bridge tsnet logout = %d; stderr=%s", code, stderr.String())
				}
				if _, err := os.Stat(state); !os.IsNotExist(err) {
					t.Fatalf("logout left the state dir: %v", err)
				}
			}},
			{"serve with tailscale disabled", "disabled", func(t *testing.T, cfgPath, _ string) {
				ctx, cancel := context.WithCancel(context.Background())
				stderr := &safeBuffer{}
				done := make(chan int, 1)
				exited := make(chan struct{})
				// Serve builds its API handler well after its mode switch,
				// where a tsnet mode would have built the node.
				built := make(chan struct{})
				var once sync.Once
				wrap := func(h http.Handler) http.Handler {
					once.Do(func() { close(built) })
					return h
				}
				go func() {
					defer close(exited)
					done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", wrapAPIHandler: wrap},
						&safeBuffer{}, stderr)
				}()
				drainServeOnCleanup(t, cancel, exited, done, stderr)
				waitForServe(t, built, "its API handler", exited, done, stderr)
			}},
		} {
			t.Run(fmt.Sprintf("%s inherited=%q", c.name, strings.ReplaceAll(inherited, noLogsUnset, "unset")), func(t *testing.T) {
				inheritNoLogsEnv(t, inherited)
				seen := stopTsnetStarts(t, nil)
				cfgPath, dataDir := tsnetTestConfig{mode: c.mode}.write(t, "")
				c.run(t, cfgPath, dataDir)
				if got := noLogsEnvNow(); got != inherited {
					t.Errorf("%s changed TS_NO_LOGS_NO_SUPPORT to %q", c.name, got)
				}
				if len(seen) != 0 {
					t.Errorf("%s started a node", c.name)
				}
			})
		}
	}
}

// writeRegisteredTsnetState writes a registered node's state into stateDir
// with upstream's own writers: its file store, the profile types' JSON as
// its profile manager writes them, and Prefs.ToBytes.
func writeRegisteredTsnetState(t *testing.T, stateDir, hostname string) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.NewFileStore(t.Logf, filepath.Join(stateDir, "tailscaled.state"))
	if err != nil {
		t.Fatal(err)
	}
	const id = ipn.ProfileID("b749")
	k := ipn.StateKey("profile-" + string(id))
	prefs := ipn.NewPrefs()
	prefs.Hostname = hostname
	prefs.Persist = &persist.Persist{PrivateNodeKey: key.NewNode(), NodeID: "nStable1",
		UserProfile: tailcfg.UserProfile{LoginName: "owner@example.com"}}
	known, err := json.Marshal(map[ipn.ProfileID]ipn.LoginProfileView{
		id: (&ipn.LoginProfile{ID: id, Key: k, Name: "owner@example.com", NodeID: "nStable1"}).View(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []struct {
		k ipn.StateKey
		v []byte
	}{{ipn.KnownProfilesStateKey, known}, {k, prefs.ToBytes()}, {ipn.CurrentProfileStateKey, []byte(k)}} {
		if err := st.WriteState(w.k, w.v); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheNodesNameOnEveryStart: an explicit tailscale.hostname wins; a node
// registered under another name than the library's keeps it, before and after
// a library rename; a new node is tsnet.DefaultHostname, never the library's
// name, again after a first start that wrote its state and registered
// nothing; and a state that cannot be read keeps the library's name, with one
// line advising tailscale.hostname.
func TestTheNodesNameOnEveryStart(t *testing.T) {
	inheritNoLogsEnv(t, noLogsUnset)
	nameOf := func(t *testing.T, c tsnetTestConfig, dir string) (string, string) {
		t.Helper()
		seen := stopTsnetStarts(t, nil)
		cfgPath, _ := c.write(t, dir)
		_, stderr, err := buildAndStartTsnet(t, cfgPath)
		if err != nil {
			t.Fatalf("newTsnetServer: %v", err)
		}
		return startSeen(t, seen, "the node's Start").hostname, stderr
	}

	t.Run("an explicit hostname", func(t *testing.T) {
		dir := t.TempDir()
		writeRegisteredTsnetState(t, filepath.Join(dir, "data", "tailscale"), "Old Library")
		if got, _ := nameOf(t, tsnetTestConfig{mode: "tsnet", hostname: "chosen", library: "Living Room"}, dir); got != "chosen" {
			t.Errorf("the node registers as %q, want tailscale.hostname", got)
		}
	})
	t.Run("a fresh dir", func(t *testing.T) {
		if got, _ := nameOf(t, tsnetTestConfig{mode: "tsnet", library: "Living Room"}, ""); got != tsnet.DefaultHostname {
			t.Errorf("a new node registers as %q, want %q", got, tsnet.DefaultHostname)
		}
	})
	t.Run("a fresh dir whose first start wrote its state and failed", func(t *testing.T) {
		dir := t.TempDir()
		// The first start writes, with upstream's own store, what upstream's
		// start writes before any authentication (the store's "{}" and the
		// machine key), and fails, as a start the tailnet never registered
		// does.
		seen := stopTsnetStarts(t, func(ts *upstreamtsnet.Server) error {
			st, err := store.NewFileStore(t.Logf, filepath.Join(ts.Dir, "tailscaled.state"))
			if err != nil {
				return err
			}
			mk, err := key.NewMachine().MarshalText()
			if err != nil {
				return err
			}
			return st.WriteState(ipn.MachineKeyStateKey, mk)
		})
		cfgPath, dataDir := tsnetTestConfig{mode: "tsnet", library: "Living Room"}.write(t, dir)
		if _, _, err := buildAndStartTsnet(t, cfgPath); err != nil {
			t.Fatal(err)
		}
		if first := startSeen(t, seen, "the first start").hostname; first != tsnet.DefaultHostname {
			t.Fatalf("the first start registers as %q, want %q", first, tsnet.DefaultHostname)
		}
		if _, err := os.Stat(filepath.Join(dataDir, "tailscale", "tailscaled.state")); err != nil {
			t.Fatalf("the first start left no state file: %v", err)
		}
		for _, library := range []string{"Living Room", "Kitchen"} {
			if got, _ := nameOf(t, tsnetTestConfig{mode: "tsnet", library: library}, dir); got != tsnet.DefaultHostname {
				t.Errorf("library %q: the next start registers as %q, want %q", library, got, tsnet.DefaultHostname)
			}
		}
	})
	t.Run("a registered node across a library rename", func(t *testing.T) {
		dir := t.TempDir()
		writeRegisteredTsnetState(t, filepath.Join(dir, "data", "tailscale"), "Old Library")
		for _, library := range []string{"Living Room", "Kitchen"} {
			if got, _ := nameOf(t, tsnetTestConfig{mode: "tsnet", library: library}, dir); got != "Old Library" {
				t.Errorf("library %q: the registered node registers as %q, want the name it has", library, got)
			}
		}
	})
	t.Run("a state that cannot be read", func(t *testing.T) {
		dir := t.TempDir()
		state := filepath.Join(dir, "data", "tailscale")
		if err := os.MkdirAll(state, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "tailscaled.state"), []byte("not the state"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, stderr := nameOf(t, tsnetTestConfig{mode: "tsnet", library: "Living Room"}, dir)
		if got != "Living Room" {
			t.Errorf("the node registers as %q over a state that cannot be read, want the library's name", got)
		}
		if strings.Count(stderr, "cannot tell which name the node has") != 1 || !strings.Contains(stderr, "tailscale.hostname") {
			t.Errorf("a state that cannot be read: want one line saying so and advising tailscale.hostname; stderr=%s", stderr)
		}
	})
}

// upstreamLogsRefusal is upstream's refusal of a node that does not upload
// logs, as ipnlocal words it; internal/tsnet's
// TestTheLogsRequirementIsUpstreamsWords pins the part the bridge matches.
const upstreamLogsRefusal = "tailnet requires logging to be enabled. Remove --no-logs-no-support from tailscaled command line."

// runningTsnetNode is a node whose status is st and whose other reads are the
// real server's: the tile's view of a node the tailnet refuses while it runs.
type runningTsnetNode struct {
	*tsnet.Server
	st *ipnstate.Status
}

func (r runningTsnetNode) Status(context.Context) (*ipnstate.Status, error) { return r.st, nil }

// TestTheTileSaysWhatATailnetThatRequiresLogsNeeds: after a start the tailnet
// refused, and while a running node reports the refusal, the console's tile
// says in LastError what to change, naming tailscale.uploadDiagnosticLogs,
// and TS_NO_LOGS_NO_SUPPORT where an inherited true holds the uploads off;
// none of it changes the environment.
func TestTheTileSaysWhatATailnetThatRequiresLogsNeeds(t *testing.T) {
	for _, c := range []struct {
		name, key, inherited string
		envNamed             bool
	}{
		{"no key", "", noLogsUnset, false},
		{"the key true and the inherited value true", "true", "true", true},
	} {
		t.Run(c.name+", a refused start", func(t *testing.T) {
			inheritNoLogsEnv(t, c.inherited)
			seen := stopTsnetStarts(t, func(*upstreamtsnet.Server) error {
				return fmt.Errorf("tsnet.Up: backend: %s", upstreamLogsRefusal)
			})
			cfgPath, _ := tsnetTestConfig{mode: "tsnet", key: c.key}.write(t, "")
			srv, _, err := buildAndStartTsnet(t, cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			atStart := startSeen(t, seen, "the refused start").env
			tile := tsnetAdminStatus(context.Background(), srv)
			mustSayWhatTheTailnetRequires(t, tile.LastError, c.envNamed)
			if got := noLogsEnvNow(); got != atStart {
				t.Errorf("the refusal changed TS_NO_LOGS_NO_SUPPORT from %q to %q", atStart, got)
			}
		})
		t.Run(c.name+", a running node", func(t *testing.T) {
			inheritNoLogsEnv(t, c.inherited)
			cfgPath, _ := tsnetTestConfig{mode: "tsnet", key: c.key}.write(t, "")
			cfg, _, err := loadCLIConfig(cfgPath)
			if err != nil {
				t.Fatal(err)
			}
			srv, err := newTsnetServer(cfg, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			before := noLogsEnvNow()
			tile := tsnetAdminStatus(context.Background(), runningTsnetNode{Server: srv, st: &ipnstate.Status{
				BackendState: "Stopped",
				Health:       []string{"The local log is misconfigured: " + upstreamLogsRefusal},
			}})
			mustSayWhatTheTailnetRequires(t, tile.LastError, c.envNamed)
			if tile.BackendState != "Stopped" {
				t.Errorf("the tile's backend state = %q, want the status's", tile.BackendState)
			}
			if got := noLogsEnvNow(); got != before {
				t.Errorf("the tile changed TS_NO_LOGS_NO_SUPPORT from %q to %q", before, got)
			}
		})
	}
}

// mustSayWhatTheTailnetRequires fails the test unless the tile's LastError is
// the bridge's words for the refusal.
func mustSayWhatTheTailnetRequires(t *testing.T, lastError string, envNamed bool) {
	t.Helper()
	if !strings.Contains(lastError, "tailscale.uploadDiagnosticLogs") || !strings.Contains(lastError, "requires Tailscale's logs") {
		t.Errorf("the tile's LastError does not say what the tailnet requires: %q", lastError)
	}
	if named := strings.Contains(lastError, tsnet.NoLogsEnv); named != envNamed {
		t.Errorf("the tile's LastError names %s: %v, want %v: %q", tsnet.NoLogsEnv, named, envNamed, lastError)
	}
	if strings.Contains(lastError, "--no-logs-no-support") || strings.Contains(lastError, "called before Start") {
		t.Errorf("the tile's LastError is not the bridge's words: %q", lastError)
	}
	if !strings.Contains(lastError, "launch it again") || strings.Contains(lastError, "restart the bridge") {
		t.Errorf("the tile's LastError does not make a new launch of the bridge what applies a change: %q", lastError)
	}
}

// buildTsnetNode builds the node from cfgPath as the commands do
// (loadCLIConfig, newTsnetServer), which applies the upload policy, and
// starts nothing.
func buildTsnetNode(t *testing.T, cfgPath string) *tsnet.Server {
	t.Helper()
	cfg, _, err := loadCLIConfig(cfgPath)
	if err != nil {
		t.Fatalf("load %s: %v", cfgPath, err)
	}
	srv, err := newTsnetServer(cfg, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newTsnetServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// TestTheTileSaysWhatHoldsTheUploadsOff: the console's tile reads the upload
// policy's status (tsnetStatusSource.Uploads) and says, in a line of its own
// (UploadsHeldOff), what holds the uploads off where the config allows them:
// the environment the bridge was launched with, this process earlier, or
// something else in it. It says it for a node the tailnet accepted, whose
// backend state and endpoints it shows as they are, and for a start the
// tailnet refused, whose LastError says it too; where the config does not
// allow the uploads, or nothing holds them off, it says nothing.
func TestTheTileSaysWhatHoldsTheUploadsOff(t *testing.T) {
	for _, c := range []struct {
		name      string
		inherited string
		keys      []string // each node's tailscale.uploadDiagnosticLogs, "" for none, in order
		between   func()   // after the first node, what else in the process does
		says      []string // what the line says; nothing where it is empty
		envNamed  bool
	}{
		{"the launch environment", "true", []string{"true"}, nil,
			[]string{tsnet.NoLogsEnv + "=true", "the environment the bridge was launched with", "launch it again"}, true},
		{"this process, earlier", noLogsUnset, []string{"", "true"}, nil,
			[]string{"this process turned", "next launch", "launch it again"}, false},
		{"something else in this process", noLogsUnset, []string{"true", "true"}, envknob.SetNoLogsNoSupport,
			[]string{"something in this process turned", "may turn them back on", "launch it again"}, false},
		{"the setting off", noLogsUnset, []string{""}, nil, nil, false},
		{"nothing holding them off", noLogsUnset, []string{"true"}, nil, nil, false},
	} {
		build := func(t *testing.T, last func(t *testing.T, cfgPath string)) {
			t.Helper()
			dir := t.TempDir()
			for i, key := range c.keys {
				if i == 1 && c.between != nil {
					c.between()
				}
				cfgPath, _ := tsnetTestConfig{mode: "tsnet", key: key}.write(t, dir)
				if i < len(c.keys)-1 {
					buildTsnetNode(t, cfgPath)
					continue
				}
				last(t, cfgPath)
			}
		}
		t.Run(c.name+", a node the tailnet accepted", func(t *testing.T) {
			inheritNoLogsEnv(t, c.inherited)
			build(t, func(t *testing.T, cfgPath string) {
				ip := netip.MustParseAddr("100.64.0.7")
				tile := tsnetAdminStatus(context.Background(), runningTsnetNode{Server: buildTsnetNode(t, cfgPath), st: &ipnstate.Status{
					BackendState: "Running",
					Self:         &ipnstate.PeerStatus{HostName: "1-bit-bridge", DNSName: "1-bit-bridge.example.ts.net.", TailscaleIPs: []netip.Addr{ip}},
				}})
				if tile.BackendState != "Running" || tile.NodeName != "1-bit-bridge" ||
					tile.MagicDNSName != "1-bit-bridge.example.ts.net." || len(tile.TailscaleIPs) != 1 || tile.TailscaleIPs[0] != ip.String() {
					t.Errorf("the tile changed what the running node reports: %+v", tile)
				}
				if tile.LastError != "" {
					t.Errorf("the tile reports an error for a node the tailnet accepted: %q", tile.LastError)
				}
				mustSayWhatHoldsTheUploadsOffInTheTile(t, tile.UploadsHeldOff, c.says, c.envNamed)
			})
		})
		t.Run(c.name+", a refused start", func(t *testing.T) {
			inheritNoLogsEnv(t, c.inherited)
			stopTsnetStarts(t, func(*upstreamtsnet.Server) error {
				return fmt.Errorf("tsnet.Up: backend: %s", upstreamLogsRefusal)
			})
			build(t, func(t *testing.T, cfgPath string) {
				srv, _, err := buildAndStartTsnet(t, cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				tile := tsnetAdminStatus(context.Background(), srv)
				mustSayWhatTheTailnetRequires(t, tile.LastError, c.envNamed)
				mustSayWhatHoldsTheUploadsOffInTheTile(t, tile.UploadsHeldOff, c.says, c.envNamed)
				for _, want := range c.says {
					if !strings.Contains(tile.LastError, want) {
						t.Errorf("the refusal in the tile does not say %q: %q", want, tile.LastError)
					}
				}
			})
		})
	}
}

// mustSayWhatHoldsTheUploadsOffInTheTile fails the test unless the tile's
// line on the uploads says each of says, or nothing where says is empty, and
// names TS_NO_LOGS_NO_SUPPORT only where the launch environment held it.
func mustSayWhatHoldsTheUploadsOffInTheTile(t *testing.T, line string, says []string, envNamed bool) {
	t.Helper()
	if len(says) == 0 {
		if line != "" {
			t.Errorf("the tile says the uploads are held off where nothing holds them against the setting: %q", line)
		}
		return
	}
	if !strings.Contains(line, "tailscale.uploadDiagnosticLogs") {
		t.Errorf("the tile's line on the uploads does not name the setting: %q", line)
	}
	for _, want := range says {
		if !strings.Contains(line, want) {
			t.Errorf("the tile's line on the uploads does not say %q: %q", want, line)
		}
	}
	if named := strings.Contains(line, tsnet.NoLogsEnv); named != envNamed {
		t.Errorf("the tile's line names %s: %v, want %v: %q", tsnet.NoLogsEnv, named, envNamed, line)
	}
	if strings.Contains(line, "restart the bridge") {
		t.Errorf("the tile's line advises a restart of the bridge, which the menu's Start now makes in this process: %q", line)
	}
}

// TestALibraryChangeKeepsTheUploadConsent: a CLI save of bridge.yaml keeps
// tailscale.uploadDiagnosticLogs as the file holds it.
func TestALibraryChangeKeepsTheUploadConsent(t *testing.T) {
	cfgPath, _ := tsnetTestConfig{mode: "tsnet", key: "true"}.write(t, "")
	lib2 := filepath.Join(t.TempDir(), "More")
	if err := os.MkdirAll(lib2, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, out := runCLI(t, "library", "add", "--config", cfgPath, lib2); code != 0 {
		t.Fatalf("bridge library add = %d: %s", code, out)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LibraryRoots) != 2 || !cfg.Tailscale.UploadDiagnosticLogs {
		t.Errorf("after bridge library add: roots %v, uploadDiagnosticLogs %v; want both roots and the consent kept",
			cfg.LibraryRoots, cfg.Tailscale.UploadDiagnosticLogs)
	}
}
