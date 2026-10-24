package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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
	for key, value := range map[string]string{"GOENV": "off", "GOFLAGS": "", "GOWORK": "off", "GOAMD64": "v1", "GOARM64": "v8.0"} {
		t.Setenv(key, value)
	}
	put(t, root, ".goreleaser.yaml", fixtureConfig)
	mod := "module example.com/app\n\ngo " + strings.TrimPrefix(runtime.Version(), "go") + "\n\nrequire (\n"
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
	put(t, root, "zulu/nOtIcE.rst", "notice text\n")
	put(t, root, "zulu/PATENTS", "patent grant\n")
	put(t, root, "zulu/COPYING.txt", "copying text without final newline")
	put(t, root, "zulu/LICENSE.md", "second licence\n```embedded fence```\n")
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
	for _, name := range []string{"LiCeNsE.txt", "COPYING.txt", "LICENSE.md"} {
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
	if bytes.Index(zulu, []byte("### COPYING.txt")) > bytes.Index(zulu, []byte("### LiCeNsE.txt")) {
		t.Fatal("files are not sorted")
	}
}

func TestBuildConfigFailsClosed(t *testing.T) {
	for _, edit := range []struct{ old, new string }{
		{"CGO_ENABLED=0", "CGO_ENABLED=1"},
		{"    flags:", "    unknown:\n      - custom\n    flags:"},
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
	files, err := notices(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || string(files[0].text) != "fork licence" {
		t.Fatalf("replacement must ship only its own terms: %v", files)
	}
	plain := module{Path: "golang.org/x/plain", Dir: filepath.Join(root, "original")}
	files, err = notices(plain)
	if err != nil || len(files) != 1 {
		t.Fatalf("synthesized module patent grant: %v %v", files, err)
	}
	put(t, root, "fork/PATENTS", "module patent grant")
	files, err = notices(m)
	if err != nil || len(files) != 2 || string(files[1].text) != "module patent grant" {
		t.Fatalf("module patent grant lost: %v %v", files, err)
	}
}

func TestUnlicense(t *testing.T) {
	root := t.TempDir()
	put(t, root, "UNLICENSE", "public domain dedication")
	files, err := notices(module{Path: "example.com/public-domain", Dir: root})
	if err != nil || len(files) != 1 || string(files[0].text) != "public domain dedication" {
		t.Fatalf("UNLICENSE lost: %v %v", files, err)
	}
}

func TestNestedLicenses(t *testing.T) {
	root := graph(t)
	put(t, root, "zulu/internal/embedded/sub/lib.go", "package sub\n")
	put(t, root, "zulu/internal/embedded/sub/child/lib.go", "package child\n")
	put(t, root, "zulu/internal/embedded/LICENSE", "distinct 2013 nested copyright\n")
	put(t, root, "zulu/internal/embedded/sub/NOTICE.md", "nested notice\n")
	put(t, root, "zulu/internal/embedded/sub/child/COPYRIGHT", "child copyright\n")
	put(t, root, "zulu/unlinked/LICENSE", "unlinked terms\n")
	put(t, root, "zulu/lib.go", "package lib\nimport ( _ \"example.com/zulu/internal/embedded/sub\"; _ \"example.com/zulu/internal/embedded/sub/child\" )\n")
	data, _, err := generate(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"### internal/embedded/LICENSE\n", "distinct 2013 nested copyright\n", "### internal/embedded/sub/NOTICE.md\n", "nested notice\n", "### internal/embedded/sub/child/COPYRIGHT\n", "child copyright\n"} {
		if bytes.Count(data, []byte(want)) != 1 {
			t.Errorf("want exactly one %q", want)
		}
	}
	if bytes.Contains(data, []byte("unlinked terms")) {
		t.Fatal("unlinked subtree included")
	}
}

func TestLicenseNames(t *testing.T) {
	for _, name := range []string{"LiCeNcE", "licence.MD", "COPYRIGHT", "copyright.txt", "LICENSE-APACHE", "LICENSE.MIT", "COPYING.rst", "NOTICE.markdown"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if strings.HasPrefix(name, "NOTICE") {
				put(t, root, "LICENSE", "licence")
			}
			put(t, root, name, "distinct notice bytes")
			files, err := notices(module{Path: "example.com/names", Dir: root})
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if file.name == name && string(file.text) == "distinct notice bytes" {
					return
				}
			}
			t.Fatalf("missing %s", name)
		})
	}
}

