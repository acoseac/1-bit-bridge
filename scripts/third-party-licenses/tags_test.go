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
			root := buildSiteFixture(t)
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

// Every case exercises the refusal through the production census, with an
// otherwise valid repository. A missing reviewed baseline is not a refusal.
func refuseNewSite(t *testing.T, name, content string) {
	t.Helper()
	root := buildSiteFixture(t)
	if err := checkBuildTags(root); err != nil {
		t.Fatalf("invalid baseline: %v", err)
	}
	put(t, root, name, content)
	if err := checkBuildTags(root); err == nil || !strings.Contains(err.Error(), "unreviewed build/test site in "+name+":") {
		t.Fatalf("new site was not refused: %v", err)
	}
}

func TestCensusVerbForms(t *testing.T) {
	for _, tc := range []struct{ name, command string }{
		{"deploy/new.sh", "EXE=; go${EXE} build ./cmd/bridge"},
		{"deploy/new.sh", "go 'build' ./cmd/bridge"},
		{"scripts/new.py", `subprocess.run(["go", "build", "./cmd/bridge"], check=True)`},
		{"Dockerfile.new", `RUN ["go", "build", "-o", "/out/bridge", "./cmd/bridge"]`},
		{"deploy/new.ps1", "& go 'build' ./cmd/bridge"},
		{"deploy/new.ps1", "Start-Process go -ArgumentList 'build','./cmd/bridge'"},
		{"new/Makefile", "go$(EXE) build ./cmd/bridge"},
		{"new/Makefile", "go${EXE} build ./cmd/bridge"},
		{"deploy/new.sh", `"${GO:-go}" build ./cmd/bridge`},
		{"deploy/new.sh", "$GOCMD build ./cmd/bridge"},
		{"new/Makefile", "$(GOCMD) build ./cmd/bridge"},
		{"deploy/new.ps1", "& $goExe build -o bridge.exe ./cmd/bridge"},
	} {
		t.Run(tc.name+"/"+tc.command, func(t *testing.T) { refuseNewSite(t, tc.name, tc.command) })
	}
	for _, executable := range []string{"$X", "${X}", "${X:-go}", "$(X)", "$env:X", "%X%", "/opt/go/bin/go", `C:\Go\bin\go.exe`, "go$EXE", "go.exe${EXE}"} {
		for _, flags := range []string{"", " -C 'some dir'", " -C=dir -modfile other.mod"} {
			for _, verb := range []string{"build", "install", "run", "test", "vet"} {
				t.Run(executable+flags+"/"+verb, func(t *testing.T) {
					refuseNewSite(t, "scripts/new.sh", executable+flags+" '"+verb+"' ./cmd/bridge")
				})
			}
		}
	}
}

func TestCensusFenceLabels(t *testing.T) {
	for _, label := range []string{"shell-session", "bash-session", "pwsh", "terminal", "sh{.line-numbers}", "{.sh}", "python", "dockerfile", "make", "unknown-language", "sh {.line-numbers}", ""} {
		t.Run(label, func(t *testing.T) {
			refuseNewSite(t, "ops/new.md", "```"+label+"\n$ go build ./cmd/bridge\n```\n")
		})
	}
}

func TestCensusFenceContainers(t *testing.T) {
	for _, prefix := range []string{"- ", "* ", "+ ", "2. ", "1) ", "123456789. ", "> ", "> - ", "- > ", "> > 2) "} {
		t.Run(prefix, func(t *testing.T) {
			refuseNewSite(t, "ops/new.md", prefix+"```sh\n  GOOS=linux GOARCH=amd64 go build -o dist/bridge ./cmd/bridge\n"+prefix+"```\n")
		})
	}
	t.Run("quoted content", func(t *testing.T) {
		refuseNewSite(t, "ops/new.md", "> ```sh\n> go build ./cmd/bridge\n> ```\n")
	})
	t.Run("inline before fence", func(t *testing.T) {
		refuseNewSite(t, "ops/new.md", "```make check``` must pass first.\n```sh\ngo build ./cmd/bridge\n```\n")
	})
}

func TestCensusPrompts(t *testing.T) {
	for _, label := range []string{"console", "shell-session", "bash-session", "terminal", "custom-console-output"} {
		for _, prompt := range []string{"$", "#", "%", ">"} {
			t.Run(label+"/"+prompt, func(t *testing.T) {
				refuseNewSite(t, "ops/new.md", "```"+label+"\n"+prompt+" go build -o /usr/local/bin/bridge ./cmd/bridge\n```\n")
			})
		}
	}
}

