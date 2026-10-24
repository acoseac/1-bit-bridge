package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var buildSiteFiles = []string{
	".goreleaser.yaml", "go.mod", "Makefile", "Dockerfile",
	"deploy/linux/deploy-bridge-vps.sh", "deploy/windows/setup-bridge-windows.ps1", "deploy/windows/update-bridge-windows.ps1",
	".github/scripts/test-shard.sh", ".github/workflows/gate.yml", ".github/workflows/fuzz.yml", "scripts/alsa-abi/check.sh",
	"scripts/player-dsd-goldens/generate.py", "internal/player/hotplug/testdata/uevent/reconstruct.py",
	"ops/deployment-runbook.md", "ops/release-playbook.md", "README.md",
	"deploy/windows/firewall-bridge-windows.ps1", "docs/LoupeReviewCycle.md",
	"internal/player/decode/testdata/alac-goldens/README.md", "internal/player/decode/testdata/dsd-goldens/README.md",
}

func buildSiteFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// Include the exact reviewed census as it grows, plus the gate inputs.
	for _, name := range append(append([]string{}, buildSiteFiles...), sortedKeys(reviewedBuildSites)...) {
		b, err := os.ReadFile(filepath.Join("../..", name))
		if err != nil {
			t.Fatal(err)
		}
		put(t, root, name, string(b))
	}
	return root
}

func TestOperationalReleaseTags(t *testing.T) {
	t.Run("documented deployment", func(t *testing.T) {
		b, err := os.ReadFile("../../ops/deployment-runbook.md")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "go build -tags ts_omit_webclient,ts_omit_debugeventbus") {
			t.Fatal("documented deployment missing release tags")
		}
	})
	tags := "ts_omit_webclient,ts_omit_debugeventbus"
	for _, name := range buildSiteFiles[4:] {
		if strings.HasSuffix(name, ".py") || name == "ops/release-playbook.md" || name == "README.md" || name == "deploy/windows/firewall-bridge-windows.ps1" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("../..", name))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), "-tags "+tags) {
				t.Fatal("operational build/test missing release tags")
			}
		})
	}
}

func TestNewBuildSitesRefused(t *testing.T) {
	for _, name := range []string{"deploy/new.sh", "deploy/new.ps1", "scripts/new.py", ".github/workflows/new.yml", "Dockerfile.new", "new/Makefile", "scripts/new", "build-bridge", "build.mk", "module.psm1", "ops/new.md", "README.md"} {
		t.Run(name, func(t *testing.T) {
			root := buildSiteFixture(t)
			if err := checkBuildTags(root); err != nil {
				t.Fatalf("invalid baseline: %v", err)
			}
			for _, executable := range []string{"go", "go.exe", "$(GO)", "${GO}", "$GO", `"$GO"`, `"${GO}"`, `"/usr/local/go/bin/go"`} {
				for _, flags := range []string{"", " -C .", " -modfile=other.mod", " -C 'some dir' -modfile=other.mod"} {
					for _, verb := range []string{"build", "install", "run", "test", "vet"} {
						t.Run(executable+flags+"/"+verb, func(t *testing.T) {
							command := executable + flags + " " + verb + " ./cmd/bridge\n"
							if strings.HasSuffix(name, ".md") {
								command = "```sh\n" + command + "```\n"
							}
							put(t, root, name, command)
							if err := checkBuildTags(root); err == nil {
								t.Fatal("unreviewed build site accepted")
							}
						})
					}
				}
			}
		})
	}
}

func TestMarkdownBuildSites(t *testing.T) {
	for _, name := range []string{"ops/new.md", "README.md"} {
		for _, fence := range []string{"```sh", "   ```bash", "~~~shell", "````zsh", "```powershell", "```console", "```", "~~~"} {
			t.Run(name+"/"+fence, func(t *testing.T) {
				root := buildSiteFixture(t)
				if err := checkBuildTags(root); err != nil {
					t.Fatal(err)
				}
				put(t, root, name, fence+"\ngo build \\\n  ./cmd/bridge\n"+strings.TrimRight(strings.TrimSpace(fence), "abcdefghijklmnopqrstuvwxyz")+"\n")
				if err := checkBuildTags(root); err == nil {
					t.Fatal("documented build site accepted")
				}
			})
		}
	}
	root := buildSiteFixture(t)
	put(t, root, "ops/new.md", "Prose mentions go build.\n```text\ngo build ./cmd/bridge\n```\n~~~go\n// go test ./...\n~~~\n```sh\n# go build ./cmd/bridge\necho done\n```\ngo test ./... is prose again.\n")
	if err := checkBuildTags(root); err != nil {
		t.Fatalf("prose, non-shell blocks or comments entered census: %v", err)
	}
}

