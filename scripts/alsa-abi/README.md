# The ALSA PCM ABI check

`internal/alsapcm` is the bridge player's ALSA backend in pure Go: it drives
the kernel's PCM interface with ioctls on `/dev/snd/pcmC<card>D<device>p`,
with no alsa-lib, no cgo and no C toolchain at run time. Its structures,
constants and ioctl numbers (`internal/alsapcm/uapi.go`) are written by hand
from `include/uapi/sound/asound.h`, and these two files hold them to the
kernel's own header:

- `probe.c` prints what a C compiler makes of the header: every size,
  offset and field size of the structures the backend passes, the bit each
  of `snd_interval`'s one-bit fields takes, the constants and the ioctl
  numbers, one fact a line.
- `check.sh` fetches `asound.h` at the kernel tag `uapi.go` pins
  (`const uapiKernel`), compiles and runs the probe with the machine's own
  compiler, and runs `TestUAPIMatchesTheKernelHeader`, which compares every
  fact with the Go definitions compiled for the same architecture. It fails
  unless that test ran and passed.

CI runs `check.sh` natively on linux/amd64 and linux/arm64, the two
architectures the backend builds for (the `alsa-abi` job in
`.github/workflows/gate.yml`). Run it by hand on a Linux box with a C
compiler, the distribution's kernel headers (`linux-libc-dev`), curl and Go:

```sh
bash scripts/alsa-abi/check.sh
ALSA_UAPI_HEADER=/path/to/asound.h bash scripts/alsa-abi/check.sh   # a copy you already have
```

`TestProbeAndGoNameTheSameFacts` runs everywhere, without a compiler: it
holds the probe's macro calls and the Go side's list to the same facts, so a
definition added on one side alone fails in the ordinary test run.

## Why a probe, and why run natively

A probe compiled against the header is the header's own answer: the compiler
lays out the structures, so nothing is a second hand-written copy of the
offsets. Running it, rather than only compiling `static_assert`s, is what
reads the interval bitfields' positions, which no constant expression can.
Running each architecture natively compares the real Go compiler's layout
on that architecture with the real C compiler's.

## The header and its licence

`asound.h` is `GPL-2.0+ WITH Linux-syscall-note`, the note that lets a user
program use the kernel's interface. It is not vendored: `check.sh` fetches
it into a temporary directory and deletes it, and it is never part of the
bridge binary or its release archives. `check.sh` prints the file's git blob
id, which `git ls-tree <tag> include/uapi/sound/asound.h` in any kernel clone
shows for the same bytes.

## Moving the pin

To follow a newer kernel, change `uapiKernel` in `internal/alsapcm/uapi.go`,
run `check.sh` on both architectures (or push and read the `alsa-abi` job),
and update what the comparison reports, `pcmVersion` first among them. The
protocol floor, `pcmVersionFloor`, is a decision about which kernels the
backend opens, not a fact of the header, and does not move with the pin.
