package transcode

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// A rendition is published only when the file its tool wrote is the WHOLE
// rendition, whatever the tool's exit status.
//
// # The defect this exists for
//
// sox 14.4.2 does not fail when a write to its output fails: it prints
// "sox FAIL sox: ... error writing output file: No space left on device" and
// exits 0, leaving the file it began (measured with Homebrew's and Debian's
// builds, backlog B264). Every route trusted that exit: a render onto a
// variants volume that filled published the part it had written, as a
// complete rendition, stamped fresh, so nothing rendered it again. Measured
// on a full HFS+ image through the real pool on main 95c894c3: the
// sox-direct, ALAC and DSD (Stage C) routes all announced "done" over a
// truncated file. sox's own temporary file does the same: `-G` keeps the
// whole signal in one (tmpfile(3), /tmp on Linux), and when it cannot be
// written sox prints "gain: error writing temporary file" and finishes a
// shorter FLAC, exit 0.
//
// # The rule
//
// The file is checked, never the exit status: flac_stream.go reads what
// the stream holds from its frames (the header is no witness: sox writes the
// expected length into it before the audio, and only a finished stream
// rewrites it). A stream is whole when it ends with an intact frame at the
// sample its STREAMINFO declares; a whole stream must then hold the length
// its source implies (the route's own reference: renderLength).
//
// A stream cut short is a write the tool made that failed. Nothing in a
// source can make a write fail, but its size can (a file larger than the
// volume takes, EFBIG), so the volume is asked: a write past the file's end
// (probeVolumeRoom). A refusal for a cause hostOutputFault names makes the
// failure the output side's, which strikes nothing (output_fault.go); any
// other refusal keeps the strike; and a volume that takes the bytes is one
// whose fault is gone: a full volume the job's own temporaries or another
// job's cleanup emptied as the tool exited (sox frees its -G file at exit,
// and on a shared volume that is most of the room), so it is the output
// side's too. A whole stream of the wrong length strikes when the temp
// volume has room for the gain-guard file. When that volume is full, or
// holds less than the guard needs, the short file is the temp volume's
// and strikes nothing (guardTempShort): sox has already freed the file,
// so the free space afterwards is what was left, not what the write saw.

// ErrRenditionIncomplete is returned when the file a render's tool wrote is
// not the whole rendition: a stream cut short, one that is not the rate or
// bit depth the job asked for, or one whose length disagrees with its
// source. Nothing is published.
var ErrRenditionIncomplete = errors.New("rendition incomplete")

// errRenditionCut is the cause of a stream cut short, for the message.
var errRenditionCut = errors.New("the stream the tool wrote was cut short")

// outputProbeBytes is how much probeVolumeRoom asks a volume to take: past a
// block on every filesystem, so a file that ends inside one is no answer.
const outputProbeBytes = 64 << 10

// wholeRendition reads the FLAC a render's tool wrote at path, in the
// sidecar directory dir, and returns what it holds when it is the whole
// rendition j asked for: a stream the tool finished, at the job's target
// rate (the length is judged at that rate). A stream cut short is the output
// side's answer (cutOutput); anything else wraps ErrRenditionIncomplete. The
// bit depth is not this check's: it says nothing about whether the stream
// is whole.
func (j JobSpec) wholeRendition(path, dir string) (flacStream, error) {
	s, err := readFLACStream(path)
	if err != nil {
		if errors.Is(err, errNotFLAC) {
			return flacStream{}, fmt.Errorf("%w: %w (%s)", ErrRenditionIncomplete, err, j.SourceLibraryRel)
		}
		return flacStream{}, markOutputFault(outputVariants, dir, fmt.Errorf("read rendition: %w", err))
	}
	if s.info.sampleRate != j.TargetSampleRate {
		return flacStream{}, fmt.Errorf("%w: the tool wrote %d Hz, the job asked for %d Hz (%s)",
			ErrRenditionIncomplete, s.info.sampleRate, j.TargetSampleRate, j.SourceLibraryRel)
	}
	if !s.whole() {
		return flacStream{}, cutOutput(outputVariants, dir, path,
			fmt.Errorf("%w: %w (%s)", ErrRenditionIncomplete, s.cut(), j.SourceLibraryRel))
	}
	return s, nil
}

// cut describes a stream that is not whole: how far it got against what
// its header declares.
func (s flacStream) cut() error {
	switch {
	case s.info.declared == 0:
		return fmt.Errorf("%w: no length declared, %d samples reached", errRenditionCut, s.reached)
	case s.held > 0:
		return fmt.Errorf("%w: it ends at sample %d of the %d declared", errRenditionCut, s.held, s.info.declared)
	default:
		return fmt.Errorf("%w: it ends inside a frame near sample %d of the %d declared", errRenditionCut, s.reached, s.info.declared)
	}
}

// seconds is the length of a whole stream.
func (s flacStream) seconds() float64 {
	if s.info.sampleRate <= 0 {
		return 0
	}
	return float64(s.held) / float64(s.info.sampleRate)
}

// renditionLengthDisagrees returns the error for a whole rendition whose
// length disagrees with its source's (durationTolerance, both ways), or nil;
// a source of unknown length gives no verdict.
func (j JobSpec) renditionLengthDisagrees(sourceSec float64, s flacStream) error {
	if !decodeLengthDisagrees(sourceSec, s.seconds()) {
		return nil
	}
	return fmt.Errorf("%w: the source is %.3fs, the rendition %.3fs (%s)",
		ErrRenditionIncomplete, sourceSec, s.seconds(), j.SourceLibraryRel)
}