func TestSourceIsNotLicense(t *testing.T) {
	for _, name := range []string{"license.go", "license_test.go", "LICENSE.c", "LICENCE.h", "COPYING.s", "NOTICE.js", "COPYRIGHT.py", "LICENSE.sh", "LICENSE.rs", "LICENSE.cpp", "LICENSE.java", "LICENSE.ts", "LICENSE-APACHE.go"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, name, "package lib\n")
			m := module{Path: "example.com/source", Dir: root}
			if _, err := notices(m); err == nil || !strings.Contains(err.Error(), "no licence") {
				t.Fatalf("source accepted as licence: %v", err)
			}
			put(t, root, "LICENSE", "real terms")
			files, err := notices(m)
			if err != nil || len(files) != 1 || files[0].name != "LICENSE" {
				t.Fatalf("source reproduced: %v %v", files, err)
			}
		})
	}
}

func TestGoStandardLibrary(t *testing.T) {
	root := graph(t)
	data, _, err := generate(root)
	if err != nil {
		t.Fatal(err)
	}
	heading := "## Go standard library and runtime (" + runtime.Version() + ")\n"
	if bytes.Count(data, []byte(heading)) != 1 {
		t.Fatalf("missing fixed Go section %q", heading)
	}
	for _, name := range []string{"LICENSE", "PATENTS"} {
		want, err := os.ReadFile(filepath.Join(runtime.GOROOT(), name))
		if os.IsNotExist(err) && name == "PATENTS" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, want) {
			t.Errorf("Go %s text missing", name)
		}
	}
}

func TestReleaseGoVersion(t *testing.T) {
	root := graph(t)
	path := filepath.Join(root, "go.mod")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "go.mod", strings.Replace(string(data), strings.TrimPrefix(runtime.Version(), "go"), "1.25.0", 1))
	_, _, err = generate(root)
	if err == nil || !strings.Contains(err.Error(), "toolchain") {
		t.Fatalf("mismatched release toolchain accepted: %v", err)
	}
}

func TestDiscoveryEnvironment(t *testing.T) {
	for _, key := range []string{"GOFLAGS", "GOWORK", "GOAMD64", "GOARM64", "persisted_GOFLAGS", "auto_workspace"} {
		t.Run(key, func(t *testing.T) {
			root := graph(t)
			switch key {
			case "GOFLAGS":
				t.Setenv(key, "-tags=custom")
			case "GOWORK":
				put(t, root, "release.work", "go "+strings.TrimPrefix(runtime.Version(), "go")+"\nuse .\n")
				t.Setenv(key, filepath.Join(root, "release.work"))
			case "GOAMD64":
				t.Setenv(key, "v2")
			case "GOARM64":
				t.Setenv(key, "v9.0")
			case "persisted_GOFLAGS":
				put(t, root, "goenv", "GOFLAGS=-tags=custom\n")
				t.Setenv("GOENV", filepath.Join(root, "goenv"))
			case "auto_workspace":
				put(t, root, "go.work", "go "+strings.TrimPrefix(runtime.Version(), "go")+"\nuse .\n")
				t.Setenv("GOWORK", "")
			}
			_, _, err := generate(root)
			want := key
			if key == "persisted_GOFLAGS" {
				want = "GOFLAGS"
			}
			if key == "auto_workspace" {
				want = "GOWORK"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("non-default %s accepted: %v", key, err)
			}
		})
	}
}

func TestReplacementFormatting(t *testing.T) {
	data, _, err := generate(graph(t))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("Replacement: ./zulu\n\n")) {
		t.Fatal("local replacement has trailing whitespace")
	}
}

func TestToolchainNoticeFiles(t *testing.T) {
	root := t.TempDir()
	if _, err := toolchainNotices(root); err == nil {
		t.Fatal("missing Go LICENSE accepted")
	}
	put(t, root, "LICENSE", " \n\t")
	if _, err := toolchainNotices(root); err == nil {
		t.Fatal("empty Go LICENSE accepted")
	}
	put(t, root, "LICENSE", "Go licence bytes\n")
	files, err := toolchainNotices(root)
	if err != nil || len(files) != 1 || string(files[0].text) != "Go licence bytes\n" {
		t.Fatalf("Go licence without optional PATENTS: %v %v", files, err)
	}
	put(t, root, "PATENTS", "toolchain patent grant\n")
	files, err = toolchainNotices(root)
	if err != nil || len(files) != 2 || string(files[1].text) != "toolchain patent grant\n" {
		t.Fatalf("Go patent grant lost: %v %v", files, err)
	}
	put(t, root, "PATENTS", " \n")
	if _, err := toolchainNotices(root); err == nil {
		t.Fatal("empty Go PATENTS accepted")
	}
}

