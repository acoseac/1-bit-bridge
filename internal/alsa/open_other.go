//go:build !linux && !darwin

package alsa

// Windows has no FIFO in its file system and no O_DIRECTORY, so an open
// here takes no extra flag, and the type of the opened descriptor is the
// whole check (openRegular, openDirectory).
const (
	openNonblock      = 0
	openDirectoryOnly = 0
)
