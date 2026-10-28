package format_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDoPIndependentVectors(t *testing.T) {
	for _, tc := range []struct{ name, file string }{{"dsf_lsb", "lsb_dsd64_partial_block.dsf"}, {"dsf_msb", "msb_dsd128_partial_block.dsf"}, {"dff_lr", "dsd64.dff"}, {"dff_rl", "dsd256_right_first.dff"}} {
		t.Run(tc.name, func(t *testing.T) {
			for _, chunk := range []int{1, 3, 777} {
				file, e := os.Open(filepath.Join("..", "decode", "testdata", "dsd-goldens", tc.file))
				if e != nil {
					t.Fatal(e)
				}
				src, e := decode.Open(file)
				if e != nil {
					file.Close()
					t.Fatal(e)
				}
				d := src.(decode.DSDDecoder)
				planes := [][]byte{make([]byte, chunk), make([]byte, chunk)}
				var left, right []byte
				for {
					n, e := d.ReadDSD(context.Background(), planes)
					left = append(left, planes[0][:n]...)
					right = append(right, planes[1][:n]...)
					if e == io.EOF {
						break
					}
					if e != nil {
						t.Fatal(e)
					}
				}
				src.Close()
				file.Close()
				out := make([]byte, len(left)*4)
				n, _, e := format.PackDoP(out, left, right, 5)
				// SHA-256 derived independently from committed pattern.bin by Python,
				// word=(marker<<24)|(chronological_a<<16)|(chronological_b<<8).
				if e != nil || n != 53152 || fmt.Sprintf("%x", sha256.Sum256(out)) != "3f7efabdfc9c9afe63bb4376ffe454ea863f65758759f0392d941df5763807c1" {
					t.Fatalf("chunk=%d bytes=%d hash=%x error=%v", chunk, n, sha256.Sum256(out), e)
				}
			}
		})
	}

	t.Run("literal", func(t *testing.T) {
		dst := make([]byte, 16)
		n, phase, err := format.PackDoP(dst, []byte{1, 0x23, 0x45, 0x67}, []byte{0x89, 0xab, 0xcd, 0xef}, 5)
		want := []byte{0, 0x23, 1, 5, 0, 0xab, 0x89, 5, 0, 0x67, 0x45, 0xfa, 0, 0xef, 0xcd, 0xfa}
		if err != nil || n != 16 || phase != 5 || !bytes.Equal(dst, want) {
			t.Fatalf("got %x n=%d phase=%x error=%v", dst, n, phase, err)
		}
	})
}

func TestDoPPackerValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		l, r, d int
		phase   byte
	}{
		{"unequal", 2, 4, 16, 5}, {"odd", 3, 3, 16, 5}, {"short", 4, 4, 15, 5}, {"phase", 2, 2, 8, 0}, {"empty", 0, 0, 0, 0xfa},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := bytes.Repeat([]byte{0x9a}, tc.d)
			before := bytes.Clone(dst)
			n, p, e := format.PackDoP(dst, make([]byte, tc.l), make([]byte, tc.r), tc.phase)
			if n != 0 || p != tc.phase || !bytes.Equal(dst, before) {
				t.Fatal("refusal changed destination or phase")
			}
			if (e == nil) != (tc.name == "empty") {
				t.Fatalf("error=%v", e)
			}
		})
	}
}
