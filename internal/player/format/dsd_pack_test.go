package format

import (
	"bytes"
	"testing"
)

// TestNativeIndependentVectors pins independently specified asymmetric words.
func TestNativeIndependentVectors(t *testing.T) {
	for _, tc := range []struct {
		f    ALSAFormat
		want []byte
	}{{DSDU32BE, []byte{1, 35, 69, 103, 137, 171, 205, 239}}, {DSDU32LE, []byte{103, 69, 35, 1, 239, 205, 171, 137}}, {DSDU16LE, []byte{35, 1, 171, 137, 103, 69, 239, 205}}} {
		t.Run(string(tc.f), func(t *testing.T) {
			dst := make([]byte, 8)
			n, e := PackDSD(dst, []byte{1, 35, 69, 103}, []byte{137, 171, 205, 239}, tc.f)
			if e != nil || n != 8 || !bytes.Equal(dst, tc.want) {
				t.Fatalf("got %x n=%d err=%v; want %x", dst, n, e, tc.want)
			}
		})
	}
}

// TestNativePackRefusals bounds all inputs before touching the destination.
func TestNativePackRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		dst, l, r []byte
		f         ALSAFormat
	}{{"format", make([]byte, 8), make([]byte, 4), make([]byte, 4), S32LE}, {"channels", make([]byte, 8), make([]byte, 4), make([]byte, 3), DSDU32LE}, {"word", make([]byte, 8), make([]byte, 3), make([]byte, 3), DSDU32LE}, {"destination", make([]byte, 7), make([]byte, 4), make([]byte, 4), DSDU32LE}} {
		t.Run(tc.name, func(t *testing.T) {
			if _, e := PackDSD(tc.dst, tc.l, tc.r, tc.f); e == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
}
