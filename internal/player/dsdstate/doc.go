// Package dsdstate is the bridge player's DSD state (B728 B3;
// ops/plan-b728-dsd-capability-design.md, sections 2.5 and 3.6): what the
// user chose for each DAC model and what was heard and observed on it, kept
// in <dataDir>/player-dsd.json, schema 1, beside the player's queue, and the
// logic of the one migration of the legacy keys (dsdMode, allowDoP and
// dsdQualification) into it. Nothing reads it yet: the effective policy (B5)
// reads it, serve's player (B6) opens it and runs the migration, and until
// then the owner's approval record decides DSD as before.
//
// A DAC is keyed by its model, dacprofile.Key (B2's: the vendor and product
// IDs, the interface and the product string), never by a serial, a port, a
// card number or a device number, none of which the file holds. The state
// keeps a bridge-wide defaultMode, the mode a DAC with no choice of its own
// takes (auto, or off where an explicit dsdMode: off was migrated or the
// state did not read), and per DAC: its choice, with the revision B9's
// compare-and-set reads; its DoP confirmation, under the firmware it was
// given for; its first-use acknowledgements, one a transport, each covering
// every rate or, when the migration made it, the rates the owner's record
// approved alone (BitRates); the latest listener answer for each transport,
// rate and firmware; and what was observed on this box. Warnings say what the
// app must be able to explain, each owned by a DAC or by the bridge.
//
// The Store is the state's one writer. Serve's player opens it once, and
// every write (the migration's, the app's choice, the console's, a check's
// answer, playback's evidence) is a Store.Update, made under the store's lock
// on the state the last write left, and answered only once the file and
// every directory entry up to the data directory are durable
// (atomicwrite.WriteBytesDurable, as the player's ID reservations are): so
// writes land one at a time in the order they take the lock. No other process
// writes the file while serve runs, since serve holds the data directory's
// lock, which bridge restore and bridge reset-player take too; a backup only
// copies the file's bytes. Where the platform cannot sync a directory
// (Windows, where the player does not run) every write is refused
// (atomicwrite.ErrNoDirectoryBarrier) and nothing is written.
//
// A file that is there and does not decode is never deleted and never read
// as the defaults: its bytes are copied, durably, to a name beside it with a
// timestamp, then the file is replaced, in one rename, by a fresh state that
// refuses DSD (Refusing: defaultMode off, the state_unreadable warning, and a
// marker that no migration runs over it), so the name always holds either
// the bad bytes or the fresh state and the refusal survives a restart. A file
// that cannot be read, or whose bytes cannot be set aside, is left where it
// is and refused in memory, and no write replaces it until it is moved. A
// missing file is the defaults, and nothing is written until the first
// write.
package dsdstate
