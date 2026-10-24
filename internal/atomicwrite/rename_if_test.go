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
