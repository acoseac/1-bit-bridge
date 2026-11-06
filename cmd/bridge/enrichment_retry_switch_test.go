package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// writeRetryCLIConfig writes a bridge.yaml for the CLI alone, its admin
// address adminAddr and yamlTail appended as written, over a data directory
// of its own that holds no store yet, and returns its path and that data
// directory.
func writeRetryCLIConfig(t *testing.T, adminAddr, yamlTail string) (cfgPath, dataDir string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	dataDir = filepath.Join(dir, "data")
	cfgPath = filepath.Join(dir, "bridge.yaml")
	body := "libraryRoots:\n  - " + lib + "\ndataDir: " + dataDir + "\nadminAddress: " + adminAddr + "\n" + yamlTail
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dataDir
}

// TestTheCLIRetryWhileOff: `bridge enrichment retry` with enrichment off
// (backlog B617). With no bridge running, the CLI's own config decides, its
// environment included: off, it refuses and names the key before it opens
// the store, so no database is made; BRIDGE_ENRICH_ENABLED=true over a file
// that says false runs it. Through a running bridge, the bridge's live value
// decides and the CLI relays its answer, the 409 enrichment-off of a bridge
// with enrichment off included, whatever the CLI's own environment says,
// both ways round.
func TestTheCLIRetryWhileOff(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, cfgPath string) (code int, stdout, stderr string) {
		t.Helper()
		var so, se bytes.Buffer
		code = enrichmentRetryCmd(ctx, []string{"--config", cfgPath}, &so, &se)
		return code, so.String(), se.String()
	}
	noStore := func(t *testing.T, dataDir string) {
		t.Helper()
		if _, err := os.Stat(manifest.DefaultDBPath(dataDir)); !os.IsNotExist(err) {
			t.Errorf("the CLI's own store is there (stat: %v); the retry was to touch no store of its own", err)
		}
	}
	offline := func(t *testing.T) string { return "127.0.0.1:" + closedLoopbackPort(t) }

	t.Run("offline, the file says off", func(t *testing.T) {
		cfgPath, dataDir := writeRetryCLIConfig(t, offline(t), "enrich:\n  enabled: false\n")
		code, stdout, stderr := run(t, cfgPath)
		if code != 1 || !strings.Contains(stderr, "enrich.enabled") {
			t.Errorf("exit %d, stderr %q; want 1 and the key named", code, stderr)
		}
		if strings.Contains(stdout, "re-queued") {
			t.Errorf("stdout says it re-queued: %q", stdout)
		}
		noStore(t, dataDir)
	})

	t.Run("offline, the environment says off", func(t *testing.T) {
		t.Setenv("BRIDGE_ENRICH_ENABLED", "false")
		cfgPath, dataDir := writeRetryCLIConfig(t, offline(t), "")
		code, _, stderr := run(t, cfgPath)
		if code != 1 || !strings.Contains(stderr, "BRIDGE_ENRICH_ENABLED") {
			t.Errorf("exit %d, stderr %q; want 1 and the variable named", code, stderr)
		}
		noStore(t, dataDir)
	})

	t.Run("offline, the environment says on over a file that says off", func(t *testing.T) {
		t.Setenv("BRIDGE_ENRICH_ENABLED", "true")
		cfgPath, _ := writeRetryCLIConfig(t, offline(t), "enrich:\n  enabled: false\n")
		code, stdout, stderr := run(t, cfgPath)
		if code != 0 || !strings.Contains(stdout, "(no bridge running)") {
			t.Errorf("exit %d, stdout %q, stderr %q; want the offline retry to run", code, stdout, stderr)
		}
	})

	t.Run("a running bridge with enrichment off, the CLI's environment on", func(t *testing.T) {
		b := startConsoleBridge(t, "enrich:\n  enabled: false\n", nil)
		t.Setenv("BRIDGE_ENRICH_ENABLED", "true")
		cfgPath, dataDir := writeRetryCLIConfig(t, strings.TrimPrefix(b.adminBase, "http://"), "")
		code, stdout, stderr := run(t, cfgPath)
		if code != 1 {
			t.Errorf("exit %d, want 1: the running bridge refused it", code)
		}
		for _, want := range []string{"the running bridge refused it (HTTP 409, enrichment-off)", "enrich.enabled"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr does not relay %q:\n%s", want, stderr)
			}
		}
		if strings.Contains(stdout, "re-queued") {
			t.Errorf("stdout says it re-queued: %q", stdout)
		}
		noStore(t, dataDir)
	})

	t.Run("a running bridge with enrichment on, the CLI's environment off", func(t *testing.T) {
		b := startConsoleBridge(t, "", nil)
		t.Setenv("BRIDGE_ENRICH_ENABLED", "false")
		cfgPath, dataDir := writeRetryCLIConfig(t, strings.TrimPrefix(b.adminBase, "http://"), "")
		code, stdout, stderr := run(t, cfgPath)
		if code != 0 || !strings.Contains(stdout, "re-queued 0 tracks via the running bridge") {
			t.Errorf("exit %d, stdout %q, stderr %q; want the running bridge's retry relayed", code, stdout, stderr)
		}
		noStore(t, dataDir)
	})
}

// TestTheCLIRetrySaysWhatTheRunningBridgeSkipped: the CLI words a running
// bridge's answer facet by facet (backlog B617): with enrichment off, that no
// track was re-queued and what the harvest's facet did; with Deezer off, that
// no track was re-queued for its photo; the scoped route's artist-image
// resets counted with its tracks, and its booklets.
func TestTheCLIRetrySaysWhatTheRunningBridgeSkipped(t *testing.T) {
	cases := []struct {
		name, answer string
		want, never  []string
	}{
		{"enrichment off, the harvest re-submitted", `{"resetTracks":0,"harvestResubmitted":true,"enrichmentOff":true}`,
			[]string{"enrichment is off on the running bridge (enrich.enabled)", "no track was re-queued", "re-submitted to Atlas"},
			[]string{"re-queued 0"}},
		{"Deezer off", `{"resetTracks":4,"deezerOff":true}`,
			[]string{"re-queued 4 tracks", "Deezer is off on the running bridge (enrich.deezerEnabled)"},
			[]string{"enrichment is off"}},
		{"scoped, with booklets", `{"resetTracks":2,"artistImageResets":3,"bookletChecksReset":1,"bookletFetchNudges":2,"enrichmentOff":false}`,
			[]string{"re-queued 5 tracks", "booklets: 1 check(s) re-armed, 2 fetch(es) nudged"},
			[]string{"Deezer is off", "enrichment is off"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.answer))
			}))
			defer srv.Close()
			var stdout, stderr bytes.Buffer
			if code := retryViaAdmin(context.Background(), strings.TrimPrefix(srv.URL, "http://"), "", &stdout, &stderr); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, stderr.String())
			}
			for _, w := range c.want {
				if !strings.Contains(stdout.String(), w) {
					t.Errorf("stdout lacks %q:\n%s", w, stdout.String())
				}
			}
			for _, n := range c.never {
				if strings.Contains(stdout.String(), n) {
					t.Errorf("stdout says %q:\n%s", n, stdout.String())
				}
			}
		})
	}
}
