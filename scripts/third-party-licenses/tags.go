package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// readRepoText reads a repository text file as its LF text: a Windows
// checkout's autocrlf gives every line a CRLF ending (CI's windows-latest
// leg), and these checks read the files by their lines. Only repository files
// the checks parse go through it; a licence text is copied byte for byte.
func readRepoText(path string) (string, error) {
	b, err := os.ReadFile(path)
	return strings.ReplaceAll(string(b), "\r\n", "\n"), err
}

// checkBuildTags deliberately accepts only the build command shapes used here.
// GoReleaser is authoritative; a new build form needs a matching guard change.
func checkBuildTags(root string) error {
	read := func(name string) (string, error) {
		return readRepoText(filepath.Join(root, name))
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
// executable shell/PowerShell/workflow sites. Potentially executable fences
// in every non-record Markdown file are sites too (buildSiteMarkdown).
var reviewedBuildSites = map[string][]string{
	"ops/b470-native-qualification.md": {
		"GOTOOLCHAIN=go1.27.3 go test -race -tags=@TAGS@ -timeout 30m ./internal/alsapcm ./internal/alsactl ./internal/alsa ./internal/player/format ./internal/player/qualification ./cmd/bridge",
		"GOTOOLCHAIN=go1.27.3 go test -tags=@TAGS@ ./internal/player/qualification -run '^TestQualificationVersionGate$' -count=1",
		"GOTOOLCHAIN=go1.27.3 go run -tags @TAGS@ ./scripts/qualification-version -base \"$BASE\" -head HEAD",
		"GOTOOLCHAIN=go1.27.3 go test -race -tags @TAGS@ -overlay /tmp/native-overlay.json ./cmd/bridge -run '^TestNativeIntegration' -timeout 5m -watchdog 10s",
	},
	"scripts/native-integration-overlay.py": {
		"\"\"\"Expose the existing ALSA fake ONLY in an explicit Go test overlay.",
	},

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
		"go run -tags @TAGS@ ./scripts/qualification-version -base \"$base\" -head HEAD",
		"go test -v -race -tags @TAGS@ -overlay \"$RUNNER_TEMP/native-overlay.json\" ./cmd/bridge -run '^TestNativeIntegration' -timeout 5m -watchdog 10s | tee \"$RUNNER_TEMP/native-integration.log\"",
		"go test -v -race -tags @TAGS@ -overlay \"$RUNNER_TEMP/native-overlay.json\" ./internal/player/playback -run '^TestNativeIntegration' -timeout 5m | tee \"$RUNNER_TEMP/native-backend-integration.log\"",

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
		"TMPDIR=\"$(TMPDIR)\" go test -tags \"$(GO_TAGS)\" -p $(P) -race -timeout 30m $(TEST_PKGS)",
		"TMPDIR=\"$(TMPDIR)\" go test -tags \"$(GO_TAGS)\" -p $(P) $(TEST_PKGS)",
		"BRIDGE_DSD_FIXTURE_TESTS=1 go test -tags \"$(GO_TAGS)\" ./internal/transcode/ -run 'TestDSDRender_|TestDSDLowpass|TestRunDSD' -count=1 -v",
		"go vet -tags \"$(GO_TAGS)\" ./...",
		"go run -tags \"$(GO_TAGS)\" $(PKG) serve --config config/bridge.yaml.example",
		"go run ./scripts/third-party-licenses -check-build-tags",
		"$(MAKE) vet",  // Recurses into the tagged Make target.
		"$(MAKE) test", // Recurses into the tagged Make target.
		`docker buildx build --load --build-arg VERSION="$(VERSION)" -t $(IMAGE):dev .`, // Docker context, not a Go package.
	},
	"deploy/linux/deploy-bridge-vps.sh": {
		"GOOS=linux GOARCH=amd64 go build -tags @TAGS@ -ldflags \"-s -w -X github.com/acoseac/1-bit-bridge/internal/version.ServerVersion=$VER\" -o \"$LOCAL_BIN\" ./cmd/bridge",
	},
	"deploy/windows/firewall-bridge-windows.ps1": {
		// Ambiguous variable/flag/verb shape in a firewall description, not Go.
		`New-NetFirewallRule -DisplayName $ruleName -Description "1-bit-bridge HTTPS listener (PR #276 fresh install)" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 7788 -Program $exePath -Profile Domain,Private,Public -Enabled True | Out-Null`,
	},
	"docs/docker.md": {
		"docker buildx build --load -t 1-bit-bridge:dev .", // Docker context, not a Go package.
	},
	"scripts/doc-split/restructure-2026-10-18/routes.py": {
		`("- how to run all of it with agents inside the usage limits.", "skill:release-playbook"),`, // Migration text, not a command.
	},
	"docs/LoupeReviewCycle.md": {
		"go test -tags @TAGS@ ./internal/x/... -count=1 -v 2>&1 | tee /tmp/t.log",
	},
	"internal/player/decode/testdata/alac-goldens/README.md": {
		"go test -tags @TAGS@ ./internal/player/decode -args -alac-goldens=DIR",
	},
	"internal/player/decode/testdata/dsd-goldens/README.md": {
		"go test -tags @TAGS@ ./internal/player/decode -args -dsd-goldens=/tmp/dsd-goldens",
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

// buildSiteMarkdown includes every Markdown file except records that quote
// commands as they were run, wherever the repository walk reaches them.
func buildSiteMarkdown(name string) bool {
	base := filepath.Base(name)
	return strings.EqualFold(filepath.Ext(name), ".md") && base != "engineering-log.md" &&
		!strings.HasPrefix(base, "History-") && !strings.HasPrefix(base, "plan-") && !strings.HasPrefix(base, "release-notes-")
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

// markdownContainer records a quote prefix or a list item's content indentation.
// Widths are relative to the enclosing container, so nested lists and quotes
// can close independently. Blank lines continue lists, but never blockquotes.
type markdownContainer struct {
	quote bool
	width int
}

func markdownContainerLine(line string, parents []markdownContainer) (string, []markdownContainer, int) {
	line = strings.ReplaceAll(line, "\t", "    ")
	var containers []markdownContainer
	for _, parent := range parents {
		if parent.quote {
			trimmed := strings.TrimLeft(line, " ")
			if !strings.HasPrefix(trimmed, ">") {
				break
			}
			line = strings.TrimPrefix(trimmed[1:], " ")
		} else if strings.TrimSpace(line) != "" {
			if len(line)-len(strings.TrimLeft(line, " ")) < parent.width {
				break
			}
			line = line[parent.width:]
		}
		containers = append(containers, parent)
	}
	continued := len(containers)
	for {
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, ">") {
			containers = append(containers, markdownContainer{quote: true})
			line = strings.TrimPrefix(trimmed[1:], " ")
			continue
		}
		if loc := markdownListMarker.FindStringIndex(trimmed); loc != nil {
			containers = append(containers, markdownContainer{width: len(line) - len(trimmed) + loc[1]})
			line = trimmed[loc[1]:]
			continue
		}
		return trimmed, containers, continued
	}
}

var markdownListMarker = regexp.MustCompile(`^(?:[-*+]|[0-9]{1,9}[.)])[ \t]+`)

// markdownShellLines skips only explicitly non-executable fence languages.
// Unknown labels and unlabelled fences are scanned. Track skipped fences too
// so their closers never turn subsequent prose into executable content.
// Blank boundaries prevent continuations from joining separate blocks.
func markdownShellLines(text string) string {
	fence := regexp.MustCompile("^(`{3,}|~{3,})(.*)$")
	var marker string
	var executable, prompts bool
	var containers, fenceContainers []markdownContainer
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		content, current, continued := markdownContainerLine(line, containers)
		if marker != "" && continued < len(fenceContainers) {
			// CommonMark closes an unclosed fence with its containing block.
			marker, executable, prompts = "", false, false
			lines = append(lines, "")
		}
		containers = current
		if marker != "" {
			// Code inside the fence cannot start another Markdown container.
			containers = fenceContainers
		}
		if match := fence.FindStringSubmatch(content); match != nil {
			if marker == "" {
				// A backtick in a backtick opener's info makes it inline code.
				if match[1][0] == '`' && strings.Contains(match[2], "`") {
					continue
				}
				marker = match[1]
				fenceContainers = containers
				var lang string
				if info := strings.Fields(strings.ToLower(match[2])); len(info) > 0 {
					lang = info[0]
				}
				executable = true
				switch lang {
				case "text", "plain", "plaintext", "txt", "output", "log", "diff", "patch", "json", "jsonc", "yaml", "yml", "toml", "ini", "xml", "html", "css", "sql", "go", "golang", "swift", "mermaid":
					executable = false
				}
				prompts = strings.Contains(lang, "console") || strings.Contains(lang, "session") || strings.Contains(lang, "terminal")
			} else if match[1][0] == marker[0] && len(match[1]) >= len(marker) && strings.TrimSpace(match[2]) == "" {
				marker, executable, prompts = "", false, false
			}
			lines = append(lines, "")
			continue
		}
		if executable {
			if prompts && len(content) >= 2 && strings.ContainsAny(content[:1], "$#%>") && (content[1] == ' ' || content[1] == '\t') {
				content = content[2:]
			}
			lines = append(lines, content)
		}
	}
	return strings.Join(lines, "\n")
}

// Keep the executable branch even without a recognizable package argument.
// Quotes and argv punctuation are separators only on the matching copy;
// the census retains the original logical line. Flags may have quoted values
// containing spaces. Ambiguous matches are deliberately sites requiring review.
const buildVariable = `\$\{[^}]*\}|\$\([^)]*\)|\$[[:alnum:]_:]+|%[^%\s]+%`

var buildCommand = regexp.MustCompile(`(?i)(?:^|[\s=(&;|!])` +
	// A path may itself contain expansions, such as $(GOROOT)/bin/go.
	`(?:(?:(?:` + buildVariable + `|[^\s;|&()])+[/\\])?go(?:[0-9][0-9.]*|tip)?(?:\.exe)?(?:` + buildVariable + `)*|` + buildVariable + `)\s+` +
	`(?:-\S+(?:[ \t]+[^\s;|&]+)*?[ \t]+)*(?:build|install|run|test|vet)\b`)

var buildCommandSeparators = strings.NewReplacer("\"", " ", "'", " ", "`", " ", ",", " ", "[", " ", "]", " ")

// The verb/package branch deliberately ignores executable spelling: a verb
// followed by a package pattern is a site whatever runs it. Flag values may
// span several normalized tokens (quoted paths, templates, argv lists).
// Ambiguity, including non-Go build/install commands, requires explicit review.
// buildVerbAt is anchored at the verb; verbPackageSite finds the verbs.
var buildVerbAt = regexp.MustCompile(`^(?:build|install|run|test|vet)\s+` +
	`(?:-\S+(?:[ \t]+[^\s;|&]+)*?[ \t]+)*` +
	`(?:\.|\.[/\\][^\s;|&]*|all|std|[^\s;|&]+/\.\.\.|github\.com/acoseac/1-bit-bridge(?:/[^\s;|&]*)?)(?:$|[\s;)])`)

// commandWords are the words a command follows: a verb after one, or at the
// start of a line, after ;, &, |, ( or !, or after a VAR=value assignment, is
// the command itself (the coreutils install, the shell's test), never an
// argument of a Go executable, so its ./ path names no package.
var commandWords = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "do": true, "while": true,
	"until": true, "sudo": true, "exec": true, "command": true, "env": true, "time": true, "nohup": true, "nice": true}

var shellAssignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// verbPackageSite reports whether line holds a Go verb, standing as an
// argument (after the token that runs it), followed by its flags and a package
// pattern.
func verbPackageSite(line string) bool {
	for i := 0; i < len(line); i++ {
		if i > 0 && strings.IndexByte(" \t=(&;|!", line[i-1]) < 0 {
			continue
		}
		if buildVerbAt.MatchString(line[i:]) && !commandPosition(line[:i]) {
			return true
		}
	}
	return false
}

// commandPosition reports whether a word that follows before stands where a
// command does.
func commandPosition(before string) bool {
	before = strings.TrimRight(before, " \t")
	if before == "" || strings.IndexByte(";&|(!", before[len(before)-1]) >= 0 {
		return true
	}
	fields := strings.Fields(before)
	last := fields[len(fields)-1]
	return commandWords[last] || shellAssignment.MatchString(last)
}

// A tool that drives the go command is a site wherever it is named: dlv's
// subcommand may follow its flags (dlv --headless debug), and golangci-lint
// is reached through its GitHub action (golangci/golangci-lint-action) too.
var buildDrivingTool = regexp.MustCompile(`(?i)(?:^|[\s=(&;|!])(?:[^\s;|&()]+[/\\])?` +
	`(?:(?:gotestsum|staticcheck|golangci-lint)(?:\.exe)?\b|` +
	`dlv(?:\.exe)?\s+(?:-\S+(?:\s+[^-\s][^\s;|&]*)?\s+)*(?:debug|test|exec)\b)`)

func isBuildCommand(line string) bool {
	// Each branch requires a literal word that normalization cannot create.
	// Avoid the path/flag regexes on unrelated lines; the refusal matrix scans
	// the same operational files thousands of times under the race detector.
	lower := strings.ToLower(line)
	hasVerb := false
	for _, verb := range []string{"build", "install", "run", "test", "vet"} {
		hasVerb = hasVerb || strings.Contains(lower, verb)
	}
	hasTool := false
	for _, tool := range []string{"gotestsum", "golangci-lint", "staticcheck", "dlv"} {
		hasTool = hasTool || strings.Contains(lower, tool)
	}
	if !hasVerb && !hasTool {
		return false
	}
	hasExecutable := strings.Contains(lower, "go") || strings.ContainsAny(line, "$%")
	match := buildCommandSeparators.Replace(line)
	return hasVerb && (verbPackageSite(match) || hasExecutable && buildCommand.MatchString(match)) || hasTool && buildDrivingTool.MatchString(match)
}

func checkBuildSites(root, tags string) error {
	remaining := map[string]map[string]int{}
	for name, commands := range reviewedBuildSites {
		remaining[name] = map[string]int{}
		for _, command := range commands {
			remaining[name][strings.ReplaceAll(command, "@TAGS@", tags)]++
		}
	}
	sites, err := discoverBuildSites(root)
	if err != nil {
		return err
	}
	for _, name := range sortedKeys(sites) {
		fileSites, ok := remaining[name]
		for _, line := range sites[name] {
			if !ok || fileSites[line] == 0 {
				return fmt.Errorf("unreviewed build/test site in %s: %s; use release tags and update reviewedBuildSites", name, line)
			}
			fileSites[line]--
		}
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

// discoverBuildSites retains even files with no sites so an audit can account
// for every in-scope Markdown file, not just files with a matched command.
func discoverBuildSites(root string) (map[string][]string, error) {
	sites := map[string][]string{}
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
		sites[name] = nil
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
			if !isBuildCommand(line) {
				continue
			}
			line = strings.Join(strings.Fields(line), " ")
			sites[name] = append(sites[name], line)
		}
		return nil
	})
	return sites, err
}
