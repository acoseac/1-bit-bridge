package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dsn"
)

func TestEverySnapshotWriterChmodsOnlyItsOwnFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits and replacement of an open temporary file are required")
	}
	for _, writer := range []string{"manifest", "copy", "vacuum"} {
		t.Run(writer, func(t *testing.T) {
			dir := t.TempDir()
			target := filepath.Join(dir, "target")
			if err := os.WriteFile(target, []byte("unchanged"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(target, 0o644); err != nil {
				t.Fatal(err)
			}
			var hookErr error
			called := false
			substitute := func(path string) {
				called = true
				if hookErr = os.Remove(path); hookErr == nil {
					hookErr = os.Symlink(target, path)
				}
			}
			dst := filepath.Join(dir, "output")
			var err error
			switch writer {
			case "manifest":
				err = writeManifestWithHook(dst, Manifest{}, substitute)
			case "copy":
				_, _, err = writeFromHashedWithHook(dst, strings.NewReader("copy"), 0o600, substitute)
			case "vacuum":
				src := filepath.Join(dir, "source.db")
				db, openErr := sql.Open("sqlite", dsn.File(src, ""))
				if openErr != nil {
					t.Fatal(openErr)
				}
				_, createErr := db.Exec("CREATE TABLE sample (n INTEGER)")
				closeErr := db.Close()
				if createErr != nil || closeErr != nil {
					t.Fatalf("source: %v, %v", createErr, closeErr)
				}
				err = vacuumIntoWithHook(t.Context(), src, dst, substitute)
			}
			if !called || hookErr != nil {
				t.Fatalf("mode-change seam called %v, substitution %v", called, hookErr)
			}
			if writer == "vacuum" && err == nil {
				t.Error("vacuum accepted a substituted symlink")
			} else if writer != "vacuum" && err != nil {
				t.Fatalf("writer: %v", err)
			}
			info, statErr := os.Stat(target)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Mode().Perm() != 0o644 {
				t.Errorf("writer changed target MODE to %v, want 0644", info.Mode())
			}
		})
	}
}

func TestBothVetoCallersGuardEveryAttemptAndReportSkippedStamps(t *testing.T) {
	for _, caller := range []string{"no barrier recheck", "publication"} {
		for _, signal := range []string{"step", "hold"} {
			for _, when := range []string{"before first attempt", "failed bundle", "existing root after failed bundle", "both attempts failed", "created bundle", "existing bundle", "created root"} {
				t.Run(caller+"/"+signal+"/"+when, func(t *testing.T) {
					dir, prior := unchangedRecord(t)
					bundle, root := filepath.Join(dir, vetoFileName), filepath.Dir(dir)
					if when != "existing bundle" {
						if err := os.Remove(bundle); err != nil {
							t.Fatal(err)
						}
					}
					if when == "existing root after failed bundle" {
						if err := os.WriteFile(rootVetoPath(dir), nil, 0o600); err != nil {
							t.Fatal(err)
						}
					}
					// This fixture is a modern record, so readVerdict sees its veto.
					m := Manifest{SchemaVersion: 1, CreatedAt: time.Now(), Verification: &prior,
						Baselines: []Baseline{{Name: ManifestDBFileName}}}
					if err := writeManifest(filepath.Join(dir, ManifestFile), m); err != nil {
						t.Fatal(err)
					}
					past := time.Now().Add(-time.Hour)
					if err := os.Chtimes(root, past, past); err != nil {
						t.Fatal(err)
					}
					before, err := os.Stat(root)
					if err != nil {
						t.Fatal(err)
					}
					given, cancel := context.WithCancel(context.Background())
					defer cancel()
					holdGivenUp := false
					giveUp := func() {
						if signal == "step" {
							cancel()
						} else {
							holdGivenUp = true
						}
					}
					p := publication{given: given, held: func() bool { return holdGivenUp }}
					attempts, lookups := 0, 0
					prevFail, prevAfter, prevLookup := failVeto, afterVetoAttempt, lookupVetoForWrite
					t.Cleanup(func() { failVeto, afterVetoAttempt, lookupVetoForWrite = prevFail, prevAfter, prevLookup })
					lookupVetoForWrite = func(path string) (os.FileInfo, error) {
						lookups++
						return os.Lstat(path)
					}
					failVeto = func(path string) error {
						attempts++
						if when == "both attempts failed" {
							// An error carrying give-up is not proof that the loop
							// skipped an attempt: both locations must be tried.
							return errors.Join(errors.New("veto refused"), ErrCheckGivenUp)
						}
						if path == bundle && (when == "failed bundle" || when == "existing root after failed bundle" || when == "created root") {
							return errors.New("bundle refused")
						}
						return nil
					}
					afterVetoAttempt = func(path string, present bool) {
						if when != "both attempts failed" && (when != "created root" || path == rootVetoPath(dir)) {
							giveUp()
						}
					}
					if when == "before first attempt" {
						giveUp()
					}
					if caller == "publication" {
						found := prior
						found.FilesState = StateFailed
						_, _, err = publishVerdict(dir, found, &prior, false, p)
					} else {
						err = vetoDisproved(dir, p)
					}
					wantLookups, wantAttempts := 1, 1
					switch when {
					case "before first attempt":
						wantLookups, wantAttempts = 0, 0
					case "existing bundle":
						wantAttempts = 0
					case "created root", "both attempts failed":
						wantLookups, wantAttempts = 2, 2
					}
					if lookups != wantLookups || attempts != wantAttempts {
						t.Errorf("lookups/creates = %d/%d, want %d/%d", lookups, attempts, wantLookups, wantAttempts)
					}
					confirmed := when == "created bundle" || when == "existing bundle" || when == "created root"
					present := confirmed || when == "existing root after failed bundle"
					if vetoedAt(dir) != present {
						t.Errorf("veto presence = %v, want %v", vetoedAt(dir), present)
					}
					if !confirmed {
						wantWords, falseWords := "the veto could not be confirmed", "could not be vetoed"
						if when == "both attempts failed" {
							wantWords, falseWords = falseWords, wantWords
						}
						if err == nil || !errors.Is(err, ErrCheckGivenUp) || !strings.Contains(err.Error(), wantWords) || strings.Contains(err.Error(), falseWords) {
							t.Errorf("unconfirmed or failed veto misreported: %v; want %q", err, wantWords)
						}
					} else {
						if err == nil || !strings.Contains(err.Error(), "record is vetoed") || !strings.Contains(err.Error(), "cache stamp was skipped") || !strings.Contains(err.Error(), "console may retain") || strings.Contains(err.Error(), "could not be vetoed") {
							t.Errorf("successful veto/skipped stamp misreported: %v", err)
						}
					}
					if present {
						record, readErr := readRecord(dir)
						if readErr != nil {
							t.Fatal(readErr)
						}
						v := readVerdict(record, time.Now(), vetoedAt(dir), OwnerGone, filepath.Base(dir))
						if v.DatabaseState != StateIncomplete || !slices.Contains(v.DatabaseReasons, ReasonUnsynced) {
							t.Errorf("record still trusted: %+v", v)
						}
					}
					if when != "created root" { // The root creation itself changes its mtime.
						after, statErr := os.Stat(root)
						if statErr != nil || !after.ModTime().Equal(before.ModTime()) {
							t.Errorf("cache stamp ran after give-up: %v", statErr)
						}
					}
				})
			}
		}
	}
}

func TestTheNoBarrierWorkerReportsAVetoAcrossGiveUp(t *testing.T) {
	for _, when := range []string{"failed bundle stops before root", "created bundle skips stamp", "existing root after failed bundle"} {
		t.Run(when, func(t *testing.T) {
			created := when == "created bundle skips stamp"
			existingRoot := when == "existing root after failed bundle"
			dir, prior := unchangedRecord(t)
			if err := os.Remove(filepath.Join(dir, vetoFileName)); err != nil {
				t.Fatal(err)
			}
			if existingRoot {
				if err := os.WriteFile(rootVetoPath(dir), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The recorded database is missing: a completed failure before
			// the no-barrier worker reaches its guarded veto step.
			m := Manifest{SchemaVersion: 1, Verification: &prior,
				Files: []string{ManifestDBFileName}, Baselines: []Baseline{{Name: ManifestDBFileName}}}
			if err := writeManifest(filepath.Join(dir, ManifestFile), m); err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(dir)
			h := newHold(context.Background(), filepath.Base(dir), root)
			h.handOff() // Isolate the veto's stamp from the overrun marker's I/O.
			prevSyncs, prevFail, prevAfter, prevLookup := syncsParentDir, failVeto, afterVetoAttempt, lookupVetoForWrite
			t.Cleanup(func() {
				syncsParentDir, failVeto, afterVetoAttempt, lookupVetoForWrite = prevSyncs, prevFail, prevAfter, prevLookup
			})
			t.Cleanup(func() {
				h.release()
				h.drop()
			})
			syncsParentDir = false
			attempts, lookups := 0, 0
			lookupVetoForWrite = func(path string) (os.FileInfo, error) {
				lookups++
				return os.Lstat(path)
			}
			failVeto = func(string) error {
				attempts++
				if !created {
					return errors.New("bundle refuses the veto")
				}
				return nil
			}
			var stamp time.Time
			var hookErr error
			afterVetoAttempt = func(string, bool) {
				// All initial lock-file creation is over by this seam. Plant
				// the root stamp now, before the stamp that must be skipped.
				past := time.Now().Add(-time.Hour)
				hookErr = os.Chtimes(root, past, past)
				if info, err := os.Stat(root); err == nil {
					stamp = info.ModTime()
				} else {
					hookErr = err
				}
				h.abandon()
			}
			pruned := false
			res, err := verifyHeld(context.Background(), dir, VerifyOptions{BackupsRoot: root, Level: CheckQuick, LockWait: time.Second},
				func(context.Context, string) { pruned = true }, h)
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			if lookups != 1 || attempts != 1 || pruned || !errors.Is(err, ErrCheckGivenUp) {
				t.Fatalf("lookups %d, attempts %d, pruned %v, result %+v, error %v", lookups, attempts, pruned, res, err)
			}
			if !res.RecordRead || res.Found.DatabaseState != StateFailed {
				t.Fatalf("worker did not reach its completed failure and read-back: %+v", res)
			}
			if created {
				if res.Record.DatabaseState != StateIncomplete || !strings.Contains(err.Error(), "record is vetoed") || !strings.Contains(err.Error(), "cache stamp was skipped") || strings.Contains(err.Error(), "could not be vetoed") {
					t.Errorf("successful veto misreported: %+v, %v", res, err)
				}
			} else {
				want := StateVerified
				if existingRoot {
					want = StateIncomplete
				}
				if res.Record.DatabaseState != want || !strings.Contains(err.Error(), "the veto could not be confirmed") || strings.Contains(err.Error(), "could not be vetoed") {
					t.Errorf("unconfirmed veto misreported: %+v, %v", res, err)
				}
				if existingRoot && !slices.Contains(res.Record.DatabaseReasons, ReasonUnsynced) {
					t.Errorf("read-back lost the existing root veto: %+v", res.Record)
				}
			}
			info, statErr := os.Stat(root)
			if statErr != nil || !info.ModTime().Equal(stamp) {
				t.Errorf("root changed after give-up: %v", statErr)
			}
		})
	}
}

func TestVacuumModeRefusesANonRegularOutput(t *testing.T) {
	dir := t.TempDir()
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := chmodVacuumOutput(dir); err == nil || !strings.Contains(err.Error(), "vacuum output is not a regular file") {
		t.Fatalf("directory refusal = %v, want not a regular file", err)
	}
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() {
		t.Errorf("changed a non-regular output's mode: %v -> %v", before.Mode(), after.Mode())
	}
}

func TestVacuumModeRefusesADirectorySymlink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "output")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	before, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = chmodVacuumOutput(link)
	if err == nil {
		t.Fatal("accepted a directory symlink as SQLite's output")
	}
	// Unix refuses the link in open itself; Windows opens the reparse entry
	// so the descriptor's kind supplies this diagnostic.
	if runtime.GOOS == "windows" && !strings.Contains(err.Error(), "vacuum output is not a regular file") {
		t.Errorf("directory symlink refusal = %v, want not a regular file", err)
	}
	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before.Mode() != after.Mode() {
		t.Errorf("changed directory symlink target mode: %v -> %v", before.Mode(), after.Mode())
	}
}
