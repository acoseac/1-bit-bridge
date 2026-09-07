#!/usr/bin/env python3
"""Generate the DSD64 measurement fixtures for the DSD → PCM render tests.

Pure stdlib on purpose — no numpy. The dev machine this was written on has
none, and a fixture generator that needs a scientific stack is a fixture
generator nobody re-runs. `array` + `math` is enough for a 5th-order
modulator at 2.8 MHz; the whole set takes a couple of minutes.

WHY 5th ORDER. The Go generator in the render tests is FIRST order, whose
own in-band noise floor sits near -45 dBFS at 64x oversampling. That is
fine for "did the level survive the chain" and useless for the question
these fixtures exist to answer — whether the decimation lets the DSD
noise shelf fold into the baseband. A probe whose own floor is 60 dB
above the spur you are hunting cannot see the spur.

WHAT THIS LOOP ACTUALLY DELIVERS, measured rather than hoped for. An
earlier version of this comment claimed a 5th-order loop puts the in-band
floor "below -120 dBFS", and the tests were written to assert an absolute
-110 dBFS bar on that basis. Both were wrong: rendered through the real
chain the fixtures carry about -104 dBFS of their OWN content near
20 kHz — partly the NTF rising toward the band edge, mostly odd-order
harmonics of the test tone (the 7th, 9th, 15th, 17th and 19th are all
visible, which is ordinary for a 1-bit quantizer). An absolute bar
therefore measures the modulator, not the decoder, which is why
TestDSDRender_AliasRejection is a DIFFERENTIAL against a chain that
filters while still massively oversampled. Whatever the fixture brings,
both sides bring equally, and it subtracts out.

That makes the fixtures fit for purpose without being laboratory-grade,
and the -104 dBFS figure is still 90 dB below the tone — ample to see a
folded shelf, which would arrive tens of dB higher.

The modulator is a 5th-order low-distortion CIFF (Cascade of Integrators,
FeedForward) with a 1-bit quantizer — see the comment on the coefficients
below for why that form and not CIFB. Coefficients are conservative on
purpose: stability matters more here than squeezing the last few dB of
SNR, because an unstable loop produces a fixture that measures the
modulator rather than the decoder.

Usage:
    python3 dsd_fixtures.py [outdir]

Writes three 1-second stereo DSD64 .dsf files (~705 KB each):

    dsd64-1khz-m6dbfs.dsf     level parity + the gain the chain bakes in
    dsd64-50khz-alias.dsf     a tone ABOVE the audio band; anything that
                              appears in 0-20 kHz after decimation is an
                              alias the pipeline created
    dsd64-ccif-19-20khz.dsf   twin tone; the 1 kHz difference product
                              measures intermodulation under HF load
"""

import array
import math
import os
import struct
import sys

DSD64_RATE = 2822400
BLOCK = 4096  # DSF block size per channel, in bytes
CHANNELS = 2
SECONDS = 1

# 5th-order low-distortion CIFF (Silva-Steensgaard): a cascade of
# integrators whose outputs are summed FORWARD into the quantizer, with
# the input fed forward too. That last term is what makes the loop
# low-distortion and unity-gain: at DC the quantizer already sees the
# input, so the integrators only ever carry the ERROR and never wind up
# tracking the signal itself.
#
# The structure was verified before these fixtures were generated, which
# is the only reason to trust them: with a DC input the output mean
# equals the input EXACTLY (mean(e) = 0.000000 over 200k samples) and
# every integrator state stays bounded well under 1. A first attempt used
# CIFB with feedback at all five integrators and measured a DC gain of
# 0.257 — stable, linear, and quietly wrong by 12 dB, which a fixture
# generator will happily bake into every measurement downstream.
B = (0.40, 0.20, 0.10, 0.05, 0.02)  # integrator gains, descending
A = (1.0, 1.0, 1.0, 1.0, 1.0)       # feed-forward weights into the quantizer

# Input ceiling. SACD authoring leaves DSD about 6 dB below PCM full
# scale, and a 5th-order 1-bit loop needs that headroom anyway — its
# stable input range runs out well before nominal full scale.
FULL_SCALE = 0.5


def modulate(sample_fn, n):
    """Run the 5th-order loop over n samples, returning a bytearray of 0/1.

    sample_fn(i) -> float in [-1, 1] BEFORE the FULL_SCALE ceiling.
    """
    s0 = s1 = s2 = s3 = s4 = 0.0
    out = bytearray(n)
    b0, b1, b2, b3, b4 = B
    a0, a1, a2, a3, a4 = A
    for i in range(n):
        u = sample_fn(i) * FULL_SCALE
        v = u + a0 * s0 + a1 * s1 + a2 * s2 + a3 * s3 + a4 * s4
        if v >= 0.0:
            y = 1.0
            out[i] = 1
        else:
            y = -1.0
        e = u - y
        # Updated from the top down so each integrator consumes the
        # PREVIOUS sample's value of the one below it — the unit delay
        # every integrator in the chain is supposed to carry.
        s4 += b4 * s3
        s3 += b3 * s2
        s2 += b2 * s1
        s1 += b1 * s0
        s0 += b0 * e
    return out


