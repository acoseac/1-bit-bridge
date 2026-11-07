// Package season says when the Christmas smart mix is served, which season
// an instant belongs to, the floor a mix must clear to be served (floor.go),
// and what each request is sent of it (pick.go).
//
// It is a leaf that both layers import: the API leaves the family out of a
// response outside the window, and picks each request's view of it,
// leaving it out below the floor; the console marks it out of season
// there; the regenerator seeds the family's order with the season's year;
// and the engine stores each format through KeepSpread and asks the same
// pick of the four views a client can declare. The API deliberately does not
// import the engine (internal/smartplaylist; see timeOfDayKind in
// internal/api), so neither the window, the floor nor the pick can live in
// the engine.
//
// Every answer is taken in UTC, which each function reads from the instant
// itself, so a caller's location cannot move an edge.
package season

import "time"

// InWindow reports whether t falls in the Christmas window: 1 December
// 00:00:00 through 5 January 23:59:59.999999999, UTC, both days included.
func InWindow(t time.Time) bool {
	u := t.UTC()
	switch u.Month() {
	case time.December:
		return true
	case time.January:
		return u.Day() <= 5
	default:
		return false
	}
}

// Year names the season t belongs to by the year of the December that opens
// its window. From 1 December through 5 January it is that December's year,
// so 31 December and 1 January name one season and a mix keeps its order
// across New Year. From 6 January on it is the coming December's: a cache
// built before the window opens already holds the order the window opens
// with, so the first regeneration after 1 December changes nothing.
//
// It is never the calendar year alone: that would reshuffle every mix at
// midnight on 1 January, under someone listening to it.
func Year(t time.Time) int {
	u := t.UTC()
	if u.Month() == time.January && u.Day() <= 5 {
		return u.Year() - 1
	}
	return u.Year()
}
