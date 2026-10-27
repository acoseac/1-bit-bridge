package main

import (
	"context"
	"flag"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocumentedNativeInvocations(t *testing.T) {
	data, err := os.ReadFile("../../ops/b470-native-qualification.md")
	if err != nil {
		t.Fatal(err)
	}
	expand := strings.NewReplacer("$CONTACT", "contact.json", "$RUN", "run", "$Q", "evidence", "$C", "config.yaml", "$F", "DSD_U32_LE", "$R", "2822400", "$M", "kernel", "$T", "10", "$P", "10", "\"", "")
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "output dsd-test "); i >= 0 && strings.HasPrefix(line, "capture &&") {
			args := strings.Fields(expand.Replace(line[i+len("output dsd-test "):]))
			o, e := parseNativeOptions(args, io.Discard)
			if e != nil {
				t.Fatalf("documented invocation: %s: %v", line, e)
			}
			t.Run(o.scenario, func(t *testing.T) {
				if len(o.list) == 0 {
					t.Fatal("no fixture IDs")
				}
			})
			count++
		}
		if i := strings.Index(line, "output play "); i >= 0 && strings.HasPrefix(line, "capture &&") {
			args := strings.Fields(expand.Replace(line[i+len("output play "):]))
			if got := runOutputPlay(context.Background(), args, outputPlayEnv{goos: "parser-only"}, io.Discard, io.Discard); got != 1 {
				t.Fatalf("documented play failed parse: %d", got)
			}
			count++
		}
	}
	if count != 5 {
		t.Fatalf("executed %d documented invocations, want 5", count)
	}
}
func TestProductionPlayReportFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		bad  bool
	}{{"none", nil, false}, {"complete", []string{"--report", "report.json", "--contact-report", "contacts.json", "--mixer-snapshot", "mixer.txt"}, false}, {"missing_contact", []string{"--report", "report.json", "--mixer-snapshot", "mixer.txt"}, true}, {"missing_report", []string{"--contact-report", "contacts.json", "--mixer-snapshot", "mixer.txt"}, true}, {"missing_mixer", []string{"--report", "report.json", "--contact-report", "contacts.json"}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("play", flag.ContinueOnError)
			o := playReportFlags(fs)
			if e := fs.Parse(tc.args); e != nil {
				t.Fatal(e)
			}
			if (o.validate() != nil) != tc.bad {
				t.Fatal("report parser interlock")
			}
		})
	}
}

func TestCandidateConstructorIsCLIOnly(t *testing.T) {
	roots := []string{"../../internal", "."}
	calls := 0
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			tree, e := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if e != nil {
				return e
			}
			ast.Inspect(tree, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := c.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "NewQualificationBackend" {
					return true
				}
				calls++
				if filepath.Base(path) != "output_dsdtest_backend.go" {
					t.Errorf("candidate constructor outside CLI lifecycle: %s", path)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("candidate constructor calls=%d", calls)
	}
}

func TestLifecycleEvidenceRequiresMeasuredCompletion(t *testing.T) {
	v := &lifecycleEvidence{entries: map[engine.OccurrenceID]int{1: 0}, lengths: map[engine.OccurrenceID]int64{1: 100}, complete: []bool{false}, actions: map[string]bool{}}
	var report nativeReport
	for _, action := range []string{"natural", "next", "previous", "pause", "resume", "released", "reopened", "stop"} {
		v.action(action)
	}
	v.position(engine.Position{Occurrence: 1, Frames: 99})
	if v.copy(&report) {
		t.Fatal("actions alone completed lifecycle")
	}
	v.position(engine.Position{Occurrence: 1, Frames: 100})
	if !v.copy(&report) || !report.Completed[0] {
		t.Fatal("measured completion omitted")
	}
	delete(v.actions, "reopened")
	if v.copy(&report) {
		t.Fatal("missing released resume accepted")
	}
}

func TestLifecycleResolverRechecksOpenedHash(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "fixture.dsf"), []byte("changed bytes"), 0600); e != nil {
		t.Fatal(e)
	}
	f := lifecycleFiles{root: dir, fixtures: []qualification.Fixture{{Path: "fixture.dsf", SHA256: strings.Repeat("0", 64)}}}
	ref, _ := engine.NewReference("file:1")
	if _, e := f.Open(context.Background(), ref); e == nil || !strings.Contains(e.Error(), "hash mismatch") {
		t.Fatalf("opened descriptor was not hashed: %v", e)
	}
}
