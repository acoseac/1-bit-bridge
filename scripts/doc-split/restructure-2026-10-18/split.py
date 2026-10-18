#!/usr/bin/env python3
"""The 2026-10-18 restructure of CLAUDE.md, reproducible from a base commit.

  split.py [--base REV]               apply to the repository (refuses unless the files are REV's)
  split.py [--base REV] --out DIR     write every changed file into DIR instead (a reproduction)
  split.py [--base REV] --routes      print where every CLAUDE.md block goes, then stop
  split.py [--base REV] --coverage    for each rules file: the entries whose named files its globs
                                      miss, those files, how many files it loads with, and the
                                      five worst single-file loads; then stop
  split.py --edits                    print the verify_moves.py arguments this step needs

REV defaults to BASE, the commit this was written against. On a newer main the routing tables in
routes.py and prose.py still apply, and every prefix and title in them must match exactly one
block or heading, so a reworded entry stops the run instead of being routed by accident.

CLAUDE.md becomes a core of what every session needs. "Things that have bitten before" moves,
entry by entry, into the path-scoped rules files of routes.RULES under its original `###`
titles (now `##`), so a code comment citing "CLAUDE.md <title>" resolves by grepping
.claude/rules/. Seven procedure sections move verbatim into six skills. In what stays, a block
over mdblocks.KEEP_MAX bytes leaves one line (its bold title, its own bolded "Don't…/Never…"
sentences, `→ History-X.md`; or a hand-polished line from core_overrides.json, checked as
validate_overrides.py checks one) and its text moves verbatim to ops/History-X.md, under a
heading naming its section. The five dated review records become one line each, their text in
ops/History-Batches.md under their own headings, anchors included. Relative links are rebased
for the folder a block lands in. The edits in prose.EDITS fix the sentences elsewhere that the
split makes false (each one a declared --edit). Rules files over budget are condensed
afterwards by condense_rules.py, from the commit that holds this step's output (README.md).

Check the result with verify_moves.py --base BASE plus the --edit and --rename arguments that
`split.py --edits` prints: one --edit per base block that prose.CORE_EDITS, prose.EDITS or a
skill's "edits" rewrite, and one --rename for the heading prose.EDITS renames.
"""
import argparse
import collections
import importlib.util
import json
import os
import re
import shlex
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, HERE)
from mdblocks import (parse, movable, condense_entry, rebase_links, read_text, write_text,  # noqa: E402
                      safe_rev, git_show, staging_dir, clear_stage, HISTORY_DIR, DONT_RE)
import routes  # noqa: E402
import prose  # noqa: E402

REPO = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
BASE = "6f816e49"
DATE = routes.DATE
OUTPUTS = ("CLAUDE.md", "AGENTS.md", ".claude/rules/*.md", ".claude/skills/*/SKILL.md", "docs/*.md",
           HISTORY_DIR + "/*.md")
THINGS = "Things that have bitten before"
# Not "History-Reviews.md": .gitignore keeps ops/*review*.md untracked (review findings docs).
REVIEWS_FILE = "History-Batches.md"


def die(msg):
    sys.exit("split.py: " + msg)


def norm(text):
    return re.sub(r"\s+", " ", text).strip()


def titles(it):
    """(## title, ### title, #### title), '' where absent."""
    by = {lvl: t for lvl, t in it["path"]}
    return by.get(2, ""), by.get(3, ""), by.get(4, "")


def label_of(it):
    return " › ".join(t for lvl, t in it["path"] if lvl >= 2) or "(top of file)"


def one(prefix, candidates, what):
    hits = [c for c in candidates if c.startswith(prefix)]
    if len(hits) != 1:
        die("%s %r matches %d, not 1" % (what, prefix[:70], len(hits)))
    return hits[0]


# ---- routing ------------------------------------------------------------------------------------

