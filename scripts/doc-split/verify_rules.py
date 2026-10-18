#!/usr/bin/env python3
"""Prove a rules-file condensation lost nothing.

  verify_rules.py --base REV [--root DIR] NAME...

REV holds the uncondensed rules file; DIR (default: the repository) holds the condensed
.claude/rules/NAME.md and ops/History-rules-NAME.md. The check fails unless:

  - every block of the original is a whole block of one of the two outputs, byte for byte, at
    least as many times as the original has it (a block that grew or shrank does not count);
  - the rules file keeps the original's headings, in order, and its frontmatter unchanged;
  - every test-count claim of the original survives (a check kept from the iOS copy; this repo
    has none, see mdblocks.load_claim_regex); and
  - every phrase another file cites in this rules file (`rules/NAME.md` § PHRASE, or
    ("PHRASE")) that resolved in the original still resolves in the rules file or its long
    form. Citations that did not resolve in the original either are listed, not counted.

The citing files are CLAUDE.md, AGENTS.md, docs/*.md, ops/*.md, .claude/rules/*.md, the skills
and scripts/* (DIR's copy where it has one, the repository's otherwise) plus every tracked source
file that can carry a comment: Go, JavaScript, HTML, CSS, shell, PowerShell, YAML and the Makefile.
"""
import argparse
import collections
import glob
import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from mdblocks import (parse, split_frontmatter, read_text, rules_names, safe_rev, git_show,  # noqa: E402
                      existing_dir, load_claim_regex, HISTORY_DIR)

REPO = os.path.abspath(os.path.join(HERE, "..", ".."))
CLAIM = load_claim_regex(REPO)
DOC_SOURCES = ("CLAUDE.md", "AGENTS.md", "docs/*.md", "ops/*.md", ".claude/rules/*.md",
               ".claude/skills/*/SKILL.md", "scripts/*")
# Tracked files whose comments may cite a rules file (git pathspecs).
SOURCE_SPECS = ("*.go", "*.js", "*.mjs", "*.html", "*.css", "*.sh", "*.ps1", "*.yml", "*.yaml", "Makefile",
                "Dockerfile")
CITE_FMT = r"rules/%s\.md`?\)?\s*(?:§\s*([^;,)\n—`]{3,70})|\(\s*[\"“]([^\"”\n]{4,90})[\"”])"
SHOW = 6  # examples printed per list


def claims(text):
    return collections.Counter((m.group(1), int(m.group(3))) for m in CLAIM.finditer(text))


def norm(text):
    return re.sub(r"\s+", " ", text)


def split_items(text):
    """(block texts, heading texts), exactly as mdblocks.parse cuts them."""
    _, items = parse(text)
    return ([it["text"] for it in items if it["type"] == "block"],
            [it["text"] for it in items if it["type"] == "heading"])


def source_texts(root):
    """{relative path: text} for every file that may cite a rules file."""
    paths = {}
    for folder in (REPO, root):  # ROOT last, so its copy of a file wins
        for pattern in DOC_SOURCES:
            for path in glob.glob(os.path.join(folder, pattern)):
                if os.path.isfile(path):
                    paths[os.path.relpath(path, folder)] = path
    tracked = subprocess.run(["git", "-C", REPO, "ls-files", "-z", "--", *SOURCE_SPECS],
                             capture_output=True, text=True, check=True).stdout
    for rel in filter(None, tracked.split("\0")):
        if os.path.isfile(os.path.join(REPO, rel)):
            paths.setdefault(rel, os.path.join(REPO, rel))
    texts = {}
    for rel, path in paths.items():
        with open(path, encoding="utf-8", errors="replace") as f:
            texts[rel] = f.read()
    return texts


def citations(name, texts):
    """Phrases other files cite in .claude/rules/NAME.md."""
    pattern = re.compile(CITE_FMT % re.escape(name))
    found = set()
    for rel, text in texts.items():
        if rel == ".claude/rules/%s.md" % name:
            continue
        for m in pattern.finditer(text):
            phrase = (m.group(1) or m.group(2)).strip().strip("*").strip()
            if len(phrase) >= 3:
                found.add(phrase)
    return found


