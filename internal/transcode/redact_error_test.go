package transcode

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestRedactErrorKeepsTheChainAndDropsThePaths pins JobSpec.RedactError: the
// text is redactSoxErr's for the job's spec, errors.Is still sees what the
// error wrapped (a caller may ask whether it was a cancel), and nil stays
// nil.
func TestRedactErrorKeepsTheChainAndDropsThePaths(t *testing.T) {
	spec := JobSpec{
		SourceAbsPath:    "/Users/operator/Music/Artist/01.dsf",
		SourceLibraryRel: "Artist/01.dsf",
		TempDir:          "/srv/tmp",
		OutputDir:        "/srv/variants",
	}
	if err := spec.RedactError(nil); err != nil {
		t.Errorf("RedactError(nil) = %v, want nil", err)
	}
	err := spec.RedactError(fmt.Errorf("decode /Users/operator/Music/Artist/01.dsf into /srv/tmp: %w", context.Canceled))
	if got, want := err.Error(), "decode Artist/01.dsf into <tempDir>: context canceled"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(err, context.Canceled) {
		t.Error("errors.Is no longer sees the error RedactError wrapped")
	}
}
