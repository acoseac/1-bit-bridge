package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// checkBuildTags deliberately accepts only the build command shapes used here.
// GoReleaser is authoritative; a new build form needs a matching guard change.
func checkBuildTags(root string) error {
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(root, name))
		return string(b), err
	}
	config, err := read(".goreleaser.yaml")
	if err != nil {
		return err
	}
	build, err := releaseBuild([]byte(config))
	if err != nil {
		return err
	}
	tags := strings.Join(build.tags, ",")
	if tags == "" {
		return fmt.Errorf("release build tags must not be empty")
	}
	makefile, err := read("Makefile")
	if err != nil {
		return err
	}
	dockerfile, err := read("Dockerfile")
	if err != nil {
		return err
	}
	active := func(s string) string {
		var lines []string
		for _, line := range strings.Split(s, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") {
				lines = append(lines, line)
			}
		}
		return strings.Join(lines, "\n")
	}
	makefile, dockerfile = active(makefile), active(dockerfile)
	if len(regexp.MustCompile(`(?m)^.*GO_TAGS\s*[:?+!]?=`).FindAllString(makefile, -1)) != 1 || !strings.Contains(makefile, "\noverride GO_TAGS := "+tags+"\n") {
		return fmt.Errorf("Makefile GO_TAGS must match .goreleaser.yaml tags %q", tags)
	}

	version, err := releaseGoVersion(root)
	if err != nil {
		return err
	}
	if !strings.Contains(dockerfile, "\nARG GO_VERSION="+strings.TrimPrefix(version, "go")+"\n") {
		return fmt.Errorf("Dockerfile GO_VERSION must match full go.mod version %s", version)
	}
	builder, final, ok := strings.Cut(dockerfile, "\nFROM alpine:")
	if !ok || !strings.Contains(builder, "\nRUN go run ./scripts/third-party-licenses\n") || !strings.Contains(final, "\nCOPY --from=builder /src/THIRD_PARTY_LICENSES.md /usr/share/doc/1-bit-bridge/THIRD_PARTY_LICENSES.md\n") {
		return fmt.Errorf("Dockerfile must generate notices in builder and COPY them into final image")
	}

	shard, err := read(".github/scripts/test-shard.sh")
	if err != nil {
		return err
	}
	shard = active(shard)
	if len(regexp.MustCompile(`(?m)^.*RACE_FLAGS\s*\+?=`).FindAllString(shard, -1)) != 1 || !strings.Contains(shard, "\nRACE_FLAGS=(-race -timeout 30m)\n") {
		return fmt.Errorf("test-shard.sh RACE_FLAGS must not override release tags")
	}
	return checkBuildSites(root, tags)
}

