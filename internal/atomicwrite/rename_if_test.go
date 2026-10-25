package atomicwrite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// RenameWithRetryIf is a config save's rename (B506's third round): its
// condition is asked before every attempt, a refusal of it ends the retries
// with no rename after it, and one context bounds the loop. These tests set
// the process-global rename seam, so none of them calls t.Parallel.

// renamesFailing makes every rename fail with errRenameForTest and returns
// how many were tried.
func renamesFailing(t *testing.T) (tried *int) {
	t.Helper()
	n := 0
	prev := SetRenameFuncForTest(func(string, string) error { n++; return errRenameForTest })
	t.Cleanup(func() { SetRenameFuncForTest(prev) })
	return &n
}

// errRenameForTest stands for a rename Windows refuses while another process
// has the file open.
var errRenameForTest = errors.New("the process cannot access the file because it is being used by another process")

// TestRenameWithRetryIfEndsAtTheConditionsRefusal: a refusal of the condition
// is returned as it is, with no rename tried after it, whichever attempt it
// comes before.
func TestRenameWithRetryIfEndsAtTheConditionsRefusal(t *testing.T) {
	tried := renamesFailing(t)
	refused := errors.New("the file changed")
	asked := 0
	err := RenameWithRetryIf(context.Background(), "src", "dst", func() error {
		asked++
		if asked == 3 {
			return refused
		}
		return nil
	})
	if err != refused {
		t.Errorf("RenameWithRetryIf = %v, want the condition's refusal as it is", err)
	}
	if asked != 3 || *tried != 2 {
		t.Errorf("the condition was asked %d times around %d renames, want three around two", asked, *tried)
	}
}

// TestRenameWithRetryIfIsBoundedByItsContext: a context that ends while the
// loop waits between attempts ends it, with the last rename's failure and the
// context's, before the schedule's last attempt.
func TestRenameWithRetryIfIsBoundedByItsContext(t *testing.T) {
	tried := renamesFailing(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := RenameWithRetryIf(ctx, "src", "dst", func() error { return nil })
	took := time.Since(start)
	if !errors.Is(err, errRenameForTest) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RenameWithRetryIf past its context = %v, want the rename's failure and the deadline", err)
	}
	if *tried >= len(renameBackoff) || took >= RenameRetryBudget() {
		t.Errorf("%d renames in %s, want the loop to stop at its context's deadline", *tried, took)
	}
}

// stagedOver lays out a staged file and the destination it would replace, in
// a directory of its own, and returns both names.
func stagedOver(t *testing.T) (src, dst string) {
	t.Helper()
	dir := t.TempDir()
	src, dst = filepath.Join(dir, "staged"), filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	return src, dst
}

// requireNotRenamed fails the test unless dst still holds what it held and
// src is still staged: no rename was made.
func requireNotRenamed(t *testing.T, src, dst string) {
	t.Helper()
	if got, err := os.ReadFile(dst); err != nil || string(got) != "old" {
		t.Errorf("the destination holds %q, %v: a rename was made", got, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("the staged file is gone (%v): a rename was made", err)
	}
}

// TestRenameWithRetryIfRenamesNothingOnceItsContextEnds: a context that ends
// while the condition runs ends the loop with no rename after it, the
// condition's nil answer notwithstanding, and the refusal is the context's,
// wrapping the last rename's failure when one failed. Until B506's fourth
// round the context was asked only during the waits, so a condition that
// answered after the save's window renamed all the same.
func TestRenameWithRetryIfRenamesNothingOnceItsContextEnds(t *testing.T) {
	t.Run("a condition that ends the context and answers nil", func(t *testing.T) {
		src, dst := stagedOver(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := RenameWithRetryIf(ctx, src, dst, func() error { cancel(); return nil })
		if !errors.Is(err, context.Canceled) {
			t.Errorf("RenameWithRetryIf after its condition ended the context = %v, want the context's end", err)
		}
		requireNotRenamed(t, src, dst)
	})
	t.Run("a condition that answers after the context's deadline", func(t *testing.T) {
		src, dst := stagedOver(t)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		err := RenameWithRetryIf(ctx, src, dst, func() error {
			<-ctx.Done()
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("RenameWithRetryIf whose condition answered past the deadline = %v, want the deadline", err)
		}
		requireNotRenamed(t, src, dst)
	})
	t.Run("after a failed rename, the context ended by the next condition", func(t *testing.T) {
		tried := renamesFailing(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		asked := 0
		err := RenameWithRetryIf(ctx, "src", "dst", func() error {
			if asked++; asked == 2 {
				cancel()
			}
			return nil
		})
		if !errors.Is(err, context.Canceled) || !errors.Is(err, errRenameForTest) {
			t.Errorf("RenameWithRetryIf = %v, want the context's end and the failed rename", err)
		}
		if *tried != 1 {
			t.Errorf("%d renames were tried, want the one that failed and none after the context ended", *tried)
		}
	})
}

// TestRenameWithRetryIfAsksNothingOfAContextAlreadyEnded: a context that has
// ended before the first attempt ends the loop there: no condition asked, no
// rename tried.
func TestRenameWithRetryIfAsksNothingOfAContextAlreadyEnded(t *testing.T) {
	src, dst := stagedOver(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	asked := false
	err := RenameWithRetryIf(ctx, src, dst, func() error { asked = true; return nil })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("RenameWithRetryIf on an ended context = %v, want the context's end", err)
	}
	if asked {
		t.Error("the condition was asked on a context that had ended")
	}
	requireNotRenamed(t, src, dst)
}

// TestRenameWithRetryIfRenames: with the condition holding, the rename lands
// as RenameWithRetry's does.
func TestRenameWithRetryIfRenames(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "staged"), filepath.Join(dir, "bridge.yaml")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameWithRetryIf(context.Background(), src, dst, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "new" {
		t.Errorf("after the rename %s holds %q, %v", dst, got, err)
	}
}
