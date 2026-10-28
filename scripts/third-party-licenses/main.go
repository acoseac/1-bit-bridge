// Command third-party-licenses gathers the release binary's dependency notices.
// Run from the repository root with the release Go toolchain.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

type module struct {
	Path, Version, Dir string
	Main               bool
	Replace            *module
	packageDirs        []string
}

type notice struct {
	name string
	text []byte
}

// Supplements are keyed by reviewed module identity and version. The release
// tags omit the former webclient and eventbus assets; no supplements remain.
// Keep this registry for future linked assets, with fixture-tested refusal of
// unreviewed upgrades and replacements.
type embeddedNotice struct{ version, text string }

var embeddedRegistry = map[string]embeddedNotice{}

func embeddedNotices(m module) (string, error) {
	registry := embeddedRegistry
	original := m
	if m.Replace != nil {
		m = *m.Replace
	}
	if _, listed := registry[original.Path]; listed && original.Path != m.Path {
		return "", fmt.Errorf("%s: embedded assets require review for replacement %s %s", original.Path, m.Path, m.Version)
	}
	entry, listed := registry[m.Path]
	if !listed {
		return "", nil
	}
	if m.Version != entry.version {
		return "", fmt.Errorf("%s %s: embedded assets reviewed only for %s; recheck supplementary notices", m.Path, m.Version, entry.version)
	}
	if strings.TrimSpace(entry.text) == "" {
		return "", fmt.Errorf("%s: empty embedded asset notices", m.Path)
	}
	return entry.text, nil
}

func main() {
	output := flag.String("output", "THIRD_PARTY_LICENSES.md", "output file")
	checkTags := flag.Bool("check-build-tags", false, "check release tags at every reviewed build and test site")
	flag.Parse()
	if *checkTags {
		if err := checkBuildTags("."); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("check-build-tags: OK")
		return
	}
	data, counts, err := generate(".")
	if err == nil {
		err = os.WriteFile(*output, data, 0644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "third-party-licenses:", err)
		os.Exit(1)
	}
	for _, target := range sortedKeys(counts) {
		fmt.Printf("%s: %d modules\n", target, counts[target])
	}
}

// releaseTargets reads only the deliberately limited builds stanza used here.
// This is not a general YAML parser: new build settings or YAML forms must fail
// rather than silently change the set of packages whose notices we ship.
func releaseTargets(data []byte) ([]string, error) {
	build, err := releaseBuild(data)
	return build.targets, err
}

// buildTagName is the grammar a release build tag must match.
var buildTagName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

type releaseConfig struct{ targets, tags []string }

