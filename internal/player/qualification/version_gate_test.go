package qualification

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// gitObject builds test repository objects without invoking any Git writer.
func gitObject(t *testing.T, dir, kind string, b []byte) string {
	t.Helper()
	raw := append([]byte(fmt.Sprintf("%s %d\x00", kind, len(b))), b...)
	h := fmt.Sprintf("%x", sha1.Sum(raw))
	var z bytes.Buffer
	w := zlib.NewWriter(&z)
	_, _ = w.Write(raw)
	_ = w.Close()
	p := filepath.Join(dir, ".git", "objects", h[:2], h[2:])
	if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, z.Bytes(), 0644); e != nil {
		t.Fatal(e)
	}
	return h
}
func gitTree(t *testing.T, dir string, files map[string]string) string {
	names := map[string]map[string]string{}
	var rows []string
	for p, v := range files {
		first, rest, sub := strings.Cut(p, "/")
		if sub {
			if names[first] == nil {
				names[first] = map[string]string{}
			}
			names[first][rest] = v
		} else {
			rows = append(rows, "100644 "+first+"\x00"+hexObject(t, gitObject(t, dir, "blob", []byte(v))))
		}
	}
	for name, children := range names {
		rows = append(rows, "40000 "+name+"\x00"+hexObject(t, gitTree(t, dir, children)))
	}
	sort.Strings(rows)
	return gitObject(t, dir, "tree", []byte(strings.Join(rows, "")))
}
func hexObject(t *testing.T, h string) string {
	var b []byte
	for i := 0; i < len(h); i += 2 {
		var x byte
		if _, e := fmt.Sscanf(h[i:i+2], "%02x", &x); e != nil {
			t.Fatal(e)
		}
		b = append(b, x)
	}
	return string(b)
}
func gitCommit(t *testing.T, dir string, files map[string]string) string {
	return gitObject(t, dir, "commit", []byte("tree "+gitTree(t, dir, files)+"\nauthor Test <test@example.invalid> 1 +0000\ncommitter Test <test@example.invalid> 1 +0000\n\nfixture\n"))
}

// TestQualificationVersionGate exercises real read-only Git diffs of temporary
// repositories: guarded change, version bump, unguarded change, rename/deletion;
// and since B728's B1, which narrowed the guard to the DSD byte path and the
// harness, a change under internal/config/ alone passes without a bump, and
// one to the DSD sink's writer still needs one.
func TestQualificationVersionGate(t *testing.T) {
	for _, name := range []string{"guarded_unchanged", "guarded_bumped", "unguarded", "rename_delete", "config_unbumped", "dsd_writer_unbumped"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if e := os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0755); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644); e != nil {
				t.Fatal(e)
			}
			v := "internal/player/qualification/version.go"
			g := GuardedPaths[0]
			config, writer := "internal/config/player.go", "internal/alsapcm/sink_dsd_writer.go"
			files := map[string]string{v: "const qualificationVersion = 1\n", g: "base\n", config: "base\n", writer: "base\n"}
			base := gitCommit(t, dir, files)
			switch name {
			case "guarded_unchanged":
				files[g] = "changed\n"
			case "guarded_bumped":
				files[g] = "changed\n"
				files[v] = "const qualificationVersion = 2\n"
			case "unguarded":
				files["README.md"] = "change\n"
			case "rename_delete":
				delete(files, g)
				files["elsewhere.go"] = "base\n"
			case "config_unbumped":
				files[config] = "changed\n"
			case "dsd_writer_unbumped":
				files[writer] = "changed\n"
			}
			head := gitCommit(t, dir, files)
			if e := os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte(head+"\n"), 0644); e != nil {
				t.Fatal(e)
			}
			e := CheckVersion(dir, base, head)
			wantErr := name == "guarded_unchanged" || name == "rename_delete" || name == "dsd_writer_unbumped"
			if (e != nil) != wantErr {
				t.Fatalf("gate=%v, want refusal=%v", e, wantErr)
			}
		})
	}
}

func TestQualificationDoPGuardedPaths(t *testing.T) {
	for _, path := range []string{"internal/player/format/dop_pack.go", "internal/player/qualification/dop.go"} {
		t.Run(path, func(t *testing.T) {
			dir := t.TempDir()
			if e := os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0755); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644); e != nil {
				t.Fatal(e)
			}
			files := map[string]string{"internal/player/qualification/version.go": "const qualificationVersion = 13\n", path: "base"}
			base := gitCommit(t, dir, files)
			files[path] = "changed"
			head := gitCommit(t, dir, files)
			if e := os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte(head+"\n"), 0644); e != nil {
				t.Fatal(e)
			}
			if e := CheckVersion(dir, base, head); e == nil {
				t.Fatal("new DoP path escaped qualification gate")
			}
		})
	}
}

