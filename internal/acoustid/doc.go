// Package acoustid is the acoustic-fingerprinting fallback for the enricher:
// it shells out to fpcalc (Chromaprint) to fingerprint a local audio file and
// looks the fingerprint up against the AcoustID web service.
//
// It exists for the tracks text matching provably cannot reach — files whose
// tags are junk ("CD 01" / "An Unknown Artist" / blank) or whose album simply
// has no acceptable MusicBrainz candidate. Fingerprinting identifies the
// recording from the audio itself, so it is the only remaining route for them.
//
// # What this package may conclude
//
// A fingerprint identifies AUDIO. AcoustID maps audio to a RECORDING. It does
// NOT identify a release — the reason one recording sits under many release
// groups is precisely that those releases contain the same audio, so picking
// one would be a uniform draw dressed up as a match. [Accept] therefore only
// ever yields an artist MBID and (when unambiguous) a recording MBID, and the
// [Decision] type has no field for a release. That is a structural guarantee,
// not a convention: there is nowhere to put one.
//
// Reaching a release MBID is the caller's job and goes through the enricher's
// existing text acceptance (pickBestRelease), with the fingerprint supplying
// only the artist name that was missing.
//
// # Why the gate is a pure function
//
// [Accept] takes no context and performs no I/O, so every clause is exercised
// by a table test in CI with no fpcalc binary, no network and no audio fixture
// in the repo. Its parameter type carries only the fields the gate may use —
// release groups are structurally absent so they cannot be reached by a future
// edit. Both properties are deliberate: this is the layer where a wrong answer
// becomes a wrong MBID in someone's library, so it is the layer that has to be
// cheapest to test and hardest to loosen by accident.
//
// # Binary and network are separate concerns from the verdict
//
// [Compute] shells fpcalc; [Client.Lookup] talks to AcoustID; [Accept] decides.
// The first two are seam-injectable so tests never touch a real binary or a
// real host. Callers that only want to know whether the toolchain is usable
// call [Probe] or [Precheck].
package acoustid

import "github.com/acoseac/1-bit-bridge/internal/logging"

// logger is the package-scoped structured logger. Resolves the default
// handler at log time (see internal/logging), so it survives logging.Init()
// running after package init.
var logger = logging.Component("acoustid")
