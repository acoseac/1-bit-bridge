package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const fixtureConfig = `builds:
  - id: bridge
    main: ./cmd/bridge
    binary: bridge
    env:
      - CGO_ENABLED=0
    goos:
      - windows
      - linux
      - darwin
    goarch:
      - arm64
      - amd64
    flags:
      - -trimpath
    ldflags:
      - -s -w
archives:
`

func put(t *testing.T, root, name, data string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func graph(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	put(t, root, ".goreleaser.yaml", fixtureConfig)
	mod := "module example.com/app\n\ngo 1.25\n\nrequire (\n"
	for _, name := range []string{"zulu", "alpha", "win", "unused", "testonly"} {
		mod += "example.com/" + name + " v1.0.0\n"
		put(t, root, name+"/go.mod", "module example.com/"+name+"\n\ngo 1.25\n")
		put(t, root, name+"/lib.go", "package lib\n")
		put(t, root, name+"/LiCeNsE.txt", name+" licence\nfull text without omissions\n")
	}
	mod += ")\n"
	for _, name := range []string{"zulu", "alpha", "win", "unused", "testonly"} {
		mod += "replace example.com/" + name + " => ./" + name + "\n"
	}
	put(t, root, "go.mod", mod)
	put(t, root, "cmd/bridge/main.go", "package main\nimport _ \"example.com/zulu\"\nfunc main() {}\n")
	put(t, root, "cmd/bridge/extra_linux.go", "package main\nimport _ \"example.com/alpha\"\n")
	put(t, root, "cmd/bridge/extra_windows.go", "package main\nimport _ \"example.com/win\"\n")
	put(t, root, "cmd/bridge/main_test.go", "package main\nimport _ \"example.com/testonly\"\n")
	put(t, root, "zulu/nOtIcE.extra", "notice text\n")
	put(t, root, "zulu/PATENTS", "patent grant\n")
	put(t, root, "zulu/COPYING.more", "copying text without final newline")
	put(t, root, "zulu/LICENSE.second", "second licence\n```embedded fence```\n")
	return root
}

func TestLinkedGraph(t *testing.T) {
	root := graph(t)
	data, counts, err := generate(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"darwin/amd64": 1, "darwin/arm64": 1, "linux/amd64": 2, "linux/arm64": 2, "windows/amd64": 2, "windows/arm64": 2}
	if !reflect.DeepEqual(counts, want) {
		t.Fatalf("counts: %v, want %v", counts, want)
	}
	for _, s := range []string{"## example.com/alpha v1.0.0", "## example.com/zulu v1.0.0", "Replacement: ./zulu", "notice text\n", "patent grant\n", "copying text without final newline", "second licence\n```embedded fence```\n", "zulu licence\nfull text without omissions\n"} {
		if !bytes.Contains(data, []byte(s)) {
			t.Errorf("missing %q", s)
		}
	}
	for _, s := range []string{"example.com/unused", "example.com/testonly", "example.com/app", root} {
		if bytes.Contains(data, []byte(s)) {
			t.Errorf("unexpected %q", s)
		}
	}
	if bytes.Count(data, []byte("## example.com/zulu v1.0.0")) != 1 {
		t.Fatal("module was not deduplicated")
	}
}

func TestNoLicenseFails(t *testing.T) {
	root := graph(t)
	for _, name := range []string{"LiCeNsE.txt", "COPYING.more", "LICENSE.second"} {
		if err := os.Remove(filepath.Join(root, "zulu", name)); err != nil {
			t.Fatal(err)
		}
	}
	_, _, err := generate(root)
	if err == nil || !strings.Contains(err.Error(), "example.com/zulu") || !strings.Contains(err.Error(), "no licence") {
		t.Fatalf("missing licence should name module: %v", err)
	}
}

func TestStableOrder(t *testing.T) {
	root := graph(t)
	first, _, err := generate(root)
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, ".goreleaser.yaml", strings.ReplaceAll(fixtureConfig, "      - windows\n      - linux\n      - darwin", "      - darwin\n      - windows\n      - linux"))
	second, _, err := generate(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("target order changed output")
	}
	if bytes.Index(first, []byte("## example.com/alpha")) > bytes.Index(first, []byte("## example.com/zulu")) {
		t.Fatal("modules are not sorted")
	}
	_, zulu, _ := bytes.Cut(first, []byte("## example.com/zulu"))
	if bytes.Index(zulu, []byte("### COPYING.more")) > bytes.Index(zulu, []byte("### LiCeNsE.txt")) {
		t.Fatal("files are not sorted")
	}
}

