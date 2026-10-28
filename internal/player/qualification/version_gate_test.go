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
// repositories: guarded change, version bump, unguarded change, rename/deletion.
func TestQualificationVersionGate(t *testing.T) {
	for _, name := range []string{"guarded_unchanged", "guarded_bumped", "unguarded", "rename_delete"} {
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
			files := map[string]string{v: "const qualificationVersion = 1\n", g: "base\n"}
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
			}
			head := gitCommit(t, dir, files)
			if e := os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte(head+"\n"), 0644); e != nil {
				t.Fatal(e)
			}
			e := CheckVersion(dir, base, head)
			wantErr := name == "guarded_unchanged" || name == "rename_delete"
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

// TestQualificationApprovedDoPGuardedPaths: each file B470 2d.2 adds is
// guarded, so a change to it alone needs a qualification version increase.
func TestQualificationApprovedDoPGuardedPaths(t *testing.T) {
	for _, path := range []string{"cmd/bridge/output_dsdtest_qualify_test.go", "cmd/bridge/serve_player_dop_test.go", "internal/player/playback/approval_dop_fixture_test.go", "internal/player/playback/dop_select_test.go", "internal/player/qualification/transition_test.go"} {
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
