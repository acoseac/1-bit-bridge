#!/usr/bin/env python3
"""Generate (or extend) the bridge.1-bit.app demo library from catalog.json.

Music comes from Google's Lyria 3 (lyria-3-pro-preview) via the Gemini API
`interactions` endpoint — it returns 44.1 kHz stereo MP3, which is shipped
AS MP3 (transcoding a lossy source into FLAC would be exactly the
fake-lossless the 1-bit app's provenance features exist to flag). Cover
art comes from a Gemini image model. Output layout matches what the bridge
scanner expects:

    <out>/library/<Artist>/<Album>/<NN> - <Title>.mp3   (ID3v2.3 + embedded cover)
    <out>/library/<Artist>/<Album>/folder.jpg

Idempotent: existing raw takes and finished files are reused, so a partial
run resumes where it stopped and a single deleted file regenerates alone.

Usage:
    export GEMINI_API_KEY=...            # or --key-file <path>
    python3 tools/demo-library/generate.py --out ~/demo-bridge-library

Then rsync <out>/library/ to the demo host and run a Full rescan (see
ops/deployment-runbook.md "Demo bridge").

Requires: python3 (stdlib only) + ffmpeg on PATH.
"""

import argparse
import base64
import json
import pathlib
import subprocess
import sys
import time
import urllib.error
import urllib.request

API = "https://generativelanguage.googleapis.com/v1beta"
MUSIC_MODEL = "lyria-3-pro-preview"
# Tried in order; the first that yields an image wins. Shapes differ per
# model generation, so the request is built per-model in gen_cover().
IMAGE_MODELS = ["gemini-3-pro-image", "gemini-2.5-flash-image"]
MIN_TRACK_SECONDS = 95   # regenerate once below this;
ACCEPT_SECONDS = 80      # ...accept the retry from here (better than a hole)


