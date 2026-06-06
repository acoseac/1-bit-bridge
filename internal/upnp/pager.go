package upnp

// DefaultPageSize is the RequestedCount per Browse page. Servers may
// return fewer; BrowseAll advances by the server's NumberReturned, not
// this value. 200 keeps each SOAP round-trip's DIDL payload modest while
// minimizing round-trips on large folders.
const DefaultPageSize = 200

// MaxBrowseAllItems is a hard backstop on items accumulated in a single
// BrowseAll, defending against a server that reports a bogus (or stuck)
// TotalMatches and never stops returning rows.
const MaxBrowseAllItems = 100_000

// NextStartingIndex returns the StartingIndex for the next Browse page
// and whether iteration should continue.
//
// Terminates on an EMPTY page (numberReturned <= 0 — treated as EOF;
// MiniDLNA can report an inaccurate TotalMatches while its DB builds) OR
// when current+numberReturned reaches a real (positive) TotalMatches.
// A non-positive or zero TotalMatches ("unknown") keeps paginating until
// the next empty page.
func NextStartingIndex(current, numberReturned, totalMatches int) (int, bool) {
	if numberReturned <= 0 {
		return 0, false
	}
	next := current + numberReturned
	if totalMatches > 0 && next >= totalMatches {
		return 0, false
	}
	return next, true
}
