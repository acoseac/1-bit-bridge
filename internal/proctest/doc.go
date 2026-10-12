// Package proctest helps a test with a process it started but cannot reap,
// such as a grandchild. Exited answers whether one has exited, and
// HoldUntilReleased holds one until the test releases it, without letting
// it outlive the test (hold.go). HoldUntilReleased is Unix-only. Like
// net/http/httptest it is imported only by tests, so none of it reaches
// the binary.
//
// kill(pid, 0) alone cannot say whether a process has exited. It succeeds
// for a ZOMBIE, a process that has exited and whose exit status nobody has
// collected, because the kernel keeps the process's entry until its parent
// reaps it. A process whose parent has died is handed to the pid namespace's
// init, and reaping those is init's job: systemd, launchd and the tini that
// `docker run --init` adds each do it within milliseconds, which is why a
// kill(pid, 0) poll passed on every host CI has. Without --init, PID 1 in a
// container is the container's own command, `go test` in the stock golang
// image, and `go` collects only the children it started. A killed grandchild
// then stays a zombie until the container exits, and kill(pid, 0) calls it
// running. Measured on Ubuntu 26.04 with golang:1.26.6: the CLI stand-in
// that TestServeLeavesNoTailscaleCLIRunning and
// TestCancelStopsTheWholeCLIProcessTree kill read `State: Z (zombie)` and
// `PPid: 1`, with `go test` as PID 1, and both tests failed without --init
// and passed with it.
//
// A zombie runs no code and holds no files, so it writes nothing, which is
// what those tests guard. On Linux, Exited asks /proc about a process that
// kill(pid, 0) still finds. Elsewhere it keeps kill(pid, 0)'s answer: macOS
// has launchd as init, and CI runs no other unix.
//
// On Windows, Exited opens the process for SYNCHRONIZE and waits with a
// zero timeout (exited_windows.go). It never signals or ends the process.
package proctest
