package qualification

// qualificationVersion changes whenever a guarded implementation changes.
const qualificationVersion = 36

// Version binds the harness's reports and records to this implementation
// (ReadApproval, ReadCandidate); production reads an owner's approval through
// ReadLegacyApproval, from LegacyVersion to Version (B728).
const Version = qualificationVersion

// LegacyVersion is the oldest version of an owner's approval that production
// plays, through ReadLegacyApproval, until B728's cutover (B7) stops it
// reading records: frozen at the version B728's design merged at, so no bump
// before the cutover strands a record before B6 migrates it.
const LegacyVersion = 35
