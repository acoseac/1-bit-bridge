//go:build !unix

package alsa

// Windows has no FIFO in its file system and no O_DIRECTORY, and the
// bridge is not built for the other systems the unix tag leaves out (Plan
// 9, js, wasip1), so an open here takes no extra flag, and the type of the
// opened descriptor is the whole check (openRegular, openDirectory).
const (
	openNonblock      = 0
	openDirectoryOnly = 0
)
