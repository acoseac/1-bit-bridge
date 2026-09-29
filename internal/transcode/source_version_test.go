package transcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// stampedAsScanned stamps spec with the version of its source on disk: the
// size and mtime a scan records in the track's row, which every enqueuer
// stamps. Run renders a file only while it is still that version, so a test
// rendering a real file starts from a spec stamped this way. A source it
// cannot stat is left unstamped, and the test fails on that file anyway.
func stampedAsScanned(spec JobSpec) JobSpec {
	if fi, err := os.Stat(spec.SourceAbsPath); err == nil {
		spec.SourceMTimeNS, spec.SourceSize = fi.ModTime().UnixNano(), fi.Size()
	}
	return spec
}

// sourceChangedFailure is what Run answers for a source that changed after
// its row was written, for a test runner that stands in for Run.
func sourceChangedFailure() error {
	return fmt.Errorf("%w: not rendered", ErrSourceChanged)
}

// retag changes path the way a tagger does: new bytes and a new mtime, a
// minute on, past the serve path's 2 s tolerance.
func retag(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("retagged")); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

// writeSource writes a stand-in source of n bytes at path.
func writeSource(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunRendersNothingFromASourceThatChangedSinceItsStamp: a job can wait
// in a queue for hours (behind a library-wide batch, or a `bridge render`
// run), and its enqueuer's check of the file against its row is as old as
// the job. Run checks again before anything else, a decoder probe or an
// album-gain claim included, and answers ErrSourceChanged, publishing
// nothing, for a source that is not the version its spec records. On main
// the job rendered the new bytes under the row's older stamp, and the serve
// path refused the rendition from then on (410 variant_stale).
//
// PATH is emptied, so a run that got past the check would fail on a missing
// tool instead; the positive control is that failure, for the same spec
// stamped with the file's own version.
func TestRunRendersNothingFromASourceThatChangedSinceItsStamp(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	resetFFmpegSnapshotForTest()
	t.Cleanup(resetFFmpegSnapshotForTest)
	for _, tc := range []struct {
		name string
		spec func(src, out string) JobSpec
	}{
		{"flac", func(src, out string) JobSpec {
			return JobSpec{SourceAbsPath: src, SourceLibraryRel: "Album/01.flac", SourceSampleRate: 96000, SourceBits: 24,
				TargetSampleRate: 48000, TargetBits: 16, Kind: JobKindOptimize, Quality: QualityVeryHigh, OutputDir: out}
		}},
		{"dsf", func(src, out string) JobSpec {
			return JobSpec{SourceAbsPath: src, SourceLibraryRel: "Album/01.dsf", SourceSampleRate: 2822400, SourceIsDSD: true,
				SourceChannels: 2, SourceDurationSec: 1, TargetSampleRate: 176400, TargetBits: 24, Kind: JobKindPCMRender,
				Quality: QualityVeryHigh, OutputDir: out, TempDir: filepath.Join(out, "tmp")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "lib", filepath.Base(tc.spec("", "").SourceLibraryRel))
			writeSource(t, src, 4096)
			spec := stampedAsScanned(tc.spec(src, filepath.Join(dir, "variants")))

			_, err := Run(context.Background(), spec)
			if errors.Is(err, ErrSourceChanged) {
				t.Fatalf("the positive control: a source that is the version its spec records answered %v", err)
			}
			if err == nil {
				t.Fatal("the positive control rendered with no tool on PATH")
			}

			retag(t, src)
			_, err = Run(context.Background(), spec)
			if !errors.Is(err, ErrSourceChanged) {
				t.Fatalf("Run over a source that changed after its stamp = %v, want ErrSourceChanged", err)
			}
			if _, statErr := os.Stat(spec.SidecarPath()); !os.IsNotExist(statErr) {
				t.Errorf("stat rendition = %v, want none published", statErr)
			}
		})
	}
}

// TestPublishingRefusesASourceThatChangedWhileItRendered: both chains
// publish through JobSpec.publishSidecar, the only publish helper, which
// asks first whether the source is still the version the spec records. A
// file that changed while it rendered was read, in part or whole, as bytes
// the stamp does not describe; its rendition is not renamed into place, and
// the temp is left for the chain's deferred cleanup.
func TestPublishingRefusesASourceThatChangedWhileItRendered(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "lib", "01.dsf")
	writeSource(t, src, 4096)
	spec := stampedAsScanned(JobSpec{SourceAbsPath: src, SourceLibraryRel: "Album/01.dsf", SourceIsDSD: true,
		SourceSampleRate: 2822400, TargetSampleRate: 44100, TargetBits: 16, Kind: JobKindOptimize,
		Quality: QualityVeryHigh, OutputDir: filepath.Join(dir, "variants")})
	final := spec.SidecarPath()
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		t.Fatal(err)
	}
	publishAt := func(name string) (string, error) {
		tmp := final + "." + name + sidecarTmpSuffix
		if err := os.WriteFile(tmp, []byte("fLaC"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := spec.publishSidecar(context.Background(), tmp, final)
		return tmp, err
	}

	if _, err := publishAt("unchanged"); err != nil {
		t.Fatalf("publishing over an unchanged source: %v", err)
	}
	if err := os.Remove(final); err != nil {
		t.Fatal(err)
	}

	retag(t, src)
	tmp, err := publishAt("changed")
	if !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("publishing over a source that changed = %v, want ErrSourceChanged", err)
	}
	if _, statErr := os.Stat(final); !os.IsNotExist(statErr) {
		t.Errorf("stat rendition = %v, want nothing published", statErr)
	}
	if _, statErr := os.Stat(tmp); statErr != nil {
		t.Errorf("stat temp = %v, want it left for the chain's cleanup", statErr)
	}
}
