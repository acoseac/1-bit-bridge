package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

func resetInstall(t *testing.T, adminAddr string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("dataDir: %s\nlibraryRoots: [%s]\nadminAddress: %s\n", dir, dir, adminAddr)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(atomicwrite.SetSyncDirForTest(func(string) error { return nil }))
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

func TestPlayerResetCommandDispatchAndRestore(t *testing.T) {
	cfg, dir := resetInstall(t, "127.0.0.1:0")
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, err := config.ReadPlayerUUID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.WriteReservation(dir, filepath.Join(dir, engine.ReservationFileName), engine.Reservation{UUID: snapshotID, IDs: 2000000000, Tokens: 2000000001}); err != nil {
		t.Fatal(err)
	}
	before, err := engine.ReadReservation(filepath.Join(dir, engine.ReservationFileName))
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := run(t.Context(), []string{"reset-player", "--config", cfg, "--yes"}, &out, &errs); code != 0 {
		t.Fatalf("reset exit %d: %s", code, &errs)
	}
	for _, want := range []string{"retained", "new identity", "new device to controllers"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q: %s", want, &out)
		}
	}
	_, _, err = backup.EnsurePlayerUUID(dir)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errs.Reset()
	if code := restoreCmd(t.Context(), []string{"--config", cfg, "--yes", snap}, strings.NewReader(""), &out, &errs); code != 0 {
		t.Fatalf("restore exit %d: %s", code, &errs)
	}
	if got, _ := config.ReadPlayerUUID(dir); got != snapshotID {
		t.Fatalf("restore identity %s want %s", got, snapshotID)
	}
	after, err := engine.ReadReservation(filepath.Join(dir, engine.ReservationFileName))
	if err != nil || after.IDs <= before.IDs || after.Tokens <= before.Tokens {
		t.Fatalf("restored floor %+v, before %+v: %v", after, before, err)
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
			if _, err := os.Stat(filepath.Join(dir, "player-floor-discarded.json")); !os.IsNotExist(err) {
				t.Fatal("refused reset wrote a discard seal")
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
	if _, err := os.Stat(filepath.Join(dir, "player-floor-discarded.json")); !os.IsNotExist(err) {
		t.Fatal("declined confirmation wrote a discard seal")
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
	if code := playerResetCmd(t.Context(), []string{"--config", cfg, "--yes"}, strings.NewReader(""), &out, &errs); code != 0 || !strings.Contains(out.String(), "reset complete") {
		t.Fatalf("exit %d: %s / %s", code, &out, &errs)
	}
	if strings.Contains(out.String(), "removed") {
		t.Fatalf("claimed removal of absent files: %s", &out)
	}
	for _, name := range []string{"player-ids.json", "player-initialized"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	out.Reset()
	errs.Reset()
	if code := playerResetCmd(t.Context(), []string{"--config", cfg, "--yes"}, strings.NewReader(""), &out, &errs); code != 0 || out.String() != "The box has no player identity; nothing changed.\n" {
		t.Fatalf("empty reset exit %d: %s / %s", code, &out, &errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "player-floor-discarded.json")); !os.IsNotExist(err) {
		t.Fatal("empty reset wrote a discard seal")
	}
	out.Reset()
	errs.Reset()
	run(t.Context(), []string{"--help"}, &out, &errs)
	if !strings.Contains(out.String()+errs.String(), "bridge reset-player") {
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
			if _, err := os.Stat(filepath.Join(dir, "player-floor-discarded.json")); !os.IsNotExist(err) {
				t.Fatal("probe refusal wrote a discard seal")
			}
		})
	}
}

func TestResetPlayerIsATopLevelCommand(t *testing.T) {
	cfg, dir := resetInstall(t, "127.0.0.1:0")
	before, err := os.ReadFile(filepath.Join(dir, config.PlayerUUIDFile))
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code := run(t.Context(), []string{"output", "player", "reset", "--config", cfg, "--yes"}, &out, &errs)
	if code != 2 || !strings.Contains(errs.String(), "unknown output player subcommand: reset") {
		t.Errorf("old spelling is not an unknown subcommand: exit %d: %s", code, &errs)
	}
	after, err := os.ReadFile(filepath.Join(dir, config.PlayerUUIDFile))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("old spelling changed identity")
	}
	out.Reset()
	errs.Reset()
	if code := run(t.Context(), []string{"reset-player", "--config", cfg, "--yes"}, &out, &errs); code != 0 {
		t.Fatalf("top-level reset exit %d: %s", code, &errs)
	}
	out.Reset()
	usage(&out)
	const want = `  reset-player
           Rotate the player's identity and clear its queue, keeping the counter
           floor. With the service stopped:
           bridge reset-player [--config X] [--yes] [--discard-floor].
           --discard-floor drops the floor and permanently seals identity restores.
`
	start := strings.Index(out.String(), "  reset-player\n")
	end := strings.Index(out.String(), "  token ")
	if start < 0 || end < start || out.String()[start:end] != want {
		t.Error("reset-player usage differs from its contract")
	}

	out.Reset()
	outputUsage(&out)
	if strings.Contains(out.String(), "player reset") || strings.Contains(out.String(), "reset-player") {
		t.Error("output help lists reset")
	}
}

func TestPlayerResetDiscardFloorCommand(t *testing.T) {
	cfg, dir := resetInstall(t, "127.0.0.1:0")
	var out, errs bytes.Buffer
	if code := playerResetCmd(t.Context(), []string{"--config", cfg, "--discard-floor"}, strings.NewReader("yes\n"), &out, &errs); code != 0 {
		t.Fatalf("discard %d: %s", code, &errs)
	}
	if !strings.Contains(out.String(), "permanently sealed") || strings.Contains(out.String(), "keeping its counter floor") {
		t.Fatalf("discard confirmation: %s", &out)
	}
	if _, err := os.Stat(filepath.Join(dir, "player-floor-discarded.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "player-ids.json")); !os.IsNotExist(err) {
		t.Fatalf("floor remains: %v", err)
	}
}
