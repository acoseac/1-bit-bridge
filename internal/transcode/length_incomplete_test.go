package transcode

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func TestLengthIncompleteFileIsTheDecodeAndNotAnOutputFault(t *testing.T) {
	wrapped := fmt.Errorf("stage A: %w", ErrFFmpegDecodeIncomplete)
	redacted := JobSpec{SourceLibraryRel: "Album/a.dsf"}.RedactError(wrapped)
	fault := &outputFaultError{err: wrapped}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"the sentinel", ErrFFmpegDecodeIncomplete, true},
		{"wrapped", wrapped, true},
		{"redacted", redacted, true},
		{"an output fault wrapping it", fault, false},
		{"a cancelled context", context.Canceled, false},
		{"any other error", fmt.Errorf("mkdir: permission denied"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LengthIncompleteFile(tc.err); got != tc.want {
				t.Errorf("LengthIncompleteFile = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRepeatedFileFactKeepsAMalformedHeaderDistinct(t *testing.T) {
	malformed := fmt.Errorf("decode: %w", manifest.ErrDSFAudioMalformed)
	fault := &outputFaultError{err: malformed}
	if LengthIncompleteFile(malformed) {
		t.Fatal("a malformed header is a short decode")
	}
	if !RepeatedFileFact(malformed) {
		t.Fatal("a malformed header is a fact a later render would repeat")
	}
	if RepeatedFileFact(fault) {
		t.Fatal("an output fault wrapping a malformed header is the volume")
	}
	if _, ok := unwritableOutput(malformed); ok {
		t.Fatal("a malformed header is an output fault")
	}
	if _, ok := unavailableTool(malformed); ok {
		t.Fatal("a malformed header is a missing tool")
	}
	if !errors.Is(malformed, manifest.ErrDSFAudioMalformed) {
		t.Fatal(malformed)
	}
}
