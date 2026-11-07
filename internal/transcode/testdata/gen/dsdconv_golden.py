#!/usr/bin/env python3
"""An independent golden for internal/player/dsdconv (B728 B10): the chain's
coefficients and converted samples, computed from its written design here,
never from the package's code.

Pure stdlib, like dsd_fixtures.py beside it, whose modulate() makes the
input. Nothing here calls a function a libm implements. The filter design
uses integers, fractions and floats with + - * / and sqrt alone: math.sqrt
is IEEE 754's correctly rounded square root, CPython rounds every float
operation on its own and fuses none, and the sine and the Bessel function
are their series, so the taps are the same on every machine. The input is a
rotation recurrence from hex constants, through the generator's modulator.
The chain after the design is integer.

Usage:
    python3 -I internal/transcode/testdata/gen/dsdconv_golden.py

It prints the SHA-256 of each filter's taps (int64 little-endian, in index
order) and, for each plan, of the input (one byte a one-bit sample, 0 or 1,
the left channel's then the right's) and of the output (int32 little-endian,
interleaved left, right), with the output's clipped samples and exact ties.
TestTheChainIsTheIndependentToolsGolden holds the package to those lines.

THE DESIGN, as B728 section 4.2 and the package's documentation give it.

Kaiser's window and estimate, A = 110 dB at every stage:
  beta = 0.1102 * (A - 8.7);
  taps(df, fs) = ceil((A - 7.95) / (2.285 * ((2 pi * df) / fs))) + 1;
  I0(x) = 1 + term_1 + term_2 + ..., term_k = (term_{k-1} * (x/2)^2) / k^2,
  added in order until a term falls below the sum / 1e21.
The sine of t turns, sin(2 pi t): t less its floor; from a half turn on, t
less a half and the sign negated; past a quarter, a half less t; then
x = 2 pi * t and x - x^3/3! + x^5/5! - ... to x^27/27!, each term the
previous times x^2, then divided by (2k)(2k+1) for k = 1..13, added in order.
A low-pass of N taps (N odd, centre c = (N - 1) / 2) cut at fc cycles a
sample: for k = 0..c, v_k = 2 fc at k = 0 and sin(2 pi fc k) / (pi k)
after; w_k = I0(beta * sqrt(1 - (k/c)^2)) / I0(beta); h_k = v_k * w_k;
s = h_0 + 2 h_1 + ... + 2 h_c, added in that order; q_k = h_k / s * 2^30
rounded half away from zero, taps c - k and c + k both; then the centre
gains 2^30 less the q's total, so the filter sums to exactly 2^30.

The stages, keep = 40 kHz (the widest plan's stopband):
  1. one bit at r to 352.8 kHz, M1 = r / 352800: n = taps(352800 - 2 keep,
     r); T = ceil((n + 1) / 8), made even by adding one; a low-pass of
     8T - 1 taps cut at 176400 / r, in slots 1..8T-1 of a window of 8T
     slots, slot 0 zero, centred on slot 4T.
  2. 352.8 to 176.4 kHz: n = taps(176400 - 2 keep, 352800); N = 3, 7, 11,
     ... the first 4L - 1 not under n; a low-pass cut at 1/4, the half-band.
  3. at 176.4 kHz from pass to stop (30 to 40 kHz, or 20 to 22.05 kHz for
     44.1 kHz): N = taps(stop - pass, 176400), made odd by adding one; a
     low-pass cut at (pass + stop) / (2 * 176400).

THE CHAIN, a channel of F one-bit samples at r, to a plan's rate:
  b(n) = +1 for a 1 bit and -1 for a 0, and 0 for n < 0 or n >= F: numeric
  zero at both edges, never zero bits;
  s1(m) = sum over slots i of g1[i] * b(m M1 - 4T + i), centred on bit m M1;
  s2(p) = sum over j of g2[j] * s1(2p - c2 + j), rounded half to even to
  2^30 for 1.0, centred on s1(2p);
  acc(k) = sum over i of g3[i] * s2(M3 k - c3 + i), M3 = 176400 / rate, for
  k = 0 .. F/D - 1, D = r / rate: frame k is centred on bit k D;
  24 bits: acc / 2^37 rounded half to even; 16 bits: (acc + d * 2^13) / 2^45
  rounded half to even, d = (x mod 2^32) - (x div 2^32), x stepped before
  each sample, the left's then the right's (x ^= x << 13, x ^= x >> 7,
  x ^= x << 17, mod 2^64), seeded 0x9E3779B97F4A7C15 at the occurrence's
  first frame: TPDF of +-1 LSB;
  saturated at +-(2^(bits-1) - 1), and counted.

THE INPUT, for each plan at DSD64, F = 2000 D bits a channel: the left is
modulate() over a rotation at 1 kHz, the right over one at 15 kHz; then the
bits of frames 750 to 1124 (bits 750 D to 1125 D - 1) are the byte TIE over
and over on the left and its complement on the right, whose stage-1 sum S
has S mod 128 = 64, so the 24-bit output of a frame whose window lies in
them is half an LSB past a step, a tie; and the bits of frames 1500 to 1749
are all ones on the left (+1.0, which saturates) and all zeros on the right
(-1.0).
"""

