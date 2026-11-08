package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A configured library root whose path is not there at all (backlog B776's
// round 2). A loopback serve refuses to start over it
// (CheckLibraryRootsAccessible), and a public one starts, warning, and serves
// a console whose first upload into it made the root's folder; B776's guard
// refused that upload as a volume not mounted. The write's admission takes it
// again where nothing is at the path and the index's count of the root's rows
// answers none. With rows, the folder made would hold the upload alone and
// the scans would then reap them (B776's harm: a removed mount point, a
// renamed folder), so that upload is refused, nothing made.

// TestALoopbackServeRefusesARootWhosePathIsNotThere: where a missing root's
// first upload cannot be reached from, a loopback serve's start.
func TestALoopbackServeRefusesARootWhosePathIsNotThere(t *testing.T) {
	cfgPath := writeValidConfig(t)
	lib := filepath.Join(filepath.Dir(cfgPath), "Music")
	if err := os.RemoveAll(lib); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runCapture(t, "serve", "--config", cfgPath)
	if code != 2 || !strings.Contains(stderr, "libraryRoots[") {
		t.Fatalf("a loopback serve over a root that is not there = %d, stderr %q; want 2, naming the root", code, stderr)
	}
}

// publicServeOverRoot inits a public install over a library folder of its own,
// lets prepare change it first (index it, take it away), turns uploads on,
// starts `bridge serve` and signs in to its console, returning the console,
// the session, the library folder and serve's stderr.
func publicServeOverRoot(t *testing.T, prepare func(lib, cfgPath string)) (*liveConsole, string, string, *safeBuffer) {
	t.Helper()
	cfgDir := t.TempDir()
	lib := filepath.Join(cfgDir, "Music")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	ports := pickPublicInitPorts(t)
	code, out := publicInit(t, cfgDir, ports, "Live", "--library", lib)
	if code != 0 {
		t.Fatalf("bridge init --public = %d:\n%s", code, out)
	}
	password := mintedPassword(t, out)
	cfgPath := filepath.Join(cfgDir, "bridge.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\nupload:") || strings.Contains(string(raw), "\ndisableHttp3:") {
		t.Fatalf("premise: the init wrote a block this test appends:\n%s", raw)
	}
	// UDP is not what this is about, and the port picked free for TCP is not
	// known to be free for UDP.
	appendToFile(t, cfgPath, "disableHttp3: true\nupload:\n  enabled: true\n")
	prepare(lib, cfgPath)

	console := newLiveConsole(ports.admin)
	ctx, cancel := context.WithCancel(context.Background())
	stdout, stderr := &safeBuffer{}, &safeBuffer{}
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		done <- run(ctx, []string{"serve", "--config", cfgPath}, stdout, stderr)
	}()
	drainServeOnCleanup(t, cancel, exited, done, stderr)
	waitForAdminReady(t, console.addr, done, stderr)
	return console, console.login(t, password, http.StatusOK, stderr), lib, stderr
}

// postUpload posts an upload of one four-byte file and answers its status and
// its body.
func postUpload(t *testing.T, c *liveConsole, session string, stderr *safeBuffer) (int, map[string]any) {
	t.Helper()
	resp := c.do(t, http.MethodPost, "/api/upload/sessions", `{"files":[{"path":"Album/01.flac","size":4}]}`, session, stderr)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// TestAPublicBridgesFirstUploadIntoARootNotMadeYetMakesIt: a public serve
// started over a root whose path is not there, and that the index holds
// nothing of, takes the first upload into it, which makes the root's folder.
func TestAPublicBridgesFirstUploadIntoARootNotMadeYetMakesIt(t *testing.T) {
	console, session, lib, stderr := publicServeOverRoot(t, func(lib, _ string) {
		if err := os.RemoveAll(lib); err != nil {
			t.Fatal(err)
		}
	})
	code, out := postUpload(t, console, session, stderr)
	if code != http.StatusCreated {
		t.Fatalf("the first upload into a root not made yet, the index holding nothing of it = %d %v, want 201", code, out)
	}
	entries, err := os.ReadDir(lib)
	if err != nil {
		t.Fatalf("the upload made no folder for the root: %v", err)
	}
	if !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == ".bridge-upload" }) {
		t.Errorf("the root's folder holds %v, want the upload's staging", entries)
	}
}

// TestAPublicBridgeRefusesAnUploadIntoAGoneRootTheIndexHoldsRowsUnder: the
// same start over a root the index holds a track of, its folder gone (a
// mount point removed, a folder renamed): the upload is refused as a volume
// not mounted, and nothing is made at the root's path.
func TestAPublicBridgeRefusesAnUploadIntoAGoneRootTheIndexHoldsRowsUnder(t *testing.T) {
	console, session, lib, stderr := publicServeOverRoot(t, func(lib, cfgPath string) {
		if err := os.MkdirAll(filepath.Join(lib, "Artist", "Album"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lib, "Artist", "Album", "01.flac"), []byte("not a real flac"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, se, code := runCapture(t, "scan", "--config", cfgPath); code != 0 {
			t.Fatalf("bridge scan = %d: %s", code, se)
		}
		if err := os.Rename(lib, filepath.Join(t.TempDir(), "volume")); err != nil {
			t.Fatal(err)
		}
	})
	code, out := postUpload(t, console, session, stderr)
	if code != http.StatusConflict || out["error"] != "root_not_mounted" {
		t.Fatalf("an upload into a root whose folder is gone, the index holding its track = %d %v, want 409 root_not_mounted", code, out)
	}
	if _, err := os.Lstat(lib); !os.IsNotExist(err) {
		t.Errorf("the refused upload made the root's folder: %v", err)
	}
}
