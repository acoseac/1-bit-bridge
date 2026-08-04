// Command printtmpdir prints os.TempDir().
//
// The Windows CI job needs to exclude the directory `t.TempDir()`
// actually writes to from Defender's on-access scanning. Guessing that
// path is how the first attempt failed: it used $RUNNER_TEMP
// (D:\a\_temp) while Go resolves os.TempDir() from %TEMP%
// (C:\Users\RUNNER~1\AppData\Local\Temp), so the exclusion landed on a
// directory the suite never touches and the experiment proved nothing.
//
// Asking the same runtime the tests use removes the guess. Lives under
// .github/scripts so `go build ./...` and `go test ./...` at the repo
// root don't pick it up as a package of the project proper.
package main

import (
	"fmt"
	"os"
)

func main() { fmt.Print(os.TempDir()) }