import hashlib
import importlib.util
import math
import os
import struct
import sys
from fractions import Fraction

A_DB = 110
SCALE = 1 << 30
DSD64 = 2822400
STAGE1_RATE = 352800
STAGE2_RATE = 176400
KEEP_HZ = 40000
TWO_PI = 2 * math.pi
DITHER_SEED = 0x9E3779B97F4A7C15
MASK64 = (1 << 64) - 1
TIE = 0x1C  # three ones in eight: -0.25, a DC whose stage-1 sum is 64 mod 128

# cos and sin of 2 pi f / 2822400, as hex: the rotation's only constants.
ROTATIONS = {
    "left": (1000, float.fromhex("0x1.ffffacda93a18p-1"), float.fromhex("0x1.23ca52113ba91p-9")),
    "right": (15000, float.fromhex("0x1.ffb6edd6c9eabp-1"), float.fromhex("0x1.1180b826db0f3p-5")),
}

PLANS = ((176400, 24), (44100, 16))
FRAMES = 2000
TIE_FRAMES = (750, 1125)
FULL_FRAMES = (1500, 1750)


def ceil_exact(x):
    return -((-Fraction(x)) // 1)


def round_half_away(x):
    f = Fraction(x)
    n = (abs(f) + Fraction(1, 2)) // 1
    return n if f >= 0 else -n


def round_half_even(x, shift):
    q, r = divmod(x, 1 << shift)
    half = 1 << (shift - 1)
    if r > half or (r == half and q % 2 == 1):
        q += 1
    return q, r == half


def sin_turns(t):
    t = t - (Fraction(t) // 1)
    neg = t >= 0.5
    if neg:
        t = t - 0.5
    if t > 0.25:
        t = 0.5 - t
    x = TWO_PI * t
    x2 = x * x
    term = total = x
    for k in range(1, 14):
        term = (term * x2) / ((2 * k) * (2 * k + 1))
        total = total - term if k % 2 == 1 else total + term
    return -total if neg else total


def bessel_i0(x):
    half = x / 2
    q = half * half
    term = total = 1.0
    for k in range(1, 500):
        term = (term * q) / (k * k)
        total = total + term
        if term < total / 1e21:
            break
    return total


def kaiser_beta():
    return 0.1102 * (A_DB - 8.7)


def kaiser_taps(df, fs):
    return ceil_exact((A_DB - 7.95) / (2.285 * ((TWO_PI * df) / fs))) + 1


def lowpass(n, fc_num, fc_den):
    c = (n - 1) // 2
    fc = fc_num / fc_den
    beta = kaiser_beta()
    ib = bessel_i0(beta)
    h = []
    for k in range(c + 1):
        v = 2 * fc if k == 0 else sin_turns(fc * k) / (math.pi * k)
        r = k / c
        w = bessel_i0(beta * math.sqrt(1 - r * r)) / ib
        h.append(v * w)
    s = h[0]
    for k in range(1, c + 1):
        s = s + 2 * h[k]
    taps = [0] * n
    for k in range(c + 1):
        q = round_half_away((h[k] / s) * float(SCALE))
        taps[c - k] = taps[c + k] = q
    taps[c] += SCALE - sum(taps)
    return taps


def stage1(rate):
    n = kaiser_taps(STAGE1_RATE - 2 * KEEP_HZ, rate)
    t = ceil_exact(Fraction(n + 1, 8))
    if t % 2:
        t += 1
    return [0] + lowpass(8 * t - 1, STAGE1_RATE // 2, rate), t


def stage2():
    n = kaiser_taps(STAGE2_RATE - 2 * KEEP_HZ, STAGE1_RATE)
    taps = 3
    while taps < n:
        taps += 4
    return lowpass(taps, 1, 4)


def stage3(pass_hz, stop_hz):
    n = kaiser_taps(stop_hz - pass_hz, STAGE2_RATE)
    if n % 2 == 0:
        n += 1
    return lowpass(n, pass_hz + stop_hz, 2 * STAGE2_RATE)


def band(rate_hz):
    return (20000, 22050) if rate_hz == 44100 else (30000, 40000)


def tie_sum(g1, byte):
    """Stage 1's sum over a window of the byte over and over."""
    return sum(g * (1 if (byte >> (7 - i % 8)) & 1 else -1) for i, g in enumerate(g1))


def convert(channels, rate, plan_rate, bits, g1, t, g2, g3):
    m1 = rate // STAGE1_RATE
    m3 = STAGE2_RATE // plan_rate
    c2 = (len(g2) - 1) // 2
    c3 = (len(g3) - 1) // 2
    f_bits = len(channels[0])
    frames = f_bits // (rate // plan_rate)
    p_lo, p_hi = -c3, m3 * (frames - 1) + c3
    m_lo, m_hi = 2 * p_lo - c2, 2 * p_hi + c2
    accs, ties2 = [], 0
    for ch in channels:
        def b(n):
            if n < 0 or n >= f_bits:
                return 0
            return 1 if ch[n] else -1
        s1 = {m: sum(g * b(m * m1 - 4 * t + i) for i, g in enumerate(g1) if g) for m in range(m_lo, m_hi + 1)}
        s2 = {}
        for p in range(p_lo, p_hi + 1):
            v, tie = round_half_even(sum(g * s1[2 * p - c2 + j] for j, g in enumerate(g2) if g), 30)
            s2[p] = v
            ties2 += tie
        accs.append([sum(g * s2[m3 * k - c3 + i] for i, g in enumerate(g3)) for k in range(frames)])
    out, clipped, ties = [], 0, 0
    top = (1 << (bits - 1)) - 1
    x = DITHER_SEED
    for k in range(frames):
        for acc in (accs[0][k], accs[1][k]):
            if bits == 16:
                x ^= (x << 13) & MASK64
                x ^= x >> 7
                x ^= (x << 17) & MASK64
                d = (x & 0xFFFFFFFF) - (x >> 32)
                v, tie = round_half_even(acc + d * (1 << 13), 45)
            else:
                v, tie = round_half_even(acc, 37)
            ties += tie
            if v > top:
                v, clipped = top, clipped + 1
            elif v < -top:
                v, clipped = -top, clipped + 1
            out.append(v)
    return out, clipped, ties, ties2


def load_modulate():
    here = os.path.dirname(os.path.abspath(__file__))
    spec = importlib.util.spec_from_file_location("dsd_fixtures", os.path.join(here, "dsd_fixtures.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod.modulate


def rotation(c, s):
    state = [0.0, 1.0]  # sin, cos

    def sample(i):
        y, x = state
        state[0], state[1] = y * c + x * s, x * c - y * s
        return y

    return sample


def make_input(modulate, d):
    n = FRAMES * d
    left = bytearray(modulate(rotation(*ROTATIONS["left"][1:]), n))
    right = bytearray(modulate(rotation(*ROTATIONS["right"][1:]), n))
    for i in range(TIE_FRAMES[0] * d, TIE_FRAMES[1] * d):
        bit = (TIE >> (7 - i % 8)) & 1
        left[i], right[i] = bit, 1 - bit
    for i in range(FULL_FRAMES[0] * d, FULL_FRAMES[1] * d):
        left[i], right[i] = 1, 0
    return left, right


def digest_taps(taps):
    return hashlib.sha256(b"".join(struct.pack("<q", v) for v in taps)).hexdigest()


def main():
    g1s = {}
    for mult in (1, 2, 4):
        g1, t = stage1(mult * DSD64)
        g1s[mult * DSD64] = (g1, t)
        print(f"taps g1 DSD{64 * mult} {len(g1)} {digest_taps(g1)}")
    g2 = stage2()
    print(f"taps g2 {len(g2)} {digest_taps(g2)}")
    g3s = {}
    for rate_hz, _ in PLANS:
        lo, hi = band(rate_hz)
        g3s[rate_hz] = stage3(lo, hi)
        print(f"taps g3 {lo}-{hi} {len(g3s[rate_hz])} {digest_taps(g3s[rate_hz])}")
    g1, t = g1s[DSD64]
    s = tie_sum(g1, TIE)
    if s % 128 != 64 or (-s) % 128 != 64:
        sys.exit(f"TIE {TIE:#04x}: stage 1 sums {s}, which is no tie")
    modulate = load_modulate()
    for rate_hz, bits in PLANS:
        d = DSD64 // rate_hz
        left, right = make_input(modulate, d)
        print(f"input {rate_hz}/{bits} {len(left)} {hashlib.sha256(bytes(left) + bytes(right)).hexdigest()}")
        out, clipped, ties, ties2 = convert((left, right), DSD64, rate_hz, bits, g1, t, g2, g3s[rate_hz])
        raw = b"".join(struct.pack("<i", v) for v in out)
        print(f"output {rate_hz}/{bits} {len(out) // 2} clipped {clipped} ties {ties} stage2-ties {ties2} {hashlib.sha256(raw).hexdigest()}")


if __name__ == "__main__":
    main()
