//go:build !windows

package transcode

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// outputFaultErrnosWant is the unix table, written out: what each cause says
// about the output side's volume.
func outputFaultErrnosWant() map[syscall.Errno]outputFaultKind {
	return map[syscall.Errno]outputFaultKind{
		syscall.EROFS:    outputReadOnly,
		syscall.ENOSPC:   outputFull,
		syscall.EDQUOT:   outputQuota,
		syscall.EIO:      outputGone,
		syscall.ENOTCONN: outputGone,
		syscall.ESTALE:   outputGone,
	}
}

// permissionErrnos are the causes fs.ErrPermission names on unix.
func permissionErrnos() []syscall.Errno { return []syscall.Errno{syscall.EACCES, syscall.EPERM} }

// notOutputFaultCauses are failures at an output step that keep their
// strike: what the source's name produces (too long, an encoding the
// filesystem refuses, a character it refuses), a file too large for the
// volume (a fact about this source on it), and entries that are, or are not,
// already there.
func notOutputFaultCauses() []error {
	var out []error
	for _, errno := range []syscall.Errno{
		syscall.ENAMETOOLONG, syscall.EILSEQ, syscall.EINVAL, syscall.EFBIG,
		syscall.EEXIST, syscall.ENOENT, syscall.ENOTDIR,
	} {
		out = append(out, &fs.PathError{Op: "open", Path: "/srv/variants/x", Err: errno})
	}
	return out
}

func readOnlyVolumeErrno() syscall.Errno { return syscall.EROFS }
func fullVolumeErrno() syscall.Errno     { return syscall.ENOSPC }

// TestMarkOutputFaultReadsWhatTheOSReports runs real operations and asks the
// classifier about what each returned, as TestUnavailableToolClassifiesWhatExecReports
// does for exec: a table of errnos is only as good as the operating system's
// agreement with it. A folder this user may not write is marked; a name too
// long for the filesystem and an entry already there are not.
func TestMarkOutputFaultReadsWhatTheOSReports(t *testing.T) {
	dir := t.TempDir()

	if os.Geteuid() != 0 {
		locked := filepath.Join(dir, "locked")
		readOnlyDir(t, locked)
		err := markOutputFault(outputVariants, locked, os.Mkdir(filepath.Join(locked, "Album"), 0o755))
		if f, ok := unwritableOutput(err); !ok || f.kind != outputDenied {
			t.Errorf("mkdir in a folder this user may not write: %v marked %+v, %v; want a permission fault", err, f, ok)
		}
		err = markOutputFault(outputVariants, locked,
			createOutput(filepath.Join(locked, "01.tmp"), filepath.Join(locked, "01.flac")))
		if f, ok := unwritableOutput(err); !ok || f.kind != outputDenied {
			t.Errorf("createOutput in a folder this user may not write: %v marked %+v, %v; want a permission fault", err, f, ok)
		}
	}

	long := filepath.Join(dir, strings.Repeat("a", 300))
	err := markOutputFault(outputVariants, dir, os.Mkdir(long, 0o755))
	if !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Fatalf("mkdir of a 300-byte name returned %v, want ENAMETOOLONG", err)
	}
	if _, ok := unwritableOutput(err); ok {
		t.Errorf("a name too long for the filesystem was marked as the output side's fault: it is the source's name, and must strike")
	}

	there := filepath.Join(dir, "there.tmp")
	if err := os.WriteFile(there, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err = markOutputFault(outputVariants, dir, createOutput(there, there))
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("createOutput over an entry already there returned %v, want an error wrapping fs.ErrExist", err)
	}
	if _, ok := unwritableOutput(err); ok {
		t.Errorf("an entry already at the path was marked as the output side's fault")
	}
}
