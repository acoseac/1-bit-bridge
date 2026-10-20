#!/usr/bin/env python3
"""Generate independent DSD goldens for the bridge player's DSF and DFF demuxers.

Ordinary tests never run this, and it needs nothing outside the standard
library. It writes small DSF and DFF fixtures under
internal/player/decode/testdata/dsd-goldens/, the canonical per-channel bytes
they carry (pattern.bin), and manifest.json.

The containers are written here from the format specifications (Sony's DSF
File Format Specification 1.01 and Philips' DSDIFF 1.5), never by the bridge.
The audio is a pattern specified independently of any decoder, in the
canonical form the decoder must produce: channel-separated bytes, bit 7
earliest in time. The pattern is chosen so that every mistake a demuxer can
make shows as a byte mismatch:

- no byte is a bit palindrome, so a missing or extra bit reversal changes
  every byte;
- left and right differ at every index, so a channel swap changes every byte;
- 0x00 never occurs, so the zero padding of DSF's final block (or any other
  zero filler) cannot pass for audio.

An LSB-first DSF stores each canonical byte bit-reversed; an MSB-first DSF and
a DFF store it unchanged. The test compares the decoder's output with
pattern.bin byte for byte.
"""

from __future__ import annotations

import json
import struct
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
OUT = ROOT / "internal" / "player" / "decode" / "testdata" / "dsd-goldens"

DSD64 = 2_822_400
BLOCK = 4096  # DSF per-channel block size, fixed by the specification


def bitrev(b: int) -> int:
    return int(f"{b:08b}"[::-1], 2)


NON_PALINDROMES = [b for b in range(256) if bitrev(b) != b]
assert len(NON_PALINDROMES) == 240 and 0 not in NON_PALINDROMES


def pattern(n: int) -> tuple[bytes, bytes]:
    """Canonical left and right bytes (bit 7 earliest)."""
    left = bytes(NON_PALINDROMES[(7 * i) % 240] for i in range(n))
    right = bytes(NON_PALINDROMES[(11 * i + 3) % 240] for i in range(n))
    for i in range(n):
        assert left[i] != right[i], i  # 4i+3 is odd, never 0 mod 240
        assert bitrev(left[i]) != left[i] and bitrev(right[i]) != right[i]
    return left, right


# ---------------------------------------------------------------- DSF (LE)


def le32(v: int) -> bytes:
    return struct.pack("<I", v)


def le64(v: int) -> bytes:
    return struct.pack("<Q", v)


