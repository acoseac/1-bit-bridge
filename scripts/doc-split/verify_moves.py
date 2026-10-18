#!/usr/bin/env python3
"""Prove that moving text between the instruction files lost nothing.

  verify_moves.py --base REV [--edit PREFIX]... [--rename OLD=>NEW]... [--moves]

The knowledge base is CLAUDE.md, AGENTS.md, .claude/rules/*.md, .claude/skills/*/SKILL.md, the
top-level docs/*.md and the top-level ops/*.md (the History files, the runbooks and the playbook),
except ops/engineering-log.md: that 3 MB log holds copies of many rules, so counting it would let a
block lost from an instruction file be "found" there. A restructure moves blocks between them, so
they are compared as one whole with REV. The working tree must still hold:
  - every block REV had, byte for byte, at least as many times: a block that vanished, shrank or
    grew by a word fails. Relative link targets are compared by the file they point at, so a
    block moved to another folder may carry rebased links (mdblocks.rebase_links). The exception
    is a block named with --edit PREFIX (the start of its text): exactly one base block starting
    with PREFIX may be gone (an unchanged sibling with the same start, such as the History long
    form of a one-line entry, does not count), and it must have exactly one rewritten successor
    starting with PREFIX, in any file; the change is printed as a diff. An --edit accounts for
    one copy: if the base held that block twice, the other copy must still be there;
  - every heading REV had, as a heading somewhere (its level may change), unless it is renamed
    with --rename "OLD=>NEW" (each rename accounts for one occurrence of OLD, gone, and one of
    NEW, added);
  - the same `XTests` (N cases) claims (a check kept from the iOS copy; this repo has none, see
    mdblocks.load_claim_regex);
  - a `paths:` list in every rules file, and in every SKILL.md a `name:` equal to its folder's
    name and a non-empty `description:`;
  - every phrase the repository cites in an instruction file ("CLAUDE.md § X", CLAUDE.md "X",
    CLAUDE.md: "X", CLAUDE.md under "X", rules/NAME.md § X) in a doc, a script, a Go, JavaScript,
    HTML, CSS, shell or PowerShell source, a workflow or the Makefile, still found somewhere in
    the knowledge base if it was found at REV. A comment's quote may run over its next lines. A
    citation may now resolve in another file than it names: the files say to grep them all.
New and duplicated blocks are listed, not failed: added text is for the reviewer to judge.
--moves prints where every moved block went.
"""
import argparse
import collections
import difflib
import fnmatch
import glob
import importlib.util
import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from mdblocks import (parse, split_frontmatter, read_text, safe_rev, git_show, load_claim_regex,  # noqa: E402
                      rebase_links)

REPO = os.path.abspath(os.path.join(HERE, "..", ".."))
PATTERNS = ("CLAUDE.md", "AGENTS.md", ".claude/rules/*.md", ".claude/skills/*/SKILL.md", "docs/*.md", "ops/*.md")
EXCLUDE = ("ops/engineering-log.md",)
CLAIM = load_claim_regex(REPO)


