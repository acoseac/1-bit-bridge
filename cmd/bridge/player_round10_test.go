package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// damagedMarkerWarning is the move-aside warning a start's preflight logs
// beside a floor that names the current UUID.
const damagedMarkerWarning = "player initialization marker is damaged"

// damageMarker truncates dir's player-initialized.
func damageMarker(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "player-initialized"), nil, 0600); err != nil {
		t.Fatal(err)
	}
}

// TestServeBootRefusesALostFloorWithoutTheMoveAsideWarning: a UUID beside a
// damaged marker and no floor is floor loss. Serve's boot turns the player
// off with the discard remedy and the phrase that keeps the marker, and logs
// no move-aside warning, whose remedy there would reissue IDs and tokens.
func TestServeBootRefusesALostFloorWithoutTheMoveAsideWarning(t *testing.T) {
	for _, enabled := range []string{"false", "true"} {
		t.Run(enabled, func(t *testing.T) {
			cfg, dir := resetInstall(t, "127.0.0.1:0")
			damageMarker(t, dir)
			if err := os.Remove(filepath.Join(dir, playerReservationFile)); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			// End boot at TLS, after the player's preflight.
			if _, err := f.WriteString("output:\n  player:\n    enabled: " + enabled + "\ntlsCertPath: " + dir + "\ntlsKeyPath: " + dir + "\n"); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			rec := loggingtest.Record(t)
			var out, errs bytes.Buffer
			if code := runServe(t.Context(), serveOpts{configPath: cfg, playerGOOS: "linux"}, &out, &errs); code != 1 || !strings.Contains(errs.String(), "TLS material") {
				t.Fatalf("boot exit %d: %s", code, &errs)
			}
			lines := rec.Lines("player: not started")
			if len(lines) != 1 || !strings.Contains(lines[0], "do not move player-initialized aside") || !strings.Contains(lines[0], "--discard-floor") {
				t.Errorf("floor-loss reason: %v", lines)
			}
			for _, line := range rec.All() {
				if strings.Contains(line, "player initialization marker") || strings.Contains(line, "re-ensures") {
					t.Errorf("move-aside warning beside a lost floor: %s", line)
				}
			}
		})
	}
}

// TestServeWarnsOnceEachBootBesideADamagedMarker: beside a floor that names
// the UUID, each boot's preflight logs the move-aside warning once, its
// automatic player start adds none, and a second boot in the same process
// logs it again.
func TestServeWarnsOnceEachBootBesideADamagedMarker(t *testing.T) {
	needsDirectorySync(t)
	persistentMedium(t)
	rec := loggingtest.Record(t)
	c := playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(), before: func(dir string) {
		if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
			t.Fatal(err)
		}
		damageMarker(t, dir)
	}}
	ps := bootPlayerServe(t, c)
	for boot := 1; boot <= 2; boot++ {
		ps.runningEngine(t) // the boot's automatic start has run
		lines := rec.Lines(damagedMarkerWarning)
		if len(lines) != boot || !strings.Contains(lines[boot-1], "move it aside") || strings.Contains(lines[boot-1], "--discard-floor") {
			t.Fatalf("after boot %d: %v", boot, lines)
		}
		if code := ps.stopServe(t); code != 0 {
			t.Fatalf("serve exit %d", code)
		}
		if boot == 1 {
			again := c
			again.standIn = newPlayerStandIn()
			ps = bootPlayerServeAt(t, again, filepath.Dir(ps.configPath), ps.music, ps.dataDir)
		}
	}
	if len(rec.Lines(damagedMarkerWarning)) != 2 {
		t.Fatalf("stopping serve warned: %v", rec.Lines(damagedMarkerWarning))
	}
}

// TestBindWarnsOnceBesideADamagedMarker: an enabled player's bind runs a
// start's preflight once and then mints, and only the preflight warns.
func TestBindWarnsOnceBesideADamagedMarker(t *testing.T) {
	persistentMedium(t)
	root := dacTree(t)
	cfgPath := dacConfig(t, "  player:\n    enabled: true\n")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backup.EnsurePlayerUUID(cfg.DataDir); err != nil {
		t.Fatal(err)
	}
	damageMarker(t, cfg.DataDir)
	rec := loggingtest.Record(t)
	for bind := 1; bind <= 2; bind++ {
		out, errs, code := runDAC(t, dacEnv(), append([]string{"bind", "5", "--binding", "serial", "--config", cfgPath}, treeFlags(root)...)...)
		if code != 0 {
			t.Fatalf("bind %d: %d %s %s", bind, code, out, errs)
		}
		if lines := rec.Lines(damagedMarkerWarning); len(lines) != bind {
			t.Fatalf("after bind %d: %v", bind, lines)
		}
	}
}
