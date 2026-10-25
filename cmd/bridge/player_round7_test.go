package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

func upgradeInstall(t *testing.T) (string, string) {
	t.Helper()
	cfg, dir := resetInstall(t, "127.0.0.1:0")
	for _, name := range []string{"player-ids.json", "player-initialized"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	return cfg, dir
}

func TestBackupUpgradeBusyNamesStoppingTheService(t *testing.T) {
	cfg, dir := upgradeInstall(t)
	holdDataDirLock(t, dir)
	var out, errs bytes.Buffer
	if code := backupCmd([]string{"--config", cfg}, &out, &errs); code == 0 || !strings.Contains(errs.String(), "stop the service and run the backup again") {
		t.Fatalf("busy backup: %d %s", code, &errs)
	}
}

func TestServeAdoptsDisabledPlayerBeforeTLS(t *testing.T) {
	cfg, dir := upgradeInstall(t)
	id, err := config.ReadPlayerUUID(dir)
	if err != nil {
		t.Fatal(err)
	}
	// End boot at TLS, after adoption but before a socket is needed.
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("tlsCertPath: " + dir + "\ntlsKeyPath: " + dir + "\n"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := runServe(t.Context(), serveOpts{configPath: cfg}, &out, &errs); code != 1 || !strings.Contains(errs.String(), "TLS material") {
		t.Fatalf("boot exit %d: %s", code, &errs)
	}
	floor, err := engine.ReadReservation(filepath.Join(dir, "player-ids.json"))
	if err != nil || floor.UUID != id || floor.IDs < 1<<24 || floor.Tokens < 1<<24 {
		t.Fatalf("disabled boot did not adopt: %+v %v", floor, err)
	}
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, bytes.Split(body, []byte("tlsCertPath:"))[0], 0600); err != nil {
		t.Fatal(err)
	}
	holdDataDirLock(t, dir)
	out.Reset()
	errs.Reset()
	if code := backupCmd([]string{"--config", cfg}, &out, &errs); code != 0 {
		t.Fatalf("backup after boot adoption: %d %s", code, &errs)
	}
}

func TestLiveBackupOfDisabledPlayerUpgrade(t *testing.T) {
	cfg, dir := upgradeInstall(t)
	bootServe(t, "--config", cfg, "--addr", "127.0.0.1:0")
	release, err := lockServeDataDir(dir)
	if err == nil {
		release()
		t.Fatal("serve did not hold server.lock")
	}
	var out, errs bytes.Buffer
	if code := backupCmd([]string{"--config", cfg}, &out, &errs); code != 0 {
		t.Fatalf("live upgrade backup: %d %s", code, &errs)
	}
}
