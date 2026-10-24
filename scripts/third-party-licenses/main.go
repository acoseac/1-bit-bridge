// Command third-party-licenses gathers the release binary's dependency notices.
// Run from the repository root with the release Go toolchain.
package main

import (
	"bytes"
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
}

type notice struct {
	name string
	text []byte
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
	for _, target := range targets {
		goos, arch, _ := strings.Cut(target, "/")
		cmd := exec.Command(goBin, "list", "-mod=readonly", "-deps", "-json", "./cmd/bridge")
		cmd.Dir = root
		// Ignore developer build flags and workspaces. Keep GOPROXY/GOMODCACHE so
		// an offline release can use its already populated module cache.
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+arch, "CGO_ENABLED=0", "GOFLAGS=", "GOWORK=off", "GOAMD64=v1", "GOARM64=v8.0")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		data, err := cmd.Output()
		if err != nil {
			return nil, nil, fmt.Errorf("go list %s: %w: %s", target, err, stderr.String())
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		linked := map[string]bool{}
		for {
			var pkg struct{ Module *module }
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
			modules[m.Path] = m
			linked[m.Path] = true
		}
		if len(linked) == 0 {
			return nil, nil, fmt.Errorf("no dependency modules for %s", target)
		}
		counts[target] = len(linked)
	}
	var out bytes.Buffer
	out.WriteString("# Third-party licences\n\nDependency modules used by the bridge release builds (union of all targets).\nThe bridge's own licence is in LICENSE. Module-root licence and notice files\nare reproduced in full below.\n\n")
	for _, target := range targets {
		fmt.Fprintf(&out, "- %s: %d modules\n", target, counts[target])
	}
	for _, path := range sortedKeys(modules) {
		m := modules[path]
		files, err := notices(m, filepath.Join(runtime.GOROOT(), "PATENTS"))
		if err != nil {
			return nil, nil, err
		}
		fmt.Fprintf(&out, "\n## %s %s\n\n", m.Path, m.Version)
		if m.Replace != nil {
			fmt.Fprintf(&out, "Replacement: %s %s\n\n", m.Replace.Path, m.Replace.Version)
		}
		for _, file := range files {
			fmt.Fprintf(&out, "### %s\n\n", file.name)
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
	return out.Bytes(), counts, nil
}

func notices(m module, goPatents string) ([]notice, error) {
	dir := m.Dir
	if m.Replace != nil {
		dir = m.Replace.Dir
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", m.Path, m.Version, err)
	}
	var files []notice
	hasLicense, hasPatents := false, false
	for _, entry := range entries {
		name := strings.ToUpper(entry.Name())
		license := strings.HasPrefix(name, "LICENSE") || strings.HasPrefix(name, "COPYING") || name == "UNLICENSE"
		if entry.IsDir() || !(license || strings.HasPrefix(name, "NOTICE") || name == "PATENTS") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("%s/%s: %w", m.Path, entry.Name(), err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, fmt.Errorf("%s/%s: empty notice", m.Path, entry.Name())
		}
		files = append(files, notice{entry.Name(), data})
		hasLicense = hasLicense || license
		hasPatents = hasPatents || name == "PATENTS"
	}
	if !hasLicense {
		return nil, fmt.Errorf("%s %s: no licence file in module root", m.Path, m.Version)
	}
	if strings.HasPrefix(m.Path, "golang.org/x/") && !hasPatents {
		data, err := os.ReadFile(goPatents)
		if err != nil {
			return nil, fmt.Errorf("%s: Go PATENTS grant: %w", m.Path, err)
		}
		if len(bytes.TrimSpace(data)) == 0 {
			return nil, fmt.Errorf("%s: empty Go PATENTS grant", m.Path)
		}
		files = append(files, notice{"PATENTS (Go project)", data})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].name < files[j].name })
	return files, nil
}