def post_json(url, body, key, timeout=300):
    req = urllib.request.Request(
        url,
        data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json", "x-goog-api-key": key},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.load(resp)


def ffprobe_duration(path):
    out = subprocess.run(
        ["ffprobe", "-v", "error", "-show_entries", "format=duration",
         "-of", "default=noprint_wrappers=1:nokey=1", str(path)],
        capture_output=True, text=True)
    try:
        return float(out.stdout.strip())
    except ValueError:
        return 0.0


def gen_track(prompt, key):
    """One Lyria call → MP3 bytes (or None). ~25-60 s per call."""
    for attempt in range(3):
        try:
            d = post_json(f"{API}/interactions",
                          {"model": MUSIC_MODEL, "input": prompt}, key)
        except urllib.error.HTTPError as e:
            if e.code in (429, 500, 502, 503) and attempt < 2:
                time.sleep(30)
                continue
            print(f"    music API error: {e}", file=sys.stderr)
            return None
        for step in d.get("steps", []):
            for c in step.get("content", []):
                if c.get("type") == "audio" and c.get("data"):
                    return base64.b64decode(c["data"])
        return None
    return None


def gen_cover(prompt, key):
    """One image → JPEG-able bytes (PNG ok; ffmpeg converts). Tries each
    model with progressively simpler request shapes and follows what the
    error messages permit."""
    shapes = [
        {"generationConfig": {"responseModalities": ["IMAGE", "TEXT"],
                              "imageConfig": {"aspectRatio": "1:1"}}},
        {"generationConfig": {"responseModalities": ["IMAGE", "TEXT"]}},
        {},
    ]
    for model in IMAGE_MODELS:
        for extra in shapes:
            body = {"contents": [{"parts": [{"text": prompt}]}], **extra}
            try:
                d = post_json(f"{API}/models/{model}:generateContent", body, key, timeout=180)
            except urllib.error.HTTPError:
                continue
            for cand in d.get("candidates", []):
                for part in cand.get("content", {}).get("parts", []):
                    inline = part.get("inlineData")
                    if inline and inline.get("data"):
                        return base64.b64decode(inline["data"])
    return None


def tag(raw_path, out_path, cover, meta):
    cmd = ["ffmpeg", "-y", "-i", str(raw_path)]
    if cover:
        cmd += ["-i", str(cover), "-map", "0:a", "-map", "1"]
    cmd += ["-c", "copy", "-id3v2_version", "3",
            "-metadata", f"title={meta['title']}",
            "-metadata", f"artist={meta['artist']}",
            "-metadata", f"album_artist={meta['artist']}",
            "-metadata", f"album={meta['album']}",
            "-metadata", f"track={meta['n']}/{meta['total']}",
            "-metadata", f"date={meta['year']}",
            "-metadata", f"genre={meta['genre']}"]
    if cover:
        cmd += ["-metadata:s:v", "title=Album cover",
                "-metadata:s:v", "comment=Cover (front)",
                "-disposition:v", "attached_pic"]
    cmd.append(str(out_path))
    subprocess.run(cmd, check=True, capture_output=True)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True, help="output root (library/ + raw/ created inside)")
    ap.add_argument("--key-file", help="file holding the Gemini API key (else $GEMINI_API_KEY)")
    ap.add_argument("--catalog", default=str(pathlib.Path(__file__).parent / "catalog.json"))
    args = ap.parse_args()

    import os
    key = (pathlib.Path(args.key_file).read_text().strip()
           if args.key_file else os.environ.get("GEMINI_API_KEY", "").strip())
    if not key:
        sys.exit("no API key: pass --key-file or set GEMINI_API_KEY")

    catalog = json.loads(pathlib.Path(args.catalog).read_text())
    out = pathlib.Path(args.out).expanduser()
    raw_dir = out / "raw"
    raw_dir.mkdir(parents=True, exist_ok=True)

    failures = []
    for ai, album in enumerate(catalog["albums"], 1):
        adir = out / "library" / album["artist"] / album["album"]
        adir.mkdir(parents=True, exist_ok=True)
        cover = adir / "folder.jpg"
        if not cover.exists():
            print(f"[{album['album']}] cover…")
            img = gen_cover(album["artPrompt"], key)
            if img:
                tmp = raw_dir / f"album{ai}-cover.bin"
                tmp.write_bytes(img)
                subprocess.run(["ffmpeg", "-y", "-i", str(tmp),
                                "-vf", "scale='min(1400,iw)':-2", "-q:v", "3",
                                str(cover)], check=True, capture_output=True)
            else:
                failures.append(f"cover: {album['album']}")

        total = len(album["tracks"])
        for tr in album["tracks"]:
            final = adir / f"{tr['n']:02d} - {tr['title']}.mp3"
            if final.exists():
                continue
            raw = raw_dir / f"album{ai}-track{tr['n']}.mp3"
            if not raw.exists() or ffprobe_duration(raw) < ACCEPT_SECONDS:
                prompt = f"{tr['prompt']}; {album['styleSuffix']}"
                print(f"[{album['album']}] {tr['n']:02d} {tr['title']}…")
                audio = gen_track(prompt, key)
                if audio:
                    raw.write_bytes(audio)
                if not raw.exists() or ffprobe_duration(raw) < MIN_TRACK_SECONDS:
                    audio = gen_track(prompt, key)  # one retry
                    if audio and len(audio) > 0:
                        cand = raw_dir / f"album{ai}-track{tr['n']}-retry.mp3"
                        cand.write_bytes(audio)
                        if ffprobe_duration(cand) >= ACCEPT_SECONDS and \
                           ffprobe_duration(cand) >= ffprobe_duration(raw):
                            cand.replace(raw)
                time.sleep(4)
            if raw.exists() and ffprobe_duration(raw) >= ACCEPT_SECONDS:
                tag(raw, final, cover if cover.exists() else None,
                    {**tr, "artist": album["artist"], "album": album["album"],
                     "year": album["year"], "genre": album["genre"], "total": total})
                print(f"    ok {ffprobe_duration(final):.0f}s")
            else:
                failures.append(f"track: {album['album']} / {tr['title']}")

    print("\nDone." if not failures else f"\nDone with failures:\n  " + "\n  ".join(failures))


if __name__ == "__main__":
    main()
