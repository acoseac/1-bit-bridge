package qualification

import (
	"os"
	"strings"
	"testing"
)

func TestQualificationWorkflowWiring(t *testing.T) {
	b, e := os.ReadFile("../../../.github/workflows/gate.yml")
	if e != nil {
		t.Fatal(e)
	}
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	start := strings.Index(text, "  native-qualification:\n")
	if start < 0 {
		t.Fatal("native-qualification job missing")
	}
	job := text[start:]
	if i := strings.Index(job[1:], "\n  gate:"); i >= 0 {
		job = job[:i+1]
	}
	for _, want := range []string{
		"fetch-depth: 0",
		"github.event.pull_request.head.sha || github.sha",
		"if: github.event_name == 'pull_request'",
		"git fetch origin main:refs/remotes/origin/main",
		"base=$(git merge-base origin/main HEAD)",
		"test -n \"$base\"",
		"go run -tags ts_omit_webclient,ts_omit_debugeventbus ./scripts/qualification-version -base \"$base\" -head HEAD",
		"set -euo pipefail",
		"grep -q '^--- PASS: TestNativeIntegration'",
	} {
		if !strings.Contains(job, want) {
			t.Errorf("qualification job missing %q", want)
		}
	}
	gate := text[strings.Index(text, "  gate:\n"):]
	line := strings.Split(strings.Split(gate, "needs: ")[1], "\n")[0]
	if !strings.Contains(line, "native-qualification") {
		t.Error("qualification absent from gate dependencies")
	}
}
func TestQualificationActivationPaths(t *testing.T) {
	for _, p := range []string{"internal/alsactl/control.go", "internal/alsactl/control_linux.go", "internal/alsactl/uapi.go", "internal/alsa/qualification.go", "internal/player/qualification/inputs.go", "internal/alsapcm/device_native.go", "cmd/bridge/output_dsdtest_input.go"} {
		t.Run(p, func(t *testing.T) {
			dir := t.TempDir()
			if e := os.MkdirAll(dir+"/.git/refs/heads", 0755); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(dir+"/.git/HEAD", []byte("ref: refs/heads/main\n"), 0644); e != nil {
				t.Fatal(e)
			}
			files := map[string]string{"internal/player/qualification/version.go": "const qualificationVersion = 1\n", p: "old"}
			base := gitCommit(t, dir, files)
			files[p] = "new"
			head := gitCommit(t, dir, files)
			if e := CheckVersion(dir, base, head); e == nil {
				t.Fatal("activation-deciding path escaped version gate")
			}
		})
	}
}

func TestNativeBackendIntegrationWorkflow(t *testing.T) {
	b, err := os.ReadFile("../../../.github/workflows/gate.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{
		`go test -v -race -tags ts_omit_webclient,ts_omit_debugeventbus -overlay "$RUNNER_TEMP/native-overlay.json" ./internal/player/playback -run '^TestNativeIntegration'`,
		`grep -q '^--- PASS: TestNativeIntegration' "$RUNNER_TEMP/native-backend-integration.log"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("backend integration gate missing %q", want)
		}
	}
}
