package transcode

import (
	"context"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// probeOp is the operation this platform's free-space probe names in its
// error.
func probeOp() string {
	if runtime.GOOS == "windows" {
		return "GetDiskFreeSpaceEx"
	}
	return "statfs"
}

// requireNoDirectory fails the test when err's text names dir or anything
// below the test's directory root, and when it does not name want, the
// operation that failed.
func requireNoDirectory(t *testing.T, what string, err error, root, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error, want one", what)
	}
	if strings.Contains(err.Error(), root) {
		t.Errorf("%s: %q names the directory", what, err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("%s: %q does not say what failed (%s)", what, err, want)
	}
}

// TestADiskProbeErrorNamesItsOperationAndNotItsDirectory: the free-space
// probe and the volume probe answer a failure with the operation and the
// OS's cause, never the directory they were asked about (a variants
// directory, a render scratch, the data dir: the callers say which by role),
// and errors.Is and errors.As answer as they did when the error quoted it.
// They reached the log through the on-demand pre-flight, the batch and the
// sweep, and the console's free-space panels.
func TestADiskProbeErrorNamesItsOperationAndNotItsDirectory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Private Volume")
	missing := filepath.Join(root, "variants")

	_, err := AvailableDiskSpace(missing)
	requireNoDirectory(t, "AvailableDiskSpace", err, root, probeOp())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("AvailableDiskSpace of a missing directory = %v, want it to match fs.ErrNotExist", err)
	}

	_, err = volumeID(missing)
	if err == nil {
		t.Fatal("volumeID of a missing directory: no error")
	}
	if strings.Contains(err.Error(), root) {
		t.Errorf("volumeID: %q names the directory", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("volumeID of a missing directory = %v, want it to match fs.ErrNotExist", err)
	}
	// On POSIX the volume probe's error is os.Stat's *fs.PathError, which a
	// caller can still take apart; Windows' named its call and wrapped the
	// errno, and still does.
	var pe *fs.PathError
	if got, want := errors.As(err, &pe), runtime.GOOS != "windows"; got != want {
		t.Errorf("errors.As(volumeID's error, *fs.PathError) = %v, want %v", got, want)
	}
}

// TestNoRoomNamesTheDirectorysRoleAndKeepsItsNumbers: an
// InsufficientDiskSpaceError says which volume had no room by the role of
// the directory its caller graded (DiskHasHeadroom's role, the batch's) and
// keeps the numbers, never the directory, while Dir still holds the
// directory for a caller that compares it and errors.Is still matches
// ErrInsufficientDiskSpace.
func TestNoRoomNamesTheDirectorysRoleAndKeepsItsNumbers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Private Volume", "variants")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ok, _, err := DiskHasHeadroom(dir, math.MaxInt64/4, DefaultDiskSafetyMargin, ProbeRoleVariants)
	if ok {
		t.Fatal("DiskHasHeadroom found room for an exabyte")
	}
	var dsk *InsufficientDiskSpaceError
	if !errors.As(err, &dsk) || !errors.Is(err, ErrInsufficientDiskSpace) {
		t.Fatalf("DiskHasHeadroom = %v, want an *InsufficientDiskSpaceError matching ErrInsufficientDiskSpace", err)
	}
	if dsk.Dir != dir {
		t.Errorf("the error's Dir = %q, want the directory graded %q", dsk.Dir, dir)
	}
	text := err.Error()
	if strings.Contains(text, filepath.Dir(dir)) {
		t.Errorf("%q names the directory", text)
	}
	if !strings.Contains(text, ProbeRoleVariants) {
		t.Errorf("%q does not say which directory's volume had no room (%s)", text, ProbeRoleVariants)
	}
	for _, n := range []int64{dsk.RequiredBytes, dsk.AvailableBytes} {
		if !strings.Contains(text, strconv.FormatInt(n, 10)) {
			t.Errorf("%q drops the number %d", text, n)
		}
	}
}

// TestABatchWithNoRoomNamesTheVariantsDirByItsRole is the batch's
// pre-flight end to end: its refusal names the variants dir by role, never
// the directory the batch was asked to write to.
func TestABatchWithNoRoomNamesTheVariantsDirByItsRole(t *testing.T) {
	s := openTempStoreForBatch(t)
	seedHugeTrack(t, s)
	p := NewPool(s, 1, 4)
	t.Cleanup(p.Stop)
	c, err := NewCoordinator(p, s, t.TempDir(), nil, scannedResolver(s))
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	root := filepath.Join(t.TempDir(), "Private Volume")
	outDir := filepath.Join(root, "variants")
	_, err = c.Submit(context.Background(), "Huge", 192000, 24, outDir)
	var dsk *InsufficientDiskSpaceError
	if !errors.As(err, &dsk) {
		t.Fatalf("Submit: want *InsufficientDiskSpaceError, got %v", err)
	}
	requireNoDirectory(t, "the batch's refusal", err, root, ProbeRoleVariants)
}