// reviewedBuildSites is an explicit census, not a list of files to search.
// Scan the whole repository for operational command sites, then compare exact
// logical lines and multiplicities. New commands (even in a new file) require
// review here. @TAGS@ comes from GoReleaser. Untagged exceptions are only the
// notice generator, the temp-dir helper, and two printed test instructions;
// none builds/tests the shipped bridge. Go source and Markdown prose are not
// executable shell/PowerShell/workflow sites. Shell fences in README.md and
// top-level ops Markdown are operational sites too, except the records'
// (buildSiteMarkdown).
var reviewedBuildSites = map[string][]string{
	".github/scripts/test-shard.sh": {
		"if ! out=\"$(go test -tags @TAGS@ \"${RACE_FLAGS[@]}\" \"$1\" -list '.*')\"; then",
		"go test -tags @TAGS@ -p \"$(cores)\" \"${RACE_FLAGS[@]}\" \"${all[@]}\"",
		"go test -tags @TAGS@ \"$pkg\" \"${RACE_FLAGS[@]}\" -run \"^($(printf '%s\\n' \"$part\" | paste -sd'|' -))\\$\"",
	},
	".github/workflows/fuzz.yml": {
		"go test -tags @TAGS@ -list 'Fuzz.*' ./... 2>/dev/null > /tmp/list.txt || true",
		"go test -tags @TAGS@ ${{ matrix.target.package }} -exec \"prlimit --as=${FUZZ_AS_LIMIT} --\" -run XXX -fuzz '^${{ matrix.target.name }}$' -fuzztime ${{ inputs.fuzztime || '5m' }} -fuzzminimizetime 1s",
	},
	".github/workflows/gate.yml": {
		"BRIDGE_TEST_SMALL_VOLUME=/mnt/b264-small go test -tags @TAGS@ ./internal/transcode/ -run 'TestARenditionThatCouldNotBeWrittenWholeIsNotPublished' -count=1 -v",
		"GOTMP=$(go run ./.github/scripts/printtmpdir 2>/dev/null || echo \"$TEMP\")",
		"run: go vet -tags @TAGS@ ./...",
		"run: go test -tags @TAGS@ -timeout 30m ./...",
	},
	".goreleaser.yaml": {
		"- go run ./scripts/third-party-licenses",
	},
	"Dockerfile": {
		"RUN go run ./scripts/third-party-licenses",
		"RUN GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -tags @TAGS@ -trimpath -ldflags=\"-s -w -X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=${VERSION}\" -o /out/bridge ./cmd/bridge",
	},
	"Makefile": {
		"go build -tags \"$(GO_TAGS)\" -ldflags \"$(LDFLAGS)\" -o bin/$(BINARY) $(PKG)",
		"GOOS=darwin GOARCH=arm64 go build -tags \"$(GO_TAGS)\" -p $(P) -ldflags \"$(LDFLAGS)\" -o $(DIST)/$(BINARY)-darwin-arm64 $(PKG)",
		"GOOS=darwin GOARCH=amd64 go build -tags \"$(GO_TAGS)\" -p $(P) -ldflags \"$(LDFLAGS)\" -o $(DIST)/$(BINARY)-darwin-amd64 $(PKG)",
		"GOOS=linux GOARCH=arm64 go build -tags \"$(GO_TAGS)\" -p $(P) -ldflags \"$(LDFLAGS)\" -o $(DIST)/$(BINARY)-linux-arm64 $(PKG)",
		"GOOS=linux GOARCH=amd64 go build -tags \"$(GO_TAGS)\" -p $(P) -ldflags \"$(LDFLAGS)\" -o $(DIST)/$(BINARY)-linux-amd64 $(PKG)",
		"GOOS=windows GOARCH=amd64 go build -tags \"$(GO_TAGS)\" -p $(P) -ldflags \"$(LDFLAGS)\" -o $(DIST)/$(BINARY)-windows-amd64.exe $(PKG)",
		"GOOS=windows GOARCH=arm64 go build -tags \"$(GO_TAGS)\" -p $(P) -ldflags \"$(LDFLAGS)\" -o $(DIST)/$(BINARY)-windows-arm64.exe $(PKG)",
		"go test -tags \"$(GO_TAGS)\" -p $(P) -race -timeout 30m ./...",
		"go test -tags \"$(GO_TAGS)\" -p $(P) ./...",
		"BRIDGE_DSD_FIXTURE_TESTS=1 go test -tags \"$(GO_TAGS)\" ./internal/transcode/ -run 'TestDSDRender_|TestDSDLowpass|TestRunDSD' -count=1 -v",
		"go vet -tags \"$(GO_TAGS)\" ./...",
		"go run -tags \"$(GO_TAGS)\" $(PKG) serve --config config/bridge.yaml.example",
		"go run ./scripts/third-party-licenses -check-build-tags",
	},
	"deploy/linux/deploy-bridge-vps.sh": {
		"GOOS=linux GOARCH=amd64 go build -tags @TAGS@ -ldflags \"-s -w -X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=$VER\" -o \"$LOCAL_BIN\" ./cmd/bridge",
	},
	"deploy/windows/setup-bridge-windows.ps1": {
		"go build -tags @TAGS@ -ldflags $ldflags -o (Join-Path $binDir 'bridge.exe') .\\cmd\\bridge",
	},
	"deploy/windows/update-bridge-windows.ps1": {
		"go build -tags @TAGS@ -ldflags $ldflags -o $exe .\\cmd\\bridge",
	},
	"internal/player/hotplug/testdata/uevent/reconstruct.py": {
		"(GOOS=linux GOARCH=arm64 go test -c ./internal/player/hotplug), copy it",
	},
	"ops/deployment-runbook.md": {
		`GOOS=linux GOARCH=amd64 go build -tags @TAGS@ -ldflags "-s -w -X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=$(git describe --tags --always)" -o dist/bridge-linux-amd64 ./cmd/bridge`,
	},
	"ops/release-playbook.md": {
		"go run ./scripts/third-party-licenses",
	},
	"scripts/alsa-abi/check.sh": {
		"go test -tags @TAGS@ ./internal/alsapcm -run '^TestUAPIMatchesTheKernelHeader$' -count=1 -v -args -abi-probe=\"$work/probe.txt\" | tee \"$work/go-test.txt\"",
	},
	"scripts/player-dsd-goldens/generate.py": {
		"\"with go test ./internal/player/decode -args -dsd-goldens=DIR\",",
	},
}

