package transcode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParseSoxFileFormats(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantKnown bool
		wantFLAC  bool
		// mustExclude is a token from the NEXT section that must not leak
		// into the format set (proves the block terminator works).
		mustExclude string
	}{
		{
			name:        "single line with flac then device drivers",
			in:          "sox: SoX v14.4.2\n\nAUDIO FILE FORMATS: 8svx aif au dat flac wav wv\nAUDIO DEVICE DRIVERS: coreaudio\nEFFECTS: allpass\n",
			wantKnown:   true,
			wantFLAC:    true,
			mustExclude: "coreaudio",
		},
		{
			name:        "wrapped flush-left continuation",
			in:          "AUDIO FILE FORMATS: 8svx aif au\ndat flac wav\nAUDIO DEVICE DRIVERS: coreaudio\n",
			wantKnown:   true,
			wantFLAC:    true,
			mustExclude: "coreaudio",
		},
		{
			name:        "wrapped indented continuation",
			in:          "AUDIO FILE FORMATS: 8svx aif au\n   dat flac wav\nEFFECTS: allpass band\n",
			wantKnown:   true,
			wantFLAC:    true,
			mustExclude: "allpass",
		},
		{
			name:      "block present but no flac",
			in:        "AUDIO FILE FORMATS: 8svx aif au wav wv\nAUDIO DEVICE DRIVERS: coreaudio\n",
			wantKnown: true,
			wantFLAC:  false,
		},
		{
			name:      "no format block at all",
			in:        "sox: SoX v14.4.2\nUsage summary: ...\n",
			wantKnown: false,
			wantFLAC:  false,
		},
		{
			name:      "formats are the last section (no trailing header)",
			in:        "AUDIO FILE FORMATS: 8svx aif flac wav\n",
			wantKnown: true,
			wantFLAC:  true,
		},
		{
			name:      "case-insensitive header still matches",
			in:        "Audio File Formats: aif flac wav\n",
			wantKnown: true,
			wantFLAC:  true,
		},
		{
			name:      "localized (non-English) header -> unknown, conservative fallback",
			in:        "AUDIODATEIFORMATE: aif flac wav\n",
			wantKnown: false,
			wantFLAC:  false,
		},
		{
			name:      "CRLF line endings",
			in:        "AUDIO FILE FORMATS: aif flac wav\r\nAUDIO DEVICE DRIVERS: coreaudio\r\n",
			wantKnown: true,
			wantFLAC:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			formats, known := parseSoxFileFormats(tc.in)
			if known != tc.wantKnown {
				t.Fatalf("FormatsKnown = %v, want %v (formats=%v)", known, tc.wantKnown, formats)
			}
			hasFLAC := false
			for _, f := range formats {
				if f == "flac" {
					hasFLAC = true
				}
				if tc.mustExclude != "" && f == tc.mustExclude {
					t.Errorf("token %q from the next section leaked into the format set: %v", tc.mustExclude, formats)
				}
			}
			if hasFLAC != tc.wantFLAC {
				t.Errorf("hasFLAC = %v, want %v (formats=%v)", hasFLAC, tc.wantFLAC, formats)
			}
		})
	}
}

func TestParseSoxVersion(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"sox:      SoX v14.4.2\n\nUsage summary: ...", "v14.4.2"},
		{"sox: SoX v14.4.2", "v14.4.2"},
		{"no banner here", ""},
		// Homebrew HEAD prints a bare "SoX v" with no number — not usable.
		{"sox:      SoX v\n\nUsage summary: ...", ""},
		{"sox: SoX v", ""},
	}
	for _, tc := range cases {
		if got := parseSoxVersion(tc.in); got != tc.want {
			t.Errorf("parseSoxVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// withFakeSox stubs the ProbeSox seams so the full flow runs without a real
// sox: soxLookPath returns a fake path, soxProbeCommand emits `helpOut` via
// /bin/sh (exiting `exitCode` — sox --help legitimately exits non-zero, so
// the probe must tolerate it). Restored via t.Cleanup.
func withFakeSox(t *testing.T, helpOut string, exitCode int) {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("test uses /bin/sh which isn't available on this platform")
	}
	origLook, origCmd := soxLookPath, soxProbeCommand
	t.Cleanup(func() { soxLookPath, soxProbeCommand = origLook, origCmd })
	soxLookPath = func(string) (string, error) { return "/fake/sox", nil }
	soxProbeCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		script := "printf '%s' '" + strings.ReplaceAll(helpOut, "'", "'\\''") + "'"
		if exitCode != 0 {
			script += "; exit " + strings.TrimSpace(itoaSox(exitCode))
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", script)
	}
}

func itoaSox(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestProbeSox_SeamFLACPresent(t *testing.T) {
	// Non-zero exit (1) + output present: must still parse cleanly.
	withFakeSox(t, "sox: SoX v14.4.2\nAUDIO FILE FORMATS: aif flac wav\nEFFECTS: allpass\n", 1)
	info, err := ProbeSox(context.Background())
	if err != nil {
		t.Fatalf("ProbeSox: %v", err)
	}
	if !info.FormatsKnown || !info.HasFLAC {
		t.Errorf("want FormatsKnown && HasFLAC, got %+v", info)
	}
	if info.Version != "v14.4.2" {
		t.Errorf("Version = %q, want v14.4.2", info.Version)
	}
}

func TestProbeSox_SeamFLACAbsent(t *testing.T) {
	withFakeSox(t, "sox: SoX v14.4.2\nAUDIO FILE FORMATS: aif wav wv\nEFFECTS: allpass\n", 0)
	info, err := ProbeSox(context.Background())
	if err != nil {
		t.Fatalf("ProbeSox: %v", err)
	}
	if !info.FormatsKnown {
		t.Fatalf("want FormatsKnown, got %+v", info)
	}
	if info.HasFLAC {
		t.Errorf("want HasFLAC=false, got %+v", info)
	}
}

func TestProbeSox_SeamMissing(t *testing.T) {
	origLook := soxLookPath
	t.Cleanup(func() { soxLookPath = origLook })
	soxLookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	_, err := ProbeSox(context.Background())
	if !errors.Is(err, ErrSoxMissing) {
		t.Errorf("want ErrSoxMissing, got %v", err)
	}
}

// TestProbeSox_RealSoxIfPresent is a smoke test against the host's real sox
// — on a dev machine with `brew install sox` it confirms FLAC is detected
// end-to-end. Skipped where sox isn't installed (e.g. minimal CI).
func TestProbeSox_RealSoxIfPresent(t *testing.T) {
	if _, err := exec.LookPath("sox"); err != nil {
		t.Skip("real sox not on PATH")
	}
	info, err := ProbeSox(context.Background())
	if err != nil {
		t.Fatalf("ProbeSox against real sox: %v", err)
	}
	if !info.FormatsKnown {
		t.Skip("host sox --help didn't expose a parseable AUDIO FILE FORMATS block")
	}
	if !info.HasFLAC {
		t.Logf("host sox lacks FLAC support (formats: %v) — feature would degrade", info.Formats)
	}
}