// cutOutput is the answer for an output the tool left cut short after it
// EXITED 0: a write it made failed. Only such a tool gets here: one that
// crashed, was killed (the job's timeout, the OOM killer) or failed on its
// input exits otherwise, and that exit is the run's failure before anything
// reads its output, as it always was. The volume holding path (the place
// where, in dir) is asked for more room. A refusal hostOutputFault names is
// that fault; a volume that takes the bytes again is the output side's too,
// the write's cause gone as the tool exited; any other refusal (EFBIG: a
// file larger than the volume can hold) is no fault of the host and keeps
// the job's strike.
func cutOutput(where, dir, path string, err error) error {
	return classifyCut(where, dir, err, probeVolumeRoom(path))
}

// classifyCut is cutOutput's decision, given what the volume answered.
func classifyCut(where, dir string, err, probe error) error {
	if probe == nil {
		return &outputFaultError{
			fault: outputFault{where: where, kind: outputFull, dir: dir, reason: reasonWriteFailedThenRoom},
			err:   err,
		}
	}
	if kind, reason, ok := hostOutputFault(probe); ok {
		return &outputFaultError{
			fault: outputFault{where: where, kind: kind, dir: dir, reason: reason},
			err:   fmt.Errorf("%w; writing past it: %w", err, probe),
		}
	}
	return fmt.Errorf("%w; writing past it: %w", err, probe)
}

// reasonWriteFailedThenRoom is the outage reason for a stream cut short on a
// volume that took more bytes when the bridge asked: what the tool met is
// gone (the room its own temporaries or another job held, freed as it
// exited).
const reasonWriteFailedThenRoom = "a write the tool made failed, and the volume took more bytes afterwards"

// scratchOutput is the answer for a DSD render's Stage A that did not leave
// a whole scratch at path, in dir (its pipe failed, or the scratch holds
// less than the source): the output side's when the scratch volume refuses
// more bytes for a cause hostOutputFault names, err as it is otherwise. The
// scratch format rewrites its length when sox closes it, so a scratch cut
// short and a source that decoded short look alike, and only the volume
// tells them apart.
func scratchOutput(dir, path string, err error) error {
	return classifyScratch(dir, err, probeVolumeRoom(path))
}

// classifyScratch is scratchOutput's decision, given what the volume
// answered.
func classifyScratch(dir string, err, probe error) error {
	if probe == nil {
		return err
	}
	kind, reason, ok := hostOutputFault(probe)
	if !ok {
		return err
	}
	return &outputFaultError{
		fault: outputFault{where: outputScratch, kind: kind, dir: dir, reason: reason},
		err:   fmt.Errorf("%w; writing past the scratch: %w", err, probe),
	}
}

// probeVolumeRoom asks the volume holding path (a file a failed render
// leaves, which its caller removes) for more room: it writes
// outputProbeBytes past the file's end and returns what the operating system
// answered, nil when the volume took them. The bytes are not zeros, which a
// filesystem that compresses would store as nothing.
func probeVolumeRoom(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		_ = f.Close()
		return err
	}
	_, err = f.Write(probeBytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// reasonGuardTempShort is the host-fault reason for a finished rendition
// whose gain-guard file did not fit. It names no path.
const reasonGuardTempShort = "the gain guard's temporary file did not fit on the temp volume"

// guardTempShort classifies a complete rendition that came out the wrong
// length. The length is asked of the source (ffprobe, or the container's
// own duration on the pipe). When the temp volume cannot hold the int32
// guard file that length implies, the failure is the host's: sox exits 0
// and finishes a shorter FLAC, and a strike would suppress a good file.
// A volume with room keeps the strike. A nil error is returned as it is.
func (j JobSpec) guardTempShort(sourceSec float64, channels int, err error) error {
	if err == nil || j.SourceIsDSD {
		return err
	}
	if channels <= 0 {
		channels = j.SourceChannels
	}
	if channels <= 0 {
		channels = 2
	}
	need := TempBytesForRender(channels, j.TargetSampleRate, sourceSec)
	if need <= 0 {
		return err
	}
	dir := renderScratchDir(j.TempDir)
	if free, ferr := AvailableDiskSpaceNearest(dir); ferr == nil && free < need {
		return &outputFaultError{
			fault: outputFault{where: outputScratch, kind: outputFull, dir: dir, reason: reasonGuardTempShort},
			err:   err,
		}
	}
	if perr := probeTempDirRoom(dir); perr != nil {
		if kind, reason, ok := hostOutputFault(perr); ok {
			return &outputFaultError{
				fault: outputFault{where: outputScratch, kind: kind, dir: dir, reason: reason},
				err:   fmt.Errorf("%w; writing in the temp dir: %w", err, perr),
			}
		}
	}
	return err
}

// probeTempDirRoom asks whether dir will take a write, the question
// probeVolumeRoom asks of a file that already exists. The gain-guard file
// is unlinked before this runs, so there is no file to extend.
func probeTempDirRoom(dir string) error {
	f, err := os.CreateTemp(dir, ".guard-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	// The file already exists, so the owner kept is the directory's: a
	// path in dir that is not there. A root CLI then leaves nothing the
	// service user cannot remove if the probe is interrupted.
	if err := fsutil.KeepOwner(f, filepath.Join(dir, ".guard-probe-absent")); err != nil {
		_ = f.Close()
		return err
	}
	_, err = f.Write(probeBytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// probeBytes is outputProbeBytes of a fixed pseudo-random pattern
// (xorshift), so nothing between the probe and the disk can store it as
// less than it is.
func probeBytes() []byte {
	b := make([]byte, outputProbeBytes)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}
