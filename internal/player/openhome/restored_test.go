package openhome

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// servesPlayer starts a test player on dir under the identity uuid, with
// serve's files beside player-uuid (player-queue.json and player-ids.json,
// engine.ReservationFileName): the reservation a backup carries and a
// restore raises. It returns why the engine does not start, if it does not.
func servesPlayer(tb testing.TB, dir, uuid string) (*testPlayer, error) {
	tb.Helper()
	p := &testPlayer{tb: tb, dir: dir, uuid: uuid, queue: "player-queue.json", reserve: engine.ReservationFileName, lib: newTestLibrary()}
	return p, p.start()
}

// ui4Value is a ui4 a reply carries.
func ui4Value(tb testing.TB, what, text string) uint32 {
	tb.Helper()
	v, err := strconv.ParseUint(text, 10, 32)
	if err != nil {
		tb.Fatalf("%s %q is no ui4: %v", what, text, err)
	}
	return uint32(v)
}

// TestARestoredIdentityHandsOutValuesPastItsSnapshot pins, through the SOAP
// layer, that the restored identity's policy (B470 PR 5a0, #1331) holds for
// the reads: a box restored from a snapshot onto a fresh data directory hands
// a controller, in its first IdArray and Id, a token and an ID above every
// one the snapshot holds (its reservation, which bounds every value its
// source handed out before it was taken); the source's last token reads as
// changed, and an ID the source handed out as none the queue holds.
func TestARestoredIdentityHandsOutValuesPastItsSnapshot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the engine keeps its IDs durable with a directory fsync, which Windows has not: the player runs on Linux")
	}
	// The identity's minter, the engine and the restore refuse a data
	// directory on storage that does not outlive a reboot, as a TMPDIR on
	// tmpfs is (atomicwrite.ErrVolatile), and the first of them is the mint
	// below: the medium is not what this test is about.
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
	source := filepath.Join(t.TempDir(), "source")
	must(t, "the source's data directory", os.Mkdir(source, 0o700))
	identity := filepath.Join(source, config.PlayerUUIDFile)
	// The source's identity as serve's start makes it, its floor and its
	// marker first: the engine refuses an identity whose floor is missing,
	// so a bare player-uuid would leave the engine nothing to start on.
	minted, _, err := backup.EnsurePlayerUUID(source)
	must(t, "the source's identity", err)
	// No skip here: the mint above has just probed this directory, so a
	// barrier or mount refusal from the engine now is a regression, not the
	// host (round 14's review).
	src, err := servesPlayer(t, source, minted)
	must(t, "the source's player", err)
	h := src.handler("")
	a, b := src.add("a", nil), src.add("b", nil)
	must(t, "delete a", src.eng.Delete(bg, a))
	c := src.add("c", nil)
	handed := read(t, h, playlist, "IdArray")
	last := ui4Value(t, "the source's token", handed["Token"])
	if got := decodeIDArray(t, handed["Array"]); !slices.Equal(got, []uint32{b, c}) {
		t.Fatalf("the source's queue %v, want %d and %d", got, b, c)
	}
	src.stop()

	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: source, PlayerUUID: identity})
	must(t, "the snapshot", err)
	mark, err := engine.ReadReservation(filepath.Join(snap, engine.ReservationFileName))
	must(t, "the snapshot's reservation", err)
	if mark.UUID != minted || mark.IDs < c || mark.Tokens < last {
		t.Fatalf("the snapshot's reservation %+v does not cover the IDs to %d and the tokens to %d its source handed out", mark, c, last)
	}

	restored := filepath.Join(t.TempDir(), "restored")
	must(t, "the fresh data directory", os.Mkdir(restored, 0o700))
	must(t, "the restore", backup.Restore(snap, backup.Targets{PlayerUUID: filepath.Join(restored, config.PlayerUUIDFile)}))
	id, err := config.ReadPlayerUUID(restored)
	must(t, "the restored identity", err)
	if id != minted {
		t.Fatalf("the restored identity %s, want the snapshot's %s", id, minted)
	}
	p, err := servesPlayer(t, restored, id)
	must(t, "the restored player", err)
	h = p.handler("")

	first := read(t, h, playlist, "IdArray")
	if token := ui4Value(t, "the first token", first["Token"]); token <= mark.Tokens {
		t.Errorf("the restored player's first IdArray: token %d, not above the snapshot's %d", token, mark.Tokens)
	}
	if first["Array"] != "" {
		t.Errorf("the restored player's first IdArray: the queue %q, want none (a snapshot never carries one)", first["Array"])
	}
	if got := read(t, h, playlist, "IdArrayChanged", "Token", u(last))["Value"]; got != "1" {
		t.Errorf("the source's last token %d: IdArrayChanged %s on the restored player, want 1", last, got)
	}
	// The first entry after the restore, as 5a4's Insert makes it, is
	// selected in the empty queue.
	n := p.add("after the restore", nil)
	if got := read(t, h, playlist, "Id")["Value"]; got != u(n) || n <= mark.IDs {
		t.Errorf("the restored player's first Id: %s (the entry %d), not above the snapshot's %d", got, n, mark.IDs)
	}
	for _, old := range []uint32{a, b, c} {
		if r := invoke(t, h, playlist, "Read", "Id", u(old)); r.fault != 800 {
			t.Errorf("Read(%d), an ID the source handed out: %d, fault %d, want 800", old, r.status, r.fault)
		}
	}
}
