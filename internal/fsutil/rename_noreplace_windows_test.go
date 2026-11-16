//go:build windows

package fsutil

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// init puts Windows' answers in the seam rows' place, and adds its own rows
// to TestARenameThatNeverReplacesLeavesAnEntryAtItsTarget: a source held as
// a scanner holds it, let go within the schedule or held past it; the two
// answers tried again, through the seam; and a context that ends between the
// tries.
func init() {
	noReplaceErrnos.unsupported = []noReplaceErrno{
		{"ERROR_NOT_SUPPORTED", windows.ERROR_NOT_SUPPORTED},
		{"ERROR_INVALID_FUNCTION", windows.ERROR_INVALID_FUNCTION},
		{"ERROR_CALL_NOT_IMPLEMENTED", windows.ERROR_CALL_NOT_IMPLEMENTED},
	}
	noReplaceErrnos.crossDevice = noReplaceErrno{"ERROR_NOT_SAME_DEVICE", windows.ERROR_NOT_SAME_DEVICE}
	noReplacePlatformRows = append(noReplacePlatformRows,
		noReplaceRow{"a source held without FILE_SHARE_DELETE and let go within the schedule is renamed", noReplaceLetGoWithinTheScheduleRow},
		noReplaceRow{"a source held past the schedule answers its sharing violation", noReplaceHeldPastTheScheduleRow},
		noReplaceRow{"a sharing violation or a refused access is tried again", noReplaceTriedAgainRow},
		noReplaceRow{"a context that ends between the tries stops them", noReplaceStoppedBetweenTriesRow},
	)
}

// noReplaceLetGoWithinTheScheduleRow holds the source as a scanner holds it,
// so the first try meets a sharing violation. Just before the second try the
// seam has a goroutine let the handle go and waits until it has, and the
// rename lands within the schedule, every answer before the last one tried
// again.
func noReplaceLetGoWithinTheScheduleRow(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	src := noReplaceWrite(t, from, "held by a scanner")
	letGo := noReplaceHold(t, from)
	answers := noReplaceRecordAnswers(t)
	now := make(chan struct{})
	gone := make(chan error, 1)
	go func() {
		<-now
		gone <- letGo()
	}()
	var signal sync.Once
	t.Cleanup(func() { signal.Do(func() { close(now) }) })
	var letGoErr error
	tries := noReplaceCountTries(t, func(try int32, _, _ string) {
		if try == 2 {
			signal.Do(func() { close(now) })
			letGoErr = <-gone
		}
	})
	err := RenameNoReplace(from, to)
	got := answers()
	if letGoErr != nil {
		t.Fatalf("letting the scanner's handle go: %v", letGoErr)
	}
	if err != nil {
		t.Fatalf("RenameNoReplace(%s, %s) = %v (the call answered %v); want the rename once the handle was let go", from, to, err, got)
	}
	if len(got) < 2 || got[len(got)-1] != nil || slices.ContainsFunc(got[:len(got)-1], func(a error) bool { return !noReplaceHeldAnswer(a) }) {
		t.Fatalf("the call answered %v; want a sharing violation or a refused access, then the rename", got)
	}
	noReplaceRequireBytes(t, to, src)
	noReplaceRequireAbsent(t, from)
	t.Logf("%d tries, the call answering %v", tries.Load(), got)
}

// noReplaceHeldPastTheScheduleRow holds the source through every try, so
// each meets the sharing violation: past the schedule the last answer is
// returned, an *os.LinkError carrying it, after noReplaceTries tries, and
// nothing is renamed.
func noReplaceHeldPastTheScheduleRow(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	src := noReplaceWrite(t, from, "held by a scanner")
	noReplaceHold(t, from)
	answers := noReplaceRecordAnswers(t)
	tries := noReplaceCountTries(t, nil)
	start := time.Now()
	err := RenameNoReplace(from, to)
	took := time.Since(start)
	noReplaceRequireLinkError(t, err, from, to)
	if !noReplaceHeldAnswer(err) || errors.Is(err, ErrNoReplaceUnsupported) {
		t.Fatalf("RenameNoReplace = %v; want the sharing violation or the refused access alone", err)
	}
	if n := tries.Load(); n != noReplaceTries {
		t.Fatalf("the call was asked %d times; want %d", n, noReplaceTries)
	}
	if got := answers(); slices.ContainsFunc(got, func(a error) bool { return !noReplaceHeldAnswer(a) }) {
		t.Fatalf("the call answered %v; want a sharing violation or a refused access each time", got)
	}
	noReplaceRequireBytes(t, from, src)
	noReplaceRequireAbsent(t, to)
	t.Logf("%d tries in %v", tries.Load(), took)
}

