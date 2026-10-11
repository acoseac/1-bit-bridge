package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// TestUpscaleCLIRefuses32BitWhenTheHostCannotEncodeIt is the CLI half of
// the planning gate: a 32-bit run whose probe says no exits non-zero with
// the fix, before any job is started.
func TestUpscaleCLIRefuses32BitWhenTheHostCannotEncodeIt(t *testing.T) {
	prev := transcode.PlanFLAC32
	t.Cleanup(func() { transcode.PlanFLAC32 = prev })
	transcode.PlanFLAC32 = func(context.Context) (bool, error) { return false, nil }

	store := openCLIStore(t)
	var stdout, stderr bytes.Buffer
	code := runUpscaleBatch(context.Background(), &stdout, &stderr, store, &config.Config{}, nil, runUpscaleParams{
		targetBits: 32,
		workers:    1,
	})
	if code == 0 {
		t.Fatalf("exit 0, want non-zero; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), transcode.FLAC32EncodeFix) {
		t.Fatalf("stderr %q, want the fix %q", stderr.String(), transcode.FLAC32EncodeFix)
	}

	transcode.PlanFLAC32 = func(context.Context) (bool, error) { return false, transcode.ErrSoxMissing }
	stderr.Reset()
	code = runUpscaleBatch(context.Background(), &stdout, &stderr, store, &config.Config{}, nil, runUpscaleParams{
		targetBits: 32,
		workers:    1,
	})
	if code != 0 {
		t.Fatalf("a probe that did not run exited %d; stderr=%q", code, stderr.String())
	}
}

func openCLIStore(t *testing.T) *manifest.Store {
	t.Helper()
	s, err := manifest.OpenStore(filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
