package alsapcm

import "sync/atomic"

// nodeGuard is GuardNodeOpens' report, nil while no guard is set.
var nodeGuard atomic.Pointer[func(string)]

// GuardNodeOpens makes every Open in this process, and so every OpenSink
// and every sink's reopen, refuse the node it would open, before anything
// is opened, and tell report the node's path, until remove is called. It
// is for a test binary's TestMain: a test that would open the box's DAC is
// refused, and the binary fails on the report. The fake kernel this
// package's own tests drive is not Open, so it is not guarded.
func GuardNodeOpens(report func(path string)) (remove func()) {
	nodeGuard.Store(&report)
	return func() { nodeGuard.CompareAndSwap(&report, nil) }
}

// guarded is the guard's refusal of ep, or nil while no guard is set.
func guarded(ep Endpoint) error {
	g := nodeGuard.Load()
	if g == nil {
		return nil
	}
	(*g)(ep.Path())
	return &Error{Reason: ReasonPermission, Op: "open", Endpoint: ep,
		Detail: "this test binary guards every open of a node: a test opens none"}
}
