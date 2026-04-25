package updater

import (
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/auth"
)

// compatGateReason is the heart of the Phase C MinClientVersion
// auto-install gate. Test directly — Install end-to-end requires
// a fake GitHub server + real archive, but the gate decision is
// pure-function and the boundary cases are what need locking in.

func TestCompatGate_NoFloorReturnsNoReason(t *testing.T) {
	tokens := []auth.Token{
		{ID: "a", Name: "iPhone Old", LastClientVersion: "0.5.0"},
	}
	for _, floor := range []string{"", "0.0.0", "  ", "  0.0.0  "} {
		if got := compatGateReason(floor, tokens); got != "" {
			t.Errorf("compatGateReason(%q): got %q, want empty (no floor)", floor, got)
		}
	}
}

func TestCompatGate_AllTokensAboveFloorAllows(t *testing.T) {
	tokens := []auth.Token{
		{ID: "a", Name: "iPhone Ars", LastClientVersion: "1.5.0"},
		{ID: "b", Name: "iPad Test", LastClientVersion: "1.5.2"},
	}
	if got := compatGateReason("1.4.0", tokens); got != "" {
		t.Errorf("compatGateReason: got %q, want empty (all above floor)", got)
	}
}

func TestCompatGate_OneBelowFloorRefusesWithDeviceName(t *testing.T) {
	tokens := []auth.Token{
		{ID: "a", Name: "iPhone Ars", LastClientVersion: "1.5.0"},
		{ID: "b", Name: "iPad Old", LastClientVersion: "1.0.0"},
	}
	got := compatGateReason("1.2.0", tokens)
	if got == "" {
		t.Fatalf("compatGateReason: expected non-empty (iPad Old below floor)")
	}
	if !strings.Contains(got, "iPad Old") {
		t.Errorf("reason must name the orphan device: %q", got)
	}
	if !strings.Contains(got, "1.0.0") {
		t.Errorf("reason must include the device's reported version: %q", got)
	}
	if !strings.Contains(got, "1.2.0") {
		t.Errorf("reason must include the floor: %q", got)
	}
}

func TestCompatGate_TokensWithoutVersionAreSkipped(t *testing.T) {
	// Older iOS builds that don't send X-Client-Version present
	// no signal to compare against. Refusing every install on
	// their behalf would mean the gate never opens until they
	// update — counterproductive. They get skipped.
	tokens := []auth.Token{
		{ID: "a", Name: "iPhone Ars", LastClientVersion: ""},
		{ID: "b", Name: "iPad", LastClientVersion: "1.5.0"},
	}
	if got := compatGateReason("1.2.0", tokens); got != "" {
		t.Errorf("token without LastClientVersion should be skipped: %q", got)
	}
}

func TestCompatGate_LeadingV_TolerantOnBothSides(t *testing.T) {
	// semver.IsValid wants a leading lowercase "v"; normalizeForSemver
	// adds it AND down-cases an upper-case prefix. Both with and
	// without leading-v should work end-to-end. The uppercase-V case
	// is the regression Gemini flagged on PR #47 — without the
	// case-insensitive normalise, "V1.0.0" was rejected as malformed
	// and the token was silently skipped.
	cases := []struct {
		floor, tokenVer string
	}{
		{"v1.5.0", "v1.0.0"},
		{"1.5.0", "1.0.0"},
		{"V1.5.0", "V1.0.0"},
		{"v1.5.0", "V1.0.0"}, // mismatched prefix style still resolves
		{"V1.5.0", "1.0.0"},
	}
	for _, tc := range cases {
		tokens := []auth.Token{{ID: "a", Name: "x", LastClientVersion: tc.tokenVer}}
		if got := compatGateReason(tc.floor, tokens); got == "" {
			t.Errorf("floor=%q tokenVer=%q: got empty, want refusal (token below)", tc.floor, tc.tokenVer)
		}
	}
}

func TestCompatGate_MalformedFloorTreatedAsNoFloor(t *testing.T) {
	// A bad release-meta.json shouldn't block every install. Log
	// the bug, then proceed.
	tokens := []auth.Token{{ID: "a", Name: "x", LastClientVersion: "1.0.0"}}
	// "12345" omitted intentionally — semver.IsValid accepts a
	// major-only version, so the gate would correctly engage. The
	// malformed cases below all fail semver.IsValid.
	for _, floor := range []string{"not-a-version", "1.x", "..", "1.2.3.4.5"} {
		if got := compatGateReason(floor, tokens); got != "" {
			t.Errorf("malformed floor %q must be treated as no floor: got %q", floor, got)
		}
	}
}

func TestCompatGate_MalformedTokenVersionIsSkipped(t *testing.T) {
	tokens := []auth.Token{
		{ID: "a", Name: "x", LastClientVersion: "garbage"},
		{ID: "b", Name: "y", LastClientVersion: "1.0.0"}, // legitimately below
	}
	got := compatGateReason("1.5.0", tokens)
	if got == "" {
		t.Fatalf("legitimate orphan should still trigger refusal: got empty")
	}
	if strings.Contains(got, "garbage") {
		t.Errorf("malformed token version must be skipped, not surfaced as orphan: %q", got)
	}
	if !strings.Contains(got, "y") {
		t.Errorf("malformed-version skip shouldn't hide the legitimate orphan: %q", got)
	}
}

func TestCompatGate_BoundaryEqualVersionAllows(t *testing.T) {
	// A token at exactly the floor is NOT below it — installation
	// proceeds. Off-by-one would be very bad here (would orphan
	// every device that just-barely-supports the new release).
	tokens := []auth.Token{{ID: "a", Name: "x", LastClientVersion: "1.5.0"}}
	if got := compatGateReason("1.5.0", tokens); got != "" {
		t.Errorf("token at exactly the floor must NOT be considered orphan: %q", got)
	}
}

// recordDeferredReason / clearDeferredReason form the operator-
// visible state surface. Confirm both directions and that the
// auto-install entry resets stale state.
func TestCompatGate_RecordAndClearDeferredReason(t *testing.T) {
	u := &Updater{
		now: func() time.Time { return time.Now() },
	}
	u.recordDeferredReason("would orphan device(s): iPad Old")
	if got := u.Status().DeferredReason; got == "" {
		t.Errorf("recordDeferredReason: Status.DeferredReason still empty")
	}
	u.clearDeferredReason()
	if got := u.Status().DeferredReason; got != "" {
		t.Errorf("clearDeferredReason: Status.DeferredReason still %q", got)
	}
}
