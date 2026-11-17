package manifest

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestResetBackupEpochReadsItsCheckpointsAnswer (T36's decoding half,
// PR 2a's brief review, finding 5): the epoch's TRUNCATE checkpoint answers a
// row (busy, log, checkpointed), and ResetBackupEpoch answers
// ErrEpochCheckpointBusy unless busy is 0 and every frame of the WAL was
// checkpointed, so its caller keeps a WAL that holds commits. The real
// reader that keeps the checkpoint from completing is internal/backup's
// TestTheFullRestoreKeepsAWALWhoseEpochDidNotCheckpoint.
func TestResetBackupEpochReadsItsCheckpointsAnswer(t *testing.T) {
	for _, c := range []struct {
		busy, log, checkpointed int
		busyAnswer              bool
	}{
		{0, 0, 0, false},
		{0, 7, 7, false},
		{1, 7, 7, true},
		{0, 7, 3, true},
		{1, -1, -1, true},
	} {
		path := filepath.Join(t.TempDir(), "bridge.db")
		openAndClose(t, path)
		restore := SetEpochCheckpointAnswerForTest(func() (int, int, int, error) { return c.busy, c.log, c.checkpointed, nil })
		err := ResetBackupEpoch(path)
		restore()
		if got := errors.Is(err, ErrEpochCheckpointBusy); got != c.busyAnswer || (!c.busyAnswer && err != nil) {
			t.Errorf("a checkpoint answering (%d, %d, %d): ResetBackupEpoch = %v, want busy %v", c.busy, c.log, c.checkpointed, err, c.busyAnswer)
		}
	}
}
