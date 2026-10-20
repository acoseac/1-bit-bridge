package decode_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

// readLog is an io.ReadSeeker over raw that records every read REQUEST: the
// offset and the length asked for. A read past the data returns EOF, which
// a decoder reports as truncation, so only the request shows where the
// decoder tried to read.
type readLog struct {
	*bytes.Reader
	reads []readRequest
}

type readRequest struct{ off, n int64 }

func (r *readLog) Read(p []byte) (int, error) {
	off, _ := r.Reader.Seek(0, io.SeekCurrent)
	r.reads = append(r.reads, readRequest{off, int64(len(p))})
	return r.Reader.Read(p)
}

// requireReadsInside fails if a read request ran past end. It subtracts, as
// the walk must: off+n wraps when off is near math.MaxInt64.
func requireReadsInside(t *testing.T, reads []readRequest, end int64) {
	t.Helper()
	for _, rd := range reads {
		if rd.off > end || rd.n > end-rd.off {
			t.Fatalf("read of %d bytes at %d runs past the form's end at %d", rd.n, rd.off, end)
		}
	}
}

// formReachingMaxInt64 is a FRM8 that declares a body ending at
// math.MaxInt64, a valid FVER, and one chunk whose declared size ends it at
// math.MaxInt64-1, with no byte of it present. Positions in a form are even
// and math.MaxInt64 is odd, so this is the last position a walk can reach:
// one byte before the form's end, with no room for a chunk header.
func formReachingMaxInt64() []byte {
	head := make([]byte, 12, 64)
	copy(head, "FRM8")
	binary.BigEndian.PutUint64(head[4:], math.MaxInt64-12)
	raw := append(head, "DSD "...)
	raw = append(raw, dffChunk("FVER", be32(0x01050000))...)
	big := make([]byte, 12)
	copy(big, "BIG ")
	binary.BigEndian.PutUint64(big[4:], math.MaxInt64-45) // body at 44: ends at MaxInt64-1
	return append(raw, big...)
}

// The walk compares what remains of the form with a chunk header's size
// before it reads one. pos+12 wrapped negative at pos = MaxInt64-1 and passed
// the check, and the walk asked for a header that ran past the form (round 3,
// Codex's probe). Every golden's walk stays inside its form too.
func TestDFFWalkReadsNothingPastItsForm(t *testing.T) {
	t.Run("a position one byte before a form ending at MaxInt64", func(t *testing.T) {
		r := &readLog{Reader: bytes.NewReader(formReachingMaxInt64())}
		_, err := decode.Open(r)
		if rf, ok := decode.AsRefusal(err); !ok || rf.Reason != decode.ReasonTruncated {
			t.Fatalf("got %v, want a truncated refusal", err)
		}
		requireReadsInside(t, r.reads, math.MaxInt64)
	})
	g := loadDSDGoldens(t)
	for _, f := range g.Fixtures {
		if !strings.HasSuffix(f.File, ".dff") {
			continue
		}
		_, raw := g.fixture(t, f.File)
		r := &readLog{Reader: bytes.NewReader(raw)}
		if s, err := decode.Open(r); err == nil {
			s.Close()
		}
		requireReadsInside(t, r.reads, 12+int64(binary.BigEndian.Uint64(raw[4:12])))
	}
}
