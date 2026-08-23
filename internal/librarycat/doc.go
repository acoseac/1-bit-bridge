// Package librarycat folds a streamed per-track projection into the
// Albums / Artists / Genres / Composers catalog the admin web player
// browses.
//
// # Why compute-on-read rather than columns
//
// The store is path/folder-shaped: every field a player groups by
// (album, albumArtist, artist, genre, composer, year, title, track,
// disc) lives inside the tags_json BLOB with no column and no index.
// The alternative to this package is a migration adding indexed
// columns, and it buys less than it looks: genres and composers are
// MULTI-VALUE axes with normalisation, inversion and dedup rules that
// a SQL column cannot express, so the Go fold is required either way.
// Measured on a 15,370-row library, the whole-table json_extract walk
// plus this fold is a fraction of a second — cheap enough to cache one
// snapshot and rebuild it when the library changes.
//
// # The contract
//
// Album identity is dupes.AlbumIDOf(dupes.Resolve(row)) — the same
// value the iOS client computes — so the browser's album partition is
// identical to the phone's BY CONSTRUCTION rather than by coincidence.
// Everything else here (genre and composer segmentation, sort keys,
// alphabet buckets, quality tiers) mirrors a named Swift twin, and the
// expected values in the tests are lifted from those twins' own
// docstrings. See genre.go's header for the do-not-unify rule this
// package inherits: a fold that is BETTER than the client's is wrong,
// because the contract is sameness.
//
// Exactly one divergence is deliberate, and it is measured rather than
// tasteful — see Classify's docblock on absent bit depth.
//
// # Determinism
//
// A Catalog is rebuilt from scratch whenever the library changes, so an
// ordering that depended on map iteration or row arrival would visibly
// reshuffle a grid the user is looking at. Every tie-break here
// resolves on values, and TestFoldIsInputOrderIndependent asserts the
// whole property by shuffling the input and requiring DeepEqual.
//
// # Memory
//
// A snapshot holds the per-album rollups plus one path string per
// track — deliberately not hydrated rows, which is what keeps a large
// library affordable on the Pi-class hosts this project supports.
// Album detail re-queries by path so variant, analysis and favourite
// state stay fresh without a rebuild. Estimated from measured string
// sizes: roughly 3.5 MB at 15k tracks, 23 MB at 100k. Callers should
// refuse to build past a few hundred thousand served tracks rather
// than risk the OOM.
//
// A Catalog is immutable once Build returns; publish it through an
// atomic.Pointer so readers never block on a rebuild and never observe
// a half-built snapshot.
package librarycat
