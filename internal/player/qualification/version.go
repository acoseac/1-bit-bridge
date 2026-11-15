package qualification

// qualificationVersion changes whenever a guarded implementation changes.
const qualificationVersion = 50

// Version binds the harness's reports and records to this implementation
// (ReadApproval, ReadCandidate): the harness (bridge output dsd-test, and
// bridge output play's --report) reads a record of this version alone. Since
// B728's cutover (B7) production reads no record: serve and bridge output
// play decide DSD by the effective policy (internal/player/policy) over the
// player's DSD state, and an owner's record is read once, by the migration
// of the legacy keys (dsdstate.ReadRecord), through ReadLegacyApproval.
const Version = qualificationVersion

// LegacyVersion is the oldest version of an owner's approval the migration
// of the legacy keys reads (ReadLegacyApproval, from LegacyVersion to
// Version): frozen at the version B728's design merged at, so a record an
// owner made before the cutover migrates on the first start after it,
// whatever version this build is. Nothing else reads it since B7.
const LegacyVersion = 35