func releaseBuild(data []byte) (releaseConfig, error) {
	lists := map[string][]string{}
	scalars := map[string]string{}
	active, field := false, ""
	for _, line := range strings.Split(string(data), "\n") {
		s := strings.TrimSpace(line)
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		if !active {
			if line == "builds:" {
				active = true
			}
			continue
		}
		if !strings.HasPrefix(line, " ") {
			break
		}
		if strings.HasPrefix(line, "  - id: ") {
			if scalars["id"] != "" {
				return releaseConfig{}, fmt.Errorf("expected one release build")
			}
			scalars["id"] = strings.TrimPrefix(line, "  - id: ")
			continue
		}
		if strings.HasPrefix(line, "      - ") {
			if field == "" {
				return releaseConfig{}, fmt.Errorf("unexpected build list: %s", s)
			}
			lists[field] = append(lists[field], strings.TrimPrefix(line, "      - "))
			continue
		}
		if strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") {
			key, value, ok := strings.Cut(s, ":")
			value = strings.TrimSpace(value)
			if !ok {
				return releaseConfig{}, fmt.Errorf("unsupported build line: %s", s)
			}
			field = ""
			switch key {
			case "main", "binary":
				if scalars[key] != "" {
					return releaseConfig{}, fmt.Errorf("duplicate build key: %s", key)
				}
				scalars[key] = value
			case "env", "goos", "goarch", "flags", "ldflags", "tags":
				if value != "" || lists[key] != nil {
					return releaseConfig{}, fmt.Errorf("expected one block list for %s", key)
				}
				lists[key] = []string{}
				field = key
			default:
				return releaseConfig{}, fmt.Errorf("unsupported build setting %q; update licence discovery with release configuration", key)
			}
			continue
		}
		return releaseConfig{}, fmt.Errorf("unsupported build line: %s", s)
	}
	if scalars["id"] != "bridge" || scalars["main"] != "./cmd/bridge" || scalars["binary"] != "bridge" || strings.Join(lists["env"], ",") != "CGO_ENABLED=0" || strings.Join(lists["flags"], ",") != "-trimpath" {
		return releaseConfig{}, fmt.Errorf("expected bridge build with CGO_ENABLED=0 and only -trimpath")
	}
	for _, ldflag := range lists["ldflags"] {
		if ldflag != "-s -w" && !strings.HasPrefix(ldflag, "-X ") {
			return releaseConfig{}, fmt.Errorf("unsupported ldflag: %s", ldflag)
		}
	}
	if tags, present := lists["tags"]; present {
		if len(tags) == 0 {
			return releaseConfig{}, fmt.Errorf("empty tags list")
		}
		seen := map[string]bool{}
		for _, tag := range tags {
			if !buildTagName.MatchString(tag) || yamlTypedTag(tag) || seen[tag] {
				return releaseConfig{}, fmt.Errorf("invalid or duplicate build tag: %q", tag)
			}
			seen[tag] = true
		}
	}
	var targets []string
	for _, key := range []string{"goos", "goarch"} {
		if len(lists[key]) == 0 {
			return releaseConfig{}, fmt.Errorf("empty %s matrix", key)
		}
		seen := map[string]bool{}
		for _, value := range lists[key] {
			if value == "" || strings.ContainsAny(value, " \t#[]{}'\"/") || seen[value] {
				return releaseConfig{}, fmt.Errorf("invalid or duplicate %s value: %q", key, value)
			}
			seen[value] = true
		}
	}
	for _, goos := range lists["goos"] {
		for _, arch := range lists["goarch"] {
			targets = append(targets, goos+"/"+arch)
		}
	}
	sort.Strings(targets)
	return releaseConfig{targets: targets, tags: lists["tags"]}, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func generate(root string) ([]byte, map[string]int, error) {
	// GoReleaser would automatically use vendor, while discovery uses the cache.
	if info, err := os.Stat(filepath.Join(root, "vendor")); err == nil && info.IsDir() {
		return nil, nil, fmt.Errorf("vendor directory exists at module root; release discovery requires module-cache sources")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, nil, fmt.Errorf("check vendor directory: %w", err)
	}
	config, err := readRepoText(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		return nil, nil, err
	}
	build, err := releaseBuild([]byte(config))
	if err != nil {
		return nil, nil, err
	}
	targets := build.targets
	modules := map[string]module{}
	counts := map[string]int{}
	goBin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		goBin += ".exe"
	}
	if err := checkEnvironment(root, goBin); err != nil {
		return nil, nil, err
	}
	version, err := releaseGoVersion(root)
	if err != nil {
		return nil, nil, err
	}
	if runtime.Version() != version {
		return nil, nil, fmt.Errorf("release toolchain must be %s from go.mod; running %s", version, runtime.Version())
	}
	goFiles, err := toolchainNotices(runtime.GOROOT())
	if err != nil {
		return nil, nil, err
	}
	for _, target := range targets {
		goos, arch, _ := strings.Cut(target, "/")
		cmd := exec.Command(goBin, "list", "-mod=readonly", "-deps", "-json", "-tags="+strings.Join(build.tags, ","), "./cmd/bridge")
		cmd.Dir = root
		// Preserve the checked build environment, including persisted go env values.
		// Only the release matrix and its explicit CGO setting vary by target.
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+arch, "CGO_ENABLED=0")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		data, err := cmd.Output()
		if err != nil {
			return nil, nil, fmt.Errorf("go list %s: %w: %s", target, err, stderr.String())
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		linked := map[string]bool{}
		for {
			var pkg struct {
				Module          *module
				Dir, ImportPath string
				EmbedFiles      []string
			}
			err := decoder.Decode(&pkg)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, nil, fmt.Errorf("go list %s JSON: %w", target, err)
			}
			if err := checkPackageEmbeds(pkg.Module, pkg.ImportPath, pkg.EmbedFiles); err != nil {
				return nil, nil, fmt.Errorf("go list %s: %w", target, err)
			}
			if pkg.Module == nil || pkg.Module.Main {
				continue
			}
			m := *pkg.Module
			m.packageDirs = append(modules[m.Path].packageDirs, pkg.Dir)
			modules[m.Path] = m
			linked[m.Path] = true
		}
		if len(linked) == 0 {
			return nil, nil, fmt.Errorf("no dependency modules for %s", target)
		}
		counts[target] = len(linked)
	}
	var out bytes.Buffer
	out.WriteString("# Third-party licences\n\nDependency modules used by the bridge release builds (union of all targets).\nThe bridge's own licence is in LICENSE. Licence and notice files from compiled\npackage directories and their ancestors through each module root are reproduced\nin full below, together with the release Go toolchain's notices.\n\n")
	for _, target := range targets {
		fmt.Fprintf(&out, "- %s: %d modules\n", target, counts[target])
	}
	fmt.Fprintf(&out, "\n## Go standard library and runtime (%s)\n\n", version)
	writeNotices(&out, goFiles)
	for _, path := range sortedKeys(modules) {
		m := modules[path]
		files, err := notices(m)
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(&out, "\n## %s %s\n\n", m.Path, m.Version)
		if m.Replace != nil {
			fmt.Fprintf(&out, "Replacement: %s", m.Replace.Path)
			if m.Replace.Version != "" {
				fmt.Fprintf(&out, " %s", m.Replace.Version)
			}
			out.WriteString("\n\n")
		}
		writeNotices(&out, files)
	}
	return out.Bytes(), counts, nil
}

