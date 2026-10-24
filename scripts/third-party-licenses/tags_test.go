package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestReleaseTagsGraph(t *testing.T) {
	root := graph(t)
	put(t, root, ".goreleaser.yaml", strings.Replace(fixtureConfig, "    env:", "    tags:\n      - omit_fixture\n    env:", 1))
	put(t, root, "cmd/bridge/optional.go", "//go:build !omit_fixture\n\npackage main\nimport _ \"example.com/unused\"\n")
	data, counts, err := generate(root)
	if err != nil {
		t.Fatal(err)
	}
	for target, count := range counts {
		want := 2
		if strings.HasPrefix(target, "darwin/") {
			want = 1
		}
		if count != want {
			t.Errorf("%s: got %d modules, want %d", target, count, want)
		}
	}
	if bytes.Contains(data, []byte("example.com/unused")) {
		t.Fatal("omitted fixture module in notices")
	}
}

func TestReleaseTagsWiring(t *testing.T) {
	for _, path := range []string{"../../.goreleaser.yaml", "../../Makefile", "../../Dockerfile"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, tag := range []string{"ts_omit_webclient", "ts_omit_debugeventbus"} {
			if !bytes.Contains(data, []byte(tag)) {
				t.Errorf("%s missing %s", path, tag)
			}
		}
	}
}

func TestReleaseTagsFailClosed(t *testing.T) {
	for _, value := range []string{
		" []", " [custom]", " custom", "\n      - {{ .Env.TAGS }}", "\n      - \"custom\"", "\n      - custom # comment", "\n      - custom,other", "\n      - custom other", "\n      - !custom", "\n      - -race", "\n      - custom\n      - custom", "\n", "\n    tags:\n      - custom", "\n      - *alias",
	} {
		t.Run(value, func(t *testing.T) {
			config := strings.Replace(fixtureConfig, "    env:", "    tags:"+value+"\n    env:", 1)
			if _, err := releaseTargets([]byte(config)); err == nil {
				t.Fatal("unreadable tags accepted")
			}
		})
	}
}

func TestBuildTagsAgree(t *testing.T) {
	if err := checkBuildTags("../.."); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ file, old, new string }{
		{".goreleaser.yaml", "      - ts_omit_webclient", "      - other"},
		{".goreleaser.yaml", "    tags:", "    tags: [custom]"},
		{"Makefile", "ts_omit_webclient,ts_omit_debugeventbus", "ts_omit_webclient"},
		{"Makefile", "override GO_TAGS :=", "GO_TAGS ?="},
		{"Makefile", `go build -tags "$(GO_TAGS)"`, "go build"},
		{"Makefile", `go build -tags "$(GO_TAGS)"`, `go build -tags "$(GO_TAGS)" -tags other`},
		{"Makefile", "DIST        := dist", "GO_TAGS += other\nDIST        := dist"},
		{"Dockerfile", "-tags ts_omit_webclient,ts_omit_debugeventbus", "-tags other"},
		{"Dockerfile", "    -trimpath", "    -tags other\n    -trimpath"},
	} {
		t.Run(tc.file+tc.new, func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{".goreleaser.yaml", "Makefile", "Dockerfile"} {
				b, err := os.ReadFile("../../" + name)
				if err != nil {
					t.Fatal(err)
				}
				s := string(b)
				if name == tc.file {
					if !strings.Contains(s, tc.old) {
						t.Fatal("mutation missed")
					}
					s = strings.Replace(s, tc.old, tc.new, 1)
				}
				put(t, root, name, s)
			}
			if err := checkBuildTags(root); err == nil {
				t.Fatal("build tag drift accepted")
			}
		})
	}
}

func TestBuildTagsGate(t *testing.T) {
	b, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"build: check-build-tags\n", "build-all: check-build-tags\n", "check-go-version: check-build-tags\n", "check-build-tags:\n\tgo run ./scripts/third-party-licenses -check-build-tags\n"} {
		if !strings.Contains(string(b), line) {
			t.Errorf("missing gate %q", line)
		}
	}
}
