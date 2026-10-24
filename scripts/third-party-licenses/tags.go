package main

import (
	"fmt"
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
	for _, line := range strings.Split(makefile, "\n") {
		if strings.Contains(line, "go build ") && strings.Count(line, "-tags") != 1 {
			return fmt.Errorf("Makefile build must use exactly one -tags flag")
		}
	}
	if strings.Count(makefile, "go build ") != 7 || strings.Count(makefile, `go build -tags "$(GO_TAGS)" `) != 7 {
		return fmt.Errorf("every Makefile build must use GO_TAGS; review changed build commands")
	}
	if strings.Count(dockerfile, "go build ") != 1 || strings.Count(dockerfile, "-tags ") != 1 || !strings.Contains(dockerfile, "go build \\\n    -tags "+tags+" \\\n") {
		return fmt.Errorf("Dockerfile go build must match .goreleaser.yaml tags %q", tags)
	}
	return nil
}
