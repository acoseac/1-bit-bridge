package qualification

import "os"

// Production playback is Linux-only; a POSIX owner cannot be established here.
func owned(os.FileInfo) bool { return false }