def dsf(
    channels: list[bytes],
    *,
    rate: int,
    bits_per_sample: int,
    sample_count: int,
    channel_type: int,
    metadata: bytes = b"",
) -> bytes:
    """A DSF file: DSD chunk, fmt chunk, block-interleaved data, metadata."""
    per_channel = len(channels[0])
    assert all(len(c) == per_channel for c in channels)
    blocks = -(-per_channel // BLOCK)
    data = bytearray()
    for b in range(blocks):
        for ch in channels:
            seg = ch[b * BLOCK : (b + 1) * BLOCK]
            if bits_per_sample == 1:  # LSB first: bit 0 is earliest
                seg = bytes(bitrev(x) for x in seg)
            data += seg + b"\x00" * (BLOCK - len(seg))  # final block padding
    fmt = (
        b"fmt "
        + le64(52)
        + le32(1)  # format version
        + le32(0)  # format id: DSD raw
        + le32(channel_type)
        + le32(len(channels))
        + le32(rate)
        + le32(bits_per_sample)
        + le64(sample_count)
        + le32(BLOCK)
        + le32(0)  # reserved
    )
    data_chunk = b"data" + le64(12 + len(data)) + bytes(data)
    body_end = 28 + len(fmt) + len(data_chunk)
    meta_ptr = body_end if metadata else 0
    head = b"DSD " + le64(28) + le64(body_end + len(metadata)) + le64(meta_ptr)
    return head + fmt + data_chunk + metadata


def id3_tag(text: bytes) -> bytes:
    """A small ID3v2.3 tag with one TIT2 frame; its bytes must never be audio."""
    frame_body = b"\x00" + text
    frame = b"TIT2" + struct.pack(">I", len(frame_body)) + b"\x00\x00" + frame_body
    size = len(frame)
    synchsafe = bytes(((size >> s) & 0x7F) for s in (21, 14, 7, 0))
    return b"ID3\x03\x00\x00" + synchsafe + frame


# ---------------------------------------------------------------- DFF (BE)


def be16(v: int) -> bytes:
    return struct.pack(">H", v)


def be32(v: int) -> bytes:
    return struct.pack(">I", v)


def be64(v: int) -> bytes:
    return struct.pack(">Q", v)


def chunk(ck_id: bytes, body: bytes) -> bytes:
    """A DSDIFF chunk; an odd body is followed by one pad byte not counted."""
    assert len(ck_id) == 4
    return ck_id + be64(len(body)) + body + (b"\x00" if len(body) % 2 else b"")


def pstring(text: bytes) -> bytes:
    """Count byte plus text, padded to an even total length."""
    s = bytes([len(text)]) + text
    return s + (b"\x00" if len(s) % 2 else b"")


def comt(text: bytes) -> bytes:
    """COMT with one comment; text length chosen odd to exercise the pad."""
    comment = (
        be16(2026) + bytes([10, 20, 12, 0])  # year, month, day, hour, minutes
        + be16(0)  # cmtType: general
        + be16(0)  # cmtRef
        + be32(len(text))
        + text
    )
    return chunk(b"COMT", be16(1) + comment)


def dff(
    channel_ids: list[bytes],
    interleaved: bytes,
    *,
    rate: int,
    compression: bytes = b"DSD ",
    compression_name: bytes = b"not compressed",
    sound_id: bytes = b"DSD ",
) -> bytes:
    prop = chunk(
        b"PROP",
        b"SND "
        + chunk(b"FS  ", be32(rate))
        + chunk(b"CHNL", be16(len(channel_ids)) + b"".join(channel_ids))
        + chunk(b"CMPR", compression + pstring(compression_name))
        + chunk(b"ABSS", be16(0) + bytes([0, 0]) + be32(0))
        + chunk(b"LSCO", be16(0)),
    )
    body = (
        b"DSD "
        + chunk(b"FVER", be32(0x01050000))
        + comt(b"B470 golden, odd length")
        + prop
        + chunk(sound_id, interleaved)
        + chunk(b"DIIN", chunk(b"DIAR", be32(5) + b"1-bit"))
        + chunk(b"ID3 ", id3_tag(b"not audio\xff\xff\xff"))
    )
    return b"FRM8" + be64(len(body)) + body


def interleave(channels: list[bytes]) -> bytes:
    out = bytearray()
    for frame in zip(*channels):
        out += bytes(frame)
    return bytes(out)


# ---------------------------------------------------------------- fixtures


def main() -> None:
    OUT.mkdir(parents=True, exist_ok=True)
    full = 3 * BLOCK + 1000  # three whole blocks and a partial fourth
    left, right = pattern(full)
    (OUT / "pattern.bin").write_bytes(left + right)

    entries = []

    def add(name: str, raw: bytes, **meta) -> None:
        (OUT / name).write_bytes(raw)
        entries.append({"file": name, **meta})

    def seeks(per_channel: int) -> list[int]:
        pts = {0, 1, 1234, BLOCK - 6, BLOCK, BLOCK + 1, per_channel - 1, per_channel}
        return sorted(p for p in pts if 0 <= p <= per_channel)

    def ok(name, raw, *, container, rate, per_channel, frames, order):
        add(
            name,
            raw,
            container=container,
            bit_rate_hz=rate,
            channels=2,
            frames=frames,
            bytes_per_channel=per_channel,
            source_bit_order=order,
            seek_bytes=seeks(per_channel),
        )

    L, R = left[:full], right[:full]
    ok(
        "lsb_dsd64_partial_block.dsf",
        dsf([L, R], rate=DSD64, bits_per_sample=1, sample_count=8 * full,
            channel_type=2, metadata=id3_tag(b"metadata, not audio")),
        container="dsf", rate=DSD64, per_channel=full, frames=8 * full, order="lsb",
    )
    ok(
        "msb_dsd128_partial_block.dsf",
        dsf([L, R], rate=2 * DSD64, bits_per_sample=8, sample_count=8 * full,
            channel_type=2),
        container="dsf", rate=2 * DSD64, per_channel=full, frames=8 * full, order="msb",
    )
    exact = 2 * BLOCK
    ok(
        "lsb_dsd64_whole_blocks.dsf",
        dsf([left[:exact], right[:exact]], rate=DSD64, bits_per_sample=1,
            sample_count=8 * exact, channel_type=2),
        container="dsf", rate=DSD64, per_channel=exact, frames=8 * exact, order="lsb",
    )
    # A sample count that is not a whole number of bytes: 797 one-bit samples
    # are 100 bytes, the last carrying three trailing bits of padding.
    ok(
        "lsb_dsd64_797_samples.dsf",
        dsf([left[:100], right[:100]], rate=DSD64, bits_per_sample=1,
            sample_count=797, channel_type=2),
        container="dsf", rate=DSD64, per_channel=100, frames=797, order="lsb",
    )
    ok(
        "dsd64.dff",
        dff([b"SLFT", b"SRGT"], interleave([L, R]), rate=DSD64),
        container="dff", rate=DSD64, per_channel=full, frames=8 * full, order="msb",
    )
    # CHNL lists the right channel first: the file interleaves R, L and the
    # decoder must still put the left channel first.
    ok(
        "dsd256_right_first.dff",
        dff([b"SRGT", b"SLFT"], interleave([R, L]), rate=4 * DSD64),
        container="dff", rate=4 * DSD64, per_channel=full, frames=8 * full, order="msb",
    )

    def refused(name, raw, reason):
        add(name, raw, refusal=reason)

    small = BLOCK
    refused(
        "mono.dsf",
        dsf([left[:small]], rate=DSD64, bits_per_sample=1, sample_count=8 * small,
            channel_type=1),
        "unsupported_channel_count",
    )
    six = [left[:small], right[:small]] * 3
    refused(
        "six_channel.dsf",
        dsf(six, rate=DSD64, bits_per_sample=1, sample_count=8 * small, channel_type=7),
        "unsupported_channel_count",
    )
    # DFF has no block size, so its refusal fixtures stay small.
    refused(
        "mono.dff",
        dff([b"C   "], left[:64], rate=DSD64),
        "unsupported_channel_count",
    )
    refused(
        "six_channel.dff",
        dff([b"MLFT", b"MRGT", b"C   ", b"LFE ", b"LS  ", b"RS  "],
            interleave([c[:64] for c in six]), rate=DSD64),
        "unsupported_channel_count",
    )
    # DST: CMPR says DST and the sound data is a DST chunk of frames.
    dst_body = chunk(b"FRTE", be32(1) + be16(75)) + chunk(b"DSTF", b"\x5a" * 33)
    refused(
        "dst.dff",
        dff([b"SLFT", b"SRGT"], dst_body, rate=DSD64, compression=b"DST ",
            compression_name=b"DST Encoded", sound_id=b"DST "),
        "unsupported_compression",
    )

    manifest = {
        "pattern_file": "pattern.bin",
        "pattern_bytes_per_channel": full,
        "fixtures": entries,
    }
    (OUT / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    print(f"wrote {len(entries)} fixtures to {OUT}")


if __name__ == "__main__":
    main()