def route(items):
    """{block index: destination} for every block of the base CLAUDE.md. A destination is "core",
    "core:<## title>", "skill:<name>", "rules:<file>" or "reviews"."""
    blocks = [(i, it) for i, it in enumerate(items) if it["type"] == "block"]
    tops = [it["title"] for it in items if it["type"] == "heading" and it["lvl"] == 2]
    subs = [it["title"] for it in items if it["type"] == "heading" and it["lvl"] == 3]
    explicit = {}
    for prefix, d in routes.BLOCK_DEST:
        hits = [i for i, it in blocks if norm(it["text"]).startswith(norm(prefix))]
        if len(hits) != 1:
            die("BLOCK_DEST prefix %r matches %d blocks, not 1" % (prefix[:70], len(hits)))
        explicit[hits[0]] = d
    top_dest = {one(p, tops, "TOP title"): d for p, d in routes.TOP}
    if set(tops) - set(top_dest):
        die("## sections with no TOP entry: %s" % sorted(set(tops) - set(top_dest)))
    sub_dest = {}
    for p, d in routes.SECTION_DEST:
        for t in subs:
            if t.startswith(p):
                sub_dest[t] = d
    if set(subs) - set(sub_dest):
        die("### sections with no SECTION_DEST entry: %s" % sorted(set(subs) - set(sub_dest)))
    out = {}
    for i, it in blocks:
        top, sub, _ = titles(it)
        if i in explicit:
            d = explicit[i]
        elif top.startswith(THINGS) and sub:
            d = sub_dest[sub]
        elif top:
            d = top_dest[top]
        else:
            d = "core"
        if d.endswith(".md") and not d.startswith("rules:"):
            d = "rules:" + d
        if d.startswith("rules:") and d[6:] not in routes.RULES:
            die("no RULES entry for %s" % d)
        out[i] = d
    return out


# ---- condensing what stays in CLAUDE.md --------------------------------------------------------

def load_core_overrides():
    path = os.path.join(HERE, "core_overrides.json")
    return json.loads(read_text(path)) if os.path.isfile(path) else {}


def history_for(it, dest):
    """The History file a core block's long form goes to."""
    sec = dest[5:] if dest.startswith("core:") else titles(it)[0]
    for prefix, hist in prose.CORE_HIST:
        if sec.startswith(prefix):
            return hist
    die("no CORE_HIST entry for section %r" % sec)


def keeps_whole(it, dest):
    sec = dest[5:] if dest.startswith("core:") else titles(it)[0]
    return any(sec.startswith(p) for p in prose.KEEP_WHOLE)


