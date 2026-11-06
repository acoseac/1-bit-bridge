package main

import (
	"context"
	"time"
)

// probationSeams is a boot test's hold on the confirmation of an install of
// this version (backlog B660). Nil in production.
type probationSeams struct {
	// timing replaces production's: a zero field is production's.
	timing probationTiming
	// observe is told each thing the confirmation does, on its goroutine, as
	// it does it.
	observe func(probationEvent)
	// storeRead stands in for a round's store read (the store's
	// CountServedTracks).
	storeRead func(ctx context.Context) error
}

// probationTiming is the confirmation's clock: the window one start stays up
// for once it has answered as ready (updater.ProbationWindow), how often it
// asks before its first yes, how often after a no at the window's end, and
// one round's bound.
type probationTiming struct {
	window, ask, retry, round time.Duration
}

// probationEventKind is what a probationEvent says happened.
type probationEventKind int

const (
	// probationRound: a round answered, ready or why not.
	probationRound probationEventKind = iota + 1
	// probationWindow: the first yes, which starts the window.
	probationWindow
	// probationConfirmed: the install was stamped installed.
	probationConfirmed
	// probationNotReady: the window lapsed with no confirmation, said once
	// and recorded in the marker.
	probationNotReady
	// probationUnconfirmed: a confirmation that could not be made at this
	// ask (its lock, its write), asked again at the next.
	probationUnconfirmed
	// probationEnded: the confirmation's goroutine returned.
	probationEnded
)

// probationEvent is one thing the confirmation did.
type probationEvent struct {
	kind  probationEventKind
	ready bool
	why   string
}
