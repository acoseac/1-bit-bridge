//go:build !linux

package main

import "os"

// playRawKeys changes no terminal off Linux, where bridge output play
// refuses before it reads a key.
func playRawKeys(*os.File) (func(), bool) { return nil, false }
