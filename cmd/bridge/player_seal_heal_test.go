package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/backup"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// sealFileName is the discard seal in the data directory.
const sealFileName = "player-floor-discarded.json"

// freshSeal is the seal a discard writes.
const freshSeal = "{\"version\":1,\"sealed\":true}\n"

// sealedMintedBox is resetInstall's box after bridge reset-player
// --discard-floor and the player's first start since, through the minter
// serve's player start calls: sealed and minted, both seal records whole.
func sealedMintedBox(t *testing.T) (cfg, dir string) {
	t.Helper()
	cfg, dir = resetInstall(t, "127.0.0.1:0")
	var out, errs bytes.Buffer
	if code := run(t.Context(), []string{"reset-player", "--discard-floor", "--config", cfg, "--yes"}, &out, &errs); code != 0 {
		t.Fatalf("discard: %d %s", code, &errs)
	}
	if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
		t.Fatal(err)
	}
	return cfg, dir
}

// obstructSeal replaces dir's seal file with an entry of kind, none of them a
// regular file: a directory, empty or holding a file, or a symbolic link to
// nothing or to a copy of the seal.
func obstructSeal(t *testing.T, dir, kind string) {
	t.Helper()
	path := filepath.Join(dir, sealFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var err error
	switch kind {
	case "directory":
		err = os.Mkdir(path, 0o700)
	case "directory_with_a_file":
		if err = os.Mkdir(path, 0o700); err == nil {
			err = os.WriteFile(filepath.Join(path, "kept"), []byte("kept"), 0o600)
		}
	case "dangling_link":
		err = os.Symlink(sealFileName+".nowhere", path)
	case "link_to_a_seal":
		target := filepath.Join(dir, sealFileName+".target")
		if err = os.WriteFile(target, []byte(freshSeal), 0o600); err == nil {
			err = os.Symlink(filepath.Base(target), path)
		}
	default:
		t.Fatalf("no seal obstruction %q", kind)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// entryKind is the type bits of dir/name, never following it, and "absent"
// for no entry.
func entryKind(t *testing.T, dir, name string) string {
	t.Helper()
	st, err := os.Lstat(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Type().String()
}

// markerState is player-initialized as a test reads it.
type markerState struct {
	Version        int
	Sealed, Minted bool
}

// playerMarker decodes dir's player-initialized, ok false when it is absent
// or does not decode.
func playerMarker(t *testing.T, dir string) (m markerState, ok bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "player-initialized"))
	if err != nil {
		return m, false
	}
	return m, json.Unmarshal(b, &m) == nil
}

// bootToTLS runs serve's boot on cfg's data directory up to its TLS step,
// which a TLS path that is a directory stops: past the player's preflight
// (seal completion, adoption and a start's check), before anything listens.
// It returns boot's stderr.
func bootToTLS(t *testing.T, cfg, dir string) string {
	t.Helper()
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	boot := filepath.Join(t.TempDir(), "boot.yaml")
	b = append(b, []byte("output:\n  player:\n    enabled: false\ntlsCertPath: "+dir+"\ntlsKeyPath: "+dir+"\n")...)
	if err := os.WriteFile(boot, b, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := runServe(t.Context(), serveOpts{configPath: boot, playerGOOS: "linux"}, &out, &errs); code != 1 || !strings.Contains(errs.String(), "TLS material") {
		t.Fatalf("boot did not stop at TLS: %d %s", code, &errs)
	}
	return errs.String()
}

// followTheSealAdvice starts the box as an operator's next start does, serve
// booting to its TLS step and then the player starting through the minter
// serve's player start calls, and follows the advice the bridge gives about
// the seal, as written, until it gives none: a line that names the seal file
// and says to move it aside, by moving the seal aside; a refusal of the
// player that names --discard-floor, by running bridge reset-player
// --discard-floor. Advice to move a seal record aside fails the test, naming
// the line, and is followed all the same, so the walk shows where it ends.
func followTheSealAdvice(t *testing.T, rec *loggingtest.Recorder, cfg, dir string) {
	t.Helper()
	for round := 1; round <= 3; round++ {
		from := len(rec.All())
		said := []string{bootToTLS(t, cfg, dir)}
		var refusals []string
		if _, _, err := backup.EnsurePlayerUUID(dir); err != nil {
			refusals = append(refusals, err.Error())
		}
		for _, line := range rec.All()[from:] {
			said = append(said, line)
			if strings.Contains(line, "player: not started") {
				refusals = append(refusals, line)
			}
		}
		aside, discard := "", ""
		for _, s := range append(said, refusals...) {
			if strings.Contains(s, sealFileName) && strings.Contains(s, "aside") {
				aside = s
			}
		}
		for _, s := range refusals {
			if strings.Contains(s, "--discard-floor") {
				discard = s
			}
		}
		switch {
		case aside != "":
			t.Errorf("round %d: advice to move the seal aside: %s", round, aside)
			if err := os.Rename(filepath.Join(dir, sealFileName), filepath.Join(dir, sealFileName+".moved-aside")); err != nil {
				t.Fatal(err)
			}
		case discard != "":
			var out, errs bytes.Buffer
			if code := run(t.Context(), []string{"reset-player", "--discard-floor", "--config", cfg, "--yes"}, &out, &errs); code != 0 {
				t.Fatalf("round %d: the advised discard: %d %s", round, code, &errs)
			}
		default:
			return
		}
	}
	t.Fatal("the bridge still gives advice about the seal after three rounds of following it")
}

// identityRestore runs bridge restore of another box's identity onto cfg's
// box and returns its exit code and stderr.
func identityRestore(t *testing.T, cfg string) (int, string) {
	t.Helper()
	src := t.TempDir()
	if err := config.WritePlayerUUIDFile(filepath.Join(src, config.PlayerUUIDFile), round11PlayerUUID); err != nil {
		t.Fatal(err)
	}
	if err := engine.WriteReservation(src, filepath.Join(src, playerReservationFile), engine.Reservation{UUID: round11PlayerUUID, IDs: 9, Tokens: 9}); err != nil {
		t.Fatal(err)
	}
	snap, err := backup.Snapshot(t.Context(), backup.Sources{DataDir: src, PlayerUUID: filepath.Join(src, config.PlayerUUIDFile)})
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runRestoreYes(t, cfg, snap)
	return code, stderr
}

// requireSealed requires cfg's box to refuse an identity restore as sealed,
// and its player-initialized, when it decodes, to record the seal.
func requireSealed(t *testing.T, cfg, dir, when string) {
	t.Helper()
	if m, ok := playerMarker(t, dir); ok && (m.Version != 1 || !m.Sealed) {
		t.Errorf("%s: player-initialized is %+v, which records no seal", when, m)
	}
	if code, stderr := identityRestore(t, cfg); code == 0 {
		id, _ := config.ReadPlayerUUID(dir)
		t.Errorf("%s: an identity restore was accepted, and the box now runs player %s", when, id)
	} else if !strings.Contains(stderr, "sealed") {
		t.Errorf("%s: the identity restore was refused, but not as sealed: %s", when, stderr)
	}
}

// TestTheSealWalkEndsSealedFollowingTheBridgesOwnAdvice is B539's sequence
// through the commands an operator runs: a sealed, minted box loses
// player-initialized, and its seal file becomes an entry that is not a
// regular file; the box starts, serve booting and the player starting, and
// every piece of advice the bridge gives about the seal is followed as
// written. Round 10's boot refused with "player-floor-discarded.json is not
// a regular file: move it aside", and following that let the next start
// write an unsealed {"version":1} marker and an identity restore succeed.
// The walk ends sealed: no advice moves a seal record aside,
// player-initialized records the seal again, the entry is kept as it was, and
// an identity restore is refused; so after a second loss of
// player-initialized.
func TestTheSealWalkEndsSealedFollowingTheBridgesOwnAdvice(t *testing.T) {
	for _, kind := range []string{"directory", "directory_with_a_file", "dangling_link", "link_to_a_seal"} {
		t.Run(kind, func(t *testing.T) {
			cfg, dir := sealedMintedBox(t)
			rec := loggingtest.Record(t)
			for loss := 1; loss <= 2; loss++ {
				if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
					t.Fatal(err)
				}
				if loss == 1 {
					obstructSeal(t, dir, kind)
				}
				want := entryKind(t, dir, sealFileName)
				followTheSealAdvice(t, rec, cfg, dir)
				if m, ok := playerMarker(t, dir); !ok || m.Version != 1 || !m.Sealed || !m.Minted {
					t.Errorf("loss %d: the starts left player-initialized %+v (decoded: %t), not sealed and minted", loss, m, ok)
				}
				if got := entryKind(t, dir, sealFileName); got != want {
					t.Errorf("loss %d: the seal entry went from %s to %s", loss, want, got)
				}
				requireSealed(t, cfg, dir, "after the walk")
			}
		})
	}
}

// failSealWrite makes the discard's write of the fresh seal fail at point,
// and returns what puts the seams back: "write" fails the staged file's
// fsync, which the seal's write reaches once player-initialized has landed
// (discard seals that record first beside a damaged seal entry); "rename"
// fails the rename of the staged file into the seal's place.
func failSealWrite(t *testing.T, point string) (stop func()) {
	t.Helper()
	broken := errors.New("the fresh seal's write failed")
	markerLanded := false
	prevRename := atomicwrite.SetRenameFuncForTest(func(src, dst string) error {
		if point == "rename" && filepath.Base(dst) == sealFileName && strings.HasPrefix(filepath.Base(src), ".player-marker-") {
			return broken
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
		if filepath.Base(dst) == "player-initialized" {
			markerLanded = true
		}
		return nil
	})
	restoreSync := func() {}
	if point == "write" {
		restoreSync = atomicwrite.SetSyncFileForTest(func(f *os.File) error {
			if markerLanded {
				return broken
			}
			return f.Sync()
		})
	}
	stopped := false
	stop = func() {
		if !stopped {
			stopped = true
			restoreSync()
			atomicwrite.SetRenameFuncForTest(prevRename)
		}
	}
	t.Cleanup(stop)
	return stop
}

// TestAFailedHealKeepsTheSealEntryThroughTheNextStarts: bridge reset-player
// --discard-floor on the walk's state, a sealed, minted box whose
// player-initialized is gone and whose seal is not a regular file, fails
// while it writes the fresh seal. Discard used to rename the damaged entry
// aside before that write, so the failure left no seal file and said nothing
// but its error: the next start sealed player-initialized again, and once
// that marker was lost too, the start after wrote {"version":1} and an
// identity restore succeeded. Discard now writes and syncs the fresh seal
// before the entry moves, and puts the entry back when the fresh one cannot
// take its place: the failure keeps the entry, its message says to run the
// discard again, and the walk ends sealed. Run again, the discard heals the
// seal.
func TestAFailedHealKeepsTheSealEntryThroughTheNextStarts(t *testing.T) {
	for _, point := range []string{"write", "rename"} {
		for _, kind := range []string{"directory", "dangling_link"} {
			t.Run(point+"/"+kind, func(t *testing.T) {
				cfg, dir := sealedMintedBox(t)
				rec := loggingtest.Record(t)
				if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
					t.Fatal(err)
				}
				obstructSeal(t, dir, kind)
				want := entryKind(t, dir, sealFileName)
				stop := failSealWrite(t, point)
				var out, errs bytes.Buffer
				code := run(t.Context(), []string{"reset-player", "--discard-floor", "--config", cfg, "--yes"}, &out, &errs)
				stop()
				if code == 0 {
					t.Fatalf("the discard went on past its failed seal write: %s", &out)
				}
				if !strings.Contains(errs.String(), "--discard-floor") || strings.Contains(errs.String(), "aside") {
					t.Errorf("the failed discard does not advise running it again: %s", &errs)
				}
				if got := entryKind(t, dir, sealFileName); got != want {
					t.Errorf("the failed discard left the seal entry %s, not the %s it found", got, want)
				}
				requireSealed(t, cfg, dir, "after the failed discard")
				// The box is started instead, and player-initialized is lost
				// again.
				followTheSealAdvice(t, rec, cfg, dir)
				if err := os.Remove(filepath.Join(dir, "player-initialized")); err != nil {
					t.Fatal(err)
				}
				followTheSealAdvice(t, rec, cfg, dir)
				requireSealed(t, cfg, dir, "after a start, the marker's loss and the next start")
				// The advice followed: the discard run again heals the seal.
				out.Reset()
				errs.Reset()
				if code := run(t.Context(), []string{"reset-player", "--discard-floor", "--config", cfg, "--yes"}, &out, &errs); code != 0 {
					t.Fatalf("the discard run again: %d %s", code, &errs)
				}
				st, err := os.Lstat(filepath.Join(dir, sealFileName))
				if b, rerr := os.ReadFile(filepath.Join(dir, sealFileName)); err != nil || !st.Mode().IsRegular() || rerr != nil || string(b) != freshSeal {
					t.Errorf("the discard run again left the seal %q (%v, %v)", b, err, rerr)
				}
				requireSealed(t, cfg, dir, "after the discard ran again")
			})
		}
	}
}
