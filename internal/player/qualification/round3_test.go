package qualification

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQualificationDependencyGate(t *testing.T) {
	for _, p := range []string{"internal/player/binding/binding.go", "internal/alsa/sysfs.go", "internal/player/decode/dsf.go", "internal/player/decode/dff.go", "internal/player/format/format.go", "internal/fsutil/notafile.go", "internal/alsapcm/sink_run.go", "cmd/bridge/output_pcmtest_keys_unix.go", "cmd/bridge/configpath.go"} {
		t.Run(filepath.Base(p), func(t *testing.T) {
			dir := t.TempDir()
			if e := os.MkdirAll(filepath.Join(dir, ".git/refs/heads"), 0755); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(dir, ".git/HEAD"), []byte("ref: refs/heads/main\n"), 0644); e != nil {
				t.Fatal(e)
			}
			files := map[string]string{"internal/player/qualification/version.go": "const qualificationVersion = 2\n", p: "before"}
			base := gitCommit(t, dir, files)
			files[p] = "after"
			head := gitCommit(t, dir, files)
			if e := os.WriteFile(filepath.Join(dir, ".git/refs/heads/main"), []byte(head+"\n"), 0644); e != nil {
				t.Fatal(e)
			}
			if e := CheckVersion(dir, base, head); e == nil {
				t.Fatal("dependency changed without version bump")
			}
		})
	}
}

func TestQualificationFixtureKinds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    os.FileMode
		allowed bool
	}{{"regular", 0, true}, {"cloud", os.ModeIrregular, true}, {"directory", os.ModeDir, false}, {"fifo", os.ModeNamedPipe, false}, {"socket", os.ModeSocket, false}, {"device", os.ModeDevice, false}} {
		t.Run(tc.name, func(t *testing.T) {
			if (fixtureKind(tc.mode) == "") != tc.allowed {
				t.Fatal("wrong file-kind admission")
			}
		})
	}
}
