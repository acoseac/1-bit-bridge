#!/usr/bin/env python3
"""Generate independent ALAC goldens for the bridge player (B470 PR 1c).

Ordinary tests never run this. It needs macOS afconvert, which is Apple's
ALAC encoder and decoder, and the Python standard library. It writes small
fixtures under internal/player/decode/testdata/alac-goldens/ and a
manifest.json.

The expected audio is an integer pattern specified here (pattern()), which
alac_golden_test.go reproduces and checks against the manifest's hash: no
expectation is derived from the bridge decoder. Every encode is decoded back
by afconvert and compared with the pattern before anything is written, so an
encode that is not lossless never reaches the manifest.

The container variants are rewritten from those encodes box by box, with the
audio bytes unchanged: moov moved after mdat with stco widened to co64, and
an edit list added. An edit's expected audio is the pattern from the edit's
media time for the frames the edit covers whole: its duration, converted
from the movie timescale to frames, rounded down, and never past the media's
end. That is what afconvert's own decode of the edited file presents (it
floored durations whose fractions were .1, .5 and .7), and the generator
checks the edited fixture against that decode too.

Two FFmpeg encodes from Codex's B470 ALAC evaluation are copied beside them
when --codex names that evaluation's directory (FFmpeg is not installed on
the host that made these goldens). Their expected audio is a SHA-256 of the
source PCM that Codex encoded, which FFmpeg's own decode matched; the
manifest records where each came from.

Usage:
    python3 scripts/player-alac-goldens/generate.py [--out DIR] [--codex DIR]
"""

from __future__ import annotations

import argparse
import hashlib
import json
import shutil
import struct
import subprocess
import sys
import tempfile
import wave
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
DEFAULT_OUT = ROOT / "internal" / "player" / "decode" / "testdata" / "alac-goldens"

# afconvert writes 4096 frames per packet; the pattern changes kind every
# BLOCK frames, so a packet holds parts of several kinds.
FRAME_LENGTH = 4096
BLOCK = 1499


def tool(name: str) -> str:
    path = shutil.which(name)
    if not path:
        sys.exit(f"missing required tool: {name}")
    return path


AFCONVERT = tool("afconvert")


# --- the pattern -------------------------------------------------------------


def xorshift32(x: int) -> int:
    x ^= (x << 13) & 0xFFFFFFFF
    x ^= x >> 17
    x ^= (x << 5) & 0xFFFFFFFF
    return x


def to_signed(u: int, bits: int) -> int:
    return u - (1 << bits) if u >= 1 << (bits - 1) else u


def tri(i: int, period: int, amp: int) -> int:
    """An integer triangle wave in [-amp, amp]; every operand is non-negative,
    so Python's floor division and Go's truncating division agree."""
    p = i % period
    half = period // 2
    if p < half:
        return -amp + (2 * amp * p) // half
    return amp - (2 * amp * (p - half)) // (period - half)


