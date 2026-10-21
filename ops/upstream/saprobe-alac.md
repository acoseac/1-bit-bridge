# saprobe-alac: defects found by the bridge, for an upstream report

**Module:** `github.com/mycophonic/saprobe-alac` at
`1b6eb83789544ce2e8624a679c14dbd33da7673a`
(`v1.0.0-rc2.0.20261020094436-1b6eb8378954`), Apache-2.0.

**Found:** 2026-10-21, building the bridge player's ALAC decoder (backlog
B470 PR 1c). The bridge calls only `NewPacketDecoder` and
`PacketDecoder.DecodePacket`, behind its own M4A demuxer and its own checks
of the magic cookie and of each packet's element header, with a `recover`
as the last resort (`internal/player/decode/alac.go`). Codex's B470 ALAC
evaluation (2026-10-20, outside this repo) found the container path's
defects (4), most of the configurations under 3, and an END-first packet
accepted through the high-level `Decoder` (part of 2). The zero-run
overrun (1), the single-channel case of 2, the short frame length and the
32-bit pair under 3 were found here.

**Status: reported privately on 2026-10-21**, through the repository's
vulnerability reporting (the panics, the unchecked configurations and the
silent stereo case; not yet the container path or the notices).

Each input quoted below with its output was run here against the pinned
revision on Go 1.27.2, darwin/arm64, and the output is what it printed; the
rest is Codex's record or the code.

## 1. The entropy decoder reads past its padded buffer and panics

`internal/alac/golomb.go`, `DynDecomp`: the loop checks `bitPos < maxPos`
only at the top of each sample. One sample's code can move `bitPos` past
the end of the packet: an escape moves it by 9 plus the channel's bits
(`getStreamBits`), and a normal code by the prefix, 1 and k. When the
running mean then falls under the zero-run threshold
(`(meanAccum << mmulShift) < quantBits`), `dynGet` is called with that
`bitPos` and no check, and its `read32bit` loads four bytes at
`bitPos >> 3`. `BitBuffer` pads a packet with four bytes, which covers a
load that starts inside the data, not one that starts past it. Apple's
`ag_dec.c` has the same unchecked `dyn_get` call; in C it reads past the
buffer instead of panicking.

The configuration is Apple's encoder's own; no check of the configuration
or of the element header tells this packet from a good one. A corrupt or
crafted file reaches it, and so does a file cut inside a packet if a
demuxer hands the library the short packet.

```go
cfg := alac.PacketConfig{FrameLength: 4096, BitDepth: 16, NumChannels: 2,
	PB: 40, MB: 10, KB: 14, MaxRun: 255, SampleRate: 44100}
dec, _ := alac.NewPacketDecoder(cfg)
dec.DecodePacket([]byte{0x20, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x01, 0xFF})
// panic: runtime error: index out of range [3] with length 2
//   internal/alac.read32bit       golomb.go:85
//   internal/alac.dynGet          golomb.go:118
//   internal/alac.DynDecomp       golomb.go:246
//   (*PacketDecoder).decodeCPECompressed decoder.go:495
```

The packet: a channel pair (tag 1), compressed, no shifted bytes, mixBits
and mixRes 0, both channels' predictors with no coefficients and a
pbFactor of 4; then the first residual of channel U is nine 1-bits that
end exactly at the packet's end. That is an escape whose 17 value bits lie
in the padding (0), after which the mean is still 10, under the threshold,
and `dynGet` reads at byte 4 of the 6 bytes left. The same packet under a
24-bit configuration panics at the same place (`index out of range [3]
with length 1`).

A fix: check `bitPos < maxPos` before `dynGet` and return
`ErrBitstreamOverrun`, or pad the buffer by the largest single advance
(9 + 33 bits) plus the four bytes the 32-bit load takes.

## 2. A stereo packet without its channel pair decodes to silence, without an error

`decoder.go`, `decodePacketInto`: the element loop leaves at END with
`numSamples` still the frame length and no channel written, and
`DecodePacket` returns that many frames of its freshly zeroed buffer. A
packet whose only channel element is a single-channel one (SCE) writes one
channel of a stereo frame and leaves the other zero.

```go
dec.DecodePacket([]byte{0xE0})
// 16384 bytes (4096 stereo 16-bit frames), all zero, err == nil
```

A partial SCE of one frame and then END, under the same stereo
configuration, returns 4 bytes with only the left sample written and
`err == nil`. Through the high-level `Decoder.Read`, which reuses its
buffer, the END case returns the previous packet's samples again (Codex's
evaluation). A stereo packet from Apple's or FFmpeg's encoder opens with a
channel pair; the bridge refuses any other first element before the
library sees the packet.

## 3. Configurations NewPacketDecoder accepts and decoding cannot take

`NewPacketDecoder` checks only the bit depth. Codex's evaluation
reproduced the channel counts, the frame length of 0, the 4,194,304-frame
allocation and the rate of 0 below; the bridge reproduced the short frame
length and the 32-bit pair:

- **A channel count of 0 or more than 8** indexes `channelLayoutOffsets`
  out of range at the first packet.
