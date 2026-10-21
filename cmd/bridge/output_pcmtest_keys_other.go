//go:build !unix

package main

import (
	"io"
	"os"
)

// pcmTestStoppableFile has no stoppable read of a file off Unix, where the
// sink mode never runs: it is Linux only.
func pcmTestStoppableFile(*os.File) (io.Reader, func(), func(), bool) { return nil, nil, nil, false }
