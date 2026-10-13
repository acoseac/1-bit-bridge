package manifest

import (
	"errors"
	"fmt"
	"io"
	"math"
)

// ErrDSFAudioMalformed is a DSF whose fmt sample count and data chunk
// disagree by more than one block, or whose header cannot describe audio.
// A short decode is a different error: this one is a fact about the file.
var ErrDSFAudioMalformed = errors.New("dsf audio header is malformed")

// ErrContainerHeaderAbsent is a file whose leading bytes are not a DSF
// or a DFF header. A render of that file keeps the probed duration.
var ErrContainerHeaderAbsent = errors.New("container header absent")

type dsfMalformedError struct{ detail string }

func (e dsfMalformedError) Error() string {
	if e.detail == "" {
		return ErrDSFAudioMalformed.Error()
	}
	return ErrDSFAudioMalformed.Error() + ": " + e.detail
}

func (e dsfMalformedError) Unwrap() error { return ErrDSFAudioMalformed }

func errDSFMalformed(detail string) error {
	return dsfMalformedError{detail: detail}
}

// DSFAudioHeader is the fmt chunk and the data chunk's declared size.
// SampleCount is samples per channel. DataBytes is the declared payload
// of every channel: the data chunk's size minus its 12-byte header.
type DSFAudioHeader struct {
	SampleRate    uint32
	Channels      int
	BitsPerSample int
	SampleCount   int64
	BlockSize     uint32
	DataBytes     uint64
}

// DurationSec is the fmt sample count divided by the sampling frequency.
// Zero means the header does not name a duration.
func (h DSFAudioHeader) DurationSec() float64 {
	if h.SampleRate == 0 || h.SampleCount <= 0 {
		return 0
	}
	return float64(h.SampleCount) / float64(h.SampleRate)
}

// ReadDSFAudioHeader reads the 28-byte DSD chunk, the 52-byte fmt chunk
// and the 12-byte data header. It does not read the payload.
func ReadDSFAudioHeader(r io.Reader) (DSFAudioHeader, error) {
	var buf [92]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return DSFAudioHeader{}, fmt.Errorf("dsf audio header: %w", absent(err))
	}
	if string(buf[0:4]) != "DSD " || string(buf[28:32]) != "fmt " || string(buf[80:84]) != "data" {
		return DSFAudioHeader{}, fmt.Errorf("dsf audio header: %w", ErrContainerHeaderAbsent)
	}
	var h DSFAudioHeader
	if raw := le32(buf[52:56]); raw > 0 && raw <= 64 {
		h.Channels = int(raw)
	}
	h.SampleRate = le32(buf[56:60])
	h.BitsPerSample = int(le32(buf[60:64]))
	h.SampleCount = int64(le64(buf[64:72]))
	h.BlockSize = le32(buf[72:76])
	if dataSize := le64(buf[84:92]); dataSize >= 12 {
		h.DataBytes = dataSize - 12
	}
	return h, nil
}

// Check refuses a header whose per-channel data bytes and sample count
// differ by more than one block, and a data chunk that declares no
// audio. A truncated file keeps both fields in agreement and is not
// this error.
func (h DSFAudioHeader) Check() error {
	if h.SampleRate == 0 || h.SampleCount <= 0 || h.Channels < 1 || h.BlockSize == 0 {
		return errDSFMalformed("")
	}
	got, ok := h.payloadPerChannel()
	if !ok {
		return errDSFMalformed("data chunk")
	}
	want := uint64(h.SampleCount) / 8
	diff := got - want
	if want > got {
		diff = want - got
	}
	if diff > uint64(h.BlockSize) {
		return errDSFMalformed("sample count and data chunk disagree")
	}
	return nil
}

func (h DSFAudioHeader) payloadPerChannel() (uint64, bool) {
	if h.Channels < 1 || h.DataBytes == 0 || h.DataBytes%uint64(h.Channels) != 0 {
		return 0, false
	}
	return h.DataBytes / uint64(h.Channels), true
}

// DFFAudioHeader is the duration an uncompressed DSDIFF names in its DSD
// chunk, or the mark that the file is DST. The DSD chunk's byte length is
// not a DST file's playing time. A compression other than absent or DSD,
// and a payload of no bytes, are not a duration either.
type DFFAudioHeader struct {
	SampleRate  uint32
	Channels    int
	DataBytes   uint64
	DST         bool
	haveDSD     bool
	compression dffCompression
}