// buildSiteMarkdown reports whether name is Markdown a reader follows to
// build: README.md and the top-level ops documents, except the records (the
// engineering log, the History files, plans and release-note drafts), which
// quote commands as they were run.
func buildSiteMarkdown(name string) bool {
	if name == "README.md" {
		return true
	}
	if filepath.ToSlash(filepath.Dir(name)) != "ops" || !strings.EqualFold(filepath.Ext(name), ".md") {
		return false
	}
	base := filepath.Base(name)
	return base != "engineering-log.md" && !strings.HasPrefix(base, "History-") && !strings.HasPrefix(base, "plan-") && !strings.HasPrefix(base, "release-notes-")
}

func buildSiteFile(name string) bool {
	if buildSiteMarkdown(name) {
		return true
	}
	base := filepath.Base(name)
	if strings.HasPrefix(base, "Makefile") || strings.HasPrefix(base, "Dockerfile") {
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".sh", ".bash", ".zsh", ".ps1", ".psm1", ".bat", ".cmd", ".mk", ".yml", ".yaml", ".py":
		return true
	}
	return filepath.Ext(name) == "" && (!strings.Contains(name, "/") || strings.HasPrefix(name, "scripts/") || strings.HasPrefix(name, "deploy/"))
}

// markdownShellLines keeps the lines of shell fences: those whose info string
// names a shell, and unlabelled ones, which a reader runs as one. Track every
// fence, including non-shell ones, so prose and code in other languages stay out.
// Blank lines at boundaries prevent a continuation from joining separate blocks.
func markdownShellLines(text string) string {
	fence := regexp.MustCompile("^\\s*(`{3,}|~{3,})(.*)$")
	var marker string
	var shell bool
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if match := fence.FindStringSubmatch(line); match != nil {
			if marker == "" {
				marker = match[1]
				var lang string
				if info := strings.Fields(strings.ToLower(match[2])); len(info) > 0 {
					lang = info[0]
				}
				switch lang {
				case "", "sh", "bash", "shell", "zsh", "ksh", "fish", "console", "powershell", "ps1", "cmd", "bat":
					shell = true
				}
			} else if match[1][0] == marker[0] && len(match[1]) >= len(marker) && strings.TrimSpace(match[2]) == "" {
				marker, shell = "", false
			}
			lines = append(lines, "")
			continue
		}
		if shell {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func checkBuildSites(root, tags string) error {
	remaining := map[string]map[string]int{}
	for name, commands := range reviewedBuildSites {
		remaining[name] = map[string]int{}
		for _, command := range commands {
			remaining[name][strings.ReplaceAll(command, "@TAGS@", tags)]++
		}
	}
	// The executable may be quoted ("$GO", "/usr/local/go/bin/go"). Global
	// flags may have an attached value or a separate (possibly quoted)
	// argument. Unknown invocation forms are still unreviewed census entries.
	command := regexp.MustCompile(`(?:^|[^[:alnum:]_])(?:go(?:\.exe)?|\$\(GO\)|\$\{GO\}|\$GO)["']?\s+(?:-\S+(?:\s+(?:"[^"\r\n]*"|'[^'\r\n]*'|\S+))?\s+)*(?:build|install|run|test|vet)\b`)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			if name == "." {
				return nil
			}
			switch d.Name() {
			case ".git", "bin", "dist", "vendor", "node_modules":
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), "_") {
				return filepath.SkipDir
			}
			if _, err := os.Lstat(filepath.Join(path, ".git")); err == nil {
				return filepath.SkipDir
			} else if !os.IsNotExist(err) {
				return err
			}
			return nil
		}
		if !buildSiteFile(name) {
			// An extensionless executable elsewhere is also an operational site;
			// non-executable fixture data (including Go fuzz corpora) is not.
			if filepath.Ext(name) != "" {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Mode()&0111 == 0 {
				return nil
			}
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("build site must not be a symlink: %s", name)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := strings.ReplaceAll(string(b), "\r\n", "\n")
		if buildSiteMarkdown(name) {
			text = markdownShellLines(text)
		}
		var active []string
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") {
				active = append(active, line)
			}
		}
		// Join shell/Docker and PowerShell continuations before the census.
		text = strings.Join(active, "\n")
		text = strings.ReplaceAll(text, "\\\n", " ")
		text = strings.ReplaceAll(text, "`\n", " ")
		for _, line := range strings.Split(text, "\n") {
			if !command.MatchString(line) {
				continue
			}
			line = strings.Join(strings.Fields(line), " ")
			if remaining[name][line] == 0 {
				return fmt.Errorf("unreviewed build/test site in %s: %s; use release tags and update reviewedBuildSites", name, line)
			}
			remaining[name][line]--
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(remaining) {
		for _, command := range sortedKeys(remaining[name]) {
			if remaining[name][command] != 0 {
				return fmt.Errorf("missing reviewed build/test site in %s: %s", name, command)
			}
		}
	}
	return nil
}