// TestQualificationApprovedDoPGuardedPaths: each file B470 2d.2 adds to the
// harness is guarded, so a change to it alone needs a qualification version
// increase; its serve and playback test files are not since B728's B1
// (TestAChangeOutsideTheDSDBytePathNeedsNoBump).
func TestQualificationApprovedDoPGuardedPaths(t *testing.T) {
	for _, path := range []string{"cmd/bridge/output_dsdtest_qualify_test.go", "internal/player/qualification/transition_test.go"} {
		t.Run(path, func(t *testing.T) {
			dir := t.TempDir()
			if e := os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0755); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644); e != nil {
				t.Fatal(e)
			}
			files := map[string]string{"internal/player/qualification/version.go": "const qualificationVersion = 16\n", path: "base"}
			base := gitCommit(t, dir, files)
			files[path] = "changed"
			head := gitCommit(t, dir, files)
			if e := os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte(head+"\n"), 0644); e != nil {
				t.Fatal(e)
			}
			if e := CheckVersion(dir, base, head); e == nil {
				t.Fatalf("a change to %s alone escaped the qualification gate", path)
			}
		})
	}
}

// TestAChangeOutsideTheDSDBytePathNeedsNoBump: since B728's B1 the guard is
// the DSD byte path and the harness alone (the design's section 8), so a
// change to anything else the two lists named until then, alone, needs no
// qualification version increase: each path B1 dropped from GuardedPaths,
// and files under each prefix it dropped, beside the paths it keeps there;
// and what round 2's audit of the DSD byte path looked at and left out (a
// read's chunk size, the sniff and the dispatch, the PCM decoders and
// packers, the evidence open where no harness runs).
func TestAChangeOutsideTheDSDBytePathNeedsNoBump(t *testing.T) {
	for _, path := range []string{
		"internal/player/engine/transport.go", "internal/player/engine/events.go", "internal/player/engine/backend.go",
		"internal/player/playback/session.go", "internal/player/playback/open.go", "internal/player/playback/producer.go", "internal/player/playback/reporter.go",
		"cmd/bridge/configpath.go", "cmd/bridge/cli_consts.go", "internal/packaging/packaging.go",
		"cmd/bridge/serve_player.go", "cmd/bridge/serve_player_output.go", "cmd/bridge/serve_player_openhome.go", "cmd/bridge/serve_player_dop_test.go",
		"internal/player/hotplug/feed.go", "internal/player/openhome/handler.go", "internal/player/openhome/listener.go", "internal/player/openhome/actions.go",
		"internal/api/outputs_bridge.go", "internal/manifest/types.go", "internal/manifest/extractors.go",
		"internal/player/playback/authorization.go", "internal/player/playback/live.go", "internal/player/playback/approval_dop_fixture_test.go", "internal/player/playback/dop_select_test.go",
		"internal/player/binding/binding.go", "internal/alsa/sysfs.go", "internal/alsa/qualification_gain.go",
		"internal/player/decode/bounds.go", "internal/player/decode/sniff.go", "internal/player/decode/decoder.go", "internal/player/decode/flac.go",
		"internal/player/format/format.go", "internal/player/format/pack.go",
		"internal/alsactl/constraints.go", "internal/fsutil/notafile_other.go", "internal/usbid/serial.go", "internal/config/player.go",
		"cmd/bridge/output_play_report.go", "cmd/bridge/output_pcmtest_keys_unix.go", "cmd/bridge/output_host.go",
	} {
		t.Run(path, func(t *testing.T) {
			dir := t.TempDir()
			if e := os.MkdirAll(filepath.Join(dir, ".git", "refs", "heads"), 0755); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0644); e != nil {
				t.Fatal(e)
			}
			files := map[string]string{"internal/player/qualification/version.go": "const qualificationVersion = 36\n", path: "base"}
			base := gitCommit(t, dir, files)
			files[path] = "changed"
			head := gitCommit(t, dir, files)
			if e := os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte(head+"\n"), 0644); e != nil {
				t.Fatal(e)
			}
			if e := CheckVersion(dir, base, head); e != nil {
				t.Fatalf("a change to %s alone, outside the DSD byte path and the harness, needed a bump: %v", path, e)
			}
		})
	}
}
