package backup

import "context"

// publishOver writes found over the record of the snapshot in dir as a
// verdict's write does over the record its manifest holds now, its step's
// give-up given and its hold's held (publication, backlog B653's sixth
// round): the one name the tests of a write's removals reach it by.
func publishOver(dir string, found Verification, given context.Context, held func() bool) (Verification, bool, error) {
	return publishVerdict(dir, found, nil, false, publication{given: given, held: held})
}
