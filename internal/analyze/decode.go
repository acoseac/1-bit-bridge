package analyze

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// decodeArgs builds the sox argv that decodes any supported source to
// headerless mono 48 kHz little-endian float32 PCM on stdout.
//
//   - 48 kHz mono is the uniform analysis target (load-bearing for the
//     Phase-2 BS.1770 loudness path; fine for peaks). sox inserts the
//     rate + channel-mix effects automatically when the source differs.
//   - `-L` forces little-endian so the Go reader is deterministic across
//     architectures (ARM/Pi vs Intel).
//   - No `-G` (gain-guard): float output can't clip, and a guard pass
//     would shrink the displayed envelope.
func decodeArgs(srcAbs string) []string {
	return []string{
		srcAbs,
		"-t", "raw", "-e", "float", "-b", "32", "-L",
		"-c", "1", "-r", "48000",
		"-",
	}
}

// decodePCM runs sox to decode srcAbs, streaming each float32 sample to
// onSample, and returns the total sample count. PCM is processed in
// blocks (never buffered whole), so memory stays flat for long tracks.
//
// **Process reaping**: the sox process is killed + reaped on any early
// return / panic via the processReleased guard — an undrained stdout
// pipe would otherwise deadlock sox on a full write buffer and leak the
// process (a worker-slot leak in the pool). A non-zero sox exit
// (truncated / corrupt file) returns an error with redacted stderr so
// the caller commits nothing.
func decodePCM(ctx context.Context, srcAbs string, onSample func(float32)) (totalSamples int64, err error) {
	cmd := exec.CommandContext(ctx, "sox", decodeArgs(srcAbs)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start sox: %w", err)
	}
	processReleased := false
	defer func() {
		if !processReleased {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	if serr := streamFloat32LE(stdout, func(s float32) {
		onSample(s)
		totalSamples++
	}); serr != nil {
		return totalSamples, fmt.Errorf("read pcm: %w", serr)
	}
	if werr := cmd.Wait(); werr != nil {
		processReleased = true
		return totalSamples, fmt.Errorf("sox: %w (stderr: %s)",
			werr, redactSoxErr(strings.TrimSpace(stderr.String()), srcAbs))
	}
	processReleased = true
	return totalSamples, nil
}

// streamFloat32LE reads little-endian float32 samples from r and calls
// fn for each, carrying a frame split across a read boundary via a
// 4-byte buffer. A trailing partial frame (1–3 bytes at EOF) is ignored.
func streamFloat32LE(r io.Reader, fn func(float32)) error {
	buf := make([]byte, 64*1024)
	var carry [4]byte
	rem := 0
	for {
		n, err := r.Read(buf)
		i := 0
		// Complete a carried partial frame from the front of this read.
		if rem > 0 && n > 0 {
			need := 4 - rem
			if n >= need {
				copy(carry[rem:], buf[:need])
				fn(f32LE(carry[:]))
				i = need
				rem = 0
			} else {
				copy(carry[rem:], buf[:n])
				rem += n
				i = n
			}
		}
		for ; i+4 <= n; i += 4 {
			fn(f32LE(buf[i : i+4]))
		}
		if i < n {
			rem = n - i
			copy(carry[:rem], buf[i:n])
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func f32LE(b []byte) float32 {
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}

// redactSoxErr strips the absolute source path from sox stderr (the
// bridge privacy contract bans surfacing absolute library paths),
// trims sox's leading prefixes, and caps the length. The source path
// is the only host-identifying token a decode-to-stdout invocation
// leaks (there's no output file path). Twin of
// internal/transcode.redactSoxErr — keep in lockstep.
func redactSoxErr(s, srcAbs string) string {
	if srcAbs != "" {
		s = strings.ReplaceAll(s, srcAbs, filepath.Base(srcAbs))
	}
	for _, prefix := range []string{"sox FAIL ", "sox WARN ", "sox: ", "exit status "} {
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
			break
		}
	}
	const maxErrBytes = 4096
	if len(s) > maxErrBytes {
		s = trimPartialTrailingRune(s[:maxErrBytes]) + "…(truncated)"
	}
	return s
}

// trimPartialTrailingRune removes at most utf8.UTFMax-1 trailing bytes
// when a byte-slice cut split a multi-byte rune, in O(1). Interior
// invalid bytes are left as-is (encoded as U+FFFD downstream). Twin of
// internal/transcode.trimPartialTrailingRune.
func trimPartialTrailingRune(s string) string {
	for i := 0; i < utf8.UTFMax-1 && len(s) > 0; i++ {
		r, size := utf8.DecodeLastRuneInString(s)
		if r != utf8.RuneError || size != 1 {
			return s
		}
		s = s[:len(s)-1]
	}
	return s
}
