// Per-field apply semantics for PATCH /api/settings.
//
// The handler used to answer with one blanket boolean:
//
//	{"restartRequired": true}
//
// That is imprecise per FIELD (a bridge cannot say which of the six
// things you just changed is the one still pending) and imprecise per
// REQUEST (a PATCH carrying a library rename, which applies instantly,
// plus a scan-interval change, which does not, returns a single `true`
// and the caller cannot tell which half it refers to).
//
// The blanket answer is survivable for an operator standing in front of
// the console — they can bounce the bridge and stop wondering. It is not
// survivable for the hosted product, where settings arrive from a control
// plane and end users have no console at all: "restart required" is not a
// UX anyone downstream can act on unless it names what is waiting.
//
// So each field that the patch supplied gets its own answer, and the
// legacy boolean becomes a derived rollup over them
// (TestRestartRequiredIsDerivedFromTheFieldReport pins that it stays in
// lockstep, in both directions).
//
// # Three statuses, deliberately not four
//
// An earlier draft carried a `partial` status for the two fields whose
// consumers were split — half a cheap struct field that could be read
// live, half a genuine boot-time lifecycle. It was dropped, because
// reporting `restart` for a field whose cheap half HAD hot-applied means
// /v1/health starts advertising a capability in the same breath that this
// response calls the change pending. The rule that replaced it is
// structural rather than cosmetic:
//
//	NEVER SPLIT A FIELD'S HALVES. Either every consumer of a config
//	field reads it live, or every consumer takes it at boot.
//
// `optimizeEnabled` resolves that way by making the auto-optimize sweeper
// unconditional (PR 3, the same move fingerprint makes); `atlasEnabled`
// resolves the other way, staying wholly boot-bound alongside its
// file-backed harvest state store. With the rule applied there is no
// remaining field a fourth status would describe.
//
// # When `reason` is populated
//
// Exactly when the OUTCOME depended on this bridge's runtime state rather
// than on a static property of the field. Two shapes qualify:
//
//   - The status itself could have been different elsewhere.
//     `autoOptimizeEnabled` answering `restart` because no sweeper is
//     wired here; `tailscaleMode` naming which transition this was.
//   - The status is `live` and the change genuinely applied, but the
//     feature is inert for a reason the operator can act on.
//     `fingerprintEnabled` on a host without fpcalc: a restart would
//     change nothing, so `restart` would be a lie — and silence would
//     have the operator move the switch, read "Saved.", and never learn
//     that nothing will run.
//
// What does NOT qualify: `listenAddress` answering `restart` because
// listeners bind once. True on every bridge, and spelling it out for all
// twenty restart-bound fields would be twenty near-identical strings the
// reader learns to skip — at which point the ones that carry information
// are skipped along with them.
//
// # Why the value is an object rather than a bare string
//
// `{"scanIntervalSec": "restart"}` is one field shorter and one breaking
// change away from being wrong. The honesty rule below IS a
// reason-generating rule — the conditional cases each want to say why —
// so the shape that needs `reason` is the shape we already have. Adding a
// key to an object value is additive; widening a string into an object is
// not, and this is a public API on an open-source self-hosted binary
// whose script consumers are unknowable.
package admin

import "sort"

// applyStatus is what happened to one field named in a settings PATCH.
type applyStatus string

const (
	// applyLive: the new value is in effect now. No further action.
	applyLive applyStatus = "live"
	// applyRestart: the new value is persisted to bridge.yaml but its
	// consumer captured the old one at boot. A supervised restart is
	// required for it to take effect.
	applyRestart applyStatus = "restart"
	// applyUnchanged: the field was supplied and already held that
	// value, so nothing was written. Reported rather than omitted
	// because a control plane pushing a full desired-state document
	// sends mostly-unchanged fields, and "I sent it and nothing
	// happened because nothing needed to" is a different fact from
	// "I did not send it" — it is what makes an idempotency assertion
	// possible and what tells the caller whether a `restartRequired`
	// belongs to THIS request or was already pending from an earlier one.
	applyUnchanged applyStatus = "unchanged"
)

// fieldApply is one field's outcome on the wire.
type fieldApply struct {
	Status applyStatus `json:"status"`
	// Reason is present only for a status that depended on this
	// bridge's runtime state — see the package comment.
	Reason string `json:"reason,omitempty"`
}

// applyReport accumulates per-field outcomes while the settings PATCH
// runs. Keyed by the field's JSON tag on settingsPatch, which
// TestApplyReportFieldNamesAreRealPatchFields pins — a typo here would
// name a field no caller can correlate with what it sent, and nothing
// else in the codebase connects the two.
//
// A map rather than a slice so recording the same field twice is
// idempotent: RuntimeConfig.Update calls its closure once today, and a
// future retry there must not be able to produce a doubled report.
type applyReport map[string]fieldApply

func (a applyReport) set(field string, st applyStatus, reason string) {
	if a == nil {
		return
	}
	a[field] = fieldApply{Status: st, Reason: reason}
}

// live records that the field took effect immediately.
func (a applyReport) live(field string) { a.set(field, applyLive, "") }

// unchanged records that the field was supplied at its current value.
func (a applyReport) unchanged(field string) { a.set(field, applyUnchanged, "") }

// restart records that the field is persisted but needs a bounce.
func (a applyReport) restart(field string) { a.set(field, applyRestart, "") }

// restartBecause records a restart whose necessity is a property of THIS
// bridge's wiring rather than of the field — the honesty rule the
// auto-optimize toggle established: when a flip cannot take effect,
// saying so with the reason is what gives the operator something to act
// on, where a silent success would have them flip the switch, watch
// nothing happen, and have nothing to go on.
func (a applyReport) restartBecause(field, reason string) {
	a.set(field, applyRestart, reason)
}

// changed records live-or-restart in one call, for the many sites whose
// only variable is whether the consumer reads the holder or captured a
// boot snapshot.
func (a applyReport) changed(field string, hot bool) {
	if hot {
		a.live(field)
		return
	}
	a.restart(field)
}

// needsRestart is the legacy `restartRequired` boolean, derived rather
// than tracked alongside. Deriving it is what stops the two from drifting:
// a future field that records `restart` but forgets to set a parallel
// flag would otherwise report success and change nothing until a bounce
// nobody was told to perform.
func (a applyReport) needsRestart() bool {
	for _, f := range a {
		if f.Status == applyRestart {
			return true
		}
	}
	return false
}

// fields returns the sorted field names, for tests and log lines that
// want a stable order. The JSON encoding sorts map keys on its own.
func (a applyReport) fields() []string {
	out := make([]string, 0, len(a))
	for k := range a {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// fingerprintDegradedMessage renders a bounded degraded-reason key as an
// operator-facing sentence for the settings response.
//
// Bounded on purpose — the keys come from the toolchain probe, never from
// an error string, so this switch cannot grow an unbounded set of
// messages (the same rule markSkipped's reason keys follow).
func fingerprintDegradedMessage(reason string) string {
	switch reason {
	case "fpcalc_missing":
		return "saved, but fpcalc is not installed on this bridge, so no fingerprinting will run"
	case "no_api_key":
		return "saved, but no AcoustID API key is configured, so no fingerprinting will run"
	default:
		return "saved, but the fingerprint toolchain is unavailable, so no fingerprinting will run"
	}
}
