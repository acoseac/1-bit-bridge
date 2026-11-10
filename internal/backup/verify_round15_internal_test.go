package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRecoveryPublicationSeparatesItsCacheStampFromDurability drives the
// worker's recovery after admission refuses its ordinary publication.
func TestRecoveryPublicationSeparatesItsCacheStampFromDurability(t *testing.T) {
	for _, tc := range []struct {
		name        string
		giveUpStamp bool
		failSync    bool
		mixed       bool
		removeVeto  bool
	}{
		{name: "live recovery stamps"},
		{name: "recovery step given up before stamp", giveUpStamp: true},
		{name: "recovery sync fails", failSync: true},
		{name: "recovery sync fails and stamp is skipped", failSync: true, giveUpStamp: true},
		{name: "mixed record with existing veto", mixed: true},
		{name: "mixed record without veto", mixed: true, removeVeto: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, prior := unchangedRecord(t)
			var err error
			dir, err = filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Dir(dir)
			if err := os.Remove(filepath.Join(dir, vetoFileName)); err != nil {
				t.Fatal(err)
			}
			prior.DatabaseState, prior.FilesState = StatePending, StatePending
			if tc.mixed {
				prior.DatabaseState = StateVerified
			}
			mpath := filepath.Join(dir, ManifestFile)
			m := Manifest{SchemaVersion: 1, Verification: &prior,
				Files: []string{ManifestDBFileName}, Baselines: []Baseline{{Name: ManifestDBFileName}}}
			if err := writeManifest(mpath, m); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(mpath)
			if err != nil {
				t.Fatal(err)
			}
			h := newHold(context.Background(), filepath.Base(dir), root)
			h.handOff() // Keep overrun-marker I/O out of the cache-stamp assertion.
			t.Cleanup(SetSyncsParentDirForTest(true))
			t.Cleanup(SetSyncVetoForTest(func(string) error { return nil }))
			var hookErr error
			var planted time.Time
			t.Cleanup(SetAfterCheckForTest(func(string) {
				h.abandon()
				if tc.removeVeto {
					hookErr = os.Remove(filepath.Join(dir, vetoFileName))
				}
				// All root entries are already present. Compare with a planted
				// hour-old stamp, not two writes within a filesystem clock tick.
				past := time.Now().Add(-time.Hour).Truncate(time.Second)
				hookErr = errors.Join(hookErr, os.Chtimes(root, past, past))
				if info, err := os.Stat(root); err != nil {
					hookErr = errors.Join(hookErr, err)
				} else {
					planted = info.ModTime()
				}
			}))
			syncs, lookupsAfterGiveUp := 0, 0
			injected := errors.New("injected recovery sync failure")
			t.Cleanup(SetSyncVerdictForTest(func(string) error {
				syncs++
				if tc.giveUpStamp {
					h.abandon() // Ends the fresh recovery step's own signal.
				}
				if tc.failSync {
					return injected
				}
				return nil
			}))
			prevLookup := lookupVetoForWrite
			t.Cleanup(func() { lookupVetoForWrite = prevLookup })
			lookupVetoForWrite = func(path string) (os.FileInfo, error) {
				if h.gaveUp() {
					lookupsAfterGiveUp++
				}
				return os.Lstat(path)
			}
			// Release and join the hold before restoring any package seam.
			t.Cleanup(func() { h.release(); h.drop() })
			pruned := false
			res, err := verifyHeld(context.Background(), dir,
				VerifyOptions{BackupsRoot: root, Level: CheckQuick, LockWait: time.Second},
				func(context.Context, string) { pruned = true }, h)
			if hookErr != nil {
				t.Fatal(hookErr)
			}
			if !errors.Is(err, ErrCheckGivenUp) || !res.RecordRead || pruned || lookupsAfterGiveUp != 0 {
				t.Fatalf("recovery: %+v, %v; pruned %v, veto lookups after give-up %d", res, err, pruned, lookupsAfterGiveUp)
			}
			wantWrite, wantSyncs := WriteRecorded, 1
			if tc.failSync {
				wantWrite = WriteNotDurable
			}
			if tc.mixed {
				wantWrite, wantSyncs = WriteFailed, 0
				after, readErr := os.ReadFile(mpath)
				if readErr != nil || string(after) != string(before) || !strings.Contains(err.Error(), "the veto could not be confirmed") {
					t.Errorf("mixed recovery changed its record or lost the refusal: %v, %v", err, readErr)
				}
				if vetoedAt(dir) == tc.removeVeto {
					t.Error("mixed recovery changed veto presence after give-up")
				}
				wantDatabase := StateIncomplete
				if tc.removeVeto {
					wantDatabase = StateVerified
				}
				if res.Record.DatabaseState != wantDatabase || slices.Contains(res.Record.DatabaseReasons, ReasonUnsynced) != !tc.removeVeto {
					t.Errorf("mixed recovery read-back lost the existing veto state: %+v", res.Record)
				}
			} else if res.Record.DatabaseState != StateIncomplete || res.Record.FilesState != StateIncomplete {
				t.Errorf("recovery read-back = %+v, want INCOMPLETE/INCOMPLETE", res.Record)
			}
			if res.Write != wantWrite || res.Recorded != (wantWrite == WriteRecorded) || syncs != wantSyncs {
				t.Errorf("write %s, recorded %v, syncs %d; want %s, recorded %v, syncs %d", res.Write, res.Recorded, syncs, wantWrite, wantWrite == WriteRecorded, wantSyncs)
			}
			if errors.Is(err, ErrVerdictNotDurable) != tc.failSync || errors.Is(err, injected) != tc.failSync {
				t.Errorf("durability failure identity lost or invented: %v", err)
			}
			if errors.Is(err, errCacheStampSkipped) != tc.giveUpStamp || strings.Contains(err.Error(), "the console may retain its cached reading") != tc.giveUpStamp {
				t.Errorf("cache warning lost or invented: %v", err)
			}
			info, statErr := os.Stat(root)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if stamped := !info.ModTime().Equal(planted); stamped != (!tc.giveUpStamp && !tc.mixed) {
				t.Errorf("root stamp changed = %v, recovery step given up %v, mixed %v", stamped, tc.giveUpStamp, tc.mixed)
			}
		})
	}
}

// TestACacheWarningNeverHidesAPublicationFailure pins wrapped and joined
// warnings without treating a warning beside another failure as success.
func TestACacheWarningNeverHidesAPublicationFailure(t *testing.T) {
	warning := fmt.Errorf("cache: %w", errCacheStampSkipped)
	for _, tc := range []struct {
		name string
		err  error
		want WriteOutcome
	}{
		{"bare warning", errCacheStampSkipped, WriteRecorded},
		{"wrapped warning", warning, WriteRecorded},
		{"joined warnings", errors.Join(warning, errCacheStampSkipped), WriteRecorded},
		{"sync failure beside warning", errors.Join(warning, ErrVerdictNotDurable), WriteNotDurable},
		{"other failure beside warning", fmt.Errorf("%w; %w", warning, errors.New("other failure")), WriteNotDurable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := published{written: true, err: tc.err}
			if got := p.outcome(true); got != tc.want {
				t.Errorf("outcome = %s, want %s", got, tc.want)
			}
			if got := p.outcome(false); got != WriteUnfinished {
				t.Errorf("unanswered publication = %s, want unfinished", got)
			}
			p.written = false
			if got := p.outcome(true); got != WriteFailed {
				t.Errorf("unwritten publication = %s, want failed", got)
			}
		})
	}
}
