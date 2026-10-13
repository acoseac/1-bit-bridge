package transcode

import (
	"context"
	"fmt"
	"testing"
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
