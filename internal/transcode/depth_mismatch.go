package transcode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// FLAC32EncodeFix is what an operator does when this host's sox cannot write
// a 32-bit FLAC. The pool's outage report and the doctor line share it, so
// the two cannot drift.
const FLAC32EncodeFix = "set the target to 24, or install a sox that can encode 32-bit FLAC"

// logDepthMismatch is the one Warn a bit-depth outage logs, and the Debug
// line every later job of that outage logs under.
const logDepthMismatch = "pool: sox cannot encode this FLAC bit depth"

// logDepthBack is the Info line when a job publishes a file at the depth the
// outage was about.
const logDepthBack = "pool: sox encoded this FLAC bit depth"

// depthMismatchError is a finished FLAC whose STREAMINFO is shallower than
// the job asked for. It wraps ErrRenditionIncomplete, so a batch still
// drains on that error, and the pool classifies it by this type: the
// fallback is the host's sox, and the source is not struck.
type depthMismatchError struct {
	wrote int
	asked int
	err   error
}

func (e *depthMismatchError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *depthMismatchError) Unwrap() error { return e.err }

// markDepthMismatch wraps err as a depth mismatch. wrote and asked are the
// STREAMINFO depth and the job's target.
func markDepthMismatch(wrote, asked int, err error) error {
	return &depthMismatchError{wrote: wrote, asked: asked, err: err}
}

// depthMismatch reports a depth mismatch in err's chain.
func depthMismatch(err error) (depthMismatchError, bool) {
	var d *depthMismatchError
	if !errors.As(err, &d) || d == nil {
		return depthMismatchError{}, false
	}
	return *d, true
}

// depthOutages reports a sox that wrote a shallower FLAC than the job asked
// for, once per asked depth. A success at another depth publishes and does
// not end the streak: sox 14.4.2 writes 16-bit and 24-bit FLAC and refuses
// 32-bit on every input.
type depthOutages struct {
	streaks outageStreaks[int, int]
	now     func() time.Time
}

// fail records one job whose writer fell back from asked bits to wrote, and
// logs it: the Warn when that asked depth's outage starts, Debug otherwise.
func (o *depthOutages) fail(wrote, asked int, path, msg string) {
	s, warn := o.streaks.note(asked, wrote, clockNow(o.now))
	note := fmt.Sprintf("the tool wrote %d-bit samples and the job asked for %d; the source is not struck", wrote, asked)
	if asked == 32 {
		note = FLAC32EncodeFix
	}
	if !warn {
		logger.Debug(logDepthMismatch, "wrote", wrote, "asked", asked, "path", path, "failedJobs", s.failed)
		return
	}
	logger.Warn(logDepthMismatch,
		"wrote", wrote,
		"asked", asked,
		"path", path,
		"err", msg,
		"failedJobs", s.failed,
		"since", s.since,
		"note", note)
}

// proven ends the outage of the depth a published file's STREAMINFO names.
func (o *depthOutages) proven(bits int) {
	if bits <= 0 {
		return
	}
	ended := o.streaks.end(func(asked int, _ int) bool { return asked == bits })
	for _, b := range ended {
		logger.Info(logDepthBack, "bits", b.key, "failedJobs", b.failed, "since", b.since)
	}
}

// CanEncodeFLAC32 reports whether this host's sox writes a FLAC whose
// STREAMINFO names 32 bits. False with a nil error is the host fact: sox
// ran and the file is missing, shallower, or not a FLAC. An error means the
// probe did not run, including a missing sox and a cancelled context.
func CanEncodeFLAC32(ctx context.Context) (bool, error) {
	found, err := soxLookPath("sox")
	if err != nil {
		return false, ErrSoxMissing
	}
	bin := found
	if !filepath.IsAbs(bin) {
		abs, aerr := filepath.Abs(bin)
		if aerr != nil {
			return false, fmt.Errorf("sox: %w", aerr)
		}
		bin = abs
	}
	dir, err := flac32ProbeDir()
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	wav := filepath.Join(dir, "tiny.wav")
	flac := filepath.Join(dir, "tiny.flac")
	// Options bind to the file that follows them. -r precedes -n, or sox
	// generates at its default rate.
	gen := exec.CommandContext(ctx, bin, "-r", "44100", "-c", "1", "-b", "16", "-n", wav, "synth", "0.05", "sine", "440")
	if out, gerr := gen.CombinedOutput(); gerr != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, fmt.Errorf("sox synth: %w (%s)", gerr, clipProbe(out))
	}
	enc := exec.CommandContext(ctx, bin, wav, "-b", "32", flac)
	if _, eerr := enc.CombinedOutput(); eerr != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// sox ran and did not write the file. That is the host fact.
		return false, nil
	}
	s, rerr := readFLACStream(flac)
	if rerr != nil {
		return false, nil
	}
	return s.info.bitsPerSample == 32, nil
}

// flac32ProbeDir makes the probe's directory under the OS temp dir.
// fsutil.Mkdir, not os.MkdirTemp: this package's writers give a directory
// they create the install's owner, and a doctor run as root is one of them.
func flac32ProbeDir() (string, error) {
	for range 8 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		dir := filepath.Join(os.TempDir(), "bridge-flac32-"+hex.EncodeToString(b[:]))
		err := fsutil.Mkdir(dir, 0o700)
		if err == nil {
			return dir, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("flac 32-bit probe: temp directory exists")
}

// clipProbe keeps a failed synth's output off a long doctor line.
func clipProbe(b []byte) string {
	const max = 160
	s := string(b)
	if len(s) > max {
		return s[:max]
	}
	return s
}