// TestMarkdownRecordsAreNotBuildSites: the engineering log, the History
// files, plans and release-note drafts quote commands as they were run, so
// their fences stay outside the census; the same fences in any other ops
// document are build sites.
func TestMarkdownRecordsAreNotBuildSites(t *testing.T) {
	const fences = "```sh\ngo build ./cmd/bridge\n```\n```\ngo test ./...\n```\n"
	for _, name := range []string{"ops/engineering-log.md", "ops/History-new.md", "ops/plan-new.md", "ops/release-notes-new.md"} {
		t.Run(name, func(t *testing.T) {
			root := buildSiteFixture(t)
			put(t, root, name, fences)
			if err := checkBuildTags(root); err != nil {
				t.Fatalf("a record entered the census: %v", err)
			}
		})
	}
	t.Run("ops/new-runbook.md", func(t *testing.T) {
		root := buildSiteFixture(t)
		put(t, root, "ops/new-runbook.md", fences)
		if err := checkBuildTags(root); err == nil {
			t.Fatal("a runbook's build stayed outside the census")
		}
	})
}

func TestYAMLTypedTagsRefused(t *testing.T) {
	for _, tag := range []string{"null", "Null", "NULL", "~", "true", "True", "TRUE", "false", "False", "FALSE", "yes", "Yes", "YES", "no", "No", "NO", "on", "On", "ON", "off", "Off", "OFF", "y", "Y", "n", "N", "0", "123", "+123", "-123", "0xFF", "0XFF", "0o77", "077", "0b11", "1_000", "1.2", ".5", "1e3", "1E+3", ".inf", ".Inf", ".INF", ".nan", ".NaN", ".NAN", "-.inf", "+.inf", "12:34"} {
		t.Run(tag, func(t *testing.T) {
			config := strings.Replace(fixtureConfig, "    env:", "    tags:\n      - "+tag+"\n    env:", 1)
			if _, err := releaseBuild([]byte(config)); err == nil {
				t.Fatal("YAML typed scalar accepted as a literal tag")
			}
		})
	}
}

func TestUnreviewedEmbedFailsDiscovery(t *testing.T) {
	root := graph(t)
	put(t, root, "zulu/lib.go", "package lib\nimport _ \"embed\"\n//go:embed bundle.js\nvar bundle string\n")
	put(t, root, "zulu/bundle.js", "unreviewed bundle")
	if _, _, err := generate(root); err == nil || !strings.Contains(err.Error(), "unreviewed embedded") {
		t.Fatalf("module root licence must not admit an unknown embed: %v", err)
	}
}

func TestWebPackagesRefusedWithoutEmbeds(t *testing.T) {
	for _, pkg := range []string{"tailscale.com/client/web", "github.com/tailscale/web-client-prebuilt"} {
		t.Run(pkg, func(t *testing.T) {
			root := graph(t)
			for _, name := range []string{"go.mod", "zulu/go.mod", "cmd/bridge/main.go"} {
				b, err := os.ReadFile(filepath.Join(root, name))
				if err != nil {
					t.Fatal(err)
				}
				put(t, root, name, strings.ReplaceAll(string(b), "example.com/zulu", pkg))
			}
			if _, _, err := generate(root); err == nil || !strings.Contains(err.Error(), "forbidden web package") {
				t.Fatalf("web package accepted: %v", err)
			}
		})
	}
}

func TestRealReleaseGraphEmbeds(t *testing.T) {
	_, counts, err := generate("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64", "windows/arm64"} {
		if counts[target] == 0 {
			t.Errorf("release graph not checked: %s", target)
		}
	}
	if len(counts) != 6 {
		t.Fatalf("expected six release graphs, got %v", counts)
	}
}

