package qualification

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQualificationEnginePlaybackGate(t *testing.T) {
	for _, file := range []string{"internal/player/engine/transport.go", "internal/player/engine/events.go", "internal/player/engine/backend.go", "internal/player/playback/session.go", "internal/player/playback/open.go", "internal/player/playback/producer.go", "internal/player/playback/reporter.go"} {
		t.Run(filepath.Base(filepath.Dir(file))+"_"+filepath.Base(file), func(t *testing.T) {
			dir := t.TempDir()
			if e := os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0755); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644); e != nil {
				t.Fatal(e)
			}
			files := map[string]string{"internal/player/qualification/version.go": "const qualificationVersion = 5\n", file: "before\n"}
			base := gitCommit(t, dir, files)
			files[file] = "after\n"
			head := gitCommit(t, dir, files)
			if e := CheckVersion(dir, base, head); e == nil {
				t.Fatal("native activation/release edit did not invalidate qualification")
			}
		})
	}
}