func TestCensusMarkdownScope(t *testing.T) {
	for _, name := range []string{"deploy/README.md", "deploy/linux/README.md", "ops/runbooks/deploy.md", "ops/runbooks/nuc.md", "docs/build.md", "docs/nested/build.md", "deploy/linux/build.md", "internal/example/README.md", "scripts/tool/README.md"} {
		t.Run(name, func(t *testing.T) {
			refuseNewSite(t, name, "```sh\ngo build ./cmd/bridge\n```\n")
		})
	}
}

func TestCensusMarkdownNonExecutable(t *testing.T) {
	for _, label := range []string{"text", "plain", "plaintext", "txt", "output", "log", "diff", "patch", "json", "jsonc", "yaml", "yml", "toml", "ini", "xml", "html", "css", "sql", "go", "golang", "swift", "mermaid"} {
		for _, prefix := range []string{"", "- ", "2. ", "> "} {
			t.Run(label+"/"+prefix, func(t *testing.T) {
				root := buildSiteFixture(t)
				put(t, root, "ops/new.md", "Prose mentions go build.\n"+prefix+"```"+label+"\ngo build ./cmd/bridge\n"+"  ```\nProse mentions go build.\n```make check``` is inline go test ./... prose.\n")
				if err := checkBuildTags(root); err != nil {
					t.Fatalf("non-executable Markdown entered census: %v", err)
				}
			})
		}
	}
}

func TestRealTreeBuildSiteCensus(t *testing.T) {
	sites, err := discoverBuildSites("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sortedKeys(sites) {
		if buildSiteMarkdown(name) {
			t.Logf("MARKDOWN %s: %d sites", name, len(sites[name]))
		}
		for _, line := range sites[name] {
			t.Logf("SITE %s: %s", name, line)
		}
	}
	if err := checkBuildSites("../..", "ts_omit_webclient,ts_omit_debugeventbus"); err != nil {
		t.Fatal(err)
	}
}

func TestCensusOriginalLinesAndMultiplicity(t *testing.T) {
	for _, replacement := range []string{
		`RUN ["go", "run", "./scripts/third-party-licenses"]`,
		"RUN go run ./scripts/third-party-licenses\nRUN go run ./scripts/third-party-licenses",
	} {
		t.Run(replacement, func(t *testing.T) {
			root := buildSiteFixture(t)
			b, err := os.ReadFile(root + "/Dockerfile")
			if err != nil {
				t.Fatal(err)
			}
			put(t, root, "Dockerfile", strings.Replace(string(b), "RUN go run ./scripts/third-party-licenses", replacement, 1))
			if err := checkBuildSites(root, "ts_omit_webclient,ts_omit_debugeventbus"); err == nil || !strings.Contains(err.Error(), "unreviewed build/test site in Dockerfile:") {
				t.Fatalf("changed original line or multiplicity accepted: %v", err)
			}
		})
	}
}

func TestCensusExcludedTreesAndRecords(t *testing.T) {
	for _, name := range []string{"ops/nested/History-old.md", "deploy/nested/plan-old.md", "docs/nested/release-notes-old.md", "vendor/tool/README.md", "node_modules/tool/README.md", "dist/README.md", "bin/README.md", "_scratch/README.md", "nested/README.md"} {
		t.Run(name, func(t *testing.T) {
			root := buildSiteFixture(t)
			put(t, root, name, "```sh\ngo build ./cmd/bridge\n```\n")
			if name == "nested/README.md" {
				put(t, root, "nested/.git", "gitdir: elsewhere\n")
			}
			if err := checkBuildTags(root); err != nil {
				t.Fatalf("excluded tree or record entered census: %v", err)
			}
		})
	}
}

func TestCensusExpandedGoPaths(t *testing.T) {
	for _, executable := range []string{"${GOROOT}/bin/go", "$(GOROOT)/bin/go", "${ROOT:-/opt/go}/bin/go.exe", "$(go env GOROOT)/bin/go", "go${EXE}${SUFFIX}"} {
		t.Run(executable, func(t *testing.T) {
			refuseNewSite(t, "scripts/new.sh", executable+" build ./cmd/bridge")
		})
	}
}
