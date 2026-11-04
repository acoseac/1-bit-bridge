#!/usr/bin/env python3
"""Hold the DAC profile database's imported DoP rates to the iOS rows they came from.

internal/player/dacprofile/profiles.json imports DoP evidence from the iOS app's registry,
DACProfile.swift, and each such claim names its row and commit ("DACProfile.swift <row> at iOS
<commit>"). The design's section 2.4 (ops/plan-b728-dsd-capability-design.md) has the author of a
database PR read the registry at that commit and check that no imported DoP rate exceeds its row:

    python3 scripts/dac-profiles/ios-check.py --registry <checkout>/com.acoseac.dsdplayer/DACProfile.swift --commit <sha>

For each profile whose DoP source is the iOS registry it finds the row by its id in the registry's
profiles list, reads the row's maxDSDRate (.dsd64 is 64 times 44100 one-bit samples a second, and
so on; .none is no DoP at all), and checks that the heard and the stated rate are at or below it,
and that each rate a device refused lies above it, where the row does not reach. It prints one line
a profile and exits 1 on a violation, a row or a commit the database names that the file is not,
or a file it cannot read. Paste its output into the PR. The registry's prose is never read: only
each row's id and its maxDSDRate.
"""

import argparse
import json
import pathlib
import re
import sys

REPO = pathlib.Path(__file__).resolve().parent.parent.parent
PROFILES = REPO / "internal" / "player" / "dacprofile" / "profiles.json"

SOURCE_REF = re.compile(r"^DACProfile\.swift (?P<row>[a-z0-9]+(?:[.-][a-z0-9]+)+) at iOS (?P<commit>[0-9a-f]{7,40})$")
ROW_ID = re.compile(r'\bid:\s*"(?P<id>[^"]+)"')
ROW_RATE = re.compile(r"\bmaxDSDRate:\s*\.(?P<rate>none|dsd64|dsd128|dsd256|dsd512)\b")
RATES = {"none": 0, "dsd64": 64 * 44100, "dsd128": 128 * 44100, "dsd256": 256 * 44100, "dsd512": 512 * 44100}


def strip_comments(swift):
    """Blank every // comment, keeping string literals whole, so a row's comments are never read."""
    out, i, n = [], 0, len(swift)
    while i < n:
        if swift.startswith("//", i):
            j = swift.find("\n", i)
            i = n if j < 0 else j
        elif swift[i] == '"':
            j = i + 1
            while j < n and swift[j] != '"' and swift[j] != "\n":
                j += 2 if swift[j] == "\\" else 1
            out.append(swift[i:j + 1])
            i = j + 1
        else:
            out.append(swift[i])
            i += 1
    return "".join(out)


def registry_rows(swift):
    """Each row of the registry's profiles list: its id and maxDSDRate, in one-bit samples a second."""
    text = strip_comments(swift)
    start = text.find("static let profiles: [DACProfile] = [")
    if start < 0:
        raise ValueError("no `static let profiles: [DACProfile] = [` in the registry")
    rows = {}
    for m in ROW_ID.finditer(text, start):
        rate = ROW_RATE.search(text, m.end())
        nxt = ROW_ID.search(text, m.end())
        if rate is None or (nxt is not None and rate.start() > nxt.start()):
            raise ValueError("row %s has no maxDSDRate before the next row" % m.group("id"))
        if m.group("id") in rows:
            raise ValueError("row %s is in the registry twice" % m.group("id"))
        rows[m.group("id")] = RATES[rate.group("rate")]
    if not rows:
        raise ValueError("the registry's profiles list holds no row")
    return rows


def check(profiles, rows, commit):
    """One line a profile with DoP from the iOS registry, and whether every one holds."""
    lines, ok = [], True
    for p in profiles:
        dop = p.get("dop")
        if not dop or dop["source"]["kind"] != "ios-registry":
            continue
        m = SOURCE_REF.match(dop["source"]["ref"])
        if m is None:
            lines.append("FAIL %s: source %r names no row and commit" % (p["id"], dop["source"]["ref"]))
            ok = False
            continue
        if not commit.startswith(m.group("commit")) and not m.group("commit").startswith(commit):
            lines.append("FAIL %s: names iOS %s, the registry read is at %s" % (p["id"], m.group("commit"), commit))
            ok = False
            continue
        row = m.group("row")
        if row not in rows:
            lines.append("FAIL %s: the registry has no row %s" % (p["id"], row))
            ok = False
            continue
        cap = rows[row]
        problems = []
        for name in ("heardMaxBitRate", "statedMaxBitRate"):
            if dop[name] > cap:
                problems.append("%s %d exceeds the row's %d" % (name, dop[name], cap))
        for refused in dop["refusedBitRates"]:
            if refused <= cap:
                problems.append("refused %d is a rate the row reaches (%d)" % (refused, cap))
        verdict = "FAIL" if problems else "ok"
        ok = ok and not problems
        lines.append("%s %s: row %s, maxDSDRate %d; heard %d, stated %d, refused %s%s" % (
            verdict, p["id"], row, cap, dop["heardMaxBitRate"], dop["statedMaxBitRate"],
            dop["refusedBitRates"], "; " + "; ".join(problems) if problems else ""))
    if not lines:
        lines.append("no profile imports DoP from the iOS registry")
    return lines, ok


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--registry", required=True, help="DACProfile.swift at the commit the profiles name")
    ap.add_argument("--commit", required=True, help="the iOS commit the registry file is at")
    ap.add_argument("--profiles", default=str(PROFILES), help="the database (default: the embedded profiles.json)")
    args = ap.parse_args()
    try:
        rows = registry_rows(pathlib.Path(args.registry).read_text(encoding="utf-8"))
        profiles = json.loads(pathlib.Path(args.profiles).read_text(encoding="utf-8"))["profiles"]
    except (OSError, ValueError, KeyError) as e:
        print("ios-check: %s" % e, file=sys.stderr)
        return 1
    lines, ok = check(profiles, rows, args.commit)
    print("iOS registry at %s: %d rows; %s" % (args.commit, len(rows), pathlib.Path(args.profiles).name))
    for line in lines:
        print("  " + line)
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
