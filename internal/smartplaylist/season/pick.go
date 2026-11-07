package season

import "github.com/acoseac/1-bit-bridge/internal/formatgate"

// MaxTracks is the most tracks a Christmas mix holds: in one request's view
// (Pick), and of each format the regenerator stores (KeepSpread).
const MaxTracks = 100

// Pick is the Christmas family's items for one request's view, picked at the
// response from what the regenerator stored (round 5 of backlog B489, two
// reviewers: one selection made at generation cannot suit four views, and its
// cap could erase the spread that qualified a view's pool). Of items, the
// ones view takes, those every client is served first, in their stored
// order, then the WavPack and the Monkey's Audio ones, the two formats in
// turn while both last (formatgate.Of of each path); before the cap, the
// first item of each of the first MinSpread distinct albums among them, or
// failing that of the first MinSpread distinct artists, moves to the front,
// so the cap never cuts away a spread those items hold; then the first
// MaxTracks. ok is the floor asked of the result (MeetsFloor): a view whose
// pick does not clear it is not sent the family. The regenerator asks it of
// the four views to decide whether a family is stored at all.
func Pick[T any](items []T, path, album, artist func(T) string, view formatgate.Set) (picked []T, ok bool) {
	var served, wavpack, ape []T
	for _, it := range items {
		p := path(it)
		if !view.Takes(p) {
			continue
		}
		switch formatgate.Of(p) {
		case formatgate.WavPack:
			wavpack = append(wavpack, it)
		case formatgate.APE:
			ape = append(ape, it)
		default:
			served = append(served, it)
		}
	}
	ordered := make([]T, 0, len(served)+len(wavpack)+len(ape))
	ordered = append(ordered, served...)
	for i := 0; i < len(wavpack) || i < len(ape); i++ {
		if i < len(wavpack) {
			ordered = append(ordered, wavpack[i])
		}
		if i < len(ape) {
			ordered = append(ordered, ape[i])
		}
	}
	keep := firstOfDistinct(ordered, album)
	if len(keep) < MinSpread {
		if keep = firstOfDistinct(ordered, artist); len(keep) < MinSpread {
			keep = nil
		}
	}
	picked = moveToFront(ordered, keep)
	picked = picked[:min(len(picked), MaxTracks)]
	return picked, MeetsFloor(picked, album, artist)
}

// KeepSpread is one format's tracks as the regenerator stores them, from
// items in the order the family takes them in: the first item of each of the
// first MinSpread distinct albums and of each of the first MinSpread distinct
// artists moved to the front, then the first MaxTracks. Both spreads, where
// Pick takes one: a format that cannot reach the floor alone (two albums, one
// artist) may complete another's in a view that declares both, so what Pick
// needs of it must survive the cap whichever spread that view's floor is
// reached by.
func KeepSpread[T any](items []T, album, artist func(T) string) []T {
	keep := mergeIndexes(firstOfDistinct(items, album), firstOfDistinct(items, artist))
	out := moveToFront(items, keep)
	return out[:min(len(out), MaxTracks)]
}

// firstOfDistinct is the index of the first item of each of the first
// MinSpread distinct values key gives in items, named as MeetsFloor names
// them (spreadKey; an empty one names nothing), in ascending order.
func firstOfDistinct[T any](items []T, key func(T) string) []int {
	seen := map[string]bool{}
	var out []int
	for i, it := range items {
		k := spreadKey(key(it))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, i)
		if len(out) == MinSpread {
			break
		}
	}
	return out
}

// mergeIndexes is the ascending union of two ascending index lists.
func mergeIndexes(a, b []int) []int {
	out := make([]int, 0, len(a)+len(b))
	for len(a) > 0 || len(b) > 0 {
		switch {
		case len(b) == 0 || (len(a) > 0 && a[0] < b[0]):
			out, a = append(out, a[0]), a[1:]
		case len(a) == 0 || b[0] < a[0]:
			out, b = append(out, b[0]), b[1:]
		default:
			out, a, b = append(out, a[0]), a[1:], b[1:]
		}
	}
	return out
}

// moveToFront is items with those at keep (ascending indexes) first, in
// their order, and the rest after, in theirs. Pick and KeepSpread both move
// what keeps the floor through it, ahead of their cap.
func moveToFront[T any](items []T, keep []int) []T {
	out := make([]T, 0, len(items))
	kept := make(map[int]bool, len(keep))
	for _, i := range keep {
		kept[i] = true
		out = append(out, items[i])
	}
	for i, it := range items {
		if !kept[i] {
			out = append(out, it)
		}
	}
	return out
}