def pattern(frames: int, bits: int) -> list[int]:
    """Interleaved left, right samples. Left and right differ in every kind
    but the silence, which carries one impulse per channel at different
    places; the signed extrema, low-bit-only activity and full-scale noise
    each appear, and the kinds change mid-packet."""
    lo, hi = -(1 << (bits - 1)), (1 << (bits - 1)) - 1
    xl, xr = 0x9E3779B9, 0x85EBCA6B
    out: list[int] = []
    for i in range(frames):
        kind = (i // BLOCK) % 8
        j = i % BLOCK
        xl, xr = xorshift32(xl), xorshift32(xr)
        if kind == 0:  # only the low bits move
            left, right = (i % 16) - 8, 7 - (i % 15)
        elif kind == 1:  # signed extrema, out of step
            left = lo if i % 2 == 0 else hi
            right = hi if (i // 3) % 2 == 0 else lo
        elif kind == 2:  # full-scale noise: escape packets
            left, right = to_signed(xl >> (32 - bits), bits), to_signed(xr >> (32 - bits), bits)
        elif kind == 3:  # smooth, different periods and levels
            left, right = tri(i, 97, hi * 4 // 5), tri(i + 13, 61, hi // 2)
        elif kind == 4:  # correlated: right is left plus a small side
            left = tri(i, 173, hi // 3)
            right = left + (i * 7) % 33 - 16
        elif kind == 5:  # silence with one impulse per channel
            left = hi if j == 100 else 0
            right = lo if j == 700 else 0
        elif kind == 6:  # next to the extrema, noise in the low byte
            left, right = hi - (xl & 0xFF), lo + (xr & 0xFF)
        else:  # a one-step square on the left, a ramp on the right
            left, right = (1 if i % 2 else -1), (j % 64) - 32
        out.extend((left, right))
    return out


def samples_sha256(samples: list[int]) -> str:
    """The decoder's wire form: interleaved int32, little-endian."""
    h = hashlib.sha256()
    for s in samples:
        h.update(struct.pack("<i", s))
    return h.hexdigest()


# --- afconvert ---------------------------------------------------------------


def write_wav(path: Path, samples: list[int], bits: int, rate: int, channels: int) -> None:
    width = bits // 8
    data = b"".join(s.to_bytes(width, "little", signed=True) for s in samples)
    with wave.open(str(path), "wb") as w:
        w.setparams((channels, width, rate, 0, "NONE", "not compressed"))
        w.writeframes(data)


def read_wav(path: Path) -> tuple[list[int], int, int, int]:
    """Integer PCM from a RIFF/WAVE file, WAVE_FORMAT_EXTENSIBLE included
    (afconvert writes it for 24 and 32 bits), read chunk by chunk."""
    data = path.read_bytes()
    if data[:4] != b"RIFF" or data[8:12] != b"WAVE":
        sys.exit(f"{path.name}: not a WAVE file")
    pos, fmt, pcm = 12, None, None
    while pos + 8 <= len(data):
        cid, size = data[pos:pos + 4], struct.unpack_from("<I", data, pos + 4)[0]
        body = data[pos + 8:pos + 8 + size]
        if cid == b"fmt ":
            fmt = body
        elif cid == b"data":
            pcm = body
        pos += 8 + size + (size & 1)
    if fmt is None or pcm is None:
        sys.exit(f"{path.name}: no fmt or data chunk")
    tag, channels, rate = struct.unpack_from("<HHI", fmt, 0)
    bits = struct.unpack_from("<H", fmt, 14)[0]
    if tag == 0xFFFE and struct.unpack_from("<H", fmt, 24)[0] != 1:
        sys.exit(f"{path.name}: extensible sub-format is not PCM")
    if tag not in (1, 0xFFFE):
        sys.exit(f"{path.name}: format tag {tag:#x} is not integer PCM")
    width = bits // 8
    samples = [int.from_bytes(pcm[o:o + width], "little", signed=True) for o in range(0, len(pcm), width)]
    return samples, bits, rate, channels


def afconvert(args: list[str]) -> None:
    subprocess.run([AFCONVERT, *args], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def encode_alac(wav: Path, dst: Path, flags: str = "") -> None:
    afconvert(["-f", "m4af", "-d", "alac" + flags, str(wav), str(dst)])
    dst.write_bytes(zero_times(dst.read_bytes()))


def zero_times(data: bytes) -> bytes:
    """afconvert stamps the time of the encode into mvhd, tkhd and mdhd. Zero
    those creation and modification times in place, so that rerunning the
    generator leaves the fixtures byte-identical; nothing else moves."""
    out = bytearray(data)

    def walk(start: int, end: int) -> None:
        pos = start
        while pos + 8 <= end:
            size, typ = struct.unpack_from(">I4s", out, pos)
            if size < 8 or pos + size > end:
                return
            body = pos + 8
            if typ in (b"moov", b"trak", b"mdia"):
                walk(body, pos + size)
            elif typ in (b"mvhd", b"tkhd", b"mdhd"):
                width = 8 if out[body] == 1 else 4
                out[body + 4:body + 4 + 2 * width] = bytes(2 * width)
            pos += size

    walk(0, len(out))
    return bytes(out)


def decode_with_afconvert(m4a: Path, bits: int, scratch: Path) -> list[int]:
    out = scratch / (m4a.stem + ".ref.wav")
    afconvert(["-f", "WAVE", "-d", f"LEI{bits}", str(m4a), str(out)])
    samples, got_bits, _, _ = read_wav(out)
    if got_bits != bits:
        sys.exit(f"{m4a.name}: afconvert decoded {got_bits}-bit PCM, want {bits}")
    return samples


# --- ISO-BMFF rewriting ------------------------------------------------------

CONTAINERS = {b"moov", b"trak", b"mdia", b"minf", b"stbl", b"edts", b"dinf"}


class Box:
    def __init__(self, typ: bytes, payload: bytes = b"", children: list[Box] | None = None):
        self.typ = typ
        self.payload = payload
        self.children = children

    def find(self, typ: bytes) -> Box | None:
        for c in self.children or []:
            if c.typ == typ:
                return c
        return None

    def encode(self) -> bytes:
        body = b"".join(c.encode() for c in self.children) if self.children is not None else self.payload
        return struct.pack(">I", 8 + len(body)) + self.typ + body


def parse(data: bytes, start: int = 0, end: int | None = None) -> list[Box]:
    end = len(data) if end is None else end
    out: list[Box] = []
    pos = start
    while pos < end:
        size, typ = struct.unpack_from(">I4s", data, pos)
        if size < 8 or pos + size > end:
            sys.exit(f"cannot rewrite box {typ!r} of size {size} at {pos}")
        if typ in CONTAINERS:
            out.append(Box(typ, children=parse(data, pos + 8, pos + size)))
        else:
            out.append(Box(typ, data[pos + 8:pos + size]))
        pos += size
    return out


def sound_trak(moov: Box) -> Box:
    for trak in moov.children:
        if trak.typ != b"trak":
            continue
        hdlr = trak.find(b"mdia").find(b"hdlr")
        if hdlr.payload[8:12] == b"soun":
            return trak
    sys.exit("no sound track")


def rebuild(data: bytes, edit_moov) -> bytes:
    """Lay the file out as ftyp, mdat, moov, after edit_moov(moov) has
    changed moov, and move every chunk offset with mdat's payload."""
    top = parse(data)
    ftyp = next(b for b in top if b.typ == b"ftyp")
    mdats = [b for b in top if b.typ == b"mdat"]
    moov = next(b for b in top if b.typ == b"moov")
    if len(mdats) != 1:
        sys.exit("expected one mdat")
    pos, old_payload = 0, None
    while pos < len(data):
        size, typ = struct.unpack_from(">I4s", data, pos)
        if typ == b"mdat":
            old_payload = pos + 8
        pos += size
    edit_moov(moov)
    new_payload = 8 + len(ftyp.payload) + 8
    stbl = sound_trak(moov).find(b"mdia").find(b"minf").find(b"stbl")
    for box in stbl.children:
        if box.typ == b"stco":
            n = struct.unpack_from(">I", box.payload, 4)[0]
            offs = struct.unpack_from(f">{n}I", box.payload, 8)
            box.payload = box.payload[:8] + struct.pack(f">{n}I", *(o - old_payload + new_payload for o in offs))
        elif box.typ == b"co64":
            n = struct.unpack_from(">I", box.payload, 4)[0]
            offs = struct.unpack_from(f">{n}Q", box.payload, 8)
            box.payload = box.payload[:8] + struct.pack(f">{n}Q", *(o - old_payload + new_payload for o in offs))
    return ftyp.encode() + mdats[0].encode() + moov.encode()


def widen_to_co64(moov: Box) -> None:
    stbl = sound_trak(moov).find(b"mdia").find(b"minf").find(b"stbl")
    for box in stbl.children:
        if box.typ == b"stco":
            n = struct.unpack_from(">I", box.payload, 4)[0]
            offs = struct.unpack_from(f">{n}I", box.payload, 8)
            box.typ = b"co64"
            box.payload = box.payload[:8] + struct.pack(f">{n}Q", *offs)


def add_edit(media_time: int, duration: int, movie_timescale: int):
    """An edit list of one entry (version 0, rate 1.0), with mvhd's timescale
    set to movie_timescale, the units the edit's duration is in."""
    def edit(moov: Box) -> None:
        mvhd = moov.find(b"mvhd")
        if mvhd.payload[0] != 0:
            sys.exit("mvhd is not version 0")
        mvhd.payload = mvhd.payload[:12] + struct.pack(">II", movie_timescale, duration) + mvhd.payload[20:]
        trak = sound_trak(moov)
        trak.children = [c for c in trak.children if c.typ != b"edts"]
        elst = Box(b"elst", struct.pack(">IIIihh", 0, 1, duration, media_time, 1, 0))
        at = next(i for i, c in enumerate(trak.children) if c.typ == b"tkhd") + 1
        trak.children.insert(at, Box(b"edts", children=[elst]))
        tkhd = trak.find(b"tkhd")
        tkhd.payload = tkhd.payload[:20] + struct.pack(">I", duration) + tkhd.payload[24:]
    return edit


def edit_frames(duration: int, movie_timescale: int, rate: int) -> int:
    """Frames the edit covers whole: floor(duration * rate / movie)."""
    return duration * rate // movie_timescale


# --- fixtures ----------------------------------------------------------------


def seek_frames(frames: int) -> list[int]:
    # The start, inside the first packet, both sides of each packet boundary,
    # a third of the way, the last frame and the end.
    pts = {0, 1, 17, frames // 3, frames - 1, frames}
    for b in range(FRAME_LENGTH, frames, FRAME_LENGTH):
        pts |= {b - 1, b, b + 1}
    return sorted(p for p in pts if 0 <= p <= frames)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT)
    ap.add_argument("--codex", type=Path, help="Codex's B470 ALAC evaluation directory")
    args = ap.parse_args()
    out: Path = args.out
    out.mkdir(parents=True, exist_ok=True)
    for old in out.iterdir():
        if old.is_file() and old.name != "README.md":
            old.unlink()

    fixtures = []
    with tempfile.TemporaryDirectory() as td:
        scratch = Path(td)

        def encode(name: str, bits: int, rate: int, frames: int, notes: str) -> bytes:
            samples = pattern(frames, bits)
            wav = scratch / (name + ".wav")
            write_wav(wav, samples, bits, rate, 2)
            dst = out / (name + ".m4a")
            encode_alac(wav, dst)
            if decode_with_afconvert(dst, bits, scratch) != samples:
                sys.exit(f"{dst.name}: afconvert's decode differs from the pattern")
            fixtures.append(entry(dst, bits, rate, frames, 0, frames, "afconvert", notes))
            return dst.read_bytes()

        def entry(path: Path, bits: int, rate: int, pattern_frames: int, start: int, frames: int,
                  encoder: str, notes: str) -> dict:
            want = pattern(pattern_frames, bits)[2 * start:2 * (start + frames)]
            return {
                "file": path.name,
                "encoder": encoder,
                "notes": notes,
                "rate_hz": rate,
                "channels": 2,
                "valid_bits": bits,
                "pattern_frames": pattern_frames,
                "start_frame": start,
                "frames": frames,
                "samples_sha256": samples_sha256(want),
                "seek_frames": seek_frames(frames),
                "refusal": "",
            }

        base16 = encode("afconvert_16bit_44100", 16, 44100, 3 * FRAME_LENGTH + 137,
                        "16-bit; moov before mdat, one chunk, no edit list; a 137-frame final packet")
        base24 = encode("afconvert_24bit_96000", 24, 96000, 3 * FRAME_LENGTH + 137,
                        "24-bit at 96 kHz; a 137-frame final packet")
        encode("afconvert_24bit_192000", 24, 192000, 3 * FRAME_LENGTH + 1,
               "24-bit at 192 kHz; a one-frame final packet")
        encode("afconvert_32bit_44100", 32, 44100, 3 * FRAME_LENGTH - 1,
               "32-bit; a final packet one frame short of a whole one")

        # Container variants: the audio bytes are afconvert's, unchanged.
        name = "afconvert_24bit_96000_co64_moov_last.m4a"
        (out / name).write_bytes(rebuild(base24, widen_to_co64))
        fixtures.append(entry(out / name, 24, 96000, 3 * FRAME_LENGTH + 137, 0, 3 * FRAME_LENGTH + 137,
                              "afconvert, rewritten", "moov after mdat, chunk offsets in co64"))

        # An edit that trims 123 frames from the start and ends inside the
        # third packet; at a movie timescale of 1000 its 251 ms are 11069.1
        # frames at 44.1 kHz, of which 11069 are covered whole.
        media_time, duration, movie = 123, 251, 1000
        frames = edit_frames(duration, movie, 44100)
        name = "afconvert_16bit_44100_edit.m4a"
        (out / name).write_bytes(rebuild(base16, add_edit(media_time, duration, movie)))
        want = pattern(3 * FRAME_LENGTH + 137, 16)[2 * media_time:2 * (media_time + frames)]
        if decode_with_afconvert(out / name, 16, scratch) != want:
            sys.exit(f"{name}: afconvert's decode is not the edited timeline")
        fixtures.append(entry(out / name, 16, 44100, 3 * FRAME_LENGTH + 137, media_time, frames,
                              "afconvert, rewritten",
                              f"edit list: media time {media_time}, {duration} at a movie timescale of {movie}"))

        # Refusals: a real 20-bit ALAC, a mono ALAC and an AAC file.
        refusals = []
        short = 137
        wav = scratch / "r20.wav"
        write_wav(wav, [s << 12 for s in pattern(short, 20)], 32, 48000, 2)
        encode_alac(wav, out / "afconvert_20bit_48000.m4a", "/2")
        refusals.append(("afconvert_20bit_48000.m4a", "unsupported_bit_depth", "afconvert -d alac/2: 20-bit ALAC"))
        wav = scratch / "mono.wav"
        write_wav(wav, pattern(short, 16)[::2], 16, 44100, 1)
        encode_alac(wav, out / "afconvert_mono_44100.m4a")
        refusals.append(("afconvert_mono_44100.m4a", "unsupported_channel_count", "afconvert: mono ALAC"))
        wav = scratch / "aac.wav"
        write_wav(wav, pattern(4 * 1024, 16), 16, 44100, 2)
        aac = out / "afconvert_aac_44100.m4a"
        afconvert(["-f", "m4af", "-d", "aac", str(wav), str(aac)])
        aac.write_bytes(zero_times(aac.read_bytes()))
        refusals.append(("afconvert_aac_44100.m4a", "lossy_refused", "afconvert -d aac: AAC in M4A"))
        for fname, reason, notes in refusals:
            fixtures.append({"file": fname, "encoder": "afconvert", "notes": notes, "refusal": reason})

    codex = []
    if args.codex:
        q = json.loads((args.codex / "results" / "qualification.json").read_text())
        refs = {r["file"]: r for r in q}
        for fname, notes in [
            ("16b-44100hz-2ch-ffmpeg.m4a", "FFmpeg's encoder: mdat before moov, an edit list over the whole media"),
            ("variant-edit123.m4a", "the edit's media time set to 123 by Codex's containers.py; FFmpeg presents 28686 frames"),
        ]:
            r = refs["generated/" + fname]
            src = args.codex / "generated" / fname
            dst = out / ("ffmpeg_" + fname)
            shutil.copyfile(src, dst)
            fmt = r["format"]
            codex.append({
                "file": dst.name,
                "encoder": "FFmpeg (Lavf63.1.102), from Codex's B470 ALAC evaluation",
                "notes": notes,
                "source": "generated/" + fname,
                "file_sha256": hashlib.sha256(dst.read_bytes()).hexdigest(),
                "rate_hz": fmt["SampleRate"],
                "channels": fmt["Channels"],
                "valid_bits": fmt["BitDepth"],
                "frames": r["frames"],
                "pcm_bytes": r["reference"]["Bytes"],
                "pcm_sha256": r["reference"]["SHA256"],
            })

    manifest = {
        "frame_length": FRAME_LENGTH,
        "block": BLOCK,
        "fixtures": fixtures,
        "ffmpeg_fixtures": codex,
    }
    (out / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"wrote {len(fixtures)} fixtures and {len(codex)} FFmpeg fixtures to {out}")


if __name__ == "__main__":
    main()
