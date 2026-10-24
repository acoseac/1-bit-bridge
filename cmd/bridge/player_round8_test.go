package main

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

func damageBootPlayer(t *testing.T, dir, kind string) {
	t.Helper()
	if _, err := backup.DiscardPlayerFloor(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "adoption":
		for _, name := range []string{"player-ids.json", "player-initialized", "player-floor-discarded.json"} {
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
		}
		previous := atomicwrite.SetRenameFuncForTest(func(a, b string) error {
			if filepath.Base(b) == "player-ids.json" {
				return errors.New("player-ids.json adoption write refused")
			}
			return os.Rename(a, b)
		})
		t.Cleanup(func() { atomicwrite.SetRenameFuncForTest(previous) })

	case "damaged marker":
		if err := os.WriteFile(filepath.Join(dir, "player-initialized"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	case "completion":
		if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "player-floor-discarded.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, "player-floor-discarded.json"), 0700); err != nil {
			t.Fatal(err)
		}
	case "preflight":
		if err := os.WriteFile(filepath.Join(dir, "player-ids.json"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPlayerFileErrorsDoNotStopServeBeforeTLS(t *testing.T) {
	for _, kind := range []string{"completion", "preflight", "adoption"} {
		for _, enabled := range []string{"false", "true"} {
			t.Run(kind+"/"+enabled, func(t *testing.T) {
				cfg, dir := resetInstall(t, "127.0.0.1:0")
				damageBootPlayer(t, dir, kind)
				f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString("output:\n  player:\n    enabled: " + enabled + "\ntlsCertPath: " + dir + "\ntlsKeyPath: " + dir + "\n")
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				rec := loggingtest.Record(t)
				var out, errs bytes.Buffer
				code := runServe(t.Context(), serveOpts{configPath: cfg, playerGOOS: "linux"}, &out, &errs)
				if code != 1 || !strings.Contains(errs.String(), "TLS material") {
					t.Fatalf("player files stopped bridge boot: %d %s", code, &errs)
				}
				if lines := rec.Lines("player: not started"); len(lines) != 1 || !strings.Contains(lines[0], "player-") {
					t.Fatalf("missing player file reason: %v", lines)
				}
			})
		}
	}
}

func TestServeKeepsLibraryWithDamagedSealedPlayerMarker(t *testing.T) {
	needsDirectorySync(t)
	for _, sealed := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			name, settings := "disabled", "    enabled: false\n"
			if enabled {
				name, settings = "enabled", goBarPlayer
			}
			if sealed {
				name += "/sealed"
			}
			t.Run(name, func(t *testing.T) {
				rec := loggingtest.Record(t)
				standIn := newPlayerStandIn()
				ps := bootPlayerServe(t, playerServeCase{player: settings, standIn: standIn, before: func(dir string) {
					damageBootPlayer(t, dir, "damaged marker")
					if !sealed {
						if err := os.Remove(filepath.Join(dir, "player-floor-discarded.json")); err != nil {
							t.Fatal(err)
						}
					}
				}})
				if code := ps.health(t); code != http.StatusOK {
					t.Fatalf("library health beside damaged marker: %d", code)
				}
				eng := ps.engine(t) // Join startup before reading the backend and its outcome.
				if ps.player.plan.start != enabled || standIn.built != enabled || (eng != nil) != enabled {
					t.Fatalf("damaged player's plan: %+v", ps.player.plan)
				}
				// Boot's preflight warns once, covering the player's start (B524 round 10).
				if lines := rec.Lines("player initialization marker is damaged"); len(lines) != 1 || !strings.Contains(lines[0], "player-initialized") || !strings.Contains(lines[0], "move it aside") || strings.Contains(lines[0], "--discard-floor") {
					t.Fatalf("want boot's one warning: %v", lines)
				}
				var out, errs bytes.Buffer
				if code := backupCmd([]string{"--config", ps.configPath}, &out, &errs); code != 0 {
					t.Fatalf("live backup: %d %s", code, &errs)
				}
				if lines := rec.Lines("player initialization marker is damaged"); len(lines) != 1 {
					t.Fatalf("the live backup warned again: %v", lines)
				}
				if code := ps.stopServe(t); code != 0 {
					t.Fatalf("serve exit %d", code)
				}
			})
		}
	}
}
