#!/usr/bin/env python3
"""Condense `.claude/rules/<name>.md` into a one-line-per-entry index + a verbatim long form.

  condense_rules.py --base REV NAME...              write the result into the repo
  condense_rules.py --base REV --out DIR NAME...    write into DIR (staging) instead
  condense_rules.py --base REV --review NAME...     list entries still on a first-sentence line
  condense_rules.py --base REV --work DIR NAME...   write per-file work lists for a polish pass

REV is a commit holding the UNCONDENSED rules file (the script refuses an already-condensed one).
Every heading and the `paths:` frontmatter stay. Blocks of KEEP_MAX bytes or less (and tables /
code) stay verbatim. Longer blocks move byte-for-byte to ops/History-rules-<name>.md and leave
one line: the whole bold title (or the first sentence), the entry's own bolded "Don't…" sentences
verbatim, and a pointer. Hand-polished lines come from overrides/<name>.json. A key there is the
start of the block's text (whitespace-collapsed; it must match exactly one block), so the same
file still applies when the rules file is regenerated on a newer base; a key of digits alone is
the block's first line number in REV's version of the file, as in the iOS copy. See README.md.

NAME must be one of the files in .claude/rules. A --out or --work DIR must be outside the
repository and hold nothing but an earlier run's output.
"""
import argparse
import datetime
import json
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from mdblocks import (parse, split_frontmatter, movable, condense_entry, read_text, write_text,  # noqa: E402
                      rules_names, iso_date, safe_rev, git_show, staging_dir, docs_in, HISTORY_DIR)

REPO = os.path.abspath(os.path.join(HERE, "..", ".."))
MARK = "> **Condensed "
STAGED = (".claude/rules/*.md", HISTORY_DIR + "/History-rules-*.md")  # what --out DIR may already hold
WORK = ("work_*.md",)                                                   # what --work DIR may already hold


def note(hist, date):
    return ("%s%s.** Each long entry here is one line — its bold title plus its own bolded "
            "\"Don't…/Never…\" sentences — ending `→ %s`: that is `%s/%s`, where the entry sits "
            "verbatim with its reasoning, measurements and rejected alternatives. **Still binding.** Edit the "
            "line here and its long form there in the same commit; new long-form detail goes to that file, "
            "never back into this one." % (MARK, date, hist, HISTORY_DIR, hist))


def header(name, hist, date):
    return ("# Long form of `.claude/rules/%s.md`\n\n"
            "> Moved verbatim from `.claude/rules/%s.md` on %s so it stops loading with every matching "
            "file. **These entries are still binding.** The rules file keeps each one as a single line (its bold "
            "title plus its own \"Don't…\" sentences, ending `→ %s`); this file keeps the reasoning, "
            "measurements, rejected alternatives and the full don't-undo list. Change an entry here and its line "
            "in the rules file in the same commit. Each `##` below names the rules-file section the entries came "
            "from; entries inside a section keep their original order." % (name, name, date, hist))


def load_base(sha, name):
    """The rules file at commit SHA (a full id from mdblocks.safe_rev)."""
    raw = git_show(REPO, sha, ".claude/rules/%s.md" % name)
    if MARK in raw:
        sys.exit("%s.md at %s is already condensed — pass the commit BEFORE its condensation" % (name, sha[:8]))
    return raw


def load_overrides(name):
    """overrides/<name>.json as written: {key: line}, a key being a text prefix or a line number."""
    path = os.path.join(HERE, "overrides", name + ".json")
    if not os.path.exists(path):
        return {}
    return json.loads(read_text(path))


def norm(text):
    return re.sub(r"\s+", " ", text).strip()


def resolve_keys(raw, items, off, label):
    """{first line number: line} from an overrides mapping keyed by the start of a block's text
    (whitespace-collapsed, matching exactly one block) or by a line number (digits only)."""
    blocks = [it for it in items if it["type"] == "block"]
    out = {}
    for key, line in raw.items():
        if re.fullmatch(r"\d+", key):
            at = int(key)
        else:
            want = norm(key)
            hits = [it for it in blocks if norm(it["text"]).startswith(want)]
            if len(hits) != 1:
                sys.exit("%s: override key %r matches %d blocks, not 1" % (label, key[:70], len(hits)))
            at = hits[0]["start"] + 1 + off
        if at in out:
            sys.exit("%s: two override keys name the block on line %d" % (label, at))
        out[at] = line
    return out


