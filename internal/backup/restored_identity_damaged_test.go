package backup_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// TestRestoreRefusesADamagedListedReservation: when the manifest lists the
// player reservation, a copy that cannot be read, or a v2 mark that names a
// different player than the identity, is damage — the restore refuses and
// writes nothing, rather than dropping the snapshot floor and letting the
// restored identity reissue IDs and tokens below its true high-water mark
// (CodeRabbit on PR #1331, reproduced). A v1 mark has no UUID and is still a
// floor, as a live floor is.
func TestRestoreRefusesADamagedListedReservation(t *testing.T) {
	reservationStorage(t)

	// damage overwrites the snapshot's listed player-ids.json in place; the
	// manifest still lists it.
	write := func(t *testing.T, snap string, b []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(snap, engine.ReservationFileName), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, snap string)
		refuse bool
	}{
		{"corrupt", func(t *testing.T, snap string) { write(t, snap, []byte("}{garbage")) }, true},
		{"truncated", func(t *testing.T, snap string) { write(t, snap, nil) }, true},
		{"uuid_mismatch", func(t *testing.T, snap string) {
			write(t, snap, []byte(fmt.Sprintf(`{"version":2,"uuid":%q,"ids":77,"tokens":88}`, boxPlayerUUID)))
		}, true},
		{"v1_floor_ok", func(t *testing.T, snap string) {
			write(t, snap, []byte(`{"version":1,"ids":55555,"tokens":66666}`))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := identitySnapshot(t, true)
			tc.damage(t, snap)
			dst := t.TempDir() // fresh box: no live reservation to fall back on
			err := fixedRestore(t, snap, dst, 123)
			resPath := filepath.Join(dst, engine.ReservationFileName)
			if tc.refuse {
				if err == nil {
					t.Fatalf("restore accepted a damaged listed reservation; floor now %+v", markAt(t, dst))
				}
				if !strings.Contains(err.Error(), engine.ReservationFileName) {
					t.Errorf("refusal does not name the reservation file: %v", err)
				}
				if _, statErr := os.Stat(resPath); statErr == nil {
					t.Fatal("restore wrote a reservation despite refusing")
				}
				if _, statErr := os.Stat(filepath.Join(dst, config.PlayerUUIDFile)); statErr == nil {
					t.Fatal("restore wrote the identity despite refusing")
				}
				return
			}
			if err != nil {
				t.Fatalf("restore refused a valid v1 floor: %v", err)
			}
			got := markAt(t, dst)
			wantI, wantT := uint32(55555)+(1<<24)+123, uint32(66666)+(1<<24)+123
			if got.IDs != wantI || got.Tokens != wantT || got.UUID != snapshotPlayerUUID {
				t.Fatalf("v1 floor not carried: got %+v, want %d/%d", got, wantI, wantT)
			}
		})
	}
}

// TestRestoreOfACorruptReservationKeepsTheFloorFromReissuing is the end the
// refusal protects: a snapshot taken at a high floor whose reservation copy is
// later corrupted must not restore the identity at the bare jump.
func TestRestoreOfACorruptReservationKeepsTheFloorFromReissuing(t *testing.T) {
	reservationStorage(t)
	dir := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(dir, config.PlayerUUIDFile), snapshotPlayerUUID); err != nil {
		t.Fatal(err)
	}
	putMark(t, dir, snapshotPlayerUUID, 2_000_000_000, 2_000_000_000)
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: dir, PlayerUUID: filepath.Join(dir, config.PlayerUUIDFile)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, engine.ReservationFileName), []byte("}{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := fixedRestore(t, snap, dst, 1); err == nil {
		t.Fatalf("restore accepted a corrupt 2e9 floor; restored at %+v", markAt(t, dst))
	}
	if _, statErr := os.Stat(filepath.Join(dst, engine.ReservationFileName)); statErr == nil {
		t.Fatal("a reservation was written below the true floor")
	}
}