func TestBuildConfigFailsClosed(t *testing.T) {
	for _, edit := range []struct{ old, new string }{
		{"CGO_ENABLED=0", "CGO_ENABLED=1"},
		{"    flags:", "    tags:\n      - custom\n    flags:"},
		{"      - -trimpath", "      - -race"},
		{"    goarch:", "    ignore:\n      - goos: windows\n    goarch:"},
		{"    main: ./cmd/bridge", "    main: ./cmd/other"},
		{"      - arm64\n      - amd64", "      - arm64\n      - arm64"},
	} {
		t.Run(edit.new, func(t *testing.T) {
			_, err := releaseTargets([]byte(strings.Replace(fixtureConfig, edit.old, edit.new, 1)))
			if err == nil {
				t.Fatal("unsupported build configuration accepted")
			}
		})
	}
}

func TestReleaseWiring(t *testing.T) {
	data, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	before, builds, ok := strings.Cut(s, "\nbuilds:\n")
	if !ok || !strings.Contains(before, "    - go run ./scripts/third-party-licenses\n") {
		t.Fatal("release before-hook must regenerate licences")
	}
	_, archives, ok := strings.Cut(builds, "\narchives:\n")
	if !ok {
		t.Fatal("missing archives")
	}
	archives, _, _ = strings.Cut(archives, "\nchecksum:")
	if strings.Count(archives, "  - id:") != 1 || !strings.Contains(archives, "      - THIRD_PARTY_LICENSES.md\n") {
		t.Fatal("every archive must ship licences; update guard if archive layout changes")
	}
}

func TestReplacementAndGoPatents(t *testing.T) {
	root := t.TempDir()
	put(t, root, "original/LICENSE", "original licence")
	put(t, root, "fork/LICENSE", "fork licence")
	put(t, root, "PATENTS", "Go project's patent grant")
	m := module{Path: "golang.org/x/example", Version: "v1.0.0", Dir: filepath.Join(root, "original"), Replace: &module{Path: "example.com/fork", Version: "v1.1.0", Dir: filepath.Join(root, "fork")}}
	files, err := notices(m, filepath.Join(root, "PATENTS"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || string(files[0].text) != "fork licence" || string(files[1].text) != "Go project's patent grant" {
		t.Fatalf("replacement licence and Go grant: %v", files)
	}
	if err := os.Remove(filepath.Join(root, "PATENTS")); err != nil {
		t.Fatal(err)
	}
	if _, err := notices(m, filepath.Join(root, "PATENTS")); err == nil {
		t.Fatal("missing Go patent grant accepted")
	}
	put(t, root, "fork/PATENTS", "module patent grant")
	files, err = notices(m, filepath.Join(root, "PATENTS"))
	if err != nil || len(files) != 2 || string(files[1].text) != "module patent grant" {
		t.Fatalf("module patent grant lost: %v %v", files, err)
	}
}

func TestUnlicense(t *testing.T) {
	root := t.TempDir()
	put(t, root, "UNLICENSE", "public domain dedication")
	files, err := notices(module{Path: "example.com/public-domain", Dir: root}, "")
	if err != nil || len(files) != 1 || string(files[0].text) != "public domain dedication" {
		t.Fatalf("UNLICENSE lost: %v %v", files, err)
	}
}
