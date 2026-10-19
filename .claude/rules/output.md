---
paths:
  - "internal/alsa/**"
  - "cmd/bridge/output*.go"
---
# The box as an audio output (`internal/alsa`, `bridge output`)

The bridge as an output: on a Linux box with a DAC and MPD, it reports the DAC and a live reading of what the DAC receives, so the app's Signal Path can show it. Step one (2026-10-19) reads `/proc` and adds `bridge output dac`; it changes no wire format, no route and no config key. The record is in `ops/engineering-log.md` under that date.

- **`owner_pid` in a substream's `status` is a THREAD id**: the thread that opened the PCM, which for MPD is its output thread (`/proc/<tid>/comm` reads `output:alsa`). The process is the `Tgid:` line of `/proc/<tid>/status`, named by `/proc/<tgid>/comm` (`mpd`). **Never read `/proc/<owner_pid>/comm` as the process's name**, and report both names.
- **An owner nobody can name is unknown, never an error**: a missing or unreadable `/proc` entry (hidepid, a thread that exited) or an `owner_pid` of 0 (outside this pid namespace) leaves that name null, and the reading is whole. **Don't turn it into a problem or a failed read.**
- **The reader takes its roots and carries no build tag**, so its tests read the captured trees in `internal/alsa/testdata` on all three CI OSes. Only `bridge output dac` refuses, and only the host's own tree, off Linux; `--proc-root` reads a tree anywhere. **Don't move the reader behind `//go:build linux`.**
- **A parser ignores a key it does not know, and reports a key it knows whose value does not parse**, a second line of a key the kernel prints once (the first value stands), and a block the kernel always prints whole that arrives without one of its lines (a file cut short). It never guesses a value: what did not parse is null. Kernels add lines, so a strict parser fails on the next kernel; a guessing one shows the app a reading nothing proved.
- **`--watch` compares readings without what moves on every read while audio plays** (`steadyView`: the buffer pointers, delay, avail, the timestamps, the measured rate and the packet size). **Don't add a moving value to the comparison**: it would print once a second for as long as anything plays.
- **The `alsa` types' JSON is `bridge output dac --json`'s diagnostic shape**, every key always present and every list `[]`. A `/v1` route that serves a reading wraps it in a DTO under `internal/api`, as the wire-type discipline in `wire.md` requires; **don't encode an `alsa` type from a handler.**
- **The fixtures are captures, byte for byte**, pinned to LF in `.gitattributes`; `testdata/README.md` names each synthetic file. A new case comes from `bridge output dac --raw` on a real box. **Never edit a captured file**: add a synthetic case beside it.
