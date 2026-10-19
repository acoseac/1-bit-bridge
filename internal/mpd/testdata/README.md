# Fixtures for the MPD client

Captured on 2026-10-19 from a Raspberry Pi 5 running Volumio 4.119 and MPD
0.24.0. Pinned to LF in `.gitattributes`. Never edit a captured file; a
synthetic case belongs in a test, beside it.

| File | What it is |
|---|---|
| `volumio-mpd.conf` | The box's `/etc/mpd.conf`. The only password-like line is a commented example inside `input {}`. |
| `volumio-mpd-replies.json` | The greeting and the raw replies to `status`, `stats`, `outputs`, `replay_gain_status`, `config` (the ACK), `currentsong` and `commands`. |
| `volumio-os-release` | The box's `/etc/os-release`, including `VOLUMIO_VERSION`, `VOLUMIO_BUILD_VERSION` and `VOLUMIO_BUILD_DATE`. |
