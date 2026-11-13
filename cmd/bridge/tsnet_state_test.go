package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A state file that holds nothing (backlog B828), on every path that starts
// the embedded node: serve, bridge tsnet auth and bridge tsnet status each end
// the node's start before its upstream start, whatever tailscale.hostname
// says, in the bridge's words, and the console's tile shows them. Tailscale's
// own start refuses such a file in words about a TPM-sealed state. The hook
// every node runs just before its upstream start (stopTsnetStarts) stops a
// start that gets that far, so a path that misses the refusal is seen without
// reaching the network.

// writeTsnetStateFile writes body as the node's state file in dataDir's
// tailscale folder, as a truncation leaves it, and returns that folder.
func writeTsnetStateFile(t *testing.T, dataDir, body string) string {
	t.Helper()
	dir := filepath.Join(dataDir, "tailscale")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tailscaled.state"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// tsnetStateDirAsItIs is what a test compares to see that nothing changed the
// node's state dir: each entry's name and mode, and the state file's bytes.
func tsnetStateDirAsItIs(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %v\n", e.Name(), info.Mode())
	}
	state, err := os.ReadFile(filepath.Join(dir, "tailscaled.state"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&b, "tailscaled.state holds %q\n", state)
	return b.String()
}

// saidAfter is the rest of out's first line that carries prefix, after it, or
// "" where no line does.
func saidAfter(out, prefix string) string {
	i := strings.Index(out, prefix)
	if i < 0 {
		return ""
	}
	line := out[i+len(prefix):]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	return line
}

// mustSayTheTsnetStateHoldsNothing fails the test unless words, what where
// printed or showed, are the bridge's refusal of a state file that holds
// nothing: the file by its name and what it holds (holds), what keeps the
// node, what to move aside and what a new device costs, that the bridge does
// neither itself; none of upstream's words, the hook's or the tile's "called
// before Start"; and no path (dataDir's, or anything below it).
func mustSayTheTsnetStateHoldsNothing(t *testing.T, where, words, holds, dataDir string) {
	t.Helper()
	for _, want := range []string{"tailscaled.state", holds, "Tailscale does not start from it",
		"put back a copy of the file", "move the file aside", "new device", "authenticates as on a first run",
		"new tailnet address", "MagicDNS name", "moves and replaces nothing"} {
		if !strings.Contains(words, want) {
			t.Errorf("%s does not say %q: %q", where, want, words)
		}
	}
	for _, not := range []string{"TPM", "failed to migrate", "unexpected end of JSON input", "before start",
		"called before Start"} {
		if strings.Contains(words, not) {
			t.Errorf("%s carries %q, not the bridge's words: %q", where, not, words)
		}
	}
	if strings.Contains(words, dataDir) {
		t.Errorf("%s names the data dir's path: %q", where, words)
	}
}

// TestTheCommandsRefuseAStateFileThatHoldsNothingInTheBridgesWords: bridge
// tsnet auth and bridge tsnet status, over a state file that holds nothing,
// with tailscale.hostname set and without, exit 1 before their node's
// upstream start, saying the bridge's words on stderr, and leave the state as
// it was.
func TestTheCommandsRefuseAStateFileThatHoldsNothingInTheBridgesWords(t *testing.T) {
	for _, c := range []struct {
		cmd, said, hostname, body, holds string
	}{
		{"auth", "tsnet auth: ", "", "", "is empty"},
		{"auth", "tsnet auth: ", "chosen-name", "", "is empty"},
		{"status", "tsnet start: ", "", "", "is empty"},
		{"status", "tsnet start: ", "chosen-name", "", "is empty"},
		{"status", "tsnet start: ", "chosen-name", "\n", "holds only white space"},
	} {
		t.Run(fmt.Sprintf("tsnet %s hostname=%q state=%q", c.cmd, c.hostname, c.body), func(t *testing.T) {
			inheritNoLogsEnv(t, noLogsUnset)
			seen := stopTsnetStarts(t, nil)
			cfgPath, dataDir := tsnetTestConfig{mode: "tsnet", hostname: c.hostname}.write(t, "")
			dir := writeTsnetStateFile(t, dataDir, c.body)
			before := tsnetStateDirAsItIs(t, dir)
			var stdout, stderr bytes.Buffer
			code := tsnetCmd(context.Background(), []string{c.cmd, "--config", cfgPath}, strings.NewReader(""), &stdout, &stderr)
			if code != 1 {
				t.Errorf("bridge tsnet %s = %d, want 1; stderr=%s", c.cmd, code, stderr.String())
			}
			if len(seen) != 0 {
				t.Errorf("bridge tsnet %s reached its node's upstream start; stderr=%s", c.cmd, stderr.String())
			}
			mustSayTheTsnetStateHoldsNothing(t, "bridge tsnet "+c.cmd, saidAfter(stderr.String(), c.said), c.holds, dataDir)
			if stdout.Len() != 0 {
				t.Errorf("bridge tsnet %s printed on stdout: %q", c.cmd, stdout.String())
			}
			if after := tsnetStateDirAsItIs(t, dir); after != before {
				t.Errorf("bridge tsnet %s changed the state dir:\nbefore:\n%s\nafter:\n%s", c.cmd, before, after)
			}
		})
	}
}

// tsnetStartEndedLine is the line serve's tailnet side prints when its node's
// start fails (bringTsnetUp).
const tsnetStartEndedLine = "tsnet: bring node up: "

// serveUntilItsTsnetStartEnds boots serve on cfgPath, its console on admin,
// and returns the line its tailnet side printed when its node's start failed,
// once both the line and the console are there, and serve's exit; the test's
// cleanup stops serve. It fails the test where serve exits first.
func serveUntilItsTsnetStartEnds(t *testing.T, cfgPath string, admin heldLoopback) (string, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stderr := &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- runServe(ctx, serveOpts{configPath: cfgPath, addrOverride: "127.0.0.1:0", adminListener: admin.ln},
			&safeBuffer{}, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	waitForAdminReady(t, admin.addr, done, stderr)
	giveUp := serveGiveUpTime(t)
	for {
		if s := stderr.String(); strings.Contains(s, tsnetStartEndedLine) {
			return tsnetStartEndedLine + saidAfter(s, tsnetStartEndedLine), exited
		}
		select {
		case <-exited:
			t.Fatalf("serve exited with code %d before its node's start ended; stderr=%s", <-done, stderr.String())
		case <-time.After(25 * time.Millisecond):
		}
		if !giveUp.IsZero() && time.Now().After(giveUp) {
			t.Fatalf("serve's node's start had not ended before the test's deadline; stderr=%s\nserve's goroutines:\n%s",
				stderr.String(), serveStacks())
		}
	}
}

// TestServeRefusesAStateFileThatHoldsNothingInTheBridgesWords: serve, over a
// state file that holds nothing, with tailscale.hostname set and without,
// ends its node's start before the upstream start, its tailnet line says the
// bridge's words beside the LAN listener it keeps, the console's tile shows
// them in LastError, and the state is left as it was.
func TestServeRefusesAStateFileThatHoldsNothingInTheBridgesWords(t *testing.T) {
	for _, hostname := range []string{"", "chosen-name"} {
		t.Run(fmt.Sprintf("hostname=%q", hostname), func(t *testing.T) {
			inheritNoLogsEnv(t, noLogsUnset)
			seen := stopTsnetStarts(t, nil)
			admin := holdLoopback(t)
			cfgPath, dataDir := tsnetTestConfig{mode: "tsnet", hostname: hostname, admin: admin.addr}.write(t, "")
			dir := writeTsnetStateFile(t, dataDir, "")
			before := tsnetStateDirAsItIs(t, dir)

			line, exited := serveUntilItsTsnetStartEnds(t, cfgPath, admin)
			const lanGoesOn = " (LAN listener still active)"
			if !strings.HasSuffix(line, lanGoesOn) {
				t.Errorf("serve's tailnet line does not say the LAN listener goes on: %q", line)
			}
			words := strings.TrimSuffix(strings.TrimPrefix(line, tsnetStartEndedLine), lanGoesOn)
			mustSayTheTsnetStateHoldsNothing(t, "serve's tailnet line", words, "is empty", dataDir)
			if len(seen) != 0 {
				t.Errorf("serve reached its node's upstream start; its line: %q", line)
			}
			var tile struct {
				LastError string `json:"lastError"`
			}
			getJSON(t, &http.Client{Timeout: 30 * time.Second}, "http://"+admin.addr+"/api/tailscale/status", "", &tile)
			mustSayTheTsnetStateHoldsNothing(t, "the console's tile", tile.LastError, "is empty", dataDir)
			select {
			case <-exited:
				t.Error("serve exited once its node's start was refused, where its LAN listener goes on")
			default:
			}
			if after := tsnetStateDirAsItIs(t, dir); after != before {
				t.Errorf("serve changed the state dir:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}
