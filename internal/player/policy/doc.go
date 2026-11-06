// Package policy is the bridge player's effective DSD policy (B728 B5;
// ops/plan-b728-dsd-capability-design.md, section 3): for the DAC the
// binding resolves to, at each DSD rate the player decides (DSD64, DSD128
// and DSD256), whether it plays native DSD, DoP or DSD converted to PCM, or
// is refused and with which word, from three inputs:
//   - the profile database (dacprofile), matched against the unit's model
//     identity: the DAC is known when exactly one profile matches;
//   - what the kernel and the silent probe establish about the DAC now: the
//     bound PCM device's playback altsets from its stream file, the probe's
//     answers under this unit, firmware and kernel (alsapcm.ProbeCache), and
//     for DoP the unit's USB descriptors, the card's controls and the
//     kernel's table of the controls it made of each unit (Mixer);
//   - the player's DSD state (dsdstate): the DAC's own choice, or the
//     state's defaultMode for a DAC with none, its DoP confirmation, its
//     first-use acknowledgements, the latest listener answers and what was
//     observed on this box.
//
// Effective is pure: its inputs are values, and it reads no file, kernel,
// clock or global. Nothing calls it yet. B7, the cutover, gathers the inputs
// through bounded adapters (the stream file, the probe cache, the sysfs
// descriptors attribute, alsactl.Reader with AllowUnreadable, the card's
// usbmixer table, the store's state) at every session, activation, 100 ms
// tick and supervisor refresh, as readPermission is read today, and replaces
// readPermission with it; it asks the hotplug snapshot's freshness first, as
// readPermission does, since a stale reading is no ground for a set.
//
// The order Effective decides in:
//  1. A state that does not read (dsdstate.State.Unreadable) refuses every
//     rate as dsd_state_unreadable, before any choice, capability or
//     conversion is looked at (the design's section 2.5).
//  2. The choice that applies is the DAC's own or defaultMode with no cap
//     (dsdstate.State.ChoiceFor), and section 3.2's table decides each rate
//     under it, with the rejection rule: the latest listener answer under
//     the unit's firmware, noise or nothing, takes a transport out at that
//     rate whatever the positive inputs say (a profile claim, a driver
//     acceptance, streaming observed, an earlier clean).
//  3. A profile's refused DoP rate (dop.refusedBitRates) outranks its stated
//     maximum and the user's confirmation, and only this box's own latest
//     listener answer for DoP at that rate, clean, overrides it. A profile's
//     native claim holds no refusal: its maximum bounds what it reports,
//     never what is offered, which the live altsets and the probe decide.
//  4. DoP needs the gain check (CheckGain): the playback path's gain stages
//     all visible, and every level control at unity, whatever its name.
//
// Reconcile is the per-occurrence predicate of the design's section 3.5:
// whether an occurrence of the session is kept, re-planned or revoked when
// the set changes, and whether doing so interrupts what is heard, which a
// user's write needs its interrupt for.
//
// The words are the design's section 1.3's: a rung is reported, driver
// accepted, streaming observed or listener confirmed, and no word here
// claims that a DAC decodes DSD.
package policy
