package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// logsCmd is a `tail`-style reader for the bridge's log file, so
// operators don't have to memorise where launchd / systemd / the
// Windows service or its sign-in launcher wrote it.
//
// Default: print the last `defaultLogTailLines` lines and exit.
// `-f` follows the file, polling for appended bytes (1 Hz). `-n N`
// overrides the default tail length. Works against rotated logs by
// honouring file truncation / shrinkage and re-seeking to start.
func logsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return logsCmdWith(ctx, args, stdout, stderr, hostLogs())
}

// logsHost is what `bridge logs` and the console's log path read of the
// host: its platform, its log off Windows and, on Windows, what the two logs'
// paths are made from, what is installed and whether this process is the
// service. hostLogs is this process's; a test gives one of its own, so that
// each platform's choice runs on any host.
type logsHost struct {
	// goos is runtime.GOOS, or the platform a test stands in for.
	goos string
	// defaultLog is packaging.DefaultLogPath: the log off Windows, and on
	// Windows the one a bridge its sign-in launcher starts writes.
	defaultLog func() (string, error)
	// dirs are what the Windows logs' paths are made from.
	dirs packaging.WindowsLogDirs
	// install is the Windows install check, packaging.CheckWindowsInstall;
	// nil tells nothing, as a check that could not tell.
	install func() packaging.WindowsInstall
	// isService says whether this process runs as the Windows service,
	// isWindowsService; nil says it does not.
	isService func() bool
}

// hostLogs is this process's logsHost.
func hostLogs() logsHost {
	return logsHost{
		goos:       runtime.GOOS,
		defaultLog: packaging.DefaultLogPath,
		dirs:       packaging.HostWindowsLogDirs(),
		install:    packaging.CheckWindowsInstall,
		isService:  isWindowsService,
	}
}

// errNoDefaultLog is the log path's answer where a host has none to give.
var errNoDefaultLog = errors.New("no log path is known on this host")

// errNoInstallCheck is the install check's answer where a host has none.
var errNoInstallCheck = errors.New("no install check was made")

// defaultLogPath asks h.defaultLog, nil included.
func (h logsHost) defaultLogPath() (string, error) {
	if h.defaultLog == nil {
		return "", errNoDefaultLog
	}
	return h.defaultLog()
}

// windowsInstall asks h.install, nil included.
func (h logsHost) windowsInstall() packaging.WindowsInstall {
	if h.install == nil {
		return packaging.WindowsInstall{ServiceErr: errNoInstallCheck}
	}
	return h.install()
}

// logsCmdWith is logsCmd on the host given.
func logsCmdWith(ctx context.Context, args []string, stdout, stderr io.Writer, host logsHost) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	follow := fs.Bool("f", false, "follow the log file as it grows (Ctrl-C to stop)")
	tail := fs.Int("n", defaultLogTailLines, "number of trailing lines to print")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	f, code := openLog(host, stderr)
	if f == nil {
		return code
	}
	defer f.Close()
	logPath := f.Name()

	pos, err := tailFile(f, *tail, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "logs: read %s: %v\n", logPath, err)
		return 1
	}
	if !*follow {
		return 0
	}

	// Follow mode. SIGINT (Ctrl-C) ends the loop cleanly.
	followCtx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return followLog(followCtx, f, pos, stdout, stderr)
}

// openLog opens the log `bridge logs` reads, or says on stderr why there is
// none and answers the exit code: DefaultLogPath, on every platform.
func openLog(host logsHost, stderr io.Writer) (*os.File, int) {
	return openDefaultLog(host, stderr)
}

// openDefaultLog opens DefaultLogPath, in the words `bridge logs` says.
func openDefaultLog(host logsHost, stderr io.Writer) (*os.File, int) {
	logPath, err := host.defaultLogPath()
	if err != nil {
		fmt.Fprintf(stderr, "logs: resolve path: %v\n", err)
		return nil, 1
	}
	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "logs: %s does not exist (yet)\n", logPath)
			fmt.Fprintln(stderr, "  the file is created on first service start; run `bridge start` (or `bridge serve` in the foreground) and retry.")
			return nil, 1
		}
		fmt.Fprintf(stderr, "logs: open %s: %v\n", logPath, err)
		return nil, 1
	}
	return f, 0
}

// windowsLogFiles are the files `bridge logs` looks at on Windows: the one
// it reads, and the other, which it names where that is there too and reads
// where the install check could not tell and the first is not there.
type windowsLogFiles struct {
	// read is the file read; readErr says why its path could not be made.
	read    string
	readErr error
	// other is the other file; otherErr says why its path could not be made.
	other    string
	otherErr error
}

// chooseWindowsLog is which log is read on Windows, from whether the service
// is registered and the directories alone, so that a test gives both on any
// host: the service's while the service is registered, running or not, and
// whether or not the sign-in launcher is there too, since the service starts
// at boot and is then the bridge that runs; the user's, DefaultLogPath's,
// where it is not; and the service's first where the check could not tell.
// Choosing the service's needs nothing of the user's path: a home directory
// that cannot be read leaves the other unnamed and the choice as it is.
func chooseWindowsLog(service packaging.ServiceRegistration, dirs packaging.WindowsLogDirs) windowsLogFiles {
	user, userErr := dirs.UserLog()
	if service == packaging.ServiceNotRegistered {
		return windowsLogFiles{read: user, readErr: userErr, other: dirs.ServiceLog()}
	}
	return windowsLogFiles{read: dirs.ServiceLog(), other: user, otherErr: userErr}
}