def checker():
    """validate_overrides.problems, loaded only when needed (it pulls in condense_rules)."""
    spec = importlib.util.spec_from_file_location("validate_overrides", os.path.join(os.path.dirname(HERE),
                                                                                     "validate_overrides.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module.problems


class Core:
    """Decides, for every core block, its line in CLAUDE.md and where its long form goes."""

    def __init__(self, items, dest, known_docs):
        self.items, self.dest, self.known_docs = items, dest, known_docs
        self.overrides = load_core_overrides()
        self.used = set()
        self.problems = checker()
        self.history = collections.defaultdict(list)  # file -> [(label, text)]
        self.lines = {}                                # block index -> its CLAUDE.md text
        self.notes = []
        for i, d in sorted(dest.items()):
            if d == "core" or d.startswith("core:"):
                self.lines[i] = self.decide(i, d)
        stale = set(self.overrides) - self.used
        if stale:
            die("core_overrides.json keys that name no condensed core block: %s" % sorted(k[:50] for k in stale))

    def decide(self, i, d):
        it = self.items[i]
        if keeps_whole(it, d) or not movable(it):
            return it["text"]
        hist = history_for(it, d)
        mech, fallback, donts = condense_entry(it, hist, known_docs=self.known_docs)
        keys = [k for k in self.overrides if norm(it["text"]).startswith(norm(k))]
        if len(keys) > 1:
            die("two core_overrides.json keys match the block on line %d" % (it["start"] + 1))
        line = mech
        if keys:
            line = self.overrides[keys[0]]
            self.used.add(keys[0])
            errs = self.problems({"block": it, "mechanical_line": mech, "donts": donts}, line)
            if errs:
                die("core_overrides.json line for L%d is rejected: %s" % (it["start"] + 1, "; ".join(errs)))
        elif fallback:
            self.notes.append("L%d has no bold title and no override: %s" % (it["start"] + 1, mech[:100]))
        self.history[hist].append((label_of(it), rebase_links(it["text"], "", HISTORY_DIR)))
        return line


# ---- assembling a file from blocks ---------------------------------------------------------------

def is_bullet(text):
    return bool(re.match(r"(?:[-*] |\d+\. )", text))


class Emitter:
    """Joins headings and blocks, keeping the base's spacing between items that were adjacent."""

    def __init__(self):
        self.parts, self.prev, self.prev_bullet = [], None, False

    def heading(self, text):
        self.parts.append(("\n\n" if self.parts else "") + text)
        self.prev, self.prev_bullet = None, False

    def block(self, text, idx=None, gap=None):
        if not self.parts:
            sep = ""
        elif idx is not None and self.prev == idx - 1 and gap is not None:
            sep = "\n" * (gap + 1)
        else:
            sep = "\n" if self.prev_bullet and is_bullet(text) else "\n\n"
        self.parts.append(sep + text)
        self.prev, self.prev_bullet = idx, is_bullet(text)

    def prose(self, text):
        self.parts.append(("\n\n" if self.parts else "") + text)
        self.prev, self.prev_bullet = None, False

    def text(self):
        return "".join(self.parts) + "\n"


# ---- CLAUDE.md ----------------------------------------------------------------------------------

def sections(items):
    """[(heading index, index of the first item after the section)]."""
    out = []
    for i, it in enumerate(items):
        if it["type"] == "heading":
            j = i + 1
            while j < len(items) and not (items[j]["type"] == "heading" and items[j]["lvl"] <= it["lvl"]):
                j += 1
            out.append((i, j))
    return out


def build_claude(items, dest, core, reviews):
    secs = sections(items)
    keep_heading = set()
    for i, j in secs:
        h = items[i]
        if h["lvl"] == 1:
            keep_heading.add(i)
        elif h["lvl"] == 2 and any(dest.get(k, "").startswith("core") for k in range(i + 1, j)):
            keep_heading.add(i)
    relocated = collections.defaultdict(list)
    for i, d in sorted(dest.items()):
        if d.startswith("core:"):
            relocated[one(d[5:], [items[k]["title"] for k, _ in secs if items[k]["lvl"] == 2], "core: target")].append(i)
    end_of = {items[i]["title"]: j for i, j in secs if items[i]["lvl"] == 2}
    first_review = min(i for i, it in enumerate(items) if it["type"] == "heading" and it["lvl"] == 3
                       and any(it["title"].startswith(p) for p, d in routes.SECTION_DEST if d == "reviews"))
    things = next(i for i, it in enumerate(items) if it["type"] == "heading" and it["title"].startswith(THINGS))
    first_sub = min(i for i, it in enumerate(items) if i > things and it["type"] == "heading" and it["lvl"] == 3)
    em = Emitter()
    intro_done = False
    tails = {one(t, list(end_of), "SECTION_TAIL title"): line for t, line in prose.SECTION_TAIL.items()}
    for i, it in enumerate(items):
        for title, stop in end_of.items():
            if i == stop and relocated.get(title):
                for k in relocated.pop(title):
                    em.block(core.lines[k])
            if i == stop and title in tails:
                em.block(tails.pop(title))
        if i == first_sub:
            em.prose(prose.MOVED_NOTE)
        if i == first_review:
            em.heading("### " + prose.REVIEW_HEADING)
            em.block(reviews)
        if it["type"] == "heading":
            if i in keep_heading:
                if it["lvl"] == 2 and it["title"] == "Build":
                    em.heading(prose.WHERE)
                em.heading(it["text"])
                if it["lvl"] == 2 and it["title"].startswith("Production deployments"):
                    em.prose(prose.DEPLOY_LINES)
            continue
        d = dest[i]
        if d == "core":
            em.block(core.lines[i], i, it["gap"])
            if not intro_done:
                em.prose(prose.ORG_NOTE)
                em.prose(prose.SKILLS_NOTE)
                intro_done = True
    for title, idxs in relocated.items():
        for k in idxs:
            em.block(core.lines[k])
    for line in tails.values():
        em.block(line)
    em.heading(prose.PROCEDURES)
    return em.text()


# ---- the review records -------------------------------------------------------------------------

def review_records(lines, items, dest):
    """[(heading item, raw text of its section)] for the dated records, in base order."""
    out = []
    for i, j in sections(items):
        h = items[i]
        if h["lvl"] == 3 and any(h["title"].startswith(p) for p, d in routes.SECTION_DEST if d == "reviews"):
            end = items[j]["start"] if j < len(items) else len(lines)
            body = "\n".join(lines[h["start"] + 1:end]).strip("\n")
            out.append((h, body, [k for k in range(i + 1, j) if items[k]["type"] == "block"]))
    return out


def review_lines(records, items):
    """CLAUDE.md's one line per record, hand-written in prose.REVIEW_LINES and checked like a
    polished line against the whole record."""
    problems = checker()
    rows = []
    for h, body, idxs in records:
        key = one_key(h["title"], prose.REVIEW_LINES)
        line = prose.REVIEW_LINES[key]
        text = h["title"] + "\n\n" + body
        donts = []
        for m in DONT_RE.finditer(text):
            s = norm(m.group(1))
            if s not in donts:
                donts.append(s)
        mech = "- **%s** → %s" % (key, REVIEWS_FILE)
        errs = problems({"block": {"text": text, "kind": "bullet"}, "mechanical_line": mech, "donts": donts}, line)
        if errs:
            die("REVIEW_LINES line for %r is rejected: %s" % (key[:50], "; ".join(errs)))
        rows.append(line)
    if len(rows) != len(prose.REVIEW_LINES):
        die("REVIEW_LINES has %d lines for %d records" % (len(prose.REVIEW_LINES), len(rows)))
    return "\n".join(rows)


def one_key(title, table):
    keys = [k for k in table if title.startswith(k) or norm(re.sub(r"<a name=\"[^\"]+\"></a>", "", title)).startswith(k)]
    if len(keys) != 1:
        die("%d REVIEW_LINES keys match the record %r" % (len(keys), title[:60]))
    return keys[0]


def history_reviews(records):
    parts = [prose.history_header("the dated review records", REVIEWS_FILE, DATE), "## " + THINGS]
    for h, body, _ in records:
        parts.append(h["text"])
        parts.append(rebase_links(body, "", HISTORY_DIR))
    return "\n\n".join(parts) + "\n"


# ---- rules files --------------------------------------------------------------------------------

def build_rules(items, dest):
    """{file: text} for every rules file."""
    groups = collections.defaultdict(list)  # file -> [(heading path, [block indexes])]
    for i, d in sorted(dest.items()):
        if not d.startswith("rules:"):
            continue
        name = d[6:]
        it = items[i]
        path = tuple((lvl, t) for lvl, t in it["path"] if lvl >= 2 and not (lvl == 2 and t.startswith(THINGS)))
        if groups[name] and groups[name][-1][0] == path:
            groups[name][-1][1].append(i)
        else:
            groups[name].append((path, [i]))
    out = {}
    for name, spec in routes.RULES.items():
        if name not in groups:
            die("rules file %s receives no block" % name)
        primary = [p for p, d in routes.SECTION_DEST if d == name] + [p for p, d in routes.TOP if d == "rules:" + name]
        own = [g for g in groups[name] if any(t.startswith(p) for _, t in g[0][:1] for p in primary)]
        moved = [g for g in groups[name] if g not in own]
        em = Emitter()
        em.heading("# " + spec["title"])
        # two short paragraphs, so that neither is long enough for a condensation to move it
        em.prose(spec["intro"])
        em.prose(prose.RULES_NOTE)
        shown = []
        for path, idxs in own + moved:
            # a group prints the part of its heading path that differs from the group before it,
            # each heading one level up from the base (### becomes ##)
            for n, (lvl, t) in enumerate(path):
                if shown[:n + 1] != list(path[:n + 1]):
                    em.heading("#" * (n + 2) + " " + t)
            shown = list(path)
            for k in idxs:
                em.block(rebase_links(items[k]["text"], "", ".claude/rules"), k, items[k]["gap"])
        fm = "---\npaths:\n" + "".join('  - "%s"\n' % g for g in spec["paths"]) + "---\n"
        out[".claude/rules/" + name] = fm + em.text()
    return out


# ---- skills -------------------------------------------------------------------------------------

def yaml_string(text):
    return json.dumps(text, ensure_ascii=False)


def build_skills(lines, items, dest):
    secs = {items[i]["title"]: (i, j) for i, j in sections(items) if items[i]["lvl"] == 2}
    out = {}
    for skill in prose.SKILLS:
        name = skill["name"]
        em = Emitter()
        em.heading("# " + skill["title"])
        em.prose(skill["intro"])
        mine = [i for i, d in sorted(dest.items()) if d == "skill:" + name]
        if not mine:
            die("skill %s receives no block" % name)
        done = set()
        for k in mine:
            if k in done:
                continue
            sec = titles(items[k])[0]
            i, j = secs[sec]
            section_blocks = [x for x in range(i + 1, j) if items[x]["type"] == "block"]
            if not any(x in done for x in section_blocks):
                em.heading("## " + sec)
            if all(dest[x] == "skill:" + name for x in section_blocks):
                # the whole section moves: its raw text, code fences and spacing included
                end = items[j]["start"] if j < len(items) else len(lines)
                raw = "\n".join(lines[items[i]["start"] + 1:end]).strip("\n")
                em.block(rebase_links(raw, "", ".claude/skills/" + name))
                done.update(section_blocks)
                em.prev = None
                continue
            em.block(rebase_links(items[k]["text"], "", ".claude/skills/" + name), k, items[k]["gap"])
            done.add(k)
        text = em.text()
        for old, new in skill.get("edits", []):
            if text.count(old) != 1:
                die("skill %s edit anchor found %d times: %s" % (name, text.count(old), old[:60]))
            text = text.replace(old, new)
        out[".claude/skills/%s/SKILL.md" % name] = "---\nname: %s\ndescription: %s\n---\n\n%s" % (
            name, yaml_string(skill["description"]), text)
    return out


# ---- History files ------------------------------------------------------------------------------

def build_history(core):
    out = {}
    for hist, entries in core.history.items():
        title = next((t for h, t in prose.HISTORY_TITLES if h == hist), None)
        if title is None:
            die("no HISTORY_TITLES entry for %s" % hist)
        parts, last = [prose.history_header(title, hist, DATE)], None
        for label, text in entries:
            if label != last:
                parts.append("## " + label)
                last = label
            parts.append(text)
        out["%s/%s" % (HISTORY_DIR, hist)] = "\n\n".join(parts) + "\n"
    return out


# ---- edits elsewhere ----------------------------------------------------------------------------

def apply_edits(files, sha):
    for rel, old, new in prose.EDITS:
        text = files.get(rel)
        if text is None:
            text = git_show(REPO, sha, rel)
        if text.count(old) != 1:
            die("%s: edit anchor found %d times, not once: %s" % (rel, text.count(old), old[:70]))
        files[rel] = text.replace(old, new)
    return files


def docs_at(sha):
    """The docs/*.md names at SHA: the doc refs a condensed line may carry."""
    out = subprocess.run(["git", "-C", REPO, "ls-tree", "--name-only", "--end-of-options", sha, "--", "docs/"],
                         capture_output=True, text=True, check=True).stdout
    return {os.path.basename(p) for p in out.split("\n") if p.endswith(".md") and p.count("/") == 1}


# ---- the whole step -----------------------------------------------------------------------------

def build(sha):
    raw = git_show(REPO, sha, "CLAUDE.md")
    lines, items = parse(raw)
    dest = route(items)
    core = Core(items, dest, docs_at(sha))
    records = review_records(lines, items, dest)
    reviews = review_lines(records, items)
    files = {"CLAUDE.md": build_claude(items, dest, core, reviews)}
    files.update(build_rules(items, dest))
    files.update(build_skills(lines, items, dest))
    files.update(build_history(core))
    files["%s/%s" % (HISTORY_DIR, REVIEWS_FILE)] = history_reviews(records)
    for rel, old, new in prose.CORE_EDITS:
        text = files["CLAUDE.md"]
        if text.count(old) != 1:
            die("CLAUDE.md edit anchor found %d times: %s" % (text.count(old), old[:70]))
        files["CLAUDE.md"] = text.replace(old, new)
    apply_edits(files, sha)
    return files, dest, items, core


# ---- reports ------------------------------------------------------------------------------------

def print_routes(dest, items):
    counts = collections.Counter()
    for i, d in sorted(dest.items()):
        it = items[i]
        counts[d] += len(it["text"].encode("utf-8"))
        print("L%-6d %-28s %s" % (it["start"] + 1, d, norm(it["text"])[:100]))
    print()
    for d, n in sorted(counts.items(), key=lambda kv: -kv[1]):
        print("%8d B  %s" % (n, d))


def budgets_module():
    spec = importlib.util.spec_from_file_location("check_doc_budgets", os.path.join(REPO, "scripts",
                                                                                     "check-doc-budgets.py"))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def coverage(files, dest, items):
    """Per rules file: entries that name source files and load with none of them, the cited files
    its globs miss, and how many tracked files it loads with; then the five worst loads."""
    import evidence
    cdb = budgets_module()
    tracked = cdb.tracked_files(REPO)
    index = evidence.Index(REPO, tracked)
    sizes, rx = {}, {}
    for rel, text in files.items():
        if rel.startswith(".claude/rules/"):
            rx[rel] = [cdb.glob_regex(g) for g in cdb.rules_globs(text)]
            sizes[rel] = len(text.encode("utf-8"))
    for rel in sorted(rx):
        name = rel.split("/")[-1]
        missed, unloaded, named = collections.Counter(), [], 0
        for i, d in sorted(dest.items()):
            if d != "rules:" + name:
                continue
            cited = index.files_named(items[i]["text"])
            if not cited:
                continue
            named += 1
            hit = [f for f in cited if any(r.match(f) for r in rx[rel])]
            missed.update(f for f in cited if f not in hit)
            if not hit:
                unloaded.append((items[i]["start"] + 1, sorted(cited)))
        loads_with = [f for f in tracked if any(r.match(f) for r in rx[rel])]
        print("%s: %d B; loads with %d tracked files; %d entries name a source file, %d of them load with none"
              % (name, sizes[rel], len(loads_with), named, len(unloaded)))
        for line, cited in unloaded:
            print("    L%-6d names %s" % (line, ", ".join(cited[:4]) + (" …" if len(cited) > 4 else "")))
        if missed:
            print("    cited files its globs miss: " + ", ".join("%s (%d)" % (f, n) for f, n in missed.most_common(10)))
    claude = len(files["CLAUDE.md"].encode("utf-8"))
    loads = []
    for f in tracked:
        pulled = [rel for rel in rx if any(r.match(f) for r in rx[rel])]
        loads.append((claude + sum(sizes[r] for r in pulled), f, pulled))
    loads.sort(key=lambda x: (-x[0], x[1]))
    print("\nCLAUDE.md: %d B. The five worst single-file loads:" % claude)
    for load, f, pulled in loads[:5]:
        print("  %7.1f KB  %s  (CLAUDE.md + %s)" % (load / 1000, f, ", ".join(r.split("/")[-1] for r in pulled)))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", default=BASE, help="the commit to restructure (default: %s)" % BASE)
    ap.add_argument("--out", help="write into this directory (outside the repository) instead of the repo")
    ap.add_argument("--routes", action="store_true", help="print where every CLAUDE.md block goes, then stop")
    ap.add_argument("--coverage", action="store_true", help="report the rules files' glob coverage, then stop")
    ap.add_argument("--edits", action="store_true", help="print the verify_moves.py arguments this step needs, "
                    "shell-quoted")
    a = ap.parse_args()
    sha = safe_rev(REPO, a.base)
    files, dest, items, core = build(sha)
    if a.routes:
        print_routes(dest, items)
        return
    if a.coverage:
        coverage(files, dest, items)
        return
    if a.edits:
        print(" ".join(["--edit " + shlex.quote(p) for p in prose.VERIFY_EDITS] +
                       ["--rename " + shlex.quote(r) for r in prose.VERIFY_RENAMES]))
        return
    if a.out:
        root = staging_dir(a.out, REPO, OUTPUTS)
        clear_stage(root, OUTPUTS)
    else:
        root = REPO
        changed = [rel for rel in files if os.path.exists(os.path.join(REPO, rel))
                   and read_text(os.path.join(REPO, rel)) != git_show_or_none(sha, rel)]
        if changed:
            die("these files differ from %s's, so this step ran already or they changed since: %s; "
                "reproduce with --out instead" % (a.base, ", ".join(changed)))
    ignored = subprocess.run(["git", "-C", REPO, "check-ignore", "--no-index", "--stdin"], input="\n".join(files),
                             capture_output=True, text=True).stdout.split()
    if ignored:
        die("these outputs match .gitignore, so no commit would carry them: %s" % ", ".join(ignored))
    for rel, text in sorted(files.items()):
        os.makedirs(os.path.dirname(os.path.join(root, rel)) or root, exist_ok=True)
        write_text(os.path.join(root, rel), text)
    for note in core.notes:
        print("note:", note)
    for rel in sorted(files):
        print("%-44s %8d bytes" % (rel, len(files[rel].encode("utf-8"))))


def git_show_or_none(sha, rel):
    r = subprocess.run(["git", "-C", REPO, "cat-file", "-e", "%s:%s" % (sha, rel)], capture_output=True)
    return git_show(REPO, sha, rel) if r.returncode == 0 else None


if __name__ == "__main__":
    main()
