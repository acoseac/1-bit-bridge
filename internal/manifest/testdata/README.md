# Manifest test fixtures

The `explicit-*.m4a` files in `m4a/` are the iOS app's
`ExplicitAdvisoryFixtures`, copied byte for byte. That copy is canonical.
Do not regenerate them from the AVTagFixtures recipe.

`explicit-verdict-cases.tsv` is the same file the iOS app loads for
`ExplicitContent.verdict`. That copy is canonical too. The columns are
tab-separated: kind, input (a JSON string), expected.

`playlist-parse-cases.tsv` is the iOS app's
`Tests/com_acoseac_dsdplayerTests/Fixtures/Playlists/playlist-parse-cases.tsv`
(B430), copied byte for byte from iOS `a9a16365`. That copy is canonical:
change it there and copy it here, never edit this one. Its header states the
columns, the escapes and the rules; `TestPlaylistParseCaseTable` runs every
row through the indexer's parser and path rule. `.gitattributes` pins it to
LF, because a CRLF checkout would change every row's bytes.

The advisory fixtures were built as 0.05 s of stereo 44.1 kHz 16-bit
silence, converted with `afconvert` to ALAC in an m4a, then tagged with
mutagen 1.47: `©nam` "One", `©ART` "Performer A", `©alb` "The Record".
`rtng` is the integer atom. `ITUNESADVISORY` and `EXPLICIT` are freeform
`----` atoms; one of them uses a mean other than `com.apple.iTunes`.
