package policy

// Occurrence is what reconciliation reads of one DSD occurrence of a session
// (the design's section 3.5), each a separate input, since replanning,
// revocation and consent use them differently.
type Occurrence struct {
	// Plan is the plan the occurrence was admitted under.
	Plan Plan
	// Submitted, Written and Played are its own frames (logical bits a
	// channel for DSD, PCM frames for a converted occurrence) that the
	// producer handed its sink, that the kernel accepted from the sink, and
	// that the kernel established played: the sink's DSDPosition counts,
	// from the occurrence's first frame in its sink. A frame Submitted and
	// not Written is buffered in the sink; a Written one is the kernel's.
	Submitted, Written, Played int64
	// Audible reports that the occurrence has been heard (the engine took
	// its Audible), and stays so: a released pause is still the occurrence
	// heard, though nothing of it is the kernel's any more.
	Audible bool
	// Retiring reports that its output is playing out the tail after its
	// last frame, which today's reconciliation fences as an open output
	// (authorization.go's reconcilePermission).
	Retiring bool
	// Opened is the identity the occurrence's sink last opened, kept across
	// a pause's release, which closes the device and keeps the sink's frames
	// for a reopen: a sink that opened another identity than the new set's
	// (Set.Identity) holds the occurrence on a unit the set was not computed
	// for. Nil only for an occurrence that has never had a sink, which has
	// no identity to compare until it opens one.
	Opened *Identity
}

// Action is what reconciliation does with an occurrence.
type Action int

const (
	// Keep: its plan is still the set's at its rate, transport, format,
	// rate and timing alike; it plays on, untouched.
	Keep Action = iota + 1
	// Replan: its plan left the set and no frame of it is the kernel's yet;
	// the backend asks the engine to hand it off again (a successor not yet
	// submitted is replaced, a submitted one cut), and it plays under the
	// new set, never skipped for the change.
	Replan
	// Revoke: its plan left the set and it was heard, a frame of it is the
	// kernel's or its output retires, or its frames sit in a sink that
	// opened another identity than the set's; it stops at once.
	Revoke
)

// Verdict is reconciliation's answer for one occurrence.
type Verdict struct {
	Action Action
	// Interrupts reports that the action interrupts what is heard: a
	// revocation, which stops playback at once, or a re-plan of an
	// occurrence whose frames are already in a sink. A user's choice write
	// that would do either needs its interrupt, and without it changes
	// nothing; a safety change (the gain leaving unity, the identity
	// changing, the DAC leaving, the state no longer reading) acts without
	// asking (the design's sections 3.5 and 7.2).
	Interrupts bool
}

// Reconcile judges o against next, the set that replaces the one o was
// planned under: kept while next plays o's plan at its rate and the
// identity o's sink last opened, if it ever had one, is next's; revoked once
// o was heard, a frame of it is the kernel's or its output retires, or when
// frames of it sit in a sink that opened another identity; re-planned
// otherwise. An identity change that leaves the same tuple available
// revokes all the same, a released pause's included, whose device is closed
// and whose reopen would play its frames to the unit that took the port:
// the tuple's membership is not the test.
func Reconcile(next Set, o Occurrence) Verdict {
	valid := o.Opened == nil || o.Opened.Same(next.Identity())
	reached := o.Audible || o.Written > 0 || o.Played > 0 || o.Retiring
	switch {
	case valid && o.Plan.Same(next.At(o.Plan.BitRate).Plan):
		return Verdict{Action: Keep}
	case reached || !valid && o.Submitted > 0:
		return Verdict{Action: Revoke, Interrupts: true}
	}
	return Verdict{Action: Replan, Interrupts: o.Submitted > 0}
}
