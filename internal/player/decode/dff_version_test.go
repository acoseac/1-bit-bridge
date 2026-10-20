package decode_test

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
)

// dffTopChunks splits a well-formed DFF golden's FRM8 body, after its "DSD "
// form type, into its chunks, each with its header and pad byte.
func dffTopChunks(t testing.TB, raw []byte) [][]byte {
	t.Helper()
	body := raw[16:]
	var out [][]byte
	for len(body) >= 12 {
		size := binary.BigEndian.Uint64(body[4:12])
		n := 12 + int(size) + int(size&1)
		if n > len(body) {
			t.Fatalf("chunk %q runs past the golden", body[0:4])
		}
		out = append(out, body[:n])
		body = body[n:]
	}
	return out
}

// dffForm wraps chunks in a FRM8 "DSD " form whose size counts them.
func dffForm(chunks ...[]byte) []byte {
	body := []byte("DSD ")
	for _, c := range chunks {
		body = append(body, c...)
	}
	head := make([]byte, 12)
	copy(head, "FRM8")
	binary.BigEndian.PutUint64(head[4:], uint64(len(body)))
	return append(head, body...)
}

// dffChunk is a chunk with its header and, after an odd body, a pad byte.
func dffChunk(id string, body []byte) []byte {
	out := make([]byte, 12, 12+len(body)+1)
	copy(out, id)
	binary.BigEndian.PutUint64(out[4:], uint64(len(body)))
	out = append(out, body...)
	if len(body)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }

// DSDIFF 1.5 §3 and §3.1: FVER is the form's first chunk, its body is the
// four-byte version, and its first byte is the major version, 1. A file
// without it, with it elsewhere or twice, of another size, or of another
// major version is refused, and the refusal names FVER.
func TestDFFFormatVersionRequired(t *testing.T) {
	g := loadDSDGoldens(t)
	f, raw := g.fixture(t, "dsd64.dff")
	chunks := dffTopChunks(t, raw)
	if string(chunks[0][0:4]) != "FVER" {
		t.Fatalf("the golden's first chunk is %q", chunks[0][0:4])
	}
	fver, rest := chunks[0], chunks[1:]
	at := func(id string) int {
		for i, c := range rest {
			if string(c[0:4]) == id {
				return i
			}
		}
		t.Fatalf("golden has no %q chunk", id)
		return -1
	}
	prop, sound := at("PROP"), at("DSD ")
	join := func(parts ...[][]byte) [][]byte {
		var out [][]byte
		for _, p := range parts {
			out = append(out, p...)
		}
		return out
	}
	one := func(c []byte) [][]byte { return [][]byte{c} }

	cases := []struct {
		name   string
		raw    []byte
		reason decode.Reason
	}{
		{"missing", dffForm(rest...), decode.ReasonUnsupportedLayout},
		{"second", dffForm(join(rest[:1], one(fver), rest[1:])...), decode.ReasonUnsupportedLayout},
		{"after PROP", dffForm(join(rest[:prop+1], one(fver), rest[prop+1:])...), decode.ReasonUnsupportedLayout},
		{"twice in a row", dffForm(join(one(fver), one(fver), rest)...), decode.ReasonUnsupportedLayout},
		{"again before the sound data", dffForm(join(one(fver), rest[:sound], one(fver), rest[sound:])...), decode.ReasonUnsupportedLayout},
		{"empty", dffForm(join(one(dffChunk("FVER", nil)), rest)...), decode.ReasonUnsupportedLayout},
		{"two bytes", dffForm(join(one(dffChunk("FVER", []byte{1, 5})), rest)...), decode.ReasonUnsupportedLayout},
		{"eight bytes", dffForm(join(one(dffChunk("FVER", append(be32(0x01050000), be32(0)...))), rest)...), decode.ReasonUnsupportedLayout},
		{"major 2", dffForm(join(one(dffChunk("FVER", be32(0x02000000))), rest)...), decode.ReasonUnsupportedLayout},
		{"major 0", dffForm(join(one(dffChunk("FVER", be32(0x00050000))), rest)...), decode.ReasonUnsupportedLayout},
		// The file ends two bytes into the version.
		{"cut inside the version", raw[:16+12+2], decode.ReasonTruncated},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			s, err := decode.Open(bytes.NewReader(c.raw))
			if err == nil {
				s.Close()
				t.Fatalf("opened; want refusal %s", c.reason)
			}
			r, ok := decode.AsRefusal(err)
			if !ok || r.Reason != c.reason || !strings.Contains(r.Message, "FVER") {
				t.Fatalf("got %v, want a %s refusal that names FVER", err, c.reason)
			}
		})
	}

	// Another minor version of 1 is the same layout and opens.
	for _, v := range []uint32{0x01040000, 0x01FF1234} {
		d, err := openDSD(bytes.NewReader(dffForm(join(one(dffChunk("FVER", be32(v))), rest)...)))
		if err != nil {
			t.Fatalf("version %#08x: %v", v, err)
		}
		requireSameDSD(t, readAllDSD(t, d, 4096), g.want(f))
		d.Close()
	}
}
