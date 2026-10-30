package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// blockedUnder is a directory below root whose parent is a regular file, so
// no probe can read it: its stat fails with ENOTDIR, which the probes do not
// take for "not there" and do not walk past, and on Windows the free-space
// probe of the file it resolves to answers ERROR_DIRECTORY.
func blockedUnder(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "a file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(file, "variants")
}

// probeCause is what a probe's failure says of the OS on this platform: the
// call that failed.
func probeCause() string {
	if runtime.GOOS == "windows" {
		return "GetDiskFreeSpaceEx"
	}
	return "stat"
}

// row is rel's track row, as a stale download's re-render looks it up.
func (b *stampBridge) row(t *testing.T, rel string) *manifest.Track {
	t.Helper()
	tr, err := b.store.LookupTrack(context.Background(), rel)
	if err != nil || tr == nil {
		t.Fatalf("LookupTrack(%s): %v, %v", rel, tr, err)
	}
	return tr
}

// requestRefused is a client's POST /v1/upscale for one rendition of rel,
// which the bridge refuses: 202, nothing queued, one rejected.
func (b *stampBridge) requestRefused(t *testing.T, rel, kind string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"path": rel, "kind": kind})
	resp := b.do(t, http.MethodPost, "/v1/upscale", body)
	defer resp.Body.Close()
	var got api.UpscaleResponse
	if resp.StatusCode != http.StatusAccepted || json.NewDecoder(resp.Body).Decode(&got) != nil {
		t.Fatalf("POST /v1/upscale %s %s: status %d", kind, rel, resp.StatusCode)
	}
	if got.Enqueued != 0 || got.Rejected != 1 {
		t.Fatalf("POST /v1/upscale %s %s queued %d and rejected %d, want it refused", kind, rel, got.Enqueued, got.Rejected)
	}
}

// requireLine fails the test unless logs holds exactly one record of msg at
// level, whose err attribute names each of want, and returns it.
func requireLine(t *testing.T, logs *loggingtest.Recorder, msg string, level slog.Level, want ...string) slog.Record {
	t.Helper()
	var got []slog.Record
	for _, rec := range logs.Records() {
		if rec.Message == msg {
			got = append(got, rec)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%q was logged %d times, want once:\n%s", msg, len(got), strings.Join(logs.All(), "\n"))
	}
	rec := got[0]
	if rec.Level != level {
		t.Errorf("%q was logged at %v, want %v", msg, rec.Level, level)
	}
	var errText string
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == "err" {
			errText = a.Value.Resolve().String()
		}
		return true
	})
	for _, w := range want {
		if !strings.Contains(errText, w) {
			t.Errorf("%q gives err=%q, which does not name %q", msg, errText, w)
		}
	}
	return rec
}

// TestARenditionTheVolumesRefuseNamesNoDirectory: a rendition the on-demand
// pre-flight refuses, for want of room or because a probe could not read a
// volume, is logged by POST /v1/upscale and by a stale download's re-render
// with the directory's role, the numbers or the probe's operation and
// errno, and never the directory: the variants directory and the render
// scratch are the operator's, and a log line names no absolute path (#1055).
// The no-room refusal is an InsufficientDiskSpaceError, whose text quoted the
// directory, and a probe failure was AvailableDiskSpace's `statfs %q` (on
// Windows `GetDiskFreeSpaceEx %q`), which the request logged at Error and
// the re-render at Warn; the no-room line of the re-render is at Debug.
func TestARenditionTheVolumesRefuseNamesNoDirectory(t *testing.T) {
	t.Run("the request finds no room on the variants volume", func(t *testing.T) {
		logs := loggingtest.Record(t)
		b := newStampBridge(t)
		root := filepath.Join(t.TempDir(), "Private Volume")
		b.adapter.outputDir = func() string { return filepath.Join(root, "variants") }
		b.adapter.diskFree = func(string) (int64, error) { return 1, nil }
		b.requestRefused(t, stampPCM, "optimize")
		requireLine(t, logs, "upscale: not enough free space for the rendition; nothing queued for it", slog.LevelWarn,
			transcode.ProbeRoleVariants, "1 available")
		if leaked := logs.Carrying(root); len(leaked) != 0 {
			t.Errorf("a line names the variants directory:\n%s", strings.Join(leaked, "\n"))
		}
	})
	t.Run("the request cannot probe the variants volume", func(t *testing.T) {
		logs := loggingtest.Record(t)
		b := newStampBridge(t)
		root := filepath.Join(t.TempDir(), "Private Volume")
		blocked := blockedUnder(t, root)
		b.adapter.outputDir = func() string { return blocked }
		b.requestRefused(t, stampPCM, "optimize")
		requireLine(t, logs, "upscale: enqueue failed", slog.LevelError,
			"check free space for the rendition", transcode.ProbeRoleVariants, probeCause())
		if leaked := logs.Carrying(root); len(leaked) != 0 {
			t.Errorf("a line names the variants directory:\n%s", strings.Join(leaked, "\n"))
		}
	})
	t.Run("a DSD request cannot probe the variants volume", func(t *testing.T) {
		logs := loggingtest.Record(t)
		b := newStampBridge(t)
		root := filepath.Join(t.TempDir(), "Private Volume")
		blocked := blockedUnder(t, root)
		b.adapter.outputDir = func() string { return blocked }
		b.requestRefused(t, stampDSD, "optimize")
		requireLine(t, logs, "upscale: enqueue failed", slog.LevelError,
			"check free space for the rendition", transcode.ProbeRoleVariants, probeCause())
		if leaked := logs.Carrying(root); len(leaked) != 0 {
			t.Errorf("a line names the variants directory:\n%s", strings.Join(leaked, "\n"))
		}
	})
	t.Run("a stale download's re-render finds no room", func(t *testing.T) {
		logs := loggingtest.Record(t)
		b := newStampBridge(t)
		root := filepath.Join(t.TempDir(), "Private Volume")
		b.adapter.outputDir = func() string { return filepath.Join(root, "variants") }
		b.adapter.diskFree = func(string) (int64, error) { return 1, nil }
		b.heal.render(context.Background(), b.row(t, stampPCM), stampPCMCompact)
		requireLine(t, logs, "a download found a rendition stale; no room to render it again", slog.LevelDebug,
			transcode.ProbeRoleVariants, "1 available")
		if leaked := logs.Carrying(root); len(leaked) != 0 {
			t.Errorf("a line names the variants directory:\n%s", strings.Join(leaked, "\n"))
		}
	})
	t.Run("a stale download's re-render cannot probe the variants volume", func(t *testing.T) {
		logs := loggingtest.Record(t)
		b := newStampBridge(t)
		root := filepath.Join(t.TempDir(), "Private Volume")
		blocked := blockedUnder(t, root)
		b.adapter.outputDir = func() string { return blocked }
		b.heal.render(context.Background(), b.row(t, stampPCM), stampPCMCompact)
		requireLine(t, logs, "a download found a rendition stale, and asking for its render failed", slog.LevelWarn,
			"check free space for the rendition", transcode.ProbeRoleVariants, probeCause())
		if leaked := logs.Carrying(root); len(leaked) != 0 {
			t.Errorf("a line names the variants directory:\n%s", strings.Join(leaked, "\n"))
		}
	})
}
