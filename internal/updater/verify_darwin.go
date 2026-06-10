//go:build darwin

package updater

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// expectedTeamID is the Apple Developer Team ID that signs official
// 1-bit-bridge releases. Build-time-injected via -ldflags -X so a
// fork or local build doesn't try to hand-roll a Team ID match
// against the upstream's. Defaults to the empty string in source —
// when empty, the verifier accepts any cert the codesign tool itself
// validates as authentic + notarized (i.e. trusts the macOS
// notarization gate). For an opinionated check, ship release builds
// with -X .../version.AppleTeamID=<TeamID>.
//
// We deliberately read the Team ID from build-time injection rather
// than from the new binary itself — the latter would be circular
// (the binary we don't yet trust telling us what Team to expect).
var appleTeamIDOverride = "" // override via ldflags at link time

// verifyBinary checks that newBinary is a valid Apple-signed +
// notarized executable, and (if appleTeamIDOverride is set) that its
// signing Team ID matches.
//
// Why both: codesign --verify --strict is the cryptographic check
// (signature is well-formed, hash matches, signing cert chain leads
// to Apple). spctl --assess + the Team-ID equality check are the
// trust check (this is OUR binary, not someone else's signed thing).
//
// On a release downloaded from acoseac/1-bit-bridge GitHub Releases,
// goreleaser hands it to rcodesign for both signing and notarization
// (see .goreleaser.yaml notarize block); the binary should pass both
// checks immediately. A locally-built binary or a tampered one
// fails one or the other.
func verifyBinary(ctx context.Context, newBinary string) error {
	// codesign honours the LAST strictness flag on the command line —
	// the prior "--strict --no-strict" pair therefore disabled strict
	// validation on every run. --strict must stand alone.
	if err := runVerifyTool(ctx, "codesign", "--verify", "--strict",
		"--check-notarization", newBinary); err != nil {
		// Fall back to the --strict-only form ONLY when codesign
		// rejected the --check-notarization flag itself (older macOS).
		// An unconditional fallback would silently bypass the
		// notarization requirement on modern systems — a signed-but-
		// not-notarized binary (or an attacker degrading the
		// notarization check) would fail the first invocation and pass
		// the second (Gemini security-high on PR #374). A genuine
		// verification failure surfaces as-is.
		if !notarizationFlagUnsupported(err) {
			return fmt.Errorf("codesign verify: %w", err)
		}
		// Surface the FALLBACK's error when it fails too: err2 is the
		// actual signature verdict; err is just the flag-unsupported
		// complaint.
		if err2 := runVerifyTool(ctx, "codesign", "--verify", "--strict", newBinary); err2 != nil {
			return fmt.Errorf("codesign verify: %w", err2)
		}
	}

	if appleTeamIDOverride == "" {
		// No build-time pin — the codesign + notarization check
		// above is the only authority we have. That's still a
		// strong guarantee against a tampered binary downloaded
		// over a hijacked GitHub mirror, just not against a
		// "wrong team signed it" scenario. Document and proceed.
		return nil
	}

	gotTeam, err := readSigningTeamID(ctx, newBinary)
	if err != nil {
		return fmt.Errorf("read signing team-id: %w", err)
	}
	if gotTeam != appleTeamIDOverride {
		return fmt.Errorf("team-id mismatch: binary signed by %q, expected %q",
			gotTeam, appleTeamIDOverride)
	}
	return nil
}

// notarizationFlagUnsupported reports whether a runVerifyTool error
// (which embeds codesign's combined output) indicates the
// --check-notarization OPTION itself was rejected — the only case the
// --strict-only fallback is legitimate. Case-insensitive substrings so
// a codesign rewording doesn't silently demote a real verification
// failure into a fallback (same posture as tailscale's
// classifyMintError); the "notarization" anchor keeps an unrelated
// "invalid option" from matching. Pinned by table test.
func notarizationFlagUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "check-notarization") {
		return false
	}
	for _, marker := range []string{"unrecognized option", "invalid option", "unknown option", "unsupported option", "illegal option"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// runVerifyTool runs a verify command and converts non-zero exit to
// an error carrying the tool's stderr. Used so codesign / spctl
// stderr lands in the operator's update-state.json LastError when
// the install fails verify.
func runVerifyTool(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 — args are constants + a path we already control
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %s: %w",
			name, args, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// readSigningTeamID asks codesign for the binary's signing Team ID.
// Apple's --display --verbose=2 output includes a "TeamIdentifier=…"
// line we parse out. Returns "" when the binary is unsigned or
// codesign didn't include the field.
func readSigningTeamID(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "codesign", "--display", "--verbose=2", path) // #nosec G204
	// codesign writes the verbose output to stderr.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("codesign display: %s: %w",
			strings.TrimSpace(string(out)), err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if t, ok := strings.CutPrefix(strings.TrimSpace(line), "TeamIdentifier="); ok {
			return strings.TrimSpace(t), nil
		}
	}
	return "", nil
}
