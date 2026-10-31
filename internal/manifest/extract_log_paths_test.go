package manifest

import (
	"bytes"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// pathFailingSource is an M4A's bytes as its audio file, whose reads fail
// once more MP4 walks have begun than walks (each walk starts by seeking to
// the end of the file, findMoov), with what an *os.File's failed read is: an
// *fs.PathError naming the file's absolute path.
type pathFailingSource struct {
	r     *bytes.Reader
	abs   string
	walks int
	ends  int
}

func (s *pathFailingSource) failing() bool { return s.ends > s.walks }

func (s *pathFailingSource) failure() error {
	return &fs.PathError{Op: "read", Path: s.abs, Err: syscall.EIO}
}

func (s *pathFailingSource) Read(p []byte) (int, error) {
	if s.failing() {
		return 0, s.failure()
	}
	return s.r.Read(p)
}

func (s *pathFailingSource) ReadAt(p []byte, off int64) (int, error) {
	if s.failing() {
		return 0, s.failure()
	}
	return s.r.ReadAt(p, off)
}

func (s *pathFailingSource) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		s.ends++
	}
	return s.r.Seek(offset, whence)
}

func (s *pathFailingSource) Close() error { return nil }

func (s *pathFailingSource) Stat() (fs.FileInfo, error) {
	return sizedInfo{name: filepath.Base(s.abs), size: s.r.Size()}, nil
}

// sizedInfo is the FileInfo of a regular file of size bytes.
type sizedInfo struct {
	name string
	size int64
}

func (i sizedInfo) Name() string       { return i.name }
func (i sizedInfo) Size() int64        { return i.size }
func (i sizedInfo) Mode() fs.FileMode  { return 0o644 }
func (i sizedInfo) ModTime() time.Time { return time.Unix(1700000000, 0) }
func (i sizedInfo) IsDir() bool        { return false }
func (i sizedInfo) Sys() any           { return nil }

// attrString is the value of rec's attribute key as a handler writes it, and
// whether rec has it.
func attrString(rec slog.Record, key string) (string, bool) {
	var v string
	var found bool
	rec.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v, found = a.Value.Resolve().String(), true
			return false
		}
		return true
	})
	return v, found
}

// TestAnMP4WalkThatCannotReadItsFileNamesNoAbsolutePath: every MP4 walk of
// the extractor that fails on a read of the file Warns, naming the file by
// its library-relative path and the failure by its operation and the OS's
// errno, and no line it logs names the file's absolute path. The walks read
// through faultNotingSource, which hands each parser the *os.File's own
// error, and an *fs.PathError names the absolute path: the lines that already
// named the file by trackLogPath still carried that path in their err, and
// the older ones named it in their path as well. The first run fails every
// read, so every walk the file's kind takes fails; the second reads the file
// whole once, so the codec walk finds ALAC and the bit-depth walk, which only
// an ALAC file takes, fails with the rest.
func TestAnMP4WalkThatCannotReadItsFileNamesNoAbsolutePath(t *testing.T) {
	const rel = "Artist/Album/01 Song.m4a"
	runs := []struct {
		name  string
		walks int
		want  []string
	}{
		{"every read fails", 0, []string{
			"mp4 codec walk failed; falling back to extension classification",
			"mp4 sample-rate walk failed; manifest will carry nil sampleRate",
			"mp4 duration walk failed; manifest will carry nil duration",
			"mp4 predefined-genre walk failed; manifest will carry no genre",
			"mp4 freeform-advisory walk failed; the other explicit signals stand",
			"mp4 content-rating walk failed; the other explicit signals stand",
		}},
		{"the reads after the codec walk fail", 1, []string{
			"mp4 ALAC bit-depth walk failed; manifest will carry nil bits",
			"mp4 sample-rate walk failed; manifest will carry nil sampleRate",
			"mp4 duration walk failed; manifest will carry nil duration",
			"mp4 predefined-genre walk failed; manifest will carry no genre",
			"mp4 freeform-advisory walk failed; the other explicit signals stand",
			"mp4 content-rating walk failed; the other explicit signals stand",
		}},
	}
	for _, run := range runs {
		t.Run(run.name, func(t *testing.T) {
			logs := loggingtest.Record(t)
			library := filepath.Join(t.TempDir(), "Private Library")
			abs := filepath.Join(library, filepath.FromSlash(rel))
			src := &pathFailingSource{r: bytes.NewReader(buildMP4WithALACConfigRate(24, 96000)), abs: abs, walks: run.walks}
			ec := &ExtractContext{openAudio: func(string) (extractSource, error) { return src, nil }}
			tr := &Track{Path: rel}
			err := ExtractWithContext(abs, tr, ec)
			if readFault(err) == nil {
				t.Fatalf("ExtractWithContext = %v, want the file's unread read (the store must not take it)", err)
			}
			for _, msg := range run.want {
				var got []slog.Record
				for _, rec := range logs.Records() {
					if rec.Message == msg {
						got = append(got, rec)
					}
				}
				if len(got) != 1 {
					t.Errorf("%q was logged %d times, want once", msg, len(got))
					continue
				}
				rec := got[0]
				if rec.Level != slog.LevelWarn {
					t.Errorf("%q was logged at %v, want Warn", msg, rec.Level)
				}
				if p, _ := attrString(rec, "path"); p != rel {
					t.Errorf("%q names the file %q, want its library-relative path %q", msg, p, rel)
				}
				e, ok := attrString(rec, "err")
				if !ok || !strings.Contains(e, "read") || !strings.Contains(e, syscall.EIO.Error()) {
					t.Errorf("%q gives err=%q, want the failed operation (read) and the OS's errno (%s)", msg, e, syscall.EIO.Error())
				}
			}
			if leaked := logs.Carrying(abs, library); len(leaked) != 0 {
				t.Errorf("a line names the file's absolute path:\n%s", strings.Join(leaked, "\n"))
			}
		})
	}
}
