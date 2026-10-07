# Manifest test fixtures

The `explicit-*.m4a` files in `m4a/` are the iOS app's
`ExplicitAdvisoryFixtures`, copied byte for byte. That copy is canonical.
Do not regenerate them from the AVTagFixtures recipe.

`explicit-verdict-cases.tsv` is the same file the iOS app loads for
`ExplicitContent.verdict`. That copy is canonical too. The columns are
tab-separated: kind, input (a JSON string), expected.

The advisory fixtures were built as 0.05 s of stereo 44.1 kHz 16-bit
silence, converted with `afconvert` to ALAC in an m4a, then tagged with
mutagen 1.47: `©nam` "One", `©ART` "Performer A", `©alb` "The Record".
`rtng` is the integer atom. `ITUNESADVISORY` and `EXPLICIT` are freeform
`----` atoms; one of them uses a mean other than `com.apple.iTunes`.