def build(sha, name, date, raw=None, overrides=None):
    """Returns (rules_text, history_text, entries) where entries lists every moved block as a
    dict with key, block, mechanical_line, fallback, donts and override. RAW, when given, is
    the rules file at SHA (saves a second `git show`); OVERRIDES, when given, replaces
    overrides/<name>.json (validate_overrides.py passes a candidate file this way)."""
    raw = load_base(sha, name) if raw is None else raw
    fm, body = split_frontmatter(raw)
    off = fm.count("\n")
    _, items = parse(body)
    hist = "History-rules-%s.md" % name
    overrides = resolve_keys(load_overrides(name) if overrides is None else overrides, items, off,
                             "%s.md at %s" % (name, sha[:8]))
    out, moved, entries = [], [], []
    noted = not (items and items[0]["type"] == "heading" and items[0]["lvl"] == 1)
    if noted:
        out.append((0, note(hist, date)))
    for it in items:
        key = it["start"] + 1 + off
        if it["type"] == "heading":
            out.append((it["gap"], it["text"]))
            if not noted and it["lvl"] == 1:
                out.append((1, note(hist, date)))
                noted = True
            continue
        if not movable(it) and key not in overrides:
            out.append((it["gap"], it["text"]))
            continue
        label = " › ".join(t for lvl, t in it["path"] if lvl >= 2) or "(top of file)"
        line, fallback, donts = condense_entry(it, hist, self_rules=name + ".md", known_docs=docs_in(REPO))
        moved.append((label, it))
        entries.append({"key": key, "block": it, "mechanical_line": line, "fallback": fallback,
                        "donts": donts, "override": overrides.get(key)})
        out.append((it["gap"], overrides.get(key, line)))
    text = ""
    for gap, t in out:
        text += ("\n" * (max(gap, 0) + 1) if text else "") + t
    parts, last = [header(name, hist, date)], None
    for label, b in moved:
        parts.append(("\n\n## " + label + "\n\n" if label != last else "\n\n") + b["text"])
        last = label
    stale = sorted(set(overrides) - {e["key"] for e in entries})
    if stale:
        sys.exit("%s: overrides for blocks that are not moved at %s (first lines %s)" % (name, sha[:8], stale))
    return fm + text + "\n", "".join(parts) + "\n", entries


def refuse_recondense(name, rules, hist):
    """Condensing is one-shot. Writing into the repository must not replace a rules file that is
    already condensed, or a long-form file that already exists, with anything but what they hold:
    later edits and entries moved there by move_entry.py would be lost. Once condensed, a file
    grows by hand and sheds entries with move_entry.py."""
    for rel, new in ((".claude/rules/%s.md" % name, rules), ("%s/History-rules-%s.md" % (HISTORY_DIR, name), hist)):
        path = os.path.join(REPO, rel)
        if not os.path.isfile(path):
            continue
        old = read_text(path)
        if old != new and (rel.startswith(HISTORY_DIR + "/") or MARK in old):
            sys.exit("%s is already condensed and differs from this run — condensing again would drop what "
                     "was added since. Move further entries with scripts/doc-split/move_entry.py." % rel)


def prefix_key(block):
    """A short start of the block's text, usable as its key in overrides/<name>.json."""
    return norm(block["text"])[:80]


def work_list(name, entries):
    out = []
    for e in entries:
        if not e["fallback"] or e["override"] is not None:
            continue
        line, b = e["mechanical_line"], e["block"]
        suffix = line[line.index(" → History-rules-"):]
        shape = ('bullet: line must start with "- "' if b["kind"] == "bullet"
                 else 'paragraph: line must NOT start with "- "')
        out.append("=== KEY %d (%s) ===\nPREFIX KEY: %s\nCURRENT: %s\nSUFFIX (copy exactly): %s\n%sORIGINAL:\n%s\n" % (
            e["key"], shape, json.dumps(prefix_key(b), ensure_ascii=False), line, suffix,
            "".join("KEEP VERBATIM: **%s**\n" % d for d in e["donts"]), b["text"]))
    return "# %d entries from .claude/rules/%s.md\n\n" % (len(out), name) + "\n".join(out)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", required=True, help="commit holding the uncondensed rules file")
    ap.add_argument("--out", help="staging directory, outside the repository (default: write into the repo)")
    ap.add_argument("--date", type=iso_date, default=datetime.date.today().isoformat())
    ap.add_argument("--review", action="store_true", help="list first-sentence lines without an override")
    ap.add_argument("--work", help="directory for polish-pass work lists, outside the repository")
    ap.add_argument("names", nargs="+", choices=rules_names(REPO), metavar="NAME")
    a = ap.parse_args()
    sha = safe_rev(REPO, a.base)
    writes_files = not (a.review or a.work)
    root = staging_dir(a.out, REPO, STAGED) if a.out and writes_files else REPO
    work = staging_dir(a.work, REPO, WORK) if a.work else None
    for name in a.names:
        raw = load_base(sha, name)
        rules, hist, entries = build(sha, name, a.date, raw)
        todo = [e for e in entries if e["fallback"] and e["override"] is None]
        if work:
            write_text(os.path.join(work, "work_%s.md" % name), work_list(name, entries))
        if a.review:
            for e in todo:
                print("  L%d %s" % (e["key"], e["mechanical_line"][:200]))
        if writes_files:
            if root == REPO:
                refuse_recondense(name, rules, hist)
            os.makedirs(os.path.join(root, ".claude", "rules"), exist_ok=True)
            os.makedirs(os.path.join(root, HISTORY_DIR), exist_ok=True)
            write_text(os.path.join(root, ".claude", "rules", name + ".md"), rules)
            write_text(os.path.join(root, HISTORY_DIR, "History-rules-%s.md" % name), hist)
        print("%s.md: %d → %d bytes; %d entries moved; %d on a first-sentence line without an override" % (
            name, len(raw.encode()), len(rules.encode()), len(entries), len(todo)))


if __name__ == "__main__":
    main()
