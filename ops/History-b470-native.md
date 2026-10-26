# B470 2c.1 — provisional native sink and qualification

The implementation deliberately uses DSDSink beside Sink. PCM remains automatically active;
its base ioctl/HW_PARAMS/byte traces for 44.1/16 and 384/32 pin pause/release and natural advance.
Native transport identity adds logical rate and packing version, zero for PCM. The production
backend still refuses DSD; the only entry point is `output dsd-test`, never an HTTP route.

Canonical planar stereo is MSB-first. Native words take the oldest byte in their most significant
byte, then serialize LE/BE and interleave channels. Independent asymmetric literals pin packing.
WriteDSD counts accepted logical bits per channel, including its owned partial-word carry. Retry
only the suffix. Empty input allocates no occurrence. Final EOF pads with phased 0x69; compatible
occurrences share the carry. Ring, retained hardware prefix, carry, scratch and fixed ledger have a
32 MiB aggregate bound and 1024 outstanding occurrences; a small-bound test refuses the fifth.
Cross-layer reservation, occurrence notifications and engine startup integration are 2c.2.

Opening native is inert. Activate rechecks binding and the provisional descriptor/mixer snapshot;
Pause wins its fence without closing, Stop/revoke closes. Accepted prefill marks Committed.
The writer exclusively owns all device operations; no ioctl is issued while it holds the sink mutex.
TrackChange cancels the current write, accounts its accepted prefix and discards the suffix/software
queue, then emits only the qualified 0x69 tail and drains. Immediate escalates without waiting.
The shared release budget is two seconds, including cleanup. Parked DROP/HW_FREE/close retains
writer ownership after a bounded unconfirmed answer; no successor opens on that answer. Close
errors remain faults. Xrun/suspend/disconnect never calls Recover. Native pause explicitly chooses
kernel or silence, releases at 30 s, and rechecks on reopen; a busy reopen remains paused.
Durations round up at the hardware rate. PCM sides have zero tail and prefill.

The CLI injects its opener, control reader, clock and input. Both real native and reference openers
refuse testing.Testing(), and the CLI TestMain installs a separate control-node tripwire. Every
mandatory flag is validated before the opener. The wall-clock budget spans keys, pause and reopen.
The manifest and contact JSON schemas are versioned, with typed exact owner confirmation and no
cryptography. Calibrated listening assets are external owner inputs; software goldens are not used.
Descriptors are qualification-only reads: missing firmware/model fields never disable PCM binding.
Control snapshots are bounded read-only UAPI, with facts added to the existing Linux C/Go ABI gate.

qualificationVersion guards an enumerated path list. Its four-case test uses temporary Git objects
and read-only diff/show, including rename/delete. R8 approval version checking remains 2c.2.
No production admission, fallback, autoplay, engine lock/event, approval reader or approval writer
is claimed here. See b470-native-qualification.md for owner commands and pending Linux execution.

Cancelled opens retain cleanup ownership in the opening worker. Inert Pause also releases at 30 s; reopen geometry cannot outgrow the allocated store. Actual close acknowledgement wins over an expired release timer. The CLI continues reading Stop/quit while TrackChange is draining.