const (
	// defaultLogTailLines matches the convention of `tail -n` defaults
	// across BSD/GNU utilities. 200 lines is enough to capture a fresh
	// boot cycle (init banners + first scan progress) without
	// flooding the terminal on a long-running deployment.
	defaultLogTailLines = 200

	// followPollInterval is how often the follow loop seeks to the
	// current end-of-file and reads any new bytes. 1 Hz keeps the
	// terminal responsive without burning CPU on an idle log.
	followPollInterval = 1 * time.Second
)

// tailFile prints the last `lines` lines of the open file to w and
// returns the file offset at which it stopped reading (= file
// size). Implementation reads backwards in 8 KiB chunks so a
// multi-GB log file doesn't materialise in memory.
//
// Chunks accumulate into a `[][]byte` and are joined ONCE at the
// end (Gemini Medium on PR #78). Pre-fix, each iteration did
// `buf = append(piece, buf...)` which is O(N²) — every prepend
// reallocates and copies the entire buffer. For a typical 200-line
// tail spanning a few KiB this is academic; for an operator who
// passed `-n 100000` against a long-running log it matters.
func tailFile(f *os.File, lines int, w io.Writer) (int64, error) {
	stat, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := stat.Size()
	if lines <= 0 || size == 0 {
		return size, nil
	}
	const chunk = int64(8192)
	var (
		// chunks collects piece slices in REVERSE order (most-
		// recent first); we reverse them once at the end.
		chunks   [][]byte
		newlines int
		pos      = size
	)
	for pos > 0 && newlines <= lines {
		readSize := chunk
		if pos < chunk {
			readSize = pos
		}
		pos -= readSize
		piece := make([]byte, readSize)
		if _, err := f.ReadAt(piece, pos); err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		newlines += bytes.Count(piece, []byte{'\n'})
		chunks = append(chunks, piece)
	}
	// Reverse chunks (we accumulated tail-first) and join in one
	// pass.
	for i, j := 0, len(chunks)-1; i < j; i, j = i+1, j-1 {
		chunks[i], chunks[j] = chunks[j], chunks[i]
	}
	buf := bytes.Join(chunks, nil)
	// Trim to last `lines` lines.
	idx := bytes.Count(buf, []byte{'\n'}) - lines
	if idx > 0 {
		// Skip past idx newlines from the start.
		for i := 0; i < idx; i++ {
			off := bytes.IndexByte(buf, '\n')
			if off < 0 {
				break
			}
			buf = buf[off+1:]
		}
	}
	if _, err := w.Write(buf); err != nil {
		return 0, err
	}
	return size, nil
}

// followLog polls the open file for appended bytes, printing each
// new chunk to w. Honours file rotation / truncation: if the file's
// size shrinks below the last known position, we close the old fd
// and reopen from the start.
//
// **FD leak fix (Gemini High on PR #78)**: pre-fix, logsCmd's outer
// `defer f.Close()` captured the original *os.File but followLog
// reassigned `f = nf` after a rotation, leaking the new fd when
// the loop exited (e.g. via ctx-cancel). The defer here closes the
// CURRENT fd (whichever has been swapped in by the latest rotation
// handling) before returning.
//
// Uses time.Ticker instead of time.After in a loop (Gemini Medium):
// time.After allocates a new timer per iteration; Ticker re-uses
// one. At 1 Hz the difference is academic but the idiom matches
// the rest of the codebase.
func followLog(ctx context.Context, f *os.File, pos int64, stdout, stderr io.Writer) int {
	defer func() {
		if f != nil {
			_ = f.Close()
		}
	}()
	logPath := f.Name()
	tick := time.NewTicker(followPollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-tick.C:
		}
		stat, err := f.Stat()
		if err != nil {
			fmt.Fprintf(stderr, "logs: stat: %v\n", err)
			return 1
		}
		size := stat.Size()
		if size < pos {
			// Truncated / rotated. Close the old fd FIRST so a
			// long-running follow against a frequently-rotated
			// log doesn't leak handles per rotation event.
			_ = f.Close()
			nf, err := os.Open(logPath)
			if err != nil {
				f = nil // ensure deferred close doesn't re-close
				fmt.Fprintf(stderr, "logs: reopen after rotation: %v\n", err)
				return 1
			}
			f = nf
			pos = 0
			continue
		}
		if size == pos {
			continue
		}
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			fmt.Fprintf(stderr, "logs: seek: %v\n", err)
			return 1
		}
		if _, err := io.CopyN(stdout, f, size-pos); err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintf(stderr, "logs: copy: %v\n", err)
			return 1
		}
		pos = size
	}
}

// adminLogPath resolves the log file the admin console's export offers,
// wired into admin.Deps.LogPath, and the one bridge doctor's log-file-size
// line grades: consoleLogPath on this process's host.
//
// Returns "" rather than an error on failure, and that empty is a real state
// rather than a swallowed fault: the path fails to resolve only where the
// user's home directory cannot be, and in that case there is no log file to
// offer either. admin.Deps documents "" as "this bridge logs to its terminal",
// which is also the honest answer for a foreground `bridge serve` — the path
// resolving says nothing about whether anything writes there.
func adminLogPath() string {
	return consoleLogPath(hostLogs())
}

// consoleLogPath is the log file the console offers: DefaultLogPath, which
// the launchd and systemd units and the Windows sign-in launcher point the
// bridge's output at.
func consoleLogPath(host logsHost) string {
	p, err := host.defaultLogPath()
	if err != nil {
		return ""
	}
	return p
}