def tone(freq_hz, amp=1.0):
    w = 2.0 * math.pi * freq_hz / DSD64_RATE
    return lambda i: amp * math.sin(w * i)


def twin_tone(f1, f2, amp=0.5):
    w1 = 2.0 * math.pi * f1 / DSD64_RATE
    w2 = 2.0 * math.pi * f2 / DSD64_RATE
    return lambda i: amp * (math.sin(w1 * i) + math.sin(w2 * i))


def pack_lsb_first(bits):
    """DSF packs 8 consecutive samples per byte, EARLIEST sample in bit 0.

    That LSB-first order is the DSF container's convention and the reason
    DoPPacker takes an `inputIsLSBFirst` flag — DSDIFF is the other way
    round. Getting it backwards here would produce a file that decodes to
    noise, which the level fixture would catch immediately.
    """
    out = bytearray(len(bits) // 8)
    for byte_i in range(len(out)):
        b = 0
        base = byte_i * 8
        for k in range(8):
            if bits[base + k]:
                b |= 1 << k
        out[byte_i] = b
    return out


def write_dsf(path, ch_bytes, rate=DSD64_RATE):
    """Write a stereo DSF. ch_bytes is a list of per-channel byte arrays.

    Block-interleaved: BLOCK bytes of channel 0, then BLOCK of channel 1,
    repeating, with the final block zero-padded — the layout the DSF spec
    requires and the one DSFParser reads.
    """
    n_ch = len(ch_bytes)
    per_ch = len(ch_bytes[0])
    blocks = (per_ch + BLOCK - 1) // BLOCK
    data_len = blocks * BLOCK * n_ch
    sample_count = per_ch * 8  # samples per channel

    fmt_chunk = struct.pack(
        "<4sQIIIIIIQII",
        b"fmt ", 52,
        1,            # format version
        0,            # format id: DSD raw
        2,            # channel type: stereo
        n_ch,
        rate,
        1,            # bits per sample
        sample_count,
        BLOCK,
        0,            # reserved
    )
    data_hdr = struct.pack("<4sQ", b"data", 12 + data_len)
    total = 28 + len(fmt_chunk) + len(data_hdr) + data_len
    dsd_chunk = struct.pack("<4sQQQ", b"DSD ", 28, total, 0)  # 0 = no metadata

    with open(path, "wb") as f:
        f.write(dsd_chunk)
        f.write(fmt_chunk)
        f.write(data_hdr)
        for b in range(blocks):
            lo = b * BLOCK
            for ch in range(n_ch):
                chunk = ch_bytes[ch][lo:lo + BLOCK]
                f.write(chunk)
                if len(chunk) < BLOCK:
                    f.write(b"\x00" * (BLOCK - len(chunk)))


def build(path, sample_fn, label):
    n = DSD64_RATE * SECONDS
    print(f"  {label}: modulating {n} samples/channel …", flush=True)
    bits = modulate(sample_fn, n)
    packed = pack_lsb_first(bits)
    # Both channels identical: these fixtures measure the decode chain,
    # not stereo separation, and a shared channel halves generation time.
    write_dsf(path, [packed, packed])
    print(f"  {label}: wrote {os.path.getsize(path)} bytes -> {path}", flush=True)


def resolve_outdir(raw):
    """Resolve the output directory, and require it to already exist.

    The generator writes three ~700 KB files. Requiring an existing
    directory rather than creating one turns a mistyped argument into an
    immediate error instead of a stray tree somewhere, and gives every
    later join a single resolved base to be checked against.
    """
    base = os.path.realpath(raw)
    if not os.path.isdir(base):
        raise SystemExit(f"output directory does not exist: {base}")
    return base


def out_path(base, name):
    """Join a fixture name onto the resolved base and re-check the result.

    `name` is a literal below, so this cannot currently escape — the check
    is here so that stays true if someone later derives a name from input,
    and so the path handed to open() is one that has been validated rather
    than merely constructed.
    """
    p = os.path.realpath(os.path.join(base, name))
    if os.path.dirname(p) != base:
        raise SystemExit(f"refusing to write outside {base}: {p}")
    return p


def main():
    default = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..")
    outdir = resolve_outdir(sys.argv[1] if len(sys.argv) > 1 else default)
    print(f"writing DSD64 fixtures to {outdir}")
    build(out_path(outdir, "dsd64-1khz-m6dbfs.dsf"), tone(1000.0), "1 kHz -6 dBFS")
    build(out_path(outdir, "dsd64-50khz-alias.dsf"), tone(50000.0), "50 kHz alias probe")
    build(out_path(outdir, "dsd64-ccif-19-20khz.dsf"), twin_tone(19000.0, 20000.0), "CCIF 19+20 kHz")
    print("done")


if __name__ == "__main__":
    main()
