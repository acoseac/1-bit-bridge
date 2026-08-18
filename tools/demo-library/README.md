# Demo library generator

Regeneration recipe for the content served by the public demo bridge
(`bridge.1-bit.app` — see `ops/deployment-runbook.md` "Demo bridge"). Every
artist, album and track name in [catalog.json](catalog.json) is invented, all
audio is generated (Google Lyria 3 via the Gemini API) and all cover art is
generated (Gemini image models) — zero licensing exposure, which is the
non-negotiable property of anything that lands in the demo library.

## Why MP3, not FLAC

Lyria 3's API output is 44.1 kHz stereo MP3 (~192 kbps) and offers no PCM/WAV
surface (verified 2026-08-18 against both the `interactions` endpoint's
`response_format` and `generateContent`'s `responseMimeType` — invalid-value
probes enumerate the accepted shapes, and none is lossless). Re-containering
that into FLAC would manufacture exactly the fake-lossless provenance the
1-bit app's own spectrum/bandwidth verdict exists to flag — on our own demo
content. So the library ships honestly as MP3; the bridge still computes
waveforms, loudness, DR, key/tempo and spectrum for it, and the app renders
honest MP3 codec chips.

## Usage

```sh
export GEMINI_API_KEY=...        # or --key-file <path>; never commit a key
python3 tools/demo-library/generate.py --out ~/demo-bridge-library
```

Idempotent: finished tracks/covers are skipped, so re-running fills only the
holes (or regenerates a file you deleted). Raw untagged API outputs are kept
under `<out>/raw/` so tagging changes don't cost regeneration credits.

Then ship it:

```sh
rsync -av --delete ~/demo-bridge-library/library/ <DEMO-SSH>:/srv/onebit-demo/library/
# then trigger a Full rescan on the bridge (admin console via SSH tunnel,
# or `sudo systemctl restart 1-bit-bridge` — startup scans; remember delta
# scans never delete, so removals need the full rescan).
```

## Editing the catalog

Add/change entries in `catalog.json` (keep the style suffix per album so
tracks stay coherent; keep "instrumental only, no vocals" unless you want
generated lyrics; keep "No text, no letters…" in art prompts — image models
render gibberish typography). Durations are steered by the prompt text
("about 2 minutes 15 seconds"); the generator accepts 80 s+ and retries once
below 95 s. Requires `ffmpeg` on PATH; each Lyria call takes ~25–60 s.
