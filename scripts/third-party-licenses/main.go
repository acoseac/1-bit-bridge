// Command third-party-licenses gathers the release binary's dependency notices.
// Run from the repository root with the release Go toolchain.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

// These notices are reviewed against exact module versions. An upgrade (or a
// replacement) must re-audit the embedded assets before changing this registry.
//
//go:embed embedded-web-client.md
var webClientNotices string

//go:embed embedded-tailscale.md
var tailscaleNotices string

func embeddedNotices(m module) (string, error) {
	registry := map[string]struct{ version, text string }{
		"github.com/tailscale/web-client-prebuilt": {"v0.0.0-20260917222731-e0ed2d0d0fea", webClientNotices},
		"tailscale.com": {"v1.104.2", tailscaleNotices},
	}
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
	flag.Parse()
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
				return nil, fmt.Errorf("expected one release build")
			}
			scalars["id"] = strings.TrimPrefix(line, "  - id: ")
			continue
		}
		if strings.HasPrefix(line, "      - ") {
			if field == "" {
				return nil, fmt.Errorf("unexpected build list: %s", s)
			}
			lists[field] = append(lists[field], strings.TrimPrefix(line, "      - "))
			continue
		}
		if strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ") {
			key, value, ok := strings.Cut(s, ":")
			value = strings.TrimSpace(value)
			if !ok {
				return nil, fmt.Errorf("unsupported build line: %s", s)
			}
			field = ""
			switch key {
			case "main", "binary":
				if scalars[key] != "" {
					return nil, fmt.Errorf("duplicate build key: %s", key)
				}
				scalars[key] = value
			case "env", "goos", "goarch", "flags", "ldflags":
				if value != "" || lists[key] != nil {
					return nil, fmt.Errorf("expected one block list for %s", key)
				}
				lists[key] = []string{}
				field = key
			default:
				return nil, fmt.Errorf("unsupported build setting %q; update licence discovery with release configuration", key)
			}
			continue
		}
		return nil, fmt.Errorf("unsupported build line: %s", s)
	}
	if scalars["id"] != "bridge" || scalars["main"] != "./cmd/bridge" || scalars["binary"] != "bridge" || strings.Join(lists["env"], ",") != "CGO_ENABLED=0" || strings.Join(lists["flags"], ",") != "-trimpath" {
		return nil, fmt.Errorf("expected bridge build with CGO_ENABLED=0 and only -trimpath")
	}
	for _, ldflag := range lists["ldflags"] {
		if ldflag != "-s -w" && !strings.HasPrefix(ldflag, "-X ") {
			return nil, fmt.Errorf("unsupported ldflag: %s", ldflag)
		}
	}
	var targets []string
	for _, key := range []string{"goos", "goarch"} {
		if len(lists[key]) == 0 {
			return nil, fmt.Errorf("empty %s matrix", key)
		}
		seen := map[string]bool{}
		for _, value := range lists[key] {
			if value == "" || strings.ContainsAny(value, " \t#[]{}'\"/") || seen[value] {
				return nil, fmt.Errorf("invalid or duplicate %s value: %q", key, value)
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
	return targets, nil
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
	config, err := os.ReadFile(filepath.Join(root, ".goreleaser.yaml"))
	if err != nil {
		return nil, nil, err
	}
	targets, err := releaseTargets(config)
	if err != nil {
		return nil, nil, err
	}
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
		cmd := exec.Command(goBin, "list", "-mod=readonly", "-deps", "-json", "./cmd/bridge")
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
				Module *module
				Dir    string
			}
			err := decoder.Decode(&pkg)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, nil, fmt.Errorf("go list %s JSON: %w", target, err)
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

func toolchainNotices(root string) ([]notice, error) {
	var files []notice
	for _, name := range []string{"LICENSE", "PATENTS"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if name == "PATENTS" && os.IsNotExist(err) {
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
