package manifest

import "regexp"

// mbidPattern is a MusicBrainz identifier's shape: a UUID, its hex digits in
// either case, anchored at both ends.
var mbidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsValidMBID reports whether s has a MusicBrainz identifier's shape. It is
// the enricher's test as well (enrich.isValidMBID answers through it): the
// enricher drops a file's release id that fails it and stores what its search
// finds, and the acoustic fallback stores the fingerprint's recording id over
// one that fails it, while the version-stale merge (mergePostScanFields) reads
// a file's id that fails it as no id. One test, so the merge keeps what the
// enricher wrote in its place, and a bump does not put the file's value back
// (backlog B188).
func IsValidMBID(s string) bool { return mbidPattern.MatchString(s) }