def budgets_module():
    spec = importlib.util.spec_from_file_location("check_doc_budgets", os.path.join(REPO, "scripts",
                                                                                     "check-doc-budgets.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def in_universe(rel):
    """REL is one of PATTERNS and not EXCLUDEd; `*` stays inside one folder, so docs/sub/x.md is
    not docs/*.md."""
    return rel not in EXCLUDE and any(fnmatch.fnmatchcase(rel, p) and rel.count("/") == p.count("/")
                                      for p in PATTERNS)


def files_at(sha):
    out = subprocess.run(["git", "-C", REPO, "ls-tree", "-r", "--name-only", "--end-of-options", sha, "--",
                          "CLAUDE.md", "AGENTS.md", ".claude", "docs", "ops"],
                         capture_output=True, text=True, check=True).stdout
    return {rel: git_show(REPO, sha, rel) for rel in out.split("\n") if rel and in_universe(rel)}


def files_now(root):
    """The knowledge base in the working tree: only files git tracks or would track. A file that
    .gitignore matches is skipped, so a long form written under an ignored name (this repo keeps
    ops/*review*.md, *audit* and *findings* untracked) is missing here, as it is from every clone."""
    listed = subprocess.run(["git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
                            capture_output=True, text=True, check=True).stdout
    candidates = set(filter(None, listed.split("\0")))
    found = {}
    for pattern in PATTERNS:
        for path in glob.glob(os.path.join(root, pattern)):
            rel = os.path.relpath(path, root).replace(os.sep, "/")
            if rel not in candidates:
                continue
            if os.path.isfile(path) and not os.path.islink(path) and in_universe(rel):
                found[rel] = read_text(path)
    return found


def split(text, rel):
    """(frontmatter, block texts, heading titles), exactly as mdblocks.parse cuts the body, with
    relative link targets rewritten from REL's folder to the repository root."""
    fm, body = split_frontmatter(text)
    _, items = parse(body)
    folder = os.path.dirname(rel)
    return (fm, [rebase_links(it["text"], folder, "") for it in items if it["type"] == "block"],
            [it["title"] for it in items if it["type"] == "heading"])


def inventory(files):
    """Block and heading counts, where each block sits, and the test-count claims."""
    blocks, heads, where, claims = collections.Counter(), collections.Counter(), {}, collections.Counter()
    for rel, text in sorted(files.items()):
        _, bs, hs = split(text, rel)
        for b in bs:
            blocks[b] += 1
            where.setdefault(b, []).append(rel)
        heads.update(hs)
        claims.update((m.group(1), int(m.group(3))) for m in CLAIM.finditer(text))
    return blocks, heads, where, claims


def resolve_edits(prefixes, base, new):
    """Pair each --edit PREFIX's base block with its one rewritten successor. A block, old or new,
    can be claimed by one --edit only: a repeated or overlapping PREFIX would otherwise excuse a
    second lost copy."""
    pairs, errors, claimed_old, claimed_new = [], [], set(), set()
    for prefix in prefixes:
        olds = [b for b in base["blocks"] if b.startswith(prefix) and new["blocks"][b] < base["blocks"][b]]
        if len(olds) != 1:
            errors.append("--edit %r matches %d base blocks that are gone, not 1" % (prefix[:50], len(olds)))
            continue
        news = [b for b in new["blocks"] if b.startswith(prefix) and b not in base["blocks"]]
        if len(news) != 1:
            errors.append("--edit %r has %d rewritten successors, not 1" % (prefix[:50], len(news)))
            continue
        if olds[0] in claimed_old or news[0] in claimed_new:
            errors.append("--edit %r names a block another --edit already claimed" % prefix[:50])
            continue
        claimed_old.add(olds[0])
        claimed_new.add(news[0])
        pairs.append((olds[0], news[0]))
    return pairs, errors


def frontmatter_problems(files):
    """The same checks check-doc-budgets.py makes: rules files are scoped, skills can be found."""
    budgets = budgets_module()
    errs = []
    for rel, text in sorted(files.items()):
        if rel.startswith(".claude/rules/") and not budgets.rules_globs(text):
            errs.append("%s has no paths: list, so it would load in every session" % rel)
        if rel.startswith(".claude/skills/"):
            errs += ["%s %s" % (rel, e) for e in budgets.skill_problems(rel, text)]
    return errs


CITE_RE = re.compile(r"(?:CLAUDE\.md|rules/[\w-]+\.md)`?\)?'?s?[:,]?\s*(?:(?:under|in|records|calls|names)\s+)?"
                     r"(?:§\s*\"?([^;,)\n—`\"]{3,80})"
                     r"|\(\s*[\"“]([^\"”\n]{4,90})[\"”]|[\"“]([^\"”\n]{4,90})[\"”])")
CITING = (".md", ".go", ".js", ".mjs", ".html", ".css", ".sh", ".ps1", ".yml", ".yaml", ".py")
# In a source file, a line break plus the comment marker that continues the comment, so a quoted
# phrase that runs over two comment lines reads as one. Markdown is read line by line, as before.
CONTINUATION = re.compile(r"[ \t]*\n[ \t]*(?://+|#+|\*+(?!/)|--)[ \t]?")


def citations():
    """Phrases the tracked sources cite in CLAUDE.md or a rules file (the doc-split tooling excepted)."""
    listed = subprocess.run(["git", "-C", REPO, "ls-files", "-z"], capture_output=True, text=True, check=True).stdout
    found = set()
    for rel in filter(None, listed.split("\0")):
        if rel.startswith("scripts/doc-split/") or not (rel.endswith(CITING) or rel in ("Makefile", "Dockerfile")):
            continue
        path = os.path.join(REPO, rel)
        if not os.path.isfile(path):
            continue
        with open(path, encoding="utf-8", errors="replace") as f:
            text = f.read()
        if not rel.endswith(".md"):
            text = CONTINUATION.sub(" ", text)
        for m in CITE_RE.finditer(text):
            phrase = (m.group(1) or m.group(2) or m.group(3)).strip().strip("*").strip().lstrip("#").strip()
            phrase = re.sub(r"\s+", " ", phrase).lower()[:60].rstrip(" .,:")
            if len(phrase) >= 4:
                found.add(phrase)
    return found


def citation_breaks(base_files, new_files):
    """(cited phrases found at the base but nowhere now, number checked). The citations themselves
    are masked first: a History file that cites a phrase would otherwise satisfy its own search."""
    flat = lambda files: re.sub(r"\s+", " ", "\n".join(CITE_RE.sub(" ", t) for t in files.values())).lower()
    base_text, new_text = flat(base_files), flat(new_files)
    cited = citations()
    return sorted(c for c in cited if c in base_text and c not in new_text), len(cited)


def word_diff(old, new):
    """The changed lines of a multi-line block (a table row, a wrapped paragraph), else the words."""
    if "\n" in old or "\n" in new:
        return ["  %s %s" % (line[0], line[2:].strip()[:300]) for line in difflib.ndiff(old.split("\n"), new.split("\n"))
                if line[:1] in "-+"]
    a, b = re.split(r"(\s+)", old), re.split(r"(\s+)", new)
    out = []
    for op, i1, i2, j1, j2 in difflib.SequenceMatcher(None, a, b, autojunk=False).get_opcodes():
        if op == "equal":
            continue
        if i2 > i1:
            out.append("  - " + "".join(a[i1:i2]).strip()[:300])
        if j2 > j1:
            out.append("  + " + "".join(b[j1:j2]).strip()[:300])
    return out


def label(text):
    return re.sub(r"\s+", " ", text)[:90]


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", required=True, help="the commit to compare with")
    ap.add_argument("--edit", action="append", default=[], metavar="PREFIX",
                    help="the start of a base block that was rewritten on purpose")
    ap.add_argument("--rename", action="append", default=[], metavar="OLD=>NEW",
                    help="a heading that was renamed on purpose")
    ap.add_argument("--moves", action="store_true", help="print where every moved block went")
    a = ap.parse_args()
    sha = safe_rev(REPO, a.base)
    base_files, new_files = files_at(sha), files_now(REPO)
    base, new = {}, {}
    base["blocks"], base["heads"], base["where"], base["claims"] = inventory(base_files)
    new["blocks"], new["heads"], new["where"], new["claims"] = inventory(new_files)

    fail = []
    pairs, errs = resolve_edits(a.edit, base, new)
    fail += errs
    # An --edit accounts for ONE copy of its old block and ONE of its successor: a second identical
    # copy that also vanished is still reported missing.
    edited_old = collections.Counter(old for old, _ in pairs)
    edited_new = collections.Counter(n for _, n in pairs)
    missing = [(b, n) for b in base["blocks"]
               for n in [base["blocks"][b] - new["blocks"][b] - edited_old[b]] if n > 0]
    added = [(b, n) for b in new["blocks"]
             for n in [new["blocks"][b] - base["blocks"][b] - edited_new[b]] if n > 0]
    # Like an --edit, a --rename accounts for ONE occurrence: OLD must be gone that many more
    # times, and NEW must appear that many more times, than the renames of each claim.
    renamed_old, renamed_new = collections.Counter(), collections.Counter()
    for r in a.rename:
        if "=>" not in r:
            fail.append("--rename %r needs OLD=>NEW" % r)
            continue
        old, _, new_title = (s.strip() for s in r.partition("=>"))
        renamed_old[old] += 1
        renamed_new[new_title] += 1
    for old, k in renamed_old.items():
        if base["heads"][old] - new["heads"][old] < k:
            fail.append("--rename: heading %r is not gone %d time(s)" % (old[:60], k))
    for new_title, k in renamed_new.items():
        if new["heads"][new_title] - base["heads"][new_title] < k:
            fail.append("--rename: heading %r does not appear %d more time(s)" % (new_title[:60], k))
    lost_heads = [h for h in base["heads"] if base["heads"][h] - new["heads"][h] - renamed_old[h] > 0]
    claims_lost = base["claims"] - new["claims"]
    claims_new = new["claims"] - base["claims"]
    fail += frontmatter_problems(new_files)
    broken, n_cited = citation_breaks(base_files, new_files)
    fail += ["CITATION BROKEN: \"%s\" was in the knowledge base and is not now" % c for c in broken]

    moved = collections.Counter()
    moved_bytes = collections.Counter()
    for b in base["blocks"]:
        if b in new["where"] and sorted(set(base["where"][b])) != sorted(set(new["where"][b])):
            key = (", ".join(sorted(set(base["where"][b]))), ", ".join(sorted(set(new["where"][b]))))
            moved[key] += 1
            moved_bytes[key] += len(b.encode("utf-8"))

    size = lambda files: sum(len(t.encode("utf-8")) for t in files.values())
    print("base %s: %d files, %d bytes, %d blocks | now: %d files, %d bytes, %d blocks" % (
        sha[:8], len(base_files), size(base_files), sum(base["blocks"].values()),
        len(new_files), size(new_files), sum(new["blocks"].values())))
    print("citations: %d phrases cited in instruction files, %d broken" % (n_cited, len(broken)))
    print("moved: %d blocks, %d bytes, between %d file pairs%s" % (
        sum(moved.values()), sum(moved_bytes.values()), len(moved), "" if a.moves else " (--moves lists them)"))
    if a.moves:
        for key, n in sorted(moved.items(), key=lambda kv: -moved_bytes[kv[0]]):
            print("  %6d B %4d blocks  %s  →  %s" % (moved_bytes[key], n, key[0], key[1]))
    for old, new_text in pairs:
        print("edited in %s: %s" % (", ".join(sorted(set(new["where"][new_text]))), label(new_text)))
        print("\n".join(word_diff(old, new_text)))
    if added:
        print("new or duplicated blocks (%d, %d bytes) — review these:" % (
            sum(n for _, n in added), sum(len(b.encode("utf-8")) * n for b, n in added)))
        for b, n in added:
            print("  %s%s: %s" % ("%d× " % n if n > 1 else "", ", ".join(sorted(set(new["where"][b]))), label(b)))
    for b, n in missing:
        fail.append("MISSING %s(was in %s): %s" % ("%d× " % n if n > 1 else "",
                                                   ", ".join(sorted(set(base["where"][b]))), label(b)))
    for h in lost_heads:
        fail.append("HEADING GONE: %s" % h)
    for (suite, count), n in claims_lost.items():
        fail.append("CLAIM LOST: `%s` (%d cases)%s" % (suite, count, " ×%d" % n if n > 1 else ""))
    for (suite, count), n in claims_new.items():
        print("new claim: `%s` (%d cases)" % (suite, count))
    for f in fail:
        print(f)
    print("RESULT: %s" % ("FAIL" if fail else "PASS"))
    sys.exit(1 if fail else 0)


if __name__ == "__main__":
    main()