func TestDockerNoticesAndToolchain(t *testing.T) {
	b, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	version, err := releaseGoVersion("../..")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, "\nARG GO_VERSION="+strings.TrimPrefix(version, "go")+"\n") {
		t.Error("Docker toolchain must match full go.mod version")
	}
	builder, runtime, ok := strings.Cut(s, "# --- runtime ---")
	if !ok || !strings.Contains(builder, "RUN go run ./scripts/third-party-licenses\n") {
		t.Error("builder must generate notices")
	}
	if !strings.Contains(runtime, "COPY --from=builder /src/THIRD_PARTY_LICENSES.md /usr/share/doc/1-bit-bridge/THIRD_PARTY_LICENSES.md\n") {
		t.Error("runtime must ship generated notices")
	}
}

func TestReviewedEmbedIdentityAndFiles(t *testing.T) {
	m := module{Path: "google.golang.org/protobuf", Version: "v1.36.12"}
	pkg := "google.golang.org/protobuf/internal/editiondefaults"
	files := []string{"editions_defaults.binpb"}
	if err := checkPackageEmbeds(&m, pkg, files); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		mod   module
		pkg   string
		files []string
	}{
		{"version", module{Path: m.Path, Version: "v1.36.13"}, pkg, files},
		{"module", module{Path: "example.com/fork", Version: m.Version}, pkg, files},
		{"replacement", module{Path: m.Path, Version: m.Version, Replace: &module{Path: m.Path, Version: m.Version}}, pkg, files},
		{"local replacement", module{Path: m.Path, Version: m.Version, Replace: &module{Path: "../fork"}}, pkg, files},
		{"package", m, pkg + "/new", files},
		{"added file", m, pkg, []string{"editions_defaults.binpb", "bundle.js"}},
		{"renamed file", m, pkg, []string{"renamed.binpb"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkPackageEmbeds(&tc.mod, tc.pkg, tc.files); err == nil {
				t.Fatal("unreviewed embedded identity/files accepted")
			}
		})
	}
	if err := checkPackageEmbeds(&module{Main: true}, "example.com/app", []string{"own.html"}); err != nil {
		t.Fatal(err)
	}
	if err := checkPackageEmbeds(nil, "embed", nil); err != nil {
		t.Fatal(err)
	}
}

func TestReviewedBuildSitesCannotLoseTags(t *testing.T) {
	for _, name := range buildSiteFiles {
		if name == "go.mod" || name == ".goreleaser.yaml" || strings.HasSuffix(name, ".py") || name == "ops/release-playbook.md" || name == "README.md" || name == "deploy/windows/firewall-bridge-windows.ps1" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			root := buildSiteFixture(t)
			if err := checkBuildTags(root); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			text := string(b)
			old := "-tags ts_omit_webclient,ts_omit_debugeventbus "
			if name == "Makefile" {
				old = `-tags "$(GO_TAGS)" `
			}
			if !strings.Contains(text, old) {
				t.Fatal("mutation missed")
			}
			put(t, root, name, strings.ReplaceAll(text, old, ""))
			if err := checkBuildTags(root); err == nil {
				t.Fatal("untagged reviewed site accepted")
			}
		})
	}
}

func TestDockerNoticeGuard(t *testing.T) {
	version, err := releaseGoVersion("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		"RUN go run ./scripts/third-party-licenses",
		"COPY --from=builder /src/THIRD_PARTY_LICENSES.md /usr/share/doc/1-bit-bridge/THIRD_PARTY_LICENSES.md",
		"ARG GO_VERSION=" + strings.TrimPrefix(version, "go"),
	} {
		t.Run(line, func(t *testing.T) {
			root := buildSiteFixture(t)
			b, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), line) {
				t.Fatal("mutation missed")
			}
			put(t, root, "Dockerfile", strings.Replace(string(b), line, "# removed", 1))
			if err := checkBuildTags(root); err == nil {
				t.Fatal("missing Docker notices/toolchain accepted")
			}
		})
	}
}

func TestShardFlagsCannotOverrideTags(t *testing.T) {
	root := buildSiteFixture(t)
	name := ".github/scripts/test-shard.sh"
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []string{
		"RACE_FLAGS=(-race -timeout 30m -tags other)",
		"RACE_FLAGS=(-race -timeout 30m)\nRACE_FLAGS+=(-tags other)",
	} {
		put(t, root, name, strings.Replace(string(b), "RACE_FLAGS=(-race -timeout 30m)", edit, 1))
		if err := checkBuildTags(root); err == nil {
			t.Fatal("overriding shard tags accepted")
		}
	}
}
