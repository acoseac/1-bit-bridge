package doctor

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

func TestCheckFLAC32Bit(t *testing.T) {
	real := probeFLAC32
	t.Cleanup(func() { probeFLAC32 = real })
	called := 0
	probeFLAC32 = func(context.Context) (bool, error) {
		called++
		return false, errors.New("probe should not run")
	}
	for _, bits := range []int{0, 16, 24} {
		c := checkFLAC32Bit(context.Background(), Deps{UpscaleTargetBits: bits})
		if c.Name != checkNameFLAC32 || c.Status != OK {
			t.Fatalf("target %d: %+v, want ok", bits, c)
		}
		if bits == 0 && c.Summary != "not targeting 32-bit FLAC" {
			t.Fatalf("unloaded target summary %q", c.Summary)
		}
	}
	if called != 0 {
		t.Fatalf("probe ran %d times for a target that is not 32", called)
	}

	probeFLAC32 = func(context.Context) (bool, error) { return true, nil }
	if c := checkFLAC32Bit(context.Background(), Deps{UpscaleTargetBits: 32}); c.Status != OK || c.Summary != "sox encodes 32-bit FLAC" {
		t.Fatalf("a sox that writes 32: %+v", c)
	}

	probeFLAC32 = func(context.Context) (bool, error) { return false, nil }
	c := checkFLAC32Bit(context.Background(), Deps{UpscaleTargetBits: 32})
	if c.Status != Warn || c.Summary != "sox cannot encode 32-bit FLAC" || c.Hint != transcode.FLAC32EncodeFix {
		t.Fatalf("a sox that falls back: %+v", c)
	}

	probeFLAC32 = func(context.Context) (bool, error) { return false, transcode.ErrSoxMissing }
	c = checkFLAC32Bit(context.Background(), Deps{UpscaleTargetBits: 32})
	if c.Status != Warn || c.Hint != transcode.FLAC32EncodeFix {
		t.Fatalf("a missing sox: %+v", c)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	probeFLAC32 = func(context.Context) (bool, error) { return false, context.Canceled }
	c = checkFLAC32Bit(ctx, Deps{UpscaleTargetBits: 32})
	if c.Status != OK || c.Summary != "32-bit FLAC probe was interrupted" {
		t.Fatalf("a cancelled probe: %+v", c)
	}
}

// TestCheckFLAC32BitOnTheHost runs the real probe against this machine's sox
// and logs the line an operator would read. It skips where sox is not
// installed, and it does not hard-code whether this sox can write 32-bit
// FLAC: the status has to match what the probe itself read back.
func TestCheckFLAC32BitOnTheHost(t *testing.T) {
	if _, err := exec.LookPath("sox"); err != nil {
		t.Skip("sox is not on PATH")
	}
	if c := checkFLAC32Bit(context.Background(), Deps{UpscaleTargetBits: 24}); c.Status != OK {
		t.Fatalf("target 24: %+v, want ok with no probe failure", c)
	}
	can, err := transcode.CanEncodeFLAC32(context.Background())
	c := checkFLAC32Bit(context.Background(), Deps{UpscaleTargetBits: 32})
	t.Logf("%s: %s — %s", c.Status, c.Summary, c.Hint)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if can {
		if c.Status != OK || c.Summary != "sox encodes 32-bit FLAC" {
			t.Fatalf("host encodes 32-bit FLAC, doctor said %+v", c)
		}
		return
	}
	if c.Status != Warn || c.Summary != "sox cannot encode 32-bit FLAC" || c.Hint != transcode.FLAC32EncodeFix {
		t.Fatalf("host cannot encode 32-bit FLAC, doctor said %+v", c)
	}
}
