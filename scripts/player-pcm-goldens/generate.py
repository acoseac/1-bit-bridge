#!/usr/bin/env python3
"""Generate independent PCM goldens for the bridge player decoders.

Ordinary tests never run this. It needs flac(1), ffmpeg and (on macOS)
afconvert. It writes small fixtures under
internal/player/decode/testdata/goldens/ plus a manifest.json whose hashes
come from the reference tools, never from the bridge decoder.

Patterns cover extrema, low-bit activity, stereo decorrelation material for
FLAC, chunk-order WAV (bext before fmt), and lengths that exercise seeks.
"""

from __future__ import annotations

import hashlib
import json
import os
import struct
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
OUT = ROOT / "internal" / "player" / "decode" / "testdata" / "goldens"


def require(cmd: str) -> str:
    from shutil import which

    p = which(cmd)
    if not p:
        sys.exit(f"missing required tool: {cmd}")
    return p


FLAC = require("flac")
FFMPEG = require("ffmpeg")
AFCONVERT = which_afconvert = None
try:
    AFCONVERT = require("afconvert")
except SystemExit:
    AFCONVERT = None


def write_raw_le(path: Path, samples: list[int], bits: int) -> None:
    bps = bits // 8
    with path.open("wb") as f:
        for s in samples:
            if bits == 16:
                f.write(struct.pack("<h", s))
            elif bits == 24:
                u = s & 0xFFFFFF
                f.write(bytes((u & 0xFF, (u >> 8) & 0xFF, (u >> 16) & 0xFF)))
            elif bits == 32:
                f.write(struct.pack("<i", s))
            else:
                raise ValueError(bits)


def stereo_pattern(name: str, frames: int, bits: int) -> list[int]:
    """Known synthetic stereo PCM used as the bit-exact source."""
    lo = -(1 << (bits - 1))
    hi = (1 << (bits - 1)) - 1
    out: list[int] = []
    for i in range(frames):
        if name == "extrema":
            left = lo if i % 2 == 0 else hi
            right = hi if i % 2 == 0 else lo
        elif name == "lowbits":
            # Only the lowest few bits active.
            left = (i * 3) & 0x7
            right = -((i * 5) & 0x7)
            if left > hi:
                left = hi
            if right < lo:
                right = lo
        elif name == "ramp":
            span = hi - lo  # avoid % (2**bits) so -left never overflows int32
            left = (i * 17) % (span + 1) + lo
            right = -left if left != lo else hi
        elif name == "decorrelated":
            # Highly correlated mid with a small side — FLAC mid/side material.
            mid = (i * 13) % 1000 - 500
            side = (i % 7) - 3
            left = mid + side
            right = mid - side
            left = max(lo, min(hi, left))
            right = max(lo, min(hi, right))
        else:
            raise ValueError(name)
        out.extend([left, right])
    return out


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 16), b""):
            h.update(chunk)
    return h.hexdigest()


def hash_pcm_samples(samples: list[int], bits: int) -> str:
    """Hash the canonical interleaved int32 little-endian dump of samples."""
    h = hashlib.sha256()
    for s in samples:
        # Sign-extended int32 LE — the decoder's wire form.
        h.update(struct.pack("<i", s))
    return h.hexdigest()