- **A frame length of 0** sizes the work buffers at 0, and decoding indexes
  them. **A frame length below 31** lets the predictor's warm-up
  (`UnpcBlock`, which runs to the coefficient count, up to 30, whatever the
  packet's length) index past them: a 16-frame configuration and a channel
  pair whose U predictor has 30 coefficients panic with `index out of range
  [16] with length 16`.
- **The frame length sizes four buffers with no bound**: a cookie of
  4,194,304 frames allocated about 80 MiB before an error.
- **A 32-bit channel pair, compressed, with no shifted byte** makes the
  residuals 33 bits (`chanBits` is the depth plus 1), and an escape at the
  end of the packet then reads past the padding in `getStreamBits`:

  ```go
  cfg.BitDepth = 32
  dec, _ := alac.NewPacketDecoder(cfg)
  dec.DecodePacket([]byte{0x20, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x01, 0xFF})
  // panic: runtime error: index out of range [6] with length 6
  //   internal/alac.getStreamBits   golomb.go:96
  //   internal/alac.DynDecomp       golomb.go:201
  ```

  `chanShift` is then `32 - 33`, which wraps. Apple's and FFmpeg's encoders
  shift two bytes out of 32-bit samples (one out of 24-bit), so no encoder
  writes this.
- `Decoder.Duration` and `Position` divide by the cookie's sample rate,
  which may be 0.

The bridge validates the cookie itself (two channels; 16, 24 or 32 bits;
a frame length of 32 to 65536; kb at most 31; a rate of 1 to 768000 Hz) and
refuses a compressed 32-bit pair with no shifted byte before the library
sees the packet.

## 4. The container path (`NewDecoder`), which the bridge does not use

From Codex's evaluation, `internal/mp4/mp4.go`: table counts drive
allocations before they are compared with the file (about 64 MiB from a
49,955-byte file); `stts` is not read, so a packet's frame count and the
stream's length are not known; edit lists are ignored, so a trimmed file
decodes untrimmed (28,809 frames where FFmpeg presents 28,686); `stsc` is
not checked for order, coverage or description index; child boxes are not
confined to their parents; an `stsd` of 2^32 - 1 zero-sized entries spins
for seconds; `Seek` rounds to a packet through floating point.

## 5. Notices

The repository says the decoder is a port of Apple's ALAC reference
(Apache-2.0, Copyright 2011 Apple Inc.), but the ported Go files carry only
Mycophonic's header. Apache-2.0 §4(c) asks that a derivative keep the
original's copyright notices (Codex's evaluation).

## 6. A 16-bit pair's shifted bytes are dropped, without an error

From Codex's review of the bridge's PR #1300 (2026-10-21), and run here.
`decodeCPE` reads a compressed packet's shifted bytes into `shiftBuffer` at
any depth, and `WriteStereo16` never reads that buffer (nor does
`WriteStereo20`), so a compressed 16-bit pair that declares a shift decodes
to its residuals alone:

```go
cfg.BitDepth = 16
dec, _ := alac.NewPacketDecoder(cfg)
dec.DecodePacket([]byte{0x20, 0x00, 0x14, 0x00, 0x00, 0x00, 0x02, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x24, 0x68, 0x70})
// one frame, (0, 0), err == nil
```

The packet is a partial channel pair of one frame with one shifted byte a
sample: zero residuals, and shifted bytes 0x12 and 0x34, which make the
frame (0x12, 0x34) when they are kept. No encoder writes such a packet
(Apple's and FFmpeg's shift only above 16 bits, and the bridge's afconvert
goldens, FFmpeg encodes and corpus have no 16-bit packet with a shift). The
bridge refuses a compressed 16-bit pair with a shift before the library
sees it, and leaves `WriteStereo16` alone.

## 7. A last code that runs past the packet decodes from the padding

From the same review, and run here. `DynDecomp` checks `bitPos < maxPos`
only at the top of each sample, so a sample's code can end past the packet,
in the four zero bytes of padding, and they decide its value. When a zero
run follows, reading its count is the panic of 1. When the code is the last
the packet holds, nothing reads after it and the packet decodes with no
error. Three cases under the configuration of 1, each a partial channel pair
whose V's last code runs past the end:

| Packet | Frames | V's last code | 1b6eb83, with no error |
|---|---|---|---|
| `20 00 10 00 00 00 02 00 00 00 00 00 00 ff 80` (Codex's) | 1 | a nine-bit escape whose 17 value bits need 10 more than the packet has | (0, 0) |
| `20 00 10 00 00 00 02 00 00 00 00 00 00 ff` | 1 | eight ones that end the packet, so its stop bit is the padding's | (0, 4) |
| `20 00 10 00 00 00 20 00 00 00 00 00 00 82` | 16 | after a zero and U's run of 15 (`1 0 000`), V's zero and a run whose code `1 0 000` has three bits past the end | 16 frames of (0, 0) |

Only the second channel's last code can do this: the first channel's
`DynDecomp` advances the bit buffer past the packet, and the second's then
refuses at its start.

## The bridge's patch

The bridge builds this module from a copy of 1b6eb83 with one change
(`third_party/saprobe-alac`, which a `replace` in its `go.mod` names), and
carries it until a release has the fix. `ops/upstream/saprobe-alac.patch` is
that change as a plain diff against 1b6eb83, for upstream. `DynDecomp` checks
every code against the packet's own length: an escape's value before
`getStreamBits` reads it, which also bounds the 33-bit read of 3; a normal
code's length before it is taken; and a zero run's count before and after
`dynGet`. A code that would end past the packet returns
`ErrBitstreamOverrun`. That fixes 1 and 7: their packets, and the 32-bit
packet of 3, return `ErrDecode` wrapping the overrun, and none panics. The
patch adds `tests/bitstream_overrun_test.go` in the module's style with
those packets, and with a code that ends exactly at the packet's end, which
still decodes. With it the bridge's goldens and corpus decode bit-exact as
before.

The patch leaves 6 alone, which the bridge refuses before the library, and
the 33-bit value itself: `getStreamBits` cannot return 33 bits (its
`32 - numBits` wraps), so a compressed 32-bit pair with no shifted byte is
still not decodable, and the bridge refuses that before the library too.