func TestDefaultDiscoveryEnvironment(t *testing.T) {
	root := graph(t)
	for _, key := range []string{"GOAMD64", "GOARM64", "GOWORK"} {
		t.Setenv(key, "")
	}
	if _, _, err := generate(root); err != nil {
		t.Fatalf("default environment refused: %v", err)
	}
}

func TestRootLicenseRequired(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			root := t.TempDir()
			put(t, root, "copied/LICENSE", "copied package only")
			m := module{Path: "example.com/rootless", Dir: root, packageDirs: []string{filepath.Join(root, "copied")}}
			if replacement {
				m.Dir = t.TempDir()
				put(t, m.Dir, "LICENSE", "original grant")
				m.Replace = &module{Path: "example.com/fork", Dir: root}
			}
			if _, err := notices(m); err == nil || !strings.Contains(err.Error(), "root") {
				t.Fatalf("nested licence must not cover module root: %v", err)
			}
		})
	}
}

func TestNoticeSuffixes(t *testing.T) {
	for _, name := range []string{"COPYING.LESSER", "LICENSE.Apache-2.0", "LICENSE.html", "NOTICE.html", "PATENTS.txt", "UNLICENSE.txt", "LICENSE.weird", "NOTICE.unknown"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, "COPYING", "module terms")
			put(t, root, name, "additional terms")
			files, err := notices(module{Path: "example.com/suffix", Dir: root})
			if strings.HasSuffix(name, ".weird") || strings.HasSuffix(name, ".unknown") {
				if err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("unknown suffix must name refused file: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				if f.name == name && string(f.text) == "additional terms" {
					return
				}
			}
			t.Fatalf("additional notice %s silently dropped", name)
		})
	}
}

func TestVendorRefused(t *testing.T) {
	root := graph(t)
	put(t, root, "vendor/patched/NOTICE", "vendored patch terms")
	if _, _, err := generate(root); err == nil || !strings.Contains(err.Error(), "vendor") {
		t.Fatalf("vendor tree accepted: %v", err)
	}
}

func TestEmbeddedNotices(t *testing.T) {
	saved := embeddedRegistry
	t.Cleanup(func() { embeddedRegistry = saved })
	embeddedRegistry = map[string]embeddedNotice{
		"example.com/assets": {"v1.0.0", "fixture component A; full notice A\nfixture component B; full notice B\n"},
	}
	for _, tc := range []struct {
		path, version string
		wants         []string
	}{
		{"example.com/assets", "v1.0.0", []string{"fixture component A; full notice A", "fixture component B; full notice B"}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			root := t.TempDir()
			put(t, root, "LICENSE", "enclosing module licence")
			m := module{Path: tc.path, Version: tc.version, Dir: root}
			files, err := notices(m)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			writeNotices(&out, files)
			for _, want := range tc.wants {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing embedded component notice %q", want)
				}
			}
			for _, changed := range []module{
				{Path: tc.path, Version: "v999.0.0", Dir: root},
				{Path: tc.path, Version: tc.version, Dir: root, Replace: &module{Path: "example.com/fork", Version: tc.version, Dir: root}},
				{Path: tc.path, Version: tc.version, Dir: root, Replace: &module{Path: tc.path, Version: "v999.0.0", Dir: root}},
				{Path: tc.path, Version: tc.version, Dir: root, Replace: &module{Path: "./local", Dir: root}},
			} {
				if _, err := notices(changed); err == nil || !strings.Contains(err.Error(), "embedded") {
					t.Errorf("unreviewed embedded assets accepted: %+v: %v", changed, err)
				}
			}
		})
	}
	root := t.TempDir()
	put(t, root, "LICENSE", "ordinary module")
	files, err := notices(module{Path: "example.com/plain", Dir: root})
	if err != nil || len(files) != 1 {
		t.Fatalf("unlinked assets added: %v %v", files, err)
	}
}
