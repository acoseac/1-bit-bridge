package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

func TestServeBacksUpItsDurableReservationAndRefusesRestore(t *testing.T) {
	needsDirectorySync(t)
	ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn()})
	e := ps.runningEngine(t)
	id, err := config.ReadPlayerUUID(ps.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	var lastID uint32
	for i := 0; i < 8; i++ {
		lastID = queue(t, e, 0, someReference(t, "entry"))
	}
	lastToken := e.Snapshot().Token()
	snap := runBackup(t, ps.configPath)
	mark, err := engine.ReadReservation(filepath.Join(snap, playerReservationFile))
	if err != nil {
		t.Fatal(err)
	}
	if mark.UUID != id || mark.IDs < lastID || mark.Tokens < lastToken {
		t.Fatalf("snapshot %+v does not cover %d/%d under %s", mark, lastID, lastToken, id)
	}
	// A different, large snapshot mark would be visible if restore got
	// past the lock. Only read the live reservation after backup; no more
	// edits run, and a possible ahead write can only increase it.
	b, err := json.Marshal(engine.Reservation{Version: 2, UUID: id, IDs: 1000000, Tokens: 2000000})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, playerReservationFile), b, 0600); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runRestoreYes(t, ps.configPath, snap)
	if code == 0 || !strings.Contains(stderr, "already running") || strings.Contains(out, "Restore complete") {
		t.Fatalf("live restore: %d %s %s", code, out, stderr)
	}
	live, err := engine.ReadReservation(filepath.Join(ps.dataDir, playerReservationFile))
	if err != nil {
		t.Fatal(err)
	}
	if live.IDs >= 1000000 || live.Tokens >= 2000000 || e.Snapshot().Token() != lastToken {
		t.Fatalf("refused live restore changed counters: %+v", live)
	}
	if got, err := config.ReadPlayerUUID(ps.dataDir); err != nil || got != id {
		t.Fatalf("refused live restore changed identity: %s %v", got, err)
	}
	if code := ps.stopServe(t); code != 0 {
		t.Fatalf("stop: %d", code)
	}
	code, out, stderr = runRestoreYes(t, ps.configPath, snap)
	if code != 0 {
		t.Fatalf("stopped restore: %d %s %s", code, out, stderr)
	}
	written, err := engine.ReadReservation(filepath.Join(ps.dataDir, playerReservationFile))
	if err != nil {
		t.Fatal(err)
	}
	if written.IDs < 1000000+(1<<24) || written.Tokens < 2000000+(1<<24) {
		t.Fatalf("CLI did not jump: %+v", written)
	}
	ps2 := bootPlayerServeAt(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn()}, filepath.Dir(ps.dataDir), ps.music, ps.dataDir)
	e2 := ps2.runningEngine(t)
	if got := e2.Snapshot().Token(); got != written.Tokens+4097 {
		t.Fatalf("serve token %d after %+v", got, written)
	}
	if got := queue(t, e2, 0, someReference(t, "after-restore")); got != written.IDs+4097 {
		t.Fatalf("serve ID %d after %+v", got, written)
	}
}

func TestServeRefusedMintLeavesTheReservationFloor(t *testing.T) {
	needsDirectorySync(t)
	for _, counter := range []string{"ids", "tokens"} {
		t.Run(counter, func(t *testing.T) {
			var before []byte
			ps := bootPlayerServe(t, playerServeCase{player: goBarPlayer, standIn: newPlayerStandIn(), before: func(dir string) {
				m := engine.Reservation{Version: 2, UUID: "7c9e6679-7425-40de-944b-e07fc1f90ae7", IDs: 1000, Tokens: 2000}
				if counter == "ids" {
					m.IDs = math.MaxUint32
				} else {
					m.Tokens = math.MaxUint32
				}
				var err error
				before, err = json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, playerReservationFile), before, 0600); err != nil {
					t.Fatal(err)
				}
			}})
			if ps.engine(t) != nil {
				t.Fatal("exhausted player started")
			}
			if _, err := os.Stat(filepath.Join(ps.dataDir, config.PlayerUUIDFile)); !os.IsNotExist(err) {
				t.Fatalf("refused mint retained identity: %v", err)
			}
			if !bytes.Equal(before, readFileT(t, filepath.Join(ps.dataDir, playerReservationFile))) {
				t.Fatal("mint undo removed or lowered reservation")
			}
		})
	}
}

func TestRestoreCLIPrintsTheNoMarkAdvice(t *testing.T) {
	needsDirectorySync(t)
	cfg, dir, _ := seedRestorableInstall(t, "127.0.0.1:0")
	if _, _, err := config.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	snap := runBackup(t, cfg)
	code, out, stderr := runRestoreYes(t, cfg, snap)
	if code != 0 || !strings.Contains(out, "no player reservation mark") || !strings.Contains(out, "newest snapshot") {
		t.Fatalf("restore %d, %s, %s", code, out, stderr)
	}
	if !strings.Contains(out, "Restore complete") {
		t.Fatal("restore did not finish")
	}
	// The same production path takes legacy schema 1 and still jumps.
	var m backup.Manifest
	if err := json.Unmarshal(readFileT(t, filepath.Join(snap, backup.ManifestFile)), &m); err != nil {
		t.Fatal(err)
	}
	m.SchemaVersion = 1
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, backup.ManifestFile), b, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := engine.ReadReservation(filepath.Join(dir, playerReservationFile))
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr = runRestoreYes(t, cfg, snap)
	if code != 0 {
		t.Fatalf("legacy restore %d, %s, %s", code, out, stderr)
	}
	after, err := engine.ReadReservation(filepath.Join(dir, playerReservationFile))
	if err != nil {
		t.Fatal(err)
	}
	if after.IDs < before.IDs+(1<<24) || after.Tokens < before.Tokens+(1<<24) {
		t.Fatalf("legacy restore lowered marks: %+v -> %+v", before, after)
	}
}
