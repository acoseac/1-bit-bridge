# ALSA test fixtures

Each directory is one case: a `/proc` tree under `proc/`, which
`alsa.Reader{ProcRoot: ...}` and `bridge output dac --proc-root` read.

## Captured: a Raspberry Pi 5 with an iFi GO bar

Six cases were captured on 2026-10-19 from a Raspberry Pi 5 running
Volumio 4.119, with an iFi GO bar (USB id `20b1:3009`) as ALSA card 5
(card id `Audio`), while MPD 0.24.0 played, or for `idle` did not:

| Case | What the DAC received |
|---|---|
| `idle` | nothing: `stream0` says `Stop`, the substream is `closed` |
| `dsd256-native` | `DSD_U32_BE` at 352800 frames/s on altset 4 (native DSD256) |
| `pcm-44k1-16` | `S16_LE` at 44100 on altset 3 |
| `pcm-96k-24` | `S24_3LE` at 96000 on altset 2 |
| `pcm-192k-24` | `S24_3LE` at 192000 on altset 2 |
| `pcm-384k-24` | `S24_3LE` at 384000 on altset 2 |

These files are byte for byte as the box's `/proc/asound` gave them:
`asound/cards`, `asound/card5/usbid`, `asound/card5/stream0`, and
`asound/card5/pcm0p/sub0/` `hw_params`, `status`, `sw_params` and `info`.
The capture's `info` reads the same for the PCM (`pcm0p/info`) and for its
only substream, and it sits under `sub0`. Only card 5's files were
captured, so the two HDMI cards (0 and 1) are in `cards` with no directory
of their own.

`mpd_status`, beside each playing case, is MPD's own `status` reply at the
time, not a proc file. It is kept for the step that reads MPD; `idle` has
none.

**Synthetic, inside the captured cases:** `proc/6584/comm`,
`proc/6584/status` and `proc/6551/comm` in the five playing cases. The
captured `owner_pid` is 6584, MPD's output thread, whose comm on the box
read `output:alsa`; its process's id was not captured, so 6551 is made up,
and each status file holds only the first lines a real one opens with.

## Synthetic cases

- `i2s-and-hdmi`: a Pi with HDMI (card 0, `vc4hdmi0`, closed) and an I2S
  DAC HAT (card 1, with a HiFiBerry DAC+'s names) playing `S32_LE` at
  192000 for `aplay`, which opens the device from its main thread, so its
  thread id is its process id (812). Neither card is USB: there is no
  `usbid` and no stream file.
- `garbled-stream0`: a USB DAC whose `stream0` has values that do not
  parse, a line no stream file has, and an end cut off in the middle of a
  line, with no newline. Its substream plays for thread 4242, which has no
  `/proc` entry here. The cut leaves altset 2 with no Bits line; that is
  unknown, not a file cut short.
- `multi-format`: one Format line per altset carrying more than one name,
  the way the kernel prints an altset's bitmask (`S16_LE S24_3LE`,
  `SPECIAL DSD_U32_BE`, `S32_LE DSD_U32_LE`). `SPECIAL` is a format name.
- `uac1-bits-less`: the printer from before Linux 5.1, which is the same
  for every USB device, UAC1 or UAC2: a full-speed endpoint line and no
  Bits line. Current kernels print Bits for both.
- `symlinked-pcm`: real directories the tests link as `stream0`, `pcm0p`
  and `sub0`, plus `Audio` pointing at `card0` the way `/proc/asound/<id>`
  does. The links are made by the test, not stored: a checkout of a symlink
  is not the same file on Windows.
- `pcm768-dsd512`: a USB card whose `stream0` playback altsets offer PCM
  through 768000 (S32_LE, S24_3LE, S16_LE) and native `DSD_U32_BE` at
  frame rates 705600 and 768000, which are DSD512 (those rates times 32).
  The card id, name and USB id are made up. No substream is playing.

`volumio-asound-chain/` (`etc/asound.conf` and `proc/asound/cards`) was captured on 2026-10-19 from a Volumio 4.119 Raspberry Pi 5 via `cat`, byte for byte, and is not edited. `etc/alsa/conf.d/` in that directory is the fourteen files from the same box's `/etc/alsa/conf.d/`, captured the same day via `cat`, and is not edited.

`volumio-with-ctl/` is synthetic: that captured `asound.conf`, unchanged, with a `ctl.!default` block, a `ctl.name` block and a `defaults.pcm.card` assignment in front of it.

`fuzz/` holds inputs the fuzz targets found; `go test` runs them as seeds.

## Adding a case

On the box, `bridge output dac --raw > capture.txt` prints every file a
reading reads, each after a `==> path <==` line that names its place in a
`/proc` tree. Lay each one out at `<case>/proc/<path>`; a file followed by
`\ No newline at end of file` ends without its last newline.
`TestOutputDACRawDumpReadsBackAsTheSameReading` does exactly that.

Commit a capture as it was read, and never edit a captured file: make a
synthetic case instead, and list it here. `.gitattributes` keeps this tree
LF on every checkout.
