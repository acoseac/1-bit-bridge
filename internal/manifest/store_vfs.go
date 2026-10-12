package manifest

import (
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"modernc.org/libc"
	sqlite3 "modernc.org/sqlite/lib"
)

// storeVFSName is the private SQLite VFS OpenStore passes as vfs=.
// It is a copy of the default VFS with xFullPathname replaced.
const storeVFSName = "bridgenf"

// modernc.org/sqlite v1.60.1 (SQLite 3.53.4) never sets
// SQLITE_OPEN_NOFOLLOW, and this amalgamation has no "nofollow" URI key,
// so nofollow=1 does not change an open. The unix VFS opens with
// O_NOFOLLOW, but its full-pathname resolves every symlink component and
// the pager keeps that resolved path, so the open then sees a regular
// file. This VFS returns the path absolutized and cleaned, and nothing
// else. The pager opens that name; O_NOFOLLOW refuses a final-component
// symlink at the database, -wal and -shm. The kernel still follows a
// parent directory, so a data directory that is itself a symlink opens.
// Windows winOpen does not pass FILE_FLAG_OPEN_REPARSE_POINT, and its
// full-pathname is GetFullPathNameW, so OpenStore does not install this
// VFS there. The Lstat check remains that platform's refusal.

var storeVFSNameBytes = [...]byte{'b', 'r', 'i', 'd', 'g', 'e', 'n', 'f', 0}

var (
	storeVFSOnce sync.Once
	storeVFSErr  error
	storeVFS     sqlite3.Tsqlite3_vfs
)

// storeVFSQuery is the DSN suffix OpenStore appends, or empty on Windows,
// where this VFS is not installed.
func storeVFSQuery() (string, error) {
	if runtime.GOOS == "windows" {
		return "", nil
	}
	if err := registerStoreVFS(); err != nil {
		return "", err
	}
	return "&vfs=" + storeVFSName, nil
}

func registerStoreVFS() error {
	storeVFSOnce.Do(func() {
		storeVFSErr = registerStoreVFSOnce()
	})
	return storeVFSErr
}

func registerStoreVFSOnce() error {
	tls := libc.NewTLS()
	defer tls.Close()
	p := sqlite3.Xsqlite3_vfs_find(tls, 0)
	if p == 0 {
		return fmt.Errorf("sqlite has no default vfs")
	}
	// Copy through libc so this package never converts a C uintptr.
	raw := libc.GoBytes(p, int(unsafe.Sizeof(storeVFS)))
	copy(unsafe.Slice((*byte)(unsafe.Pointer(&storeVFS)), len(raw)), raw)
	storeVFS.FpNext = 0
	storeVFS.FzName = uintptr(unsafe.Pointer(&storeVFSNameBytes[0]))
	storeVFS.FxFullPathname = ccgoFuncPtr(storeFullPathname)
	rc := sqlite3.Xsqlite3_vfs_register(tls, uintptr(unsafe.Pointer(&storeVFS)), 0)
	if rc != sqlite3.SQLITE_OK {
		return fmt.Errorf("sqlite vfs register: %d", rc)
	}
	return nil
}

// ccgoFuncPtr is the code pointer modernc's __ccgo_fp stores: the data
// word of the interface. f must be a package-level function, not a closure.
func ccgoFuncPtr(f interface{}) uintptr {
	type iface [2]uintptr
	return (*iface)(unsafe.Pointer(&f))[1]
}

// storeFullPathname is xFullPathname. SQLite calls it as
// func(*libc.TLS, vfs, zPath, nOut, zOut) int32.
func storeFullPathname(_ *libc.TLS, _ uintptr, zPath uintptr, nOut int32, zOut uintptr) int32 {
	if zPath == 0 || nOut < 2 {
		return sqlite3.SQLITE_CANTOPEN
	}
	path := libc.GoString(zPath)
	if path == "" {
		return sqlite3.SQLITE_CANTOPEN
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return sqlite3.SQLITE_CANTOPEN
	}
	cleaned := filepath.Clean(abs)
	b := append([]byte(cleaned), 0)
	if int32(len(b)) > nOut {
		return sqlite3.SQLITE_CANTOPEN
	}
	copy(libc.GoBytes(zOut, len(b)), b)
	return sqlite3.SQLITE_OK
}