func writeNotices(out *bytes.Buffer, files []notice) {
	for _, file := range files {
		fmt.Fprintf(out, "### %s\n\n", file.name)
		// A fence longer than any run in the source preserves literal Markdown.
		fence := "```"
		for bytes.Contains(file.text, []byte(fence)) {
			fence += "`"
		}
		out.WriteString(fence + "text\n")
		out.Write(file.text)
		if !bytes.HasSuffix(file.text, []byte("\n")) {
			out.WriteByte('\n')
		}
		out.WriteString(fence + "\n\n")
	}
}

// checkEnvironment observes both inherited variables and go env -w settings.
// Clearing them here would discover a different graph from GoReleaser's build.
func checkEnvironment(root, goBin string) error {
	cmd := exec.Command(goBin, "env", "-json", "GOFLAGS", "GOWORK", "GOAMD64", "GOARM64")
	cmd.Dir = root
	data, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("read release Go environment: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	defaults := map[string]string{"GOFLAGS": "", "GOWORK": "off", "GOAMD64": "v1", "GOARM64": "v8.0"}
	for _, key := range sortedKeys(defaults) {
		value, ok := values[key]
		if !ok || (value != "" && value != defaults[key]) {
			return fmt.Errorf("non-default %s=%q; release discovery requires default build settings", key, value)
		}
	}
	return nil
}

func releaseGoVersion(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "go" {
			return "go" + fields[1], nil
		}
	}
	return "", fmt.Errorf("go.mod has no release toolchain version")
}

// toolchainNotices reads the release toolchain's LICENSE and PATENTS from
// its GOROOT. Homebrew's Go keeps GOROOT in its keg's libexec and moves
// LICENSE to the keg's root (install_metafiles), so a notice GOROOT lacks
// is read there, but only from a keg whose install receipt is
// homebrew/core's of this very Go version (homebrewKeg): never a LICENSE
// that merely sits beside a GOROOT.
func toolchainNotices(root string) ([]notice, error) {
	var files []notice
	for _, name := range []string{"LICENSE", "PATENTS"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if errors.Is(err, fs.ErrNotExist) {
			if keg, kegErr := homebrewKeg(root, runtime.Version()); kegErr == nil {
				data, err = os.ReadFile(filepath.Join(keg, name))
			} else {
				err = fmt.Errorf("%w (%v)", err, kegErr)
			}
		}
		if name == "PATENTS" && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("Go toolchain %s: %w", name, err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, fmt.Errorf("Go toolchain %s: empty notice", name)
		}
		files = append(files, notice{name, data})
	}
	return files, nil
}

// homebrewKeg is the Homebrew keg whose libexec is root, when the keg's
// install receipt says homebrew/core installed Go version there; otherwise
// it says why root is no such keg.
func homebrewKeg(root, version string) (string, error) {
	if filepath.Base(root) != "libexec" {
		return "", errors.New("GOROOT is not a Homebrew keg's libexec")
	}
	keg := filepath.Dir(root)
	data, err := os.ReadFile(filepath.Join(keg, "INSTALL_RECEIPT.json"))
	if err != nil {
		return "", fmt.Errorf("no Homebrew install receipt: %w", err)
	}
	var receipt struct {
		Source struct {
			Tap      string
			Versions struct{ Stable string }
		}
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		return "", fmt.Errorf("Homebrew install receipt: %w", err)
	}
	if receipt.Source.Tap != "homebrew/core" || "go"+receipt.Source.Versions.Stable != version {
		return "", fmt.Errorf("the Homebrew keg is %q's %q, not homebrew/core's %s", receipt.Source.Tap, receipt.Source.Versions.Stable, version)
	}
	return keg, nil
}

// noticeName classifies suffixes only after the caller identifies a notice
// prefix. Unknown suffixes are errors, even beside another valid licence.
func noticeName(name string) (bool, error) {
	// Match the complete dotted suffix: filepath.Ext would read Apache-2.0 as .0.
	_, suffix, _ := strings.Cut(strings.ToLower(name), ".")
	switch suffix {
	case "", "txt", "md", "rst", "markdown", "html", "htm", "mit", "apache", "apache-2.0", "bsd", "isc", "lesser", "gpl", "lgpl", "mpl-2.0", "bsd-2-clause", "bsd-3-clause":
		return true, nil
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".go", ".c", ".h", ".s", ".js", ".py", ".sh", ".rs", ".cpp", ".java", ".ts", ".cc", ".cxx", ".hpp", ".m", ".mm", ".jsx", ".tsx", ".swift", ".rb", ".php", ".cs":
		return false, nil
	}
	return false, fmt.Errorf("unknown notice suffix: %s", name)
}

