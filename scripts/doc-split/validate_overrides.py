#!/usr/bin/env python3
"""Vet hand-polished index lines (overrides/<name>.json) before they replace condensed entries.

  validate_overrides.py --base REV NAME...            check overrides/<name>.json
  validate_overrides.py --base REV --file F NAME      check a candidate JSON file for one NAME

A line is rejected when it: breaks the bullet/paragraph shape; doesn't end with the entry's
pointer suffix; drops one of the entry's own bolded "Don't…/Never…" sentences; carries a code
span (of any backtick length) or #number that is not in the original entry as a whole
identifier (the guard against invented or truncated symbols); has unbalanced ** or backticks,
a line break, or more than 600 bytes; or copies a test-count claim (a check kept from the iOS
copy; this repo has no claims, see mdblocks.load_claim_regex). Keys are resolved as
condense_rules.py resolves them: the start of the block's text, or its first line number.
"""
import argparse
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from condense_rules import build, REPO  # noqa: E402
from mdblocks import existing_file, load_claim_regex, read_text, rules_names, safe_rev  # noqa: E402

CLAIM = load_claim_regex(REPO)
# A CommonMark code span: a run of N backticks, its content, and the next run of exactly N.
SPAN_RE = re.compile(r"(?<!`)(`+)(?!`)(.+?)(?<!`)\1(?!`)")
MAX_BYTES = 600


def appears_whole(span, text):
    """SPAN occurs in TEXT without being part of a longer identifier: `Dashboard` does not match
    DashboardController, while `seek(to:)` still matches PlayerService.seek(to:)."""
    before = r"(?<!\w)" if re.match(r"\w", span) else ""
    after = r"(?!\w)" if re.search(r"\w$", span) else ""
    return re.search(before + re.escape(span) + after, text) is not None


def span_content(inner):
    """The content CommonMark renders: one space is dropped from each end when both ends have one."""
    if len(inner) > 2 and inner.startswith(" ") and inner.endswith(" ") and inner.strip():
        return inner[1:-1]
    return inner


def shape_problems(entry, line, suffix):
    errs = []
    if "\n" in line:
        errs.append("line break")
    if (entry["block"]["kind"] == "bullet") != line.startswith("- "):
        errs.append("bullet/paragraph shape")
    if not line.endswith(suffix):
        errs.append("pointer suffix changed")
    errs += ["dropped Don't: %s" % d[:60] for d in entry["donts"] if "**%s**" % d not in line]
    size = len(line.encode("utf-8"))
    if size > MAX_BYTES:
        errs.append("%d bytes > %d" % (size, MAX_BYTES))
    if CLAIM.search(line):
        errs.append("copies a test-count claim")
    return errs


def content_problems(entry, body):
    """Checks on the text before the pointer suffix: invented symbols and broken markdown."""
    text = entry["block"]["text"]
    orig = re.sub(r"\s+", " ", text)
    errs = []
    for m in SPAN_RE.finditer(body):
        span = re.sub(r"\s+", " ", span_content(m.group(2)))
        if not appears_whole(span, orig):
            errs.append("code span not in original: `%s`" % span[:60])
    errs += ["number not in original: %s" % num for num in re.findall(r"#\d+", body)
             if not re.search(re.escape(num) + r"(?!\d)", text)]
    rest = SPAN_RE.sub("", body)
    if "`" in rest or rest.count("**") % 2:
        errs.append("unbalanced markdown")
    return errs


def problems(entry, line):
    mech = entry["mechanical_line"]
    suffix = mech[mech.index(" → History-"):]
    body = line[: -len(suffix)] if line.endswith(suffix) else line
    return shape_problems(entry, line, suffix) + content_problems(entry, body)


def check(name, path, sha, label):
    """Prints a summary for one NAME; returns the number of rejected lines. A key that matches no
    block, or two, or a block that does not move, stops build() with its reason."""
    cand = json.loads(read_text(path))
    _, _, entries = build(sha, name, "check", overrides=cand)
    lines = {e["key"]: e for e in entries if e["override"] is not None}
    if len(lines) != len(cand):
        sys.exit("%s: %d override keys resolved to %d entries at %s" % (name, len(cand), len(lines), label))
    fails = {}
    for key, e in sorted(lines.items()):
        errs = problems(e, e["override"])
        if errs:
            fails[key] = errs
    sizes = sorted(len(e["override"].encode("utf-8")) for e in lines.values())
    print("%s: %d lines, %d rejected; bytes median %d, max %d" % (
        name, len(lines), len(fails), sizes[len(sizes) // 2] if sizes else 0, sizes[-1] if sizes else 0))
    for key, errs in fails.items():
        print("  L%d: %s" % (key, "; ".join(errs)))
    return len(fails)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", required=True, help="commit holding the uncondensed rules file")
    ap.add_argument("--file", help="candidate JSON for a single NAME")
    ap.add_argument("names", nargs="+", choices=rules_names(REPO), metavar="NAME")
    a = ap.parse_args()
    if a.file and len(a.names) != 1:
        sys.exit("--file takes exactly one NAME")
    sha = safe_rev(REPO, a.base)
    bad = 0
    for name in a.names:
        path = existing_file(a.file, ".json") if a.file else os.path.join(HERE, "overrides", name + ".json")
        bad += check(name, path, sha, a.base)
    sys.exit(1 if bad else 0)


if __name__ == "__main__":
    main()
