package analyze

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestShortClips runs analysis on complete short clips and on the same clips
// cut to 60% of their bytes. The committed 116 ms iTunes AAC file is the
// field case: complete, it is accepted; cut, the probe still names the
// container duration and the empty decode is a truncation.
func TestShortClips(t *testing.T) {
	requireSox(t)
	if !ffmpegToolsAvailable() {
		t.Skip("ffmpeg toolchain not installed")
	}
	for _, c := range shortClipCases(t) {
		t.Run(c.name, func(t *testing.T) {
			err := analyzeFile(t, c.path)
			if !c.unreadable {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !SourceUnreadable(err) {
				t.Fatalf("got %v, want an unreadable source", err)
			}
			if c.truncated && !strings.Contains(err.Error(), "truncated") {
				t.Fatalf("got %v, want the duration verdict", err)
			}
		})
	}
}

func TestPrimingAllowanceIsAACOnly(t *testing.T) {
	for _, codec := range []string{"aac", "AAC"} {
		if got := primingAllowanceFor(codec); got != aacPrimingAllowance {
			t.Fatalf("%q allowance = %g, want %g", codec, got, aacPrimingAllowance)
		}
	}
	for _, codec := range []string{"", "alac", "mp3", "flac", "pcm_s16le"} {
		if got := primingAllowanceFor(codec); got != 0 {
			t.Fatalf("%q allowance = %g, want 0", codec, got)
		}
	}
}

type shortClip struct {
	name       string
	path       string
	unreadable bool
	truncated  bool
}

type clipEncoder struct {
	name         string
	ext          string
	truncatedCut bool
	run          func(t *testing.T, wav, dst string)
}

func shortClipCases(t *testing.T) []shortClip {
	t.Helper()
	dir := t.TempDir()
	fixture := itunesAACPop(t)
	cases := []shortClip{
		{name: "itunes-116ms", path: fixture},
		{name: "itunes-cut60", path: cutBytes(t, dir, fixture, "itunes-cut60.m4a", 0.6), unreadable: true, truncated: true},
	}
	for _, length := range []string{"0.1", "0.5", "1", "5"} {
		wav := synthWav(t, dir, length)
		for _, enc := range clipEncoders(t) {
			full := filepath.Join(dir, enc.name+"-"+length+enc.ext)
			enc.run(t, wav, full)
			cases = append(cases,
				shortClip{name: enc.name + "-" + length, path: full},
				shortClip{name: enc.name + "-" + length + "-cut60", path: cutBytes(t, dir, full, enc.name+"-"+length+"-cut60"+enc.ext, 0.6), unreadable: true, truncated: enc.truncatedCut},
			)
		}
	}
	return cases
}

func clipEncoders(t *testing.T) []clipEncoder {
	t.Helper()
	enc := []clipEncoder{
		{name: "flac", ext: ".flac", run: encodeWithSox},
		{name: "aac", ext: ".m4a", run: func(t *testing.T, wav, dst string) { encodeWithFFmpeg(t, wav, dst, "aac") }},
		{name: "alac", ext: ".m4a", run: func(t *testing.T, wav, dst string) { encodeWithFFmpeg(t, wav, dst, "alac") }},
	}
	// libmp3lame writes a Xing header that still names the original duration
	// after a prefix cut. sox's own MP3 writer does not: its probe recounts
	// the frames that remain, so the ratio sees a shorter complete file.
	if ffmpegListsEncoder(t, "libmp3lame") {
		enc = append(enc, clipEncoder{
			name: "mp3", ext: ".mp3", truncatedCut: true,
			run: func(t *testing.T, wav, dst string) {
				encodeWithFFmpeg(t, wav, dst, "libmp3lame", "-q:a", "2")
			},
		})
	}
	return enc
}

func analyzeFile(t *testing.T, src string) error {
	t.Helper()
	_, err := RunAnalysis(context.Background(), AnalyzeSpec{
		SourceAbsPath:    src,
		SourceLibraryRel: filepath.Base(src),
		OutputDir:        t.TempDir(),
	})
	return err
}

func itunesAACPop(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	p := filepath.Join(filepath.Dir(file), "..", "manifest", "testdata", "m4a", "itunes_aac_pop.m4a")
	if _, err := os.Stat(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func synthWav(t *testing.T, dir, length string) string {
	t.Helper()
	dst := filepath.Join(dir, "src-"+length+".wav")
	cmd := exec.Command(resolveBin(soxLookPath, "sox"), "-n", "-r", "44100", "-c", "2", dst, "synth", length, "sine", "440")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("synth %s: %v\n%s", length, err, out)
	}
	return dst
}

func encodeWithSox(t *testing.T, wav, dst string) {
	t.Helper()
	cmd := exec.Command(resolveBin(soxLookPath, "sox"), wav, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sox encode %s: %v\n%s", filepath.Base(dst), err, out)
	}
}

func encodeWithFFmpeg(t *testing.T, wav, dst, codec string, extra ...string) {
	t.Helper()
	args := []string{"-y", "-hide_banner", "-loglevel", "error", "-i", wav, "-c:a", codec}
	args = append(args, extra...)
	args = append(args, dst)
	cmd := exec.Command(resolveBin(ffmpegLookPath, "ffmpeg"), args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %s: %v\n%s", codec, err, out)
	}
}

func cutBytes(t *testing.T, dir, src, name string, fraction float64) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	n := int(float64(len(b)) * fraction)
	if n < 1 {
		n = 1
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, b[:n], 0o644); err != nil {
		t.Fatal(err)
	}
	return dst
}

func ffmpegListsEncoder(t *testing.T, name string) bool {
	t.Helper()
	out, err := exec.Command(resolveBin(ffmpegLookPath, "ffmpeg"), "-hide_banner", "-encoders").CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), " "+name+" ")
}