// noReplaceTriedAgainRow drives the two answers tried again through the
// call's seam: each answered twice and then the real call made, which
// lands; and each answered at every try, the last returned as it is once the
// schedule has run.
func noReplaceTriedAgainRow(t *testing.T) {
	for _, answer := range []noReplaceErrno{
		{"ERROR_SHARING_VIOLATION", windows.ERROR_SHARING_VIOLATION},
		{"ERROR_ACCESS_DENIED", windows.ERROR_ACCESS_DENIED},
	} {
		t.Run(answer.name+" twice, then the call", func(t *testing.T) {
			dir := t.TempDir()
			from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
			src := noReplaceWrite(t, from, "the source's bytes")
			asked := 0
			noReplaceAnswerWith(t, func(from, to string) error {
				if asked++; asked <= 2 {
					return answer.err
				}
				return platformRenameNoReplace(from, to)
			})
			tries := noReplaceCountTries(t, nil)
			if err := RenameNoReplace(from, to); err != nil {
				t.Fatalf("RenameNoReplace = %v; want the rename at the third try", err)
			}
			// At least three: a scanner on the file just written can answer
			// the real call too.
			if n := tries.Load(); n < 3 || n > noReplaceTries {
				t.Fatalf("the call was asked %d times; want from 3 to %d", n, noReplaceTries)
			}
			noReplaceRequireBytes(t, to, src)
			noReplaceRequireAbsent(t, from)
		})
		t.Run(answer.name+" at every try", func(t *testing.T) {
			dir := t.TempDir()
			from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
			src := noReplaceWrite(t, from, "the source's bytes")
			noReplaceAnswerWith(t, func(string, string) error { return answer.err })
			tries := noReplaceCountTries(t, nil)
			err := RenameNoReplace(from, to)
			if link := noReplaceRequireLinkError(t, err, from, to); link.Err != answer.err {
				t.Fatalf("RenameNoReplace = %v; want %s as it is", err, answer.name)
			}
			if n := tries.Load(); n != noReplaceTries {
				t.Fatalf("the call was asked %d times; want %d", n, noReplaceTries)
			}
			noReplaceRequireBytes(t, from, src)
			noReplaceRequireAbsent(t, to)
		})
	}
}

// noReplaceStoppedBetweenTriesRow ends the context in the seam of the first
// try, which answers a sharing violation: the wait before the second try
// returns the context's own error, after one try, with nothing renamed.
func noReplaceStoppedBetweenTriesRow(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "from"), filepath.Join(dir, "to")
	src := noReplaceWrite(t, from, "the source's bytes")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	noReplaceAnswerWith(t, func(string, string) error {
		cancel()
		return windows.ERROR_SHARING_VIOLATION
	})
	tries := noReplaceCountTries(t, nil)
	err := RenameNoReplaceCtx(ctx, from, to)
	if !errors.Is(err, context.Canceled) || noReplaceHeldAnswer(err) {
		t.Fatalf("RenameNoReplaceCtx = %v; want the context's own error, not the sharing violation", err)
	}
	if n := tries.Load(); n != 1 {
		t.Fatalf("the call was asked %d times; want once", n)
	}
	noReplaceRequireBytes(t, from, src)
	noReplaceRequireAbsent(t, to)
}

// noReplaceHeldAnswer reports whether err is one of the two answers a held
// source gives MoveFileEx, written here apart from the code's own reading.
func noReplaceHeldAnswer(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// noReplaceHold opens path as a scanner holds a file it reads: for reading,
// sharing reads and writes and not delete, which MoveFileEx, opening its
// source for delete, meets as a sharing violation. letGo closes the handle
// once, whoever asks first; the row's cleanup asks too, before its
// directory's removal.
func noReplaceHold(t *testing.T, path string) (letGo func() error) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("CreateFile(%s): %v", path, err)
	}
	var once sync.Once
	var closeErr error
	letGo = func() error {
		once.Do(func() { closeErr = windows.CloseHandle(h) })
		return closeErr
	}
	t.Cleanup(func() { _ = letGo() })
	return letGo
}

// noReplaceRecordAnswers puts in the call's place one that makes the real
// call and keeps each answer, in order, for the row to read once the rename
// has returned.
func noReplaceRecordAnswers(t *testing.T) (answers func() []error) {
	t.Helper()
	var mu sync.Mutex
	var kept []error
	noReplaceAnswerWith(t, func(from, to string) error {
		err := platformRenameNoReplace(from, to)
		mu.Lock()
		kept = append(kept, err)
		mu.Unlock()
		return err
	})
	return func() []error {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(kept)
	}
}