def ref_decode_flac(path: Path, bits: int, frames: int) -> list[int]:
    raw = path.with_suffix(".ref.raw")
    subprocess.check_call(
        [FLAC, "-d", "-f", "-o", str(raw), "--force-raw-format",
         "--endian=little", "--sign=signed", str(path)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    data = raw.read_bytes()
    raw.unlink(missing_ok=True)
    return unpack_raw(data, bits, frames)


def ref_decode_ffmpeg(path: Path, bits: int, frames: int) -> list[int]:
    raw = path.with_suffix(".ref.raw")
    fmt = {16: "s16le", 24: "s24le", 32: "s32le"}[bits]
    subprocess.check_call(
        [FFMPEG, "-y", "-i", str(path), "-f", fmt, str(raw)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    data = raw.read_bytes()
    raw.unlink(missing_ok=True)
    return unpack_raw(data, bits, frames)


def unpack_raw(data: bytes, bits: int, frames: int) -> list[int]:
    bps = bits // 8
    need = frames * 2 * bps
    if len(data) < need:
        raise SystemExit(f"reference decode short: {len(data)} < {need}")
    data = data[:need]
    out: list[int] = []
    for i in range(0, need, bps):
        chunk = data[i : i + bps]
        if bits == 16:
            out.append(struct.unpack("<h", chunk)[0])
        elif bits == 24:
            v = chunk[0] | (chunk[1] << 8) | (chunk[2] << 16)
            if v & 0x800000:
                v -= 0x1000000
            out.append(v)
        elif bits == 32:
            out.append(struct.unpack("<i", chunk)[0])
    return out


def make_wav_ffmpeg(raw: Path, out: Path, rate: int, bits: int) -> None:
    fmt = {16: "s16le", 24: "s24le", 32: "s32le"}[bits]
    subprocess.check_call(
        [FFMPEG, "-y", "-f", fmt, "-ar", str(rate), "-ac", "2", "-i", str(raw),
         "-c:a", f"pcm_s{bits}le", str(out)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )


def make_wav_bext_before_fmt(raw_samples: list[int], bits: int, rate: int, out: Path) -> None:
    """Hand-rolled WAV with a bext chunk before fmt (recorder-like order)."""
    bps = bits // 8
    data = bytearray()
    for s in raw_samples:
        if bits == 16:
            data += struct.pack("<h", s)
        elif bits == 24:
            u = s & 0xFFFFFF
            data += bytes((u & 0xFF, (u >> 8) & 0xFF, (u >> 16) & 0xFF))
        else:
            data += struct.pack("<i", s)
    bext = b"bext" + struct.pack("<I", 60) + b"X" * 60
    fmt = b"fmt " + struct.pack("<IHHIIHH", 16, 1, 2, rate, rate * 2 * bps, 2 * bps, bits)
    data_chunk = b"data" + struct.pack("<I", len(data)) + data
    if len(data) & 1:
        data_chunk += b"\x00"
    body = bext + fmt + data_chunk
    out.write_bytes(b"RIFF" + struct.pack("<I", len(body) + 4) + b"WAVE" + body)


def make_flac(raw: Path, out: Path, rate: int, bits: int) -> None:
    subprocess.check_call(
        [FLAC, "--force", "--endian=little", "--sign=signed", "--channels=2",
         f"--bps={bits}", f"--sample-rate={rate}", "-o", str(out), str(raw)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )


def make_aiff(raw: Path, out: Path, rate: int, bits: int, sowt: bool) -> None:
    fmt = {16: "s16le", 24: "s24le", 32: "s32le"}[bits]
    if sowt:
        # ffmpeg emits AIFF-C sowt; afconvert's AIFC path is awkward for LEI*.
        codec = {16: "pcm_s16le", 24: "pcm_s24le", 32: "pcm_s32le"}[bits]
        subprocess.check_call(
            [FFMPEG, "-y", "-f", fmt, "-ar", str(rate), "-ac", "2", "-i", str(raw),
             "-c:a", codec, str(out)],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        return
    if AFCONVERT is not None:
        wav = out.with_suffix(".tmp.wav")
        make_wav_ffmpeg(raw, wav, rate, bits)
        d = {16: "BEI16", 24: "BEI24", 32: "BEI32"}[bits]
        subprocess.check_call(
            [AFCONVERT, "-f", "AIFF", "-d", d, str(wav), str(out)],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        wav.unlink(missing_ok=True)
        return
    codec = {16: "pcm_s16be", 24: "pcm_s24be", 32: "pcm_s32be"}[bits]
    subprocess.check_call(
        [FFMPEG, "-y", "-f", fmt, "-ar", str(rate), "-ac", "2", "-i", str(raw),
         "-c:a", codec, str(out)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )


def main() -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    for old in OUT.iterdir():
        if old.name in ("README.md",):
            continue
        if old.is_file():
            old.unlink()

    manifest = []
    cases = [
        ("extrema", 16, 44100, 512),
        ("extrema", 24, 48000, 512),
        ("lowbits", 16, 44100, 256),
        ("lowbits", 24, 96000, 256),
        ("ramp", 32, 44100, 400),
        ("decorrelated", 16, 44100, 2048),
        ("decorrelated", 24, 44100, 2048),
    ]

    with tempfile.TemporaryDirectory() as td:
        td_path = Path(td)
        for name, bits, rate, frames in cases:
            samples = stereo_pattern(name, frames, bits)
            sample_hash = hash_pcm_samples(samples, bits)
            raw = td_path / f"{name}_{bits}.raw"
            write_raw_le(raw, samples, bits)

            # FLAC via reference encoder. Skip 32-bit: mewkiz/flac v1.0.14
            # still treats frame sample-size code 111 as reserved.
            if bits != 32:
                flac_path = OUT / f"{name}_{bits}bit_{rate}.flac"
                make_flac(raw, flac_path, rate, bits)
                ref = ref_decode_flac(flac_path, bits, frames)
                if ref != samples:
                    mismatches = sum(1 for a, b in zip(ref, samples) if a != b)
                    sys.exit(f"FLAC ref mismatch for {flac_path.name}: {mismatches} samples differ")
                manifest.append({
                    "file": flac_path.name,
                    "container": "flac",
                    "rate_hz": rate,
                    "channels": 2,
                    "valid_bits": bits,
                    "frames": frames,
                    "samples_sha256": sample_hash,
                    "file_sha256": sha256_file(flac_path),
                    "pattern": name,
                    "seek_frames": [0, 1, frames // 3, max(0, frames - 1)],
                })

            # WAV via ffmpeg
            wav_path = OUT / f"{name}_{bits}bit_{rate}.wav"
            make_wav_ffmpeg(raw, wav_path, rate, bits)
            ref = ref_decode_ffmpeg(wav_path, bits, frames)
            if ref != samples:
                mismatches = sum(1 for a, b in zip(ref, samples) if a != b)
                sys.exit(f"WAV ref mismatch for {wav_path.name}: {mismatches} samples differ")
            manifest.append({
                "file": wav_path.name,
                "container": "wav",
                "rate_hz": rate,
                "channels": 2,
                "valid_bits": bits,
                "frames": frames,
                "samples_sha256": sample_hash,
                "file_sha256": sha256_file(wav_path),
                "pattern": name,
                "seek_frames": [0, frames // 2],
            })

            # AIFF / AIFC sowt
            aiff_path = OUT / f"{name}_{bits}bit_{rate}.aiff"
            make_aiff(raw, aiff_path, rate, bits, sowt=False)
            ref = ref_decode_ffmpeg(aiff_path, bits, frames)
            if ref != samples:
                mismatches = sum(1 for a, b in zip(ref, samples) if a != b)
                sys.exit(f"AIFF ref mismatch for {aiff_path.name}: {mismatches} samples differ")
            manifest.append({
                "file": aiff_path.name,
                "container": "aiff",
                "rate_hz": rate,
                "channels": 2,
                "valid_bits": bits,
                "frames": frames,
                "samples_sha256": sample_hash,
                "file_sha256": sha256_file(aiff_path),
                "pattern": name,
                "seek_frames": [0, frames // 4],
            })

            if bits == 16:
                sowt_path = OUT / f"{name}_{bits}bit_{rate}_sowt.aifc"
                make_aiff(raw, sowt_path, rate, bits, sowt=True)
                ref = ref_decode_ffmpeg(sowt_path, bits, frames)
                if ref != samples:
                    mismatches = sum(1 for a, b in zip(ref, samples) if a != b)
                    sys.exit(f"sowt ref mismatch for {sowt_path.name}: {mismatches} samples differ")
                manifest.append({
                    "file": sowt_path.name,
                    "container": "aiff",
                    "rate_hz": rate,
                    "channels": 2,
                    "valid_bits": bits,
                    "frames": frames,
                    "samples_sha256": sample_hash,
                    "file_sha256": sha256_file(sowt_path),
                    "pattern": name + "+sowt",
                    "seek_frames": [0, 7],
                })

        # Chunk-order WAV: bext before fmt
        samples = stereo_pattern("ramp", 128, 16)
        bext_path = OUT / "chunkorder_bext_before_fmt_16bit_44100.wav"
        make_wav_bext_before_fmt(samples, 16, 44100, bext_path)
        ref = ref_decode_ffmpeg(bext_path, 16, 128)
        if ref != samples:
            sys.exit("bext-before-fmt WAV ref mismatch")
        manifest.append({
            "file": bext_path.name,
            "container": "wav",
            "rate_hz": 44100,
            "channels": 2,
            "valid_bits": 16,
            "frames": 128,
            "samples_sha256": hash_pcm_samples(samples, 16),
            "file_sha256": sha256_file(bext_path),
            "pattern": "chunkorder-bext-before-fmt",
            "seek_frames": [0, 64],
        })

    (OUT / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"wrote {len(manifest)} goldens to {OUT}")


def which(cmd: str):
    from shutil import which as w
    return w(cmd)


if __name__ == "__main__":
    main()
