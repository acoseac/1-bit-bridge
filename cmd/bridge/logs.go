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
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/packaging"
)

// logsCmd is a `tail`-style reader for the per-OS bridge log file,
// so operators don't have to memorise where launchd / systemd /
// %PROGRAMDATA% wrote it.
//
// Default: print the last `defaultLogTailLines` lines and exit.
// `-f` follows the file, polling for appended bytes (1 Hz). `-n N`
// overrides the default tail length. Works against rotated logs by
// honouring file truncation / shrinkage and re-seeking to start.
func logsCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	follow := fs.Bool("f", false, "follow the log file as it grows (Ctrl-C to stop)")
	tail := fs.Int("n", defaultLogTailLines, "number of trailing lines to print")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	logPath, err := packaging.DefaultLogPath()
	if err != nil {
		fmt.Fprintf(stderr, "logs: resolve path: %v\n", err)
		return 1
	}

	f, err := os.Open(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "logs: %s does not exist (yet)\n", logPath)
			fmt.Fprintln(stderr, "  the file is created on first service start; run `bridge start` (or `bridge serve` in the foreground) and retry.")
			return 1
		}
		fmt.Fprintf(stderr, "logs: open %s: %v\n", logPath, err)
		return 1
	}
	defer f.Close()

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
		buf      []byte
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
		buf = append(piece, buf...)
		newlines = bytes.Count(buf, []byte{'\n'})
	}
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
// size shrinks below the last known position, we reopen and read
// from the start.
func followLog(ctx context.Context, f *os.File, pos int64, stdout, stderr io.Writer) int {
	logPath := f.Name()
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(followPollInterval):
		}
		stat, err := f.Stat()
		if err != nil {
			fmt.Fprintf(stderr, "logs: stat: %v\n", err)
			return 1
		}
		size := stat.Size()
		if size < pos {
			// Truncated / rotated. Reopen and start from the new
			// beginning.
			f.Close()
			nf, err := os.Open(logPath)
			if err != nil {
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