// DurationSec is the DSD chunk's declared payload, in seconds. DST, a
// compression other than absent or DSD, a payload of no bytes, and a
// header that has not named a rate, a channel count and a DSD chunk
// report false.
func (h DFFAudioHeader) DurationSec() (float64, bool) {
	if h.DST || !h.referenceCompression() || !h.haveDSD || h.SampleRate == 0 || h.Channels < 1 || h.DataBytes == 0 {
		return 0, false
	}
	return float64(h.DataBytes) * 8 / (float64(h.Channels) * float64(h.SampleRate)), true
}

// referenceCompression is the absent case, which means uncompressed, and an
// explicit DSD compression. Anything else is not a completeness reference.
func (h DFFAudioHeader) referenceCompression() bool {
	return h.compression == dffCompressionAbsent || h.compression == dffCompressionUncompressed
}

// answered is true once the header has decided whether it names a duration.
func (h DFFAudioHeader) answered() bool {
	if h.DST || !h.referenceCompression() {
		return true
	}
	if _, ok := h.DurationSec(); ok {
		return true
	}
	return h.haveDSD && h.DataBytes == 0 && h.SampleRate != 0 && h.Channels >= 1
}

const (
	maxDFFHeaderChunks = 64
	maxDFFPropBytes    = 1 << 20
)

// ReadDFFAudioHeader walks the form until it has a rate, a channel count
// and a DSD chunk, or until it sees DST. It seeks past chunk payloads and
// does not read them. A DSD chunk's declared size is the reference even
// when the payload runs past the end of the file.
func ReadDFFAudioHeader(r io.ReadSeeker) (DFFAudioHeader, error) {
	if err := readDFFForm(r); err != nil {
		return DFFAudioHeader{}, err
	}
	var h DFFAudioHeader
	for range maxDFFHeaderChunks {
		id, size, err := readDFFChunkHeader(r)
		if err != nil {
			return h.finish(err)
		}
		done, err := h.takeDFFChunk(r, id, size)
		if err != nil {
			return DFFAudioHeader{}, err
		}
		if done {
			return h, nil
		}
	}
	return h.finish(errors.New("dff audio header names no duration"))
}

func (h DFFAudioHeader) finish(err error) (DFFAudioHeader, error) {
	if h.answered() {
		return h, nil
	}
	return DFFAudioHeader{}, fmt.Errorf("dff audio header: %w", err)
}

func (h *DFFAudioHeader) takeDFFChunk(r io.ReadSeeker, id string, size uint64) (bool, error) {
	switch id {
	case "PROP":
		if err := h.readDFFProp(r, size); err != nil {
			return false, err
		}
	case "DST ":
		h.DST = true
		_ = skipDFFPayload(r, size)
		return true, nil
	case "DSD ":
		h.haveDSD = true
		h.DataBytes = size
		// A short file still names this declared length. Seeking past
		// the end succeeds, and a seek that fails keeps the length.
		_ = skipDFFPayload(r, size)
	default:
		if err := skipDFFPayload(r, size); err != nil {
			return false, err
		}
	}
	return h.answered(), nil
}

func (h *DFFAudioHeader) readDFFProp(r io.ReadSeeker, size uint64) error {
	if size < 4 || size > maxDFFPropBytes {
		return fmt.Errorf("dff audio header: prop size %d", size)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("dff audio header: %w", err)
	}
	if size%2 == 1 {
		if _, err := r.Seek(1, io.SeekCurrent); err != nil {
			return err
		}
	}
	info := parsePropChunks(body[4:])
	if info.haveFS {
		h.SampleRate = info.fsRate
	}
	if info.haveCHNL {
		h.Channels = info.channels
	}
	h.compression = info.compression
	if info.compression == dffCompressionDST {
		h.DST = true
	}
	return nil
}

func readDFFForm(r io.Reader) error {
	var hdr [16]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("dff audio header: %w", absent(err))
	}
	if string(hdr[0:4]) != "FRM8" || string(hdr[12:16]) != "DSD " {
		return fmt.Errorf("dff audio header: %w", ErrContainerHeaderAbsent)
	}
	return nil
}

func absent(err error) error {
	return fmt.Errorf("%w: %w", ErrContainerHeaderAbsent, err)
}

func readDFFChunkHeader(r io.Reader) (string, uint64, error) {
	var hdr [12]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", 0, err
	}
	return string(hdr[0:4]), be64(hdr[4:12]), nil
}

func skipDFFPayload(r io.ReadSeeker, size uint64) error {
	n := size
	if size%2 == 1 {
		if size == math.MaxUint64 {
			return errors.New("dff audio header: chunk size")
		}
		n++
	}
	if n > uint64(math.MaxInt64) {
		return errors.New("dff audio header: chunk size")
	}
	_, err := r.Seek(int64(n), io.SeekCurrent)
	return err
}
