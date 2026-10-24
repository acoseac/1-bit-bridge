// Package alsapcm plays PCM through one physical ALSA playback endpoint by
// talking to the Linux kernel directly: the ioctls of
// include/uapi/sound/asound.h, with no alsa-lib, no cgo and no C toolchain
// at run time. Device is the first layer of the bridge player's backend
// (B470 PR 2a): open, negotiate, write and stop. Sink is the second (PR 2b):
// a stream of frames through a preallocated lookahead, the position the
// kernel establishes, pause and resume by the kernel's PAUSE or by zeros
// with the device released after a long pause, xruns reported and
// recovered only when asked, drop and drain, and the gapless decision
// between tracks. PR 4b2 gave the sink a cut at a track boundary (Cut: the
// store's tail the kernel has not taken, dropped and never written) and a
// reopen at the endpoint the binding resolves to now (SinkConfig.Locate),
// checked by Admit like the open. DSD and DoP are later layers.
//
// What it will do, and what it refuses:
//
//   - It opens only /dev/snd/pcmC<card>D<device>p, from the card and device
//     numbers in an Endpoint. There is no way to pass a name: no "default",
//     "hw:…", "plughw:…", dmix, softvol, alias or path, and no ALSA
//     configuration file is read. The node is opened non-blocking, and one
//     another process holds (MPD playing) is a typed busy refusal: the
//     kernel's exclusive open decides who plays, and nothing here stops MPD
//     or touches its configuration.
//   - The kernel's PCM protocol is checked at open (pcmVersionFloor).
//   - Rate, the two channels and the sample format are asked for exactly
//     and read back after HW_PARAMS; anything else the kernel committed is
//     a typed refusal, never a substitute. The buffer geometry is aimed near
//     200 ms and reported as granted.
//   - An underrun stops the stream (the stop threshold is the buffer size),
//     so it surfaces as an xrun, and the kernel is never asked to fill
//     silence.
//   - A write waits for room with poll, and a cancelled context ends the
//     wait at once; xrun, suspend and disconnect come back typed.
//   - A Sink never conceals: an xrun stops it and is counted as a gap
//     until the caller asks for Recover; silence never counts as content;
//     a frame is never written twice or skipped, a release and a reopen
//     included; and the device is closed once per open.
//   - A panic on a sink's writer is recovered there, logged once
//     (SinkConfig.Log), and ends the sink typed (ReasonPanicked): the device
//     closed, and whatever waited on the writer answered. A node a panic
//     comes to between its open and a sink taking it (the kernel's setup,
//     the check after open) is closed before the panic goes on to its
//     caller, or, at a reopen, ends the sink the same way.
//   - No buffer is ever shared with the kernel past a call: nothing is
//     mmapped, and each ioctl's argument is kept alive across the call
//     (ioctlOn).
//
// The kernel interface is the sys seam: the real one (linuxSys) is built
// on linux/amd64 and linux/arm64 only, the two layouts the definitions in
// uapi.go are checked against (scripts/alsa-abi, the alsa-abi job in
// .github/workflows/gate.yml). Everything above the seam builds and is
// tested on every OS through a fake kernel; Open itself refuses with
// ReasonUnsupported anywhere else.
package alsapcm