def parts(citation):
    """What a citation names: "A + § B" names two things; a leading date stands for its section."""
    out = []
    for piece in citation.split("+"):
        piece = piece.strip().lstrip("§").strip().strip("\"“”*").strip()
        m = re.match(r"^(20\d\d-\d\d-\d\d)", piece)
        piece = m.group(1) if m else piece.rstrip(".,;: ").strip("\"“”*").strip()
        if len(piece) >= 3:
            out.append(norm(piece))
    return out


def resolves(pieces, *texts):
    return bool(pieces) and all(any(p in t for t in texts) for p in pieces)


def citation_status(name, texts, base, new, hist):
    """Cited phrases sorted into (in the rules file, long form only, stale before, broken)."""
    base_n, new_n, hist_n = norm(base), norm(new), norm(hist)
    status = {"in_file": [], "long_form": [], "stale": [], "broken": []}
    for c in sorted(citations(name, texts)):
        pieces = parts(c)
        if resolves(pieces, new_n):
            status["in_file"].append(c)
        elif resolves(pieces, new_n, hist_n):
            status["long_form"].append(c)
        elif resolves(pieces, base_n):
            status["broken"].append(c)
        else:
            status["stale"].append(c)
    return status


def verify(name, sha, root, texts):
    base = git_show(REPO, sha, ".claude/rules/%s.md" % name)
    new = read_text(os.path.join(root, ".claude", "rules", name + ".md"))
    hist = read_text(os.path.join(root, HISTORY_DIR, "History-rules-%s.md" % name))
    fm_base, body_base = split_frontmatter(base)
    fm_new, body_new = split_frontmatter(new)
    base_blocks, base_heads = split_items(body_base)
    new_blocks, new_heads = split_items(body_new)
    hist_blocks, _ = split_items(hist)
    have = collections.Counter(new_blocks) + collections.Counter(hist_blocks)
    missing = [t for t, n in collections.Counter(base_blocks).items() if have[t] < n]
    before, after = claims(base), claims(new) + claims(hist)
    short = {k: (n, after[k]) for k, n in before.items() if after[k] < n}
    cites = citation_status(name, texts, base, new, hist)
    ok = not missing and new_heads == base_heads and not short and fm_base == fm_new and not cites["broken"]
    print("%s.md: %d→%d B | blocks %d, missing %d | headings %d, %s | claims %d, short %d | "
          "frontmatter %s | citations %d: in file %d, long form only %d, stale before %d, BROKEN %d" % (
              name, len(base.encode()), len(new.encode()), len(base_blocks), len(missing), len(base_heads),
              "unchanged" if new_heads == base_heads else "CHANGED", sum(before.values()), len(short),
              "unchanged" if fm_base == fm_new else "CHANGED", sum(len(v) for v in cites.values()),
              len(cites["in_file"]), len(cites["long_form"]), len(cites["stale"]), len(cites["broken"])))
    for t in missing:
        print("     MISSING BLOCK:", repr(t[:100]))
    if new_heads != base_heads:
        k = next((i for i, (x, y) in enumerate(zip(base_heads, new_heads)) if x != y), min(len(base_heads), len(new_heads)))
        print("     FIRST CHANGED HEADING: %r → %r" % ((base_heads[k:k + 1] or ["(none)"])[0],
                                                     (new_heads[k:k + 1] or ["(none)"])[0]))
    for k, (n, m) in short.items():
        print("     CLAIM LOST: %s (%d) — %d before, %d after" % (k[0], k[1], n, m))
    for c in cites["broken"]:
        print("     BROKEN CITATION:", c[:90])
    for c in cites["stale"][:SHOW]:
        print("     stale before the condensation (not counted):", c[:80])
    return ok


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", required=True, help="commit holding the uncondensed rules files")
    ap.add_argument("--root", default=REPO, help="directory holding the condensed files (default: the repository)")
    ap.add_argument("names", nargs="+", choices=rules_names(REPO), metavar="NAME")
    a = ap.parse_args()
    sha = safe_rev(REPO, a.base)
    root = existing_dir(a.root)
    texts = source_texts(root)
    ok = all([verify(name, sha, root, texts) for name in a.names])
    print("RESULT:", "PASS" if ok else "FAIL")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