func notices(m module) ([]notice, error) {
	dir := m.Dir
	if m.Replace != nil {
		dir = m.Replace.Dir
	}
	dir = filepath.Clean(dir)
	dirs := map[string]bool{dir: true}
	for _, pkgDir := range m.packageDirs {
		rel, err := filepath.Rel(dir, pkgDir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s: package directory outside module root: %s", m.Path, pkgDir)
		}
		for current := filepath.Clean(pkgDir); current != dir; current = filepath.Dir(current) {
			dirs[current] = true
		}
	}
	var files []notice
	hasLicense := false
	for _, current := range sortedKeys(dirs) {
		entries, err := os.ReadDir(current)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", m.Path, m.Version, err)
		}
		for _, entry := range entries {
			name := strings.ToUpper(entry.Name())
			license := strings.HasPrefix(name, "LICENSE") || strings.HasPrefix(name, "LICENCE") || strings.HasPrefix(name, "COPYING") || strings.HasPrefix(name, "COPYRIGHT") || strings.HasPrefix(name, "UNLICENSE")
			if entry.IsDir() || !(license || strings.HasPrefix(name, "NOTICE") || strings.HasPrefix(name, "PATENTS")) {
				continue
			}
			include, err := noticeName(entry.Name())
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", m.Path, entry.Name(), err)
			}
			if !include {
				continue
			}
			full := filepath.Join(current, entry.Name())
			rel, err := filepath.Rel(dir, full)
			if err != nil {
				return nil, err
			}
			label := filepath.ToSlash(rel)
			data, err := os.ReadFile(full)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", m.Path, label, err)
			}
			if len(bytes.TrimSpace(data)) == 0 {
				return nil, fmt.Errorf("%s/%s: empty notice", m.Path, label)
			}
			files = append(files, notice{label, data})
			hasLicense = hasLicense || (license && current == dir)
		}
	}
	if !hasLicense {
		return nil, fmt.Errorf("%s %s: no licence file at module root", m.Path, m.Version)
	}
	supplement, err := embeddedNotices(m)
	if err != nil {
		return nil, err
	}
	if supplement != "" {
		files = append(files, notice{"Embedded third-party assets", []byte(supplement)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}

// yamlTypedTag refuses YAML 1.1 and 1.2 implicit non-string words. Numeric
// spellings are excluded by the tag grammar's letter/underscore first byte.
func yamlTypedTag(tag string) bool {
	switch strings.ToLower(tag) {
	case "null", "true", "false", "yes", "no", "on", "off", "y", "n":
		return true
	}
	return false
}

// These embedded files were reviewed as module-owned data/scripts covered by
// their module notices. Keys are exact module@version plus import path; files
// are an exact set, never a glob. Any replacement needs a fresh review.
var reviewedEmbeds = map[string][]string{
	"google.golang.org/protobuf@v1.36.12 google.golang.org/protobuf/internal/editiondefaults": {"editions_defaults.binpb"},
	"tailscale.com@v1.104.2 tailscale.com":                                                    {"ALPINE.txt", "VERSION.txt", "go.toolchain.next.rev", "go.toolchain.rev"},
	"tailscale.com@v1.104.2 tailscale.com/net/dns":                                            {"resolvconf-workaround.sh"},
	"tailscale.com@v1.104.2 tailscale.com/net/dnsfallback":                                    {"dns-fallback-servers.json"},
}

func checkPackageEmbeds(m *module, pkg string, files []string) error {
	if pkg == "tailscale.com/client/web" || pkg == "github.com/tailscale/web-client-prebuilt" || strings.HasPrefix(pkg, "github.com/tailscale/web-client-prebuilt/") || (m != nil && m.Path == "github.com/tailscale/web-client-prebuilt") {
		return fmt.Errorf("forbidden web package %s; review release omission tags", pkg)
	}
	if m == nil || m.Main || len(files) == 0 {
		return nil
	}
	key := m.Path + "@" + m.Version + " " + pkg
	expected, ok := reviewedEmbeds[key]
	actual := append([]string(nil), files...)
	sort.Strings(actual)
	if !ok || m.Replace != nil || strings.Join(actual, "\n") != strings.Join(expected, "\n") {
		return fmt.Errorf("unreviewed embedded files in %s: %v; review module version, package and exact file set", key, files)
	}
	return nil
}
