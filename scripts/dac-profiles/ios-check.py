#!/usr/bin/env python3
"""Hold the DAC profile database's imported DoP rates to the iOS rows they came from.

internal/player/dacprofile/profiles.json imports DoP evidence from the iOS app's registry,
DACProfile.swift, and each such claim names its row and commit ("DACProfile.swift <row> at iOS
<commit>"). The design's section 2.4 (ops/plan-b728-dsd-capability-design.md) has the author of a
database PR read the registry at that commit and check that no imported DoP rate exceeds its row:

    python3 scripts/dac-profiles/ios-check.py --registry <checkout>/com.acoseac.dsdplayer/DACProfile.swift --commit <sha>

For each profile whose DoP source is the iOS registry it finds the row by its id in the registry's
profiles list, reads the row's maxDSDRate (.dsd64 is 64 times 44100 one-bit samples a second, and
so on; .none is no DoP at all), and checks that the heard and the stated rate are at or below it.
A rate a device refused is not compared with the row, which may keep a published maximum beside a
refusal at or below it (ifi.generic does); its evidence is in the row's comments, and the
database's lint holds it to the profile's own provenance. It prints one line a profile and exits 1
on a violation, a row or a commit the database names that the file is not, or a file it cannot
read. Paste its output into the PR. The registry's prose is never read: only each row's id and its
maxDSDRate. `--self-test` pins the reader and the check on a synthetic registry.
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
        # A refusal is not compared with the row: a row may keep a published
        # maximum beside a refusal measured at or below it (ifi.generic keeps
        # DSD256 while both measured units refused it), and the evidence for a
        # refusal is in the row's comments, which this reads none of. The lint
        # holds each refusal to the profile's own provenance.
        verdict = "FAIL" if problems else "ok"
        ok = ok and not problems
        lines.append("%s %s: row %s, maxDSDRate %d; heard %d, stated %d, refused %s%s" % (
            verdict, p["id"], row, cap, dop["heardMaxBitRate"], dop["statedMaxBitRate"],
            dop["refusedBitRates"], "; " + "; ".join(problems) if problems else ""))
    if not lines:
        lines.append("no profile imports DoP from the iOS registry")
    return lines, ok


# The self-test's registry: synthetic rows in the shape registry_rows reads,
# with comments and strings that name a rate or a row and must not be read: a
# line comment, a block comment, a nested one (Swift's nest), a row a block
# comment holds, and "/*" in a string and in a line comment, which opens none.
SELF_TEST_REGISTRY = """
struct DACProfile { let maxDSDRate: DSDRateCapability }
nonisolated enum DACProfileRegistry {
    static let profiles: [DACProfile] = [
        DACProfile(
            id: "fixture.published",
            // a comment naming maxDSDRate: .dsd512, which is never read; nor is its /*
            maxDSDRate: .dsd256,
            notes: ["/* a string's, never a comment's", "a note // with maxDSDRate: .dsd64 in it"]
        ),
        DACProfile(
            id: "fixture.lowered",
            /* was maxDSDRate: .dsd512, before a refusal lowered it */
            maxDSDRate: .dsd128
        ),
        /* withdrawn:
        DACProfile(
            id: "fixture.withdrawn",
            maxDSDRate: .dsd512
        ),
        */
        DACProfile(
            id: "fixture.nested",
            /* a comment /* holding another */ goes on: maxDSDRate: .dsd512, id: "fixture.inner" */
            maxDSDRate: .dsd64
        ),
        DACProfile(
            id: "fixture.pcm",
            maxDSDRate: .none
        )
    ]
}
"""

# (name, dop's heard, stated and refused rates, its row, the verdict wanted).
SELF_TEST_CASES = [
    ("a published maximum with a refusal at it (ifi.generic's shape)", 0, 11289600, [11289600], "fixture.published", True),
    ("a refusal below the row's maximum", 2822400, 0, [5644800], "fixture.published", True),
    ("a refusal above the row's maximum (the Mytek's shape)", 0, 0, [11289600], "fixture.lowered", True),
    ("a heard rate at the row's maximum", 5644800, 0, [], "fixture.lowered", True),
    ("a heard rate above the row's maximum, a block comment naming a higher one before it", 11289600, 0, [], "fixture.lowered", False),
    ("a stated rate above the row's maximum", 0, 11289600, [], "fixture.lowered", False),
    ("a heard rate above the row's maximum, a nested comment naming a higher one before it", 5644800, 0, [], "fixture.nested", False),
    ("a heard rate from a row with no DoP", 2822400, 0, [], "fixture.pcm", False),
    ("a row the registry does not have", 2822400, 0, [], "fixture.absent", False),
    ("a row only a block comment holds", 2822400, 0, [], "fixture.withdrawn", False),
    ("a row only a nested comment names", 2822400, 0, [], "fixture.inner", False),
]

# What the reader does not read, each refused rather than read past: (name,
# a text of the registry, what replaces it).
SELF_TEST_REFUSED = [
    ("a row with no maxDSDRate before the next row", "maxDSDRate: .dsd128", "notes: []"),
    ("a block comment left open", "maxDSDRate: .none\n", "maxDSDRate: .none /* left open\n"),
    ("a multi-line string", 'id: "fixture.pcm",\n',
     'id: "fixture.pcm",\n            notes: """\n            maxDSDRate: .dsd512\n            """,\n'),
    ("an interpolation", '"a note // with', '"\\(note) // with'),
    ("a raw string", '"a note // with maxDSDRate: .dsd64 in it"', '#"a note // with maxDSDRate: .dsd64 in it"#'),
]


def self_test():
    """Pin the registry reader and the check on the synthetic registry: 0 when every case holds."""
    failures = []
    try:
        rows = registry_rows(SELF_TEST_REGISTRY)
    except ValueError as e:
        rows = {}
        failures.append("the registry does not read: %s" % e)
    if rows != {"fixture.published": 256 * 44100, "fixture.lowered": 128 * 44100, "fixture.nested": 64 * 44100,
                "fixture.pcm": 0}:
        failures.append("the registry read as %r" % rows)
    for name, heard, stated, refused, row, want in SELF_TEST_CASES:
        dop = {"heardMaxBitRate": heard, "statedMaxBitRate": stated, "refusedBitRates": refused,
               "source": {"kind": "ios-registry", "ref": "DACProfile.swift %s at iOS cd125088" % row}}
        lines, ok = check([{"id": "fixture.profile", "dop": dop}], rows, "cd125088")
        if ok != want:
            failures.append("%s: %s, want %s (%s)" % (name, ok, want, "; ".join(lines)))
    dop = {"heardMaxBitRate": 0, "statedMaxBitRate": 0, "refusedBitRates": [11289600],
           "source": {"kind": "ios-registry", "ref": "DACProfile.swift fixture.lowered at iOS cd125088"}}
    if check([{"id": "fixture.profile", "dop": dop}], rows, "cea8bdf6")[1]:
        failures.append("a registry read at another commit than the profile names passes")
    for name, old, new in SELF_TEST_REFUSED:
        if old not in SELF_TEST_REGISTRY:
            failures.append("%s: the registry has no %r" % (name, old))
            continue
        try:
            registry_rows(SELF_TEST_REGISTRY.replace(old, new, 1))
            failures.append("%s: the registry reads" % name)
        except ValueError:
            pass
    for f in failures:
        print("ios-check self-test: FAIL " + f)
    print("ios-check self-test: %s (%d cases)" % ("FAIL" if failures else "PASS",
                                                  len(SELF_TEST_CASES) + len(SELF_TEST_REFUSED) + 2))
    return 1 if failures else 0


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--registry", help="DACProfile.swift at the commit the profiles name")
    ap.add_argument("--commit", help="the iOS commit the registry file is at")
    ap.add_argument("--profiles", default=str(PROFILES), help="the database (default: the embedded profiles.json)")
    ap.add_argument("--self-test", action="store_true", help="pin the reader and the check on a synthetic registry")
    args = ap.parse_args()
    if args.self_test:
        return self_test()
    if not args.registry or not args.commit:
        ap.error("--registry and --commit are required, unless --self-test")
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
