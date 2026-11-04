#!/usr/bin/env python3
"""Generate a DAC's DSD qualification listening assets and their manifests.

The bridge player's owner qualification (ops/b470-native-qualification.md)
needs independent listening assets: DSF and DSDIFF fixtures at each DSD rate,
each with a matched PCM reference of identical duration, and versioned
manifests (manifest-v1) whose hashes bind them. This script builds sdm.c, the
generator beside it, with the system C compiler, runs its self-test, and
renders the asset set build_plan describes; nothing here shares code with the
bridge or its decoder test goldens.

It never writes inside a Git checkout: the assets are hundreds of megabytes
and are never committed. Copy this directory somewhere else and run the copy;
it writes into an out/ directory beside itself:

    out/manifest.json            the qualification manifest (approval evidence)
    out/manifest-edge44.json     the matrix's 44.1 kHz PCM-edge manifest
    out/manifest-edge384.json    the matrix's 384 kHz PCM-edge manifest
    out/runs.json                the session's runs: manifest, transport, rate,
                                 scenario and fixture list
    out/assets/                  the DSF, DFF and WAV files the manifests name
    out/SHA256SUMS, out/README.txt, out/generation.json
    out/assets.tar               all of the above but generation.json

--self-test builds and checks the generator in a temporary directory and
writes nothing else; it may run in place. The assets' own decode check, with
the bridge's decoders, is scripts/qualification/listening-verify.

Standard library only.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import re
import shutil
import socket
import struct
import subprocess
import sys
import tarfile
import tempfile
import zlib
from pathlib import Path

HERE = Path(__file__).resolve().parent
SOURCE = HERE / "sdm.c"
OUT = HERE / "out"
ASSETS = "assets"

DSD = {64: 2_822_400, 128: 5_644_800, 256: 11_289_600}
CFLAGS = ["-std=c99", "-O2", "-ffp-contract=off", "-Wall", "-Wextra", "-pedantic"]
PEAK_DBFS = -30
CALIBRATION_PHRASE = "I confirm the matched -30 dBFS listening fixtures and end silences"

# The modulator's design, sdm.c's defaults, restated so the manifest's
# reference names them; the run checks sdm design prints the same.
DESIGN = {"order": 5, "osr": 64, "hinf": 1.4, "dither": 0.4}


# ------------------------------------------------------------------ plan


def odd_frames(rate: int) -> int:
    """About 3.5 s whose bit count is 2352 times an odd number: an odd number
    of 16-bit groups (DoP carrier frames), half a 32-bit word past a whole
    one, and a whole number of frames at 176.4, 352.8 and 384 kHz."""
    k = round(3.5 * rate / 2352)
    if k % 2 == 0:
        k += 1
    return 2352 * k


def subbyte_frames(rate: int) -> int:
    """About 3.5 s whose bit count is 147 times an odd number: it ends inside
    a byte, and is a whole number of frames at 384 kHz."""
    k = round(3.5 * rate / 147)
    if k % 2 == 0:
        k += 1
    return 147 * k


def continuation_frames(rate: int, start: int) -> int:
    """About 3.5 s, 147 times an odd number of bits again, that brings a chain
    which reached bit start to a whole 32-bit word: the run that ends with it
    leaves the sink no partial word to pad."""
    k = round(3.5 * rate / 147)
    while (start + 147 * k) % 32:
        k += 1
    return 147 * k


def ref_frames(dsd_frames: int, dsd_rate: int, ref_rate: int) -> int:
    n = dsd_frames * ref_rate
    if n % dsd_rate:
        raise SystemExit(f"no {ref_rate} Hz reference has the exact duration of {dsd_frames} bits at {dsd_rate}")
    return n // dsd_rate


def seed_of(name: str) -> int:
    return zlib.crc32(name.encode())


class Plan:
    """The asset set: modulation jobs, WAV files, manifests and runs."""

    def __init__(self) -> None:
        self.dsd_jobs: list[dict] = []
        self.wavs: dict[str, dict] = {}
        self.fixtures: dict[str, dict] = {}  # file name -> fixture facts
        self.manifests: dict[str, list[dict]] = {"manifest.json": [], "manifest-edge44.json": [], "manifest-edge384.json": []}
        self.runs: list[dict] = []

    def wav(self, name: str, content: str, rate: int, frames: int, bits: int) -> str:
        if name in self.wavs:
            w = self.wavs[name]
            if (w["content"], w["rate"], w["frames"], w["bits"]) != (content, rate, frames, bits):
                raise SystemExit(f"{name} planned twice differently")
            return name
        self.wavs[name] = {"content": content, "rate": rate, "frames": frames, "bits": bits}
        self.fixtures[name] = {"Container": "WAV", "Order": "RL" if content.endswith("-rl") else "LR", "Rate": rate, "Frames": frames}
        return name

    def add(self, manifest: str, fid: str, path: str, ref: str = "") -> None:
        self.manifests[manifest].append({"ID": fid, "Path": path, "PCMReference": ref})


def build_plan() -> Plan:
    p = Plan()
    for nn, rate in DSD.items():
        main = 8 * rate
        quiet = 6 * rate
        # The chain odd, subbyte, continuous-split is one modulation in three
        # files: odd ends after an odd number of 16-bit groups (DoP carrier
        # frames), half a 32-bit word past a whole one; subbyte ends inside a
        # byte, at an odd bit, where only its continuation joins it cleanly;
        # continuous-split ends the chain on a whole word.
        odd = odd_frames(rate)
        sub = subbyte_frames(rate)
        split = continuation_frames(rate, odd + sub)
        assert odd % 32 == 16 and (odd // 16) % 2 == 1, odd
        assert sub % 2 == 1 and split % 2 == 1 and (odd + sub + split) % 32 == 0, (sub, split)
        files = {
            "lr-lsb": f"dsd{nn}-lr-lsb.dsf",
            "rl-lsb": f"dsd{nn}-rl-lsb.dsf",
            "lr-msb": f"dsd{nn}-lr-msb.dsf",
            "lr-dff": f"dsd{nn}-lr.dff",
            "rl-dff": f"dsd{nn}-rl.dff",
            "quiet-tail": f"dsd{nn}-quiet-tail.dsf",
            "odd": f"dsd{nn}-odd.dsf",
            "continuous-split": f"dsd{nn}-continuous-split.dsf",
            "subbyte": f"dsd{nn}-subbyte.dsf",
        }
        p.dsd_jobs += [
            {"name": f"dsd{nn}-main", "rate": rate, "content": [("main", main)],
             "write": [("dsf-lsb", 0, files["lr-lsb"]), ("dsf-lsb-rl", 0, files["rl-lsb"]), ("dsf-msb", 0, files["lr-msb"]),
                       ("dff", 0, files["lr-dff"]), ("dff-chnl-rl", 0, files["rl-dff"])]},
            {"name": f"dsd{nn}-quiet-tail", "rate": rate, "content": [("quiet-tail", quiet)], "write": [("dsf-lsb", 0, files["quiet-tail"])]},
            {"name": f"dsd{nn}-chain", "rate": rate, "content": [("short", odd), ("short", sub), ("short", split)],
             "write": [("dsf-lsb", 0, files["odd"]), ("dsf-lsb", 1, files["subbyte"]), ("dsf-lsb", 2, files["continuous-split"])]},
        ]
        facts = {
            "lr-lsb": ("DSF", "LR", main, "main"), "rl-lsb": ("DSF", "RL", main, "main-rl"),
            "lr-msb": ("DSF", "LR", main, "main"), "lr-dff": ("DFF", "LR", main, "main"),
            "rl-dff": ("DFF", "RL", main, "main-rl"), "quiet-tail": ("DSF", "LR", quiet, "quiet-tail"),
            "odd": ("DSF", "LR", odd, "short"), "continuous-split": ("DSF", "LR", split, "short"),
            "subbyte": ("DSF", "LR", sub, "short"),
        }
        for key, (container, order, frames, content) in facts.items():
            p.fixtures[files[key]] = {"Container": container, "Order": order, "Rate": rate, "Frames": frames, "content": content}

        # The qualification manifest: DoP's references are at its carrier
        # (rate / 16) where DoP exists on the DAC (DSD64, DSD128), and every
        # reference is 32-bit, which a DAC offering only S32_LE plays.
        carrier = rate // 16
        primary = {64: carrier, 128: carrier, 256: 352_800}[nn]
        odd_ref = {64: carrier, 128: carrier, 256: 384_000}[nn]

        def ref(content: str, frames: int, ref_rate: int, tag: str) -> str:
            name = f"ref{ref_rate // 1000}-{tag}.wav"
            return p.wav(name, content, ref_rate, ref_frames(frames, rate, ref_rate), 32)

        refs = {
            "lr-lsb": ref("main", main, primary, "lr"), "rl-lsb": ref("main-rl", main, primary, "rl"),
            "lr-msb": ref("main", main, primary, "lr"), "lr-dff": ref("main", main, primary, "lr"),
            "rl-dff": ref("main-rl", main, primary, "rl"),
            "quiet-tail": ref("quiet-tail", quiet, primary, "quiet-tail"),
            "odd": ref("short", odd, odd_ref, f"dsd{nn}-odd"),
            "continuous-split": ref("short", split, 384_000, f"dsd{nn}-continuous-split"),
            "subbyte": ref("short", sub, 384_000, f"dsd{nn}-subbyte"),
        }
        for key in files:
            p.add("manifest.json", f"dsd{nn}-{key}", files[key], Path(refs[key]).stem)
        # The PCM-edge manifests: the matrix's first and last references at
        # 44.1 kHz and at 384 kHz.
        for manifest, edge in (("manifest-edge44.json", 44_100), ("manifest-edge384.json", 384_000)):
            for key in ("lr-lsb", "rl-lsb", "lr-msb", "lr-dff", "rl-dff"):
                r = ref("main-rl" if key.startswith("rl") else "main", main, edge, "rl" if key.startswith("rl") else "lr")
                p.add(manifest, f"dsd{nn}-{key}", files[key], Path(r).stem)

    # References, once each, then the PCM edge fixtures the lifecycles,
    # output play and serve take: the main programme at 24 bits, the depth
    # library files have, so its edges are the ones they meet.
    for name, w in sorted(p.wavs.items()):
        stem = Path(name).stem
        if any(f["PCMReference"] == stem for f in p.manifests["manifest.json"]):
            p.add("manifest.json", stem, name)
        for edge in ("manifest-edge44.json", "manifest-edge384.json"):
            if any(f["PCMReference"] == stem for f in p.manifests[edge]):
                p.add(edge, stem, name)
    for rate in (44_100, 88_200, 176_400, 352_800, 384_000):
        name = f"pcm{rate // 1000}.wav"
        p.wav(name, "main", rate, 8 * rate, 24)
        p.add("manifest.json", f"wav{rate // 1000}", name)

    main5 = ["lr-lsb", "rl-lsb", "lr-msb", "lr-dff", "rl-dff"]
    for nn in DSD:
        ids = {k: f"dsd{nn}-{k}" for k in ("lr-lsb", "rl-lsb", "lr-msb", "lr-dff", "rl-dff", "quiet-tail", "odd", "continuous-split", "subbyte")}
        for fmt in ("DSD_U32_BE",):
            base = {"manifest": "manifest.json", "transport": "native", "format": fmt, "rate": DSD[nn]}
            p.runs += [
                dict(base, scenario="level-check", fixtures=[ids["lr-lsb"]], purpose="contact"),
                dict(base, scenario="matrix", fixtures=[ids[k] for k in main5], purpose="matrix"),
                dict(base, scenario="interactive", fixtures=[ids["lr-lsb"], ids["rl-dff"], ids["lr-lsb"], ids["rl-dff"]], purpose="navigation"),
                dict(base, scenario="interactive", fixtures=[ids["quiet-tail"], ids["odd"], ids["subbyte"], ids["continuous-split"]], purpose="joins"),
                dict(base, scenario="backend-lifecycle", fixtures=["wav44", ids["lr-lsb"], ids["rl-dff"], "wav384", ids["lr-lsb"]], purpose="lifecycle"),
            ]
            for edge in ("manifest-edge44.json", "manifest-edge384.json"):
                p.runs += [
                    dict(base, manifest=edge, scenario="level-check", fixtures=[ids["lr-lsb"]], purpose="contact"),
                    dict(base, manifest=edge, scenario="matrix", fixtures=[ids[k] for k in main5], purpose="pcm-edge matrix"),
                ]
    for fmt in ("DSD_U32_LE", "DSD_U16_LE"):
        p.runs.append({"manifest": "manifest.json", "transport": "native", "format": fmt, "rate": DSD[64], "scenario": "level-check",
                       "fixtures": ["dsd64-lr-lsb"], "purpose": "unsupported format, expected to be refused at the DSD open"})
    for nn, wav in ((64, "wav176"), (128, "wav352")):
        base = {"manifest": "manifest.json", "transport": "dop", "format": "S32_LE", "rate": DSD[nn]}
        ids = {k: f"dsd{nn}-{k}" for k in ("lr-lsb", "rl-lsb", "lr-msb", "lr-dff", "rl-dff", "quiet-tail", "odd", "continuous-split")}
        p.runs += [
            dict(base, scenario="level-check", fixtures=[ids["lr-lsb"]], purpose="contact, and the mixer experiment's contacts"),
            dict(base, scenario="matrix", fixtures=[ids[k] for k in main5], purpose="matrix"),
            dict(base, scenario="interactive", fixtures=[ids["quiet-tail"], ids["odd"], ids["odd"]], purpose="joins"),
            dict(base, scenario="backend-lifecycle", fixtures=[wav, ids["lr-lsb"], ids["rl-dff"], "wav44", ids["lr-lsb"]], purpose="lifecycle"),
        ]
    p.runs.append({"manifest": "manifest.json", "transport": "dop", "format": "S32_LE", "rate": DSD[64], "scenario": "approved-native-dop-native",
                   "native": {"format": "DSD_U32_BE", "rate": DSD[64]}, "fixtures": ["dsd64-lr-lsb", "dsd64-rl-dff", "dsd64-lr-lsb"], "purpose": "transition"})
    return p


# --------------------------------------------------------------- running


def find_cc() -> str:
    cc = shutil.which("cc") or shutil.which("gcc") or shutil.which("clang")
    if not cc:
        raise SystemExit("no C compiler (cc, gcc or clang) on PATH")
    return str(Path(cc).resolve())


def build(work: Path) -> tuple[Path, str]:
    cc = find_cc()
    exe = work / "sdm"
    subprocess.run([cc, *CFLAGS, "-o", str(exe), str(SOURCE), "-lm"], check=True)
    version = subprocess.run([cc, "--version"], check=True, capture_output=True, text=True).stdout.splitlines()[0]
    return exe, version


def run_json(cmd: list[str], cwd: Path | None = None) -> dict:
    out = subprocess.run(cmd, check=True, capture_output=True, text=True, cwd=cwd).stdout
    return json.loads(out.strip().splitlines()[-1])


def sha256(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def inside_checkout(path: Path) -> bool:
    for d in (path, *path.parents):
        if (d / ".git").exists():
            return True
    return False


def reference_text(design: dict, cc_version: str, digests: dict) -> str:
    return (
        f"Generator: scripts/qualification/listening-assets sdm.c version {design['version']} (sha256 {digests['sdm.c']}) "
        f"driven by generate.py (sha256 {digests['generate.py']}), compiled with {cc_version} {' '.join(CFLAGS)}. "
        f"DSD: a 1-bit error-feedback sigma-delta modulator of order {design['order']}, its noise transfer function by "
        f"the synthesizeNTF procedure (zeros at the Legendre roots across 0..pi/{design['osr']:g}, maximally flat poles, "
        f"|H(-1)| = {design['hinf']:g}), the same design at every rate so its band edge is the rate/128, signal transfer "
        f"exactly 1, in-loop TPDF dither of peak {design['dither']:g} (units of the +-1 output), xorshift64* seeded per "
        "job from the CRC-32 of its name, each channel its own; silences are that loop with a zero input. PCM references: "
        "the same programme sampled at their own rates, 32-bit integer WAVE_FORMAT_EXTENSIBLE (the pcmNN.wav PCM-edge "
        "files 24-bit), TPDF dither of +-1 LSB, rounded. Waveform: main, 8 s: 1 s silence, 2 s left 1 kHz, 1 s silence, "
        "2 s right 2 kHz, 2 s silence; quiet-tail, 6 s: 1 s silence, left 1 kHz with right 2 kHz from 1 s, steady to "
        "1.5 s, then decaying 60 dB/s to 3.5 s, then silence; short (odd, subbyte, continuous-split): 1 s silence, "
        "0.5 s left 1 kHz, 0.25 s silence, 0.5 s right 2 kHz, silence to the end, at least 1.25 s. Every burst rises "
        "and falls over a 10 ms raised cosine, its sine at phase zero at its start, but for the quiet tail's decaying bursts, "
        "which start at the phase that leaves them no net area (a 1-bit loop keeps an input's fractional area for good). dsdNN-odd, dsdNN-subbyte and "
        "dsdNN-continuous-split are one modulation in three files: odd is 2352 times an odd number of bits (an odd "
        "number of DoP carrier frames), subbyte 147 times an odd number (it ends inside a byte, where only "
        "continuous-split, its continuation, joins it cleanly), continuous-split 147 times an odd number that ends "
        "the chain on a whole 32-bit word. At every file boundary at an even bit the loop was re-dithered from 10 ms "
        "before it until the running sum of its bits stood within 0.02 of its mean over the 0.5 ms either side and "
        "the bits either side left at most -100 dB SA-CD from 20 Hz to 20 kHz against 0x69 (or else the least of 2000 "
        "tries was kept), so a join between files, or with a player's 0x69 silence, leaves no step; the boundary "
        "inside subbyte's last byte is clean only as the loop's own continuation. Levels: peaks -30 dBFS. PCM: 10^(-30/20) of full scale. DSD: 0.5 x 10^(-30/20) of the +-1 "
        "bitstream, -30 dB re 0 dB SA-CD (50 % modulation). Analogue calibration: 0 dB SA-CD is taken as the "
        "analogue level of PCM full scale, so a DAC that follows the convention plays a DSD fixture and its PCM "
        "reference at one level, and one that maps full modulation to full scale plays the DSD 6.02 dB quieter, "
        "never louder; the owner's off-ear level check fixes the matched knob position. Channel IDs: LR is the "
        "1 kHz burst on the left and the 2 kHz on the right, RL the reverse; DSF channel type 2 (FL, FR); DFF CHNL "
        "SLFT SRGT; the RL DSF carries the LR streams swapped; the RL DFF lists CHNL SRGT SLFT over the LR DFF's "
        "storage, so a reader honouring CHNL plays it RL. Each reference is its fixture's programme, channel order "
        "included, of identical duration."
    )


def generate() -> None:
    if inside_checkout(HERE):
        raise SystemExit(
            f"{HERE} is inside a Git checkout, and the assets are hundreds of megabytes that are never committed: "
            "copy this directory out of the repository and run the copy (--self-test runs anywhere)")
    plan = build_plan()
    if OUT.exists():
        raise SystemExit(f"{OUT} exists: remove it first, so no file of an earlier run survives")
    work = Path(tempfile.mkdtemp(prefix="sdm-build-"))
    try:
        exe, cc_version = build(work)
        subprocess.run([str(exe), "selftest"], check=True)
        n = render(plan, OUT, exe, cc_version)
        print(f"wrote {n} assets, {len(plan.manifests)} manifests and {len(plan.runs)} runs under {OUT}")
    finally:
        shutil.rmtree(work)


def render(plan: Plan, out: Path, exe: Path, cc_version: str) -> int:
    """Write plan's assets, manifests, runs.json, generation.json, README.txt,
    SHA256SUMS and assets.tar into out, which must not exist yet, and answer
    how many assets it wrote. sdm runs inside out on paths relative to it, so
    generation.json records each run's arguments as they were given, and no
    file written names out, a directory above it, the user or the host."""
    design = run_json([str(exe), "design"])
    for k, v in DESIGN.items():
        if design[k] != v:
            raise SystemExit(f"sdm design says {k} {design[k]}, the plan {v}")
    assets = out / ASSETS
    assets.mkdir(parents=True)
    record = {"design": design, "compiler": cc_version, "cflags": CFLAGS, "dsd": [], "wav": []}
    for job in plan.dsd_jobs:
        args = ["dsd", "--rate", str(job["rate"]), "--seed", str(seed_of(job["name"]))]
        for content, frames in job["content"]:
            args += ["--content", f"{content}:{frames}"]
        for kind, seg, name in job["write"]:
            args += ["--write", f"{kind}:{seg}:{ASSETS}/{name}"]
        print("dsd", job["name"], flush=True)
        summary = run_json([str(exe), *args], cwd=out)
        if max(summary["vmax"]) >= 3 or max(summary["longestRun"]) > 16:
            raise SystemExit(f"{job['name']}: the loop left its bounds: {summary}")
        record["dsd"].append({"job": job["name"], "args": args, "summary": summary})
    for name, w in sorted(plan.wavs.items()):
        args = ["wav", "--rate", str(w["rate"]), "--bits", str(w["bits"]), "--seed", str(seed_of(name)),
                "--content", f"{w['content']}:{w['frames']}", "--out", f"{ASSETS}/{name}"]
        print("wav", name, flush=True)
        record["wav"].append({"file": name, "args": args, "summary": run_json([str(exe), *args], cwd=out)})
    digests = {"sdm.c": sha256(SOURCE), "generate.py": sha256(Path(__file__).resolve())}
    reference = reference_text(design, cc_version, digests)
    hashes = {name: sha256(assets / name) for name in sorted(os.listdir(assets))}
    for manifest, entries in plan.manifests.items():
        fixtures = []
        for e in entries:
            f = plan.fixtures[e["Path"]]
            fixtures.append({"ID": e["ID"], "Path": e["Path"], "SHA256": hashes[e["Path"]], "Container": f["Container"],
                             "Order": f["Order"], "PCMReference": e["PCMReference"], "Rate": f["Rate"], "Channels": 2,
                             "Frames": f["Frames"]})
        doc = {"schemaVersion": 1,
               "calibration": {"reference": reference, "peakDBFS": PEAK_DBFS, "leadingSilenceMS": 1000,
                               "trailingSilenceMS": 1000, "attestation": CALIBRATION_PHRASE},
               "fixtures": fixtures}
        (out / manifest).write_text(json.dumps(doc, indent=2) + "\n")
    (out / "runs.json").write_text(json.dumps({"runs": plan.runs}, indent=2) + "\n")
    record["digests"] = digests
    (out / "generation.json").write_text(json.dumps(record, indent=2) + "\n")
    readme = [
        "DSD qualification listening assets (manifest-v1), from scripts/qualification/listening-assets.",
        "Extract into the evidence directory Q, then: cd \"$Q\" && sha256sum -c SHA256SUMS",
        "manifest.json is the qualification manifest; manifest-edge44.json and manifest-edge384.json",
        "are the matrix's PCM-edge manifests; runs.json lists the session's runs; assets/ is --fixtures.",
        "",
        reference,
    ]
    (out / "README.txt").write_text("\n".join(readme) + "\n")
    members = sorted(["README.txt", "runs.json", *plan.manifests] + [f"{ASSETS}/{n}" for n in hashes])
    sums = [f"{sha256(out / m)}  {m}" for m in members]
    (out / "SHA256SUMS").write_text("\n".join(sums) + "\n")
    with tarfile.open(out / "assets.tar", "w", format=tarfile.PAX_FORMAT) as tar:
        def owned(info: tarfile.TarInfo) -> tarfile.TarInfo:
            info.uid = info.gid = 0
            info.uname = info.gname = ""
            info.mtime = 0
            info.mode = 0o700 if info.isdir() else 0o600
            return info
        tar.add(out / ASSETS, arcname=ASSETS, recursive=False, filter=owned)
        for m in members + ["SHA256SUMS"]:
            tar.add(out / m, arcname=m, filter=owned)
    return len(hashes)


# What of a generated file's text could say where, or by whom, it was made:
# an absolute path, POSIX, a drive's, a share's or one under ~, starting a
# word. A slash inside a word ("rate/128", "pi/64", "assets/x.dsf") is none.
ABSOLUTE = re.compile(r"""(?:^|[\s"'=:(\[,])(/[^\s"',)\]]+|[A-Za-z]:[\\/][^\s"',)\]]*|\\\\[^\s"',)\]]+|~[A-Za-z0-9_.-]*/[^\s"',)\]]*)""")


def exposure(text: str, names: list[str]) -> list[str]:
    """The absolute paths text holds, and each of names (the user's and the
    host's) it holds as a word; a name shorter than four characters is too
    likely a word of the text's own to be read as one."""
    found = [m.group(1) for m in ABSOLUTE.finditer(text)]
    for name in names:
        if len(name) >= 4 and re.search(r"(?<![A-Za-z0-9_])" + re.escape(name) + r"(?![A-Za-z0-9_])", text):
            found.append(name)
    return found


def identifying_names() -> list[str]:
    """The names a generated file must not carry: the user's, as the
    environment and the home directory give it, and the host's."""
    names = [os.environ.get(v, "") for v in ("USER", "LOGNAME", "USERNAME")] + [Path.home().name]
    host = socket.gethostname()
    names += [host, host.split(".")[0]]
    return sorted({n for n in names if n})


def exposures(out: Path, names: list[str]) -> list[str]:
    """What of every file render wrote into out says where or by whom it was
    made: the text files read whole, the assets for the directories' own
    absolute paths, and each tar member's name, link, owner and PAX records."""
    dirs = [str(out.resolve()), str(HERE), str(Path.home()), tempfile.gettempdir()]
    found = []
    for path in sorted(p for p in out.rglob("*") if p.is_file()):
        rel = path.relative_to(out).as_posix()
        if path.suffix in (".dsf", ".dff", ".wav", ".tar"):
            data = path.read_bytes()
            found += [f"{rel} holds {d}" for d in dirs if len(d) > 4 and d.encode() in data]
            continue
        found += [f"{rel} names {hit}" for hit in exposure(path.read_text(), names)]
    with tarfile.open(out / "assets.tar") as tar:
        for m in tar.getmembers():
            for field in (m.name, m.linkname, m.uname, m.gname, *m.pax_headers.values()):
                found += [f"assets.tar member {m.name} names {hit}" for hit in exposure(field, names)]
            if m.uid or m.gid or m.uname or m.gname:
                found.append(f"assets.tar member {m.name} has an owner")
    return found


# ------------------------------------------------------------- self-test


def bitrev(b: int) -> int:
    return int(f"{b:08b}"[::-1], 2)


def read_dsf(path: Path) -> dict:
    b = path.read_bytes()
    if b[0:4] != b"DSD " or struct.unpack("<Q", b[4:12])[0] != 28 or struct.unpack("<Q", b[12:20])[0] != len(b):
        raise AssertionError(f"{path.name}: DSD chunk")
    if b[28:32] != b"fmt " or struct.unpack("<Q", b[32:40])[0] != 52:
        raise AssertionError(f"{path.name}: fmt chunk")
    ver, fid, ctype, chans, rate, bps = struct.unpack("<6I", b[40:64])
    count = struct.unpack("<Q", b[64:72])[0]
    block = struct.unpack("<I", b[72:76])[0]
    if (ver, fid, ctype, chans, block) != (1, 0, 2, 2, 4096) or bps not in (1, 8):
        raise AssertionError(f"{path.name}: fmt fields")
    if b[80:84] != b"data" or struct.unpack("<Q", b[84:92])[0] != len(b) - 80:
        raise AssertionError(f"{path.name}: data chunk")
    data = b[92:]
    nbytes = (count + 7) // 8
    chans_bytes = [bytearray(), bytearray()]
    for at in range(0, len(data), 8192):
        for c in range(2):
            chans_bytes[c] += data[at + c * 4096: at + (c + 1) * 4096]
    out = []
    for c in range(2):
        raw = bytes(chans_bytes[c][:nbytes])
        if any(chans_bytes[c][nbytes:]):
            raise AssertionError(f"{path.name}: the padding after the samples is not zero")
        out.append(bytes(bitrev(x) for x in raw) if bps == 1 else raw)
    return {"rate": rate, "bits": count, "lsb": bps == 1, "ch": out}


def read_dff(path: Path) -> dict:
    b = path.read_bytes()
    if b[0:4] != b"FRM8" or struct.unpack(">Q", b[4:12])[0] != len(b) - 12 or b[12:16] != b"DSD ":
        raise AssertionError(f"{path.name}: FRM8")
    pos, chunks = 16, {}
    while pos < len(b):
        cid, size = b[pos:pos + 4], struct.unpack(">Q", b[pos + 4:pos + 12])[0]
        chunks[cid] = b[pos + 12:pos + 12 + size]
        pos += 12 + size + (size & 1)
    if chunks[b"FVER"] != b"\x01\x05\x00\x00":
        raise AssertionError(f"{path.name}: FVER")
    prop = chunks[b"PROP"]
    if prop[0:4] != b"SND ":
        raise AssertionError(f"{path.name}: PROP")
    sub, at = {}, 4
    while at < len(prop):
        cid, size = prop[at:at + 4], struct.unpack(">Q", prop[at + 4:at + 12])[0]
        sub[cid] = prop[at + 12:at + 12 + size]
        at += 12 + size + (size & 1)
    rate = struct.unpack(">I", sub[b"FS  "])[0]
    n = struct.unpack(">H", sub[b"CHNL"][0:2])[0]
    ids = [sub[b"CHNL"][2 + 4 * i: 6 + 4 * i] for i in range(n)]
    if n != 2 or sorted(ids) != [b"SLFT", b"SRGT"] or sub[b"CMPR"][0:4] != b"DSD ":
        raise AssertionError(f"{path.name}: CHNL or CMPR")
    sound = chunks[b"DSD "]
    stored = [sound[0::2], sound[1::2]]
    left = stored[ids.index(b"SLFT")]
    right = stored[ids.index(b"SRGT")]
    return {"rate": rate, "bits": 8 * len(left), "ids": ids, "sound": sound, "ch": [left, right]}


def read_wav(path: Path) -> dict:
    b = path.read_bytes()
    if b[0:4] != b"RIFF" or struct.unpack("<I", b[4:8])[0] != len(b) - 8 or b[8:12] != b"WAVE":
        raise AssertionError(f"{path.name}: RIFF")
    tag, chans, rate, byte_rate, align, bits, cb, valid, mask = struct.unpack("<HHIIHHHHI", b[20:44])
    if b[12:16] != b"fmt " or tag != 0xFFFE or chans != 2 or cb != 22 or valid != bits or mask != 3 or align != 2 * bits // 8:
        raise AssertionError(f"{path.name}: fmt")
    if b[44:60] != bytes.fromhex("0100000000001000800000aa00389b71") or b[60:64] != b"data":
        raise AssertionError(f"{path.name}: sub-format or data")
    data = b[68:]
    width = bits // 8
    frames = len(data) // (2 * width)
    peak = 0
    for i in range(0, len(data), width):
        v = int.from_bytes(data[i:i + width], "little", signed=True)
        peak = max(peak, abs(v))
    return {"rate": rate, "bits": bits, "frames": frames, "peak": peak}


def bits_of(ch: bytes, n: int) -> list[int]:
    return [(ch[k >> 3] >> (7 - (k & 7))) & 1 for k in range(n)]


def mini_plan() -> Plan:
    """A quarter-second plan, one DSD job written as DSF and DFF beside its
    reference: what the self-test renders to read what a generation writes."""
    p = Plan()
    p.manifests = {"manifest.json": []}
    rate = DSD[64]
    frames = rate // 4
    p.dsd_jobs.append({"name": "mini", "rate": rate, "content": [("short", frames)],
                       "write": [("dsf-lsb", 0, "mini.dsf"), ("dff", 0, "mini.dff")]})
    for name, container in (("mini.dsf", "DSF"), ("mini.dff", "DFF")):
        p.fixtures[name] = {"Container": container, "Order": "LR", "Rate": rate, "Frames": frames, "content": "short"}
    ref = p.wav("ref44-mini.wav", "short", 44_100, ref_frames(frames, rate, 44_100), 32)
    for fid, name in (("mini-dsf", "mini.dsf"), ("mini-dff", "mini.dff")):
        p.add("manifest.json", fid, name, Path(ref).stem)
    p.add("manifest.json", Path(ref).stem, ref)
    p.runs.append({"manifest": "manifest.json", "transport": "native", "format": "DSD_U32_BE", "rate": rate,
                   "scenario": "matrix", "fixtures": ["mini-dsf", "mini-dff"], "purpose": "the self-test's"})
    return p


def self_test() -> None:
    plan = build_plan()
    for name, f in plan.fixtures.items():
        seconds = f["Frames"] / f["Rate"]
        if not 2 <= seconds <= 10:
            raise AssertionError(f"{name}: {seconds} s is outside the manifest's 2..10 s")
    for manifest, entries in plan.manifests.items():
        ids = [e["ID"] for e in entries]
        if len(ids) != len(set(ids)) or len(ids) > 128:
            raise AssertionError(f"{manifest}: duplicate or too many IDs")
        by_id = {e["ID"]: plan.fixtures[e["Path"]] for e in entries}
        for e in entries:
            f = plan.fixtures[e["Path"]]
            if f["Container"] != "WAV":
                r = by_id[e["PCMReference"]]
                if r["Frames"] * f["Rate"] != f["Frames"] * r["Rate"]:
                    raise AssertionError(f"{manifest}: {e['ID']}'s reference differs in duration")
    for r in plan.runs:
        if r["transport"] == "native" or r["transport"] == "dop":
            by_id = {e["ID"]: plan.fixtures[e["Path"]] for e in plan.manifests[r["manifest"]]}
            bits = sum(by_id[i]["Frames"] for i in r["fixtures"] if by_id[i]["Container"] != "WAV")
            if r["scenario"] in ("matrix", "interactive", "level-check") and bits % 32:
                raise AssertionError(f"{r['fixtures']}: ends inside a 32-bit word")
    print(f"plan: {len(plan.fixtures)} files, manifests {', '.join(f'{m} {len(e)}' for m, e in plan.manifests.items())}, {len(plan.runs)} runs", flush=True)
    with tempfile.TemporaryDirectory(prefix="sdm-selftest-") as tmp:
        work = Path(tmp)
        exe, cc_version = build(work)
        print("compiler:", cc_version, flush=True)
        subprocess.run([str(exe), "selftest"], check=True)
        rate = DSD[64]
        a, b = 8 * 1001 + 3, 8 * 500 + 5
        common = [str(exe), "dsd", "--rate", str(rate), "--seed", "4242", "--content", f"main:{8 * 3001}", "--content", f"short:{a}",
                  "--content", f"short:{b}"]
        names = {k: work / f"{k}.dat" for k in ("lsb", "msb", "lsb-rl", "dff", "dff-rl", "a", "b", "ab", "again")}
        run_json(common + ["--write", f"dsf-lsb:0:{names['lsb']}", "--write", f"dsf-msb:0:{names['msb']}",
                           "--write", f"dsf-lsb-rl:0:{names['lsb-rl']}", "--write", f"dff:0:{names['dff']}",
                           "--write", f"dff-chnl-rl:0:{names['dff-rl']}", "--write", f"dsf-lsb:1:{names['a']}",
                           "--write", f"dsf-lsb:2:{names['b']}", "--write", f"dsf-lsb:1+2:{names['ab']}"])
        run_json(common + ["--write", f"dsf-lsb:0:{names['again']}"])
        lsb, msb, rl = read_dsf(names["lsb"]), read_dsf(names["msb"]), read_dsf(names["lsb-rl"])
        dff, dffrl = read_dff(names["dff"]), read_dff(names["dff-rl"])
        checks = [
            ("an LSB-first and an MSB-first DSF of one stream carry the same bits", lsb["ch"] == msb["ch"] and lsb["lsb"] and not msb["lsb"]),
            ("the RL DSF carries the LR streams swapped", rl["ch"] == [lsb["ch"][1], lsb["ch"][0]]),
            ("the DFF carries the DSF's bits", dff["ch"] == lsb["ch"] and dff["ids"] == [b"SLFT", b"SRGT"]),
            ("the RL DFF stores the LR DFF's bytes under CHNL SRGT SLFT",
             dffrl["sound"] == dff["sound"] and dffrl["ids"] == [b"SRGT", b"SLFT"] and dffrl["ch"] == [dff["ch"][1], dff["ch"][0]]),
            ("the same seed writes the same file", names["lsb"].read_bytes() == names["again"].read_bytes()),
        ]
        pa, pb, pab = read_dsf(names["a"]), read_dsf(names["b"]), read_dsf(names["ab"])
        split_ok = pa["bits"] == a and pb["bits"] == b and pab["bits"] == a + b and all(
            bits_of(pa["ch"][c], a) + bits_of(pb["ch"][c], b) == bits_of(pab["ch"][c], a + b) for c in range(2))
        checks.append(("a split inside a byte keeps every bit: part a then part b is the unsplit stream", split_ok))
        wav = work / "w.wav"
        run_json([str(exe), "wav", "--rate", "44100", "--bits", "24", "--seed", "1", "--content", "main:352800", "--out", str(wav)])
        w = read_wav(wav)
        peak = 20 * math.log10(w["peak"] / 2 ** 23)
        checks.append((f"a 24-bit WAV reference peaks at -30 dBFS ({peak:.3f})", w["frames"] == 352800 and abs(peak + 30) < 0.01))
        # What a generation writes says nothing of where or by whom it was
        # made: the scan, on planted paths and names, then a small plan
        # rendered as a generation renders, every file it wrote read.
        planted = ["/home/someone/out", "C:\\Users\\someone", "\\\\share\\dir", "~/out", "~someone/x"]
        clean = "rate/128, 0..pi/64, 60 dB/s, +-1, assets/dsd64-lr-lsb.dsf, dsf-lsb:0:assets/a.dsf, 10^(-30/20)"
        checks.append(("the scan finds a planted absolute path of each kind and nothing in a clean line",
                       all(exposure(f"x {p} y", []) == [p] for p in planted) and exposure(clean, []) == []))
        who = identifying_names()
        checks.append((f"the scan finds this user's and this host's names standing as words ({len(who)} names)",
                       all(exposure(f"by {n}.", [n]) == [n] for n in who if len(n) >= 4)))
        out = work / "out"
        render(mini_plan(), out, exe, cc_version)
        found = exposures(out, who)
        wrote = sum(1 for p in out.rglob("*") if p.is_file())
        checks.append((f"no file of a generation names an absolute path, the user or the host ({wrote} files read)", not found))
        for f in found[:8]:
            print("      ", f, flush=True)
        failed = 0
        for what, ok in checks:
            print(f"  {'ok  ' if ok else 'FAIL'} {what}", flush=True)
            failed += not ok
        if failed:
            raise SystemExit("self-test FAILED")
    print("generate.py self-test passed")


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--self-test", action="store_true", help="build and check the generator, writing nothing else")
    args = parser.parse_args()
    if args.self_test:
        self_test()
    else:
        generate()


if __name__ == "__main__":
    main()
