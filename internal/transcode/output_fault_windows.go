//go:build windows

package transcode

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// outputFaultErrnos are the causes, besides a permission, that describe the
// output side's volume rather than the job: hostOutputFault's table, in the
// codes Windows reports.
var outputFaultErrnos = map[syscall.Errno]outputFaultKind{
	windows.ERROR_WRITE_PROTECT:       outputReadOnly,
	windows.ERROR_DISK_FULL:           outputFull,
	windows.ERROR_HANDLE_DISK_FULL:    outputFull,
	windows.ERROR_DISK_QUOTA_EXCEEDED: outputQuota,
	windows.ERROR_IO_DEVICE:           outputGone,
	windows.ERROR_NOT_READY:           outputGone,
	windows.ERROR_DEV_NOT_EXIST:       outputGone,
	windows.ERROR_NETNAME_DELETED:     outputGone,
}

// createOutput does nothing on Windows: the tool creates its own output
// there, as it did before (fsutil.Precreate, which the unix createOutput
// replaced, did nothing on Windows either, where a file takes its
// directory's ACL). A file freshly closed on Windows can be held by Defender
// or the Search Indexer (atomicwrite.RenameWithRetry), and whether such a
// hold makes the tool's own open of it for writing fail has not been
// measured, so the bridge does not create a file there for a tool to open.
func createOutput(string, string) error { return nil }
