// Package listeningverify checks a DAC qualification's listening assets, the
// ones scripts/qualification/listening-assets generates, before an owner
// listens to them (ops/b470-native-qualification.md, "Preparing listening
// assets"). Its tests are all it holds.
//
// TestListeningAssets reads a generated directory (-listening-assets): every
// manifest through the qualification package's own loader and asset check
// and against manifest-v1.schema.json; every DSD fixture through the
// bridge's decoder and through a DSF and DSDIFF reader of its own, which
// must agree bit for bit; and every decoded fixture against its WAV
// reference: each channel's tones, their frequencies and levels block by
// block, the silences, the channel order, the frame counts, the noise from
// 20 to 100 kHz in silence and in tones, lines standing out of that noise,
// the bitstream's runs and density, and the joins the session's runs make.
// ffmpeg's dsd_* decoders, when ffmpeg is on PATH, decode each fixture a
// third way. Every run of runs.json is held to the fixture-list rules
// bridge output dsd-test applies before it opens anything, and no file
// beside the assets, nor the tar's headers, may name an absolute path, the
// user or the host.
//
// The other tests need no assets. By default they decode a quarter-second
// stream and catch it once its channels are swapped, and hold the metadata
// scan, the schema and the run rules to cases of their own. With
// -listening-assets the costly ones run too, beside the directory's check:
// they build a two-second set in the test's own directory, pass it, and
// require the check to refuse it once its channels are swapped, its bit
// order is wrong or a step ends it.
package listeningverify
