package qualification

import (
	"os"
	"path/filepath"
	"testing"
)

func TestQualificationEnginePlaybackGate(t *testing.T) {
	for _, file := range []string{"cmd/bridge/serve_player_output.go", "internal/player/openhome/handler.go", "internal/player/openhome/listener.go", "internal/manifest/types.go", "internal/player/qualification/approval.go", "internal/player/playback/authorization.go", "internal/player/playback/edges.go", "internal/player/playback/live.go", "cmd/bridge/output_play_report.go", "cmd/bridge/output_dsdtest_backend.go", "internal/alsa/qualification_gain.go", "internal/alsactl/constraints.go", "cmd/bridge/serve_player.go", "cmd/bridge/serve_player_openhome.go", "internal/player/hotplug/feed.go", "internal/player/openhome/actions.go", "internal/api/outputs_bridge.go", "internal/manifest/extractors.go", "internal/player/engine/transport.go", "internal/player/engine/events.go", "internal/player/engine/backend.go", "internal/player/playback/session.go", "internal/player/playback/open.go", "internal/player/playback/producer.go", "internal/player/playback/reporter.go"} {
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
