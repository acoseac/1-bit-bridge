package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"syscall"

	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
)

func resetInstall(t *testing.T, adminAddr string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("dataDir: %s\nlibraryRoots: [%s]\nadminAddress: %s\n", dir, dir, adminAddr)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	return cfg, dir
}

func TestPlayerResetCommandDispatchAndRestoreRefusal(t *testing.T) {
	cfg, dir := resetInstall(t, "127.0.0.1:0")
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := run(t.Context(), []string{"output", "player", "reset", "--config", cfg, "--yes"}, &out, &errs); code != 0 {
		t.Fatalf("reset exit %d: %s", code, &errs)
	}
	for _, want := range []string{"recorded", "new identity", "new device to controllers"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, &out)
		}
	}
	id, _, err := config.EnsurePlayerUUID(dir)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errs.Reset()
	if code := restoreCmd(t.Context(), []string{"--config", cfg, "--yes", snap}, strings.NewReader(""), &out, &errs); code == 0 || !strings.Contains(errs.String(), "given up") {
		t.Fatalf("restore exit %d: %s", code, &errs)
	}
	if got, _ := config.ReadPlayerUUID(dir); got != id {
		t.Fatal("restore replaced new identity")
	}
}

func TestPlayerResetCommandRequiresExclusiveLock(t *testing.T) {
	for _, kind := range []string{"held", "unavailable"} {
		t.Run(kind, func(t *testing.T) {
			cfg, dir := resetInstall(t, "127.0.0.1:0")
			if kind == "held" {
				holdDataDirLock(t, dir)
			} else {
				blockDataDirLock(t, dir)
			}
			before, err := os.ReadFile(filepath.Join(dir, config.PlayerUUIDFile))
			if err != nil {
				t.Fatal(err)
			}
			var out, errs bytes.Buffer
			code := playerResetCmd(t.Context(), []string{"--config", cfg, "--yes"}, strings.NewReader(""), &out, &errs)
			want := "already running"
			if kind == "unavailable" {
				want = "exclusive lock could not be taken"
			}
			if code == 0 || !strings.Contains(errs.String(), want) {
				t.Fatalf("reset exit %d: %s", code, &errs)
			}
			after, err := os.ReadFile(filepath.Join(dir, config.PlayerUUIDFile))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refused reset changed identity")
			}
			if _, err := os.Stat(filepath.Join(dir, backup.PlayerHistoryFileName)); !os.IsNotExist(err) {
				t.Fatal("refused reset wrote history")
			}
		})
	}
}

type resetPromptReader struct {
	read  func()
	input io.Reader
}

func (r resetPromptReader) Read(p []byte) (int, error) { r.read(); return r.input.Read(p) }

func TestPlayerResetCommandHoldsLockDuringConfirmation(t *testing.T) {
	cfg, dir := resetInstall(t, "127.0.0.1:0")
	called := false
	input := resetPromptReader{input: strings.NewReader("no\n"), read: func() {
		called = true
		release, err := lockServeDataDir(dir)
		if err == nil {
			release()
			t.Error("confirmation held no data-dir lock")
		}
	}}
	var out, errs bytes.Buffer
	if code := playerResetCmd(t.Context(), []string{"--config", cfg}, input, &out, &errs); code != 1 || !called || !strings.Contains(errs.String(), "Type 'yes'") {
		t.Fatalf("confirmation exit %d: %s / %s", code, &out, &errs)
	}
	if _, err := config.ReadPlayerUUID(dir); err != nil {
		t.Fatal("declined confirmation changed identity")
	}
	if _, err := os.Stat(filepath.Join(dir, backup.PlayerHistoryFileName)); !os.IsNotExist(err) {
		t.Fatal("declined confirmation wrote history")
	}
	release, err := lockServeDataDir(dir)
	if err != nil {
		t.Fatal("command leaked lock")
	}
	release()
}

func TestPlayerResetCommandNoIdentityAndHelp(t *testing.T) {
	cfg, dir := resetInstall(t, "127.0.0.1:0")
	id, err := config.ReadPlayerUUID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.ForgetPlayerUUID(dir, id); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := playerResetCmd(t.Context(), []string{"--config", cfg, "--yes"}, strings.NewReader(""), &out, &errs); code != 0 || !strings.Contains(out.String(), "no player identity") {
		t.Fatalf("exit %d: %s / %s", code, &out, &errs)
	}
	if _, err := os.Stat(filepath.Join(dir, backup.PlayerHistoryFileName)); !os.IsNotExist(err) {
		t.Fatal("empty reset wrote history")
	}
	out.Reset()
	errs.Reset()
	run(t.Context(), []string{"--help"}, &out, &errs)
	if !strings.Contains(out.String()+errs.String(), "bridge output player reset") {
		t.Fatal("help omits reset")
	}
}

type resetProbeTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
}

func (rt resetProbeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return rt.roundTrip(r)
}

func TestPlayerResetCommandProbesAdminBeforeAndAfterPrompt(t *testing.T) {
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			cfg, dir := resetInstall(t, "127.0.0.1:7789")
			answering := phase == "before"
			prompted := false
			calls := 0
			old := http.DefaultTransport
			http.DefaultTransport = resetProbeTransport{roundTrip: func(r *http.Request) (*http.Response, error) {
				calls++
				if !answering {
					return nil, syscall.ECONNREFUSED
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
			}}
			defer func() { http.DefaultTransport = old }()
			input := resetPromptReader{input: strings.NewReader("yes\n"), read: func() { prompted = true; answering = true }}
			var out, errs bytes.Buffer
			if code := playerResetCmd(t.Context(), []string{"--config", cfg}, input, &out, &errs); code == 0 || !strings.Contains(errs.String(), "a bridge is answering") {
				t.Fatalf("reset while admin answers: %s / %s", &out, &errs)
			}
			if phase == "before" && prompted {
				t.Error("running admin was not refused before confirmation")
			}
			want := 1
			if phase == "after" {
				want = 2
			}
			if calls != want {
				t.Fatalf("probes %d want %d", calls, want)
			}
			if _, err := config.ReadPlayerUUID(dir); err != nil {
				t.Fatal("probe refusal removed identity")
			}
			if _, err := os.Stat(filepath.Join(dir, backup.PlayerHistoryFileName)); !os.IsNotExist(err) {
				t.Fatal("probe refusal wrote history")
			}
		})
	}
}
