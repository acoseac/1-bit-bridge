#!/usr/bin/env python3
"""Keep the instruction files Claude Code loads on its own within their byte budgets.

CLAUDE.md loads with every request, and each .claude/rules/*.md with every file its `paths:` globs
match, so a byte added to them is paid again on every request that loads it.

    budget   what
     64 KB   CLAUDE.md (the core since 2026-10-18: topic invariants live in rules files)
    100 KB   each .claude/rules/*.md (one without `paths:` loads everywhere, and is refused)
    480 KB   CLAUDE.md plus every rules file one tracked file pulls in, for the worst such file

A skill (.claude/skills/<name>/SKILL.md) costs only its description until it is invoked, so it
has no byte budget, but one whose `name:` differs from its folder or that has no `description:`
is refused: the description is all Claude sees of it at startup.

  check-doc-budgets.py              check all of them (`make docs-check` and CI's docs-check job run this)
  check-doc-budgets.py --hook       PostToolUse hook: after an edit to one of these files, tell
                                    Claude when a budget is exceeded (exit 2 shows it the message)
  check-doc-budgets.py --self-test  pin the glob matching, the checks and the hook's input handling

Over budget? Don't cut anything. In CLAUDE.md, first move a topic entry to the rules file that
loads with its code (verify_moves.py proves the move lost nothing). Or move an entry's long form
into a History file, which keeps it byte for byte and leaves a one-line pointer; the tool creates
the file or adds to an existing one:

  scripts/doc-split/move_entry.py FILE --largest     the candidates, each with its command
  scripts/doc-split/move_entry.py FILE --line N      move one (--dry-run shows it first)
"""
import glob
import json
import os
import re
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BUDGETS = {"claude": 64_000, "rules": 100_000, "load": 480_000}
NEAR = 0.97  # at or above this share of a budget the full check says so, and still passes


def glob_regex(pattern):
    """A `paths:` glob as a regex over repo-relative paths: `**/` spans any number of directories
    (none included, so it matches files at the root), `*` and `?` stay inside one path segment,
    and {a,b} alternates."""
    out, i = "", 0
    while i < len(pattern):
        if pattern.startswith("**/", i):
            out, i = out + "(?:.*/)?", i + 3
        elif pattern.startswith("**", i):
            out, i = out + ".*", i + 2
        elif pattern[i] == "*":
            out, i = out + "[^/]*", i + 1
        elif pattern[i] == "?":
            out, i = out + "[^/]", i + 1
        elif pattern[i] == "{" and "}" in pattern[i:]:
            end = pattern.index("}", i)
            out += "(?:" + "|".join(re.escape(p) for p in pattern[i + 1:end].split(",")) + ")"
            i = end + 1
        else:
            out, i = out + re.escape(pattern[i]), i + 1
    return re.compile(out + r"\Z")


def rules_globs(text):
    """The `paths:` list of a rules file's frontmatter ([] when it has none)."""
    if not text.startswith("---\n") or "\n---\n" not in text[3:]:
        return []
    front = text[4:text.index("\n---\n", 3)]
    globs, inside = [], False
    for line in front.split("\n"):
        stripped = line.strip()
        if re.fullmatch(r"paths:\s*(#.*)?", line):
            inside = True
        elif not stripped or stripped.startswith("#"):
            continue  # blank lines and YAML comments sit inside the list without ending it
        elif inside and re.match(r"\s*-\s", line):
            m = re.match(r"""\s*-\s*(?:"([^"]*)"|'([^']*)'|([^#]*?))\s*(?:#.*)?$""", line)
            globs.append(next(g for g in m.groups() if g is not None))
        else:
            inside = False
    return globs


def tracked_files(root):
    r = subprocess.run(["git", "-C", root, "ls-files", "-z"], capture_output=True, text=True)
    return [p for p in r.stdout.split("\0") if p] if r.returncode == 0 else []


# Plain scalars YAML 1.2's core schema reads as null, a boolean or a number rather than a string.
YAML_NON_STRING = re.compile(r"null|Null|NULL|~|true|True|TRUE|false|False|FALSE"
                             r"|[-+]?(?:\d+\.?\d*|\.\d+)(?:[eE][-+]?\d+)?|0o[0-7]+|0x[0-9a-fA-F]+"
                             r"|[-+]?\.(?:inf|Inf|INF)|\.(?:nan|NaN|NAN)")


def frontmatter_scalar(front, key):
    """The string value of the top-level KEY in YAML frontmatter FRONT, or None when absent: a plain,
    'single'- or "double"-quoted, or | / > block scalar. That is all of YAML a skill's name and
    description need, and it keeps these scripts free of a YAML dependency. Raises ValueError for
    a value YAML would not read as that string: an unclosed quote, ": " in a plain scalar, or a
    plain scalar YAML reads as null, a boolean or a number (`description: null`)."""
    lines = front.split("\n")
    for i, line in enumerate(lines):
        m = re.match(r"%s:(?:[ \t]+(.*))?$" % re.escape(key), line)
        if not m:
            continue
        value = (m.group(1) or "").strip()
        if value[:1] in ("|", ">"):
            block = []
            for nxt in lines[i + 1:]:
                if nxt.strip() and nxt[:1] not in (" ", "\t"):
                    break
                block.append(nxt.strip())
            return ("\n" if value[0] == "|" else " ").join(block).strip()
        if value.startswith('"'):
            try:
                return json.loads(value)  # YAML's double-quoted escapes include JSON's
            except ValueError:
                raise ValueError("%s: an unclosed or malformed double-quoted value" % key)
        if value.startswith("'"):
            if len(value) < 2 or not value.endswith("'"):
                raise ValueError("%s: an unclosed single-quoted value" % key)
            return value[1:-1].replace("''", "'")
        value = re.sub(r"\s+#.*$", "", value)
        if YAML_NON_STRING.fullmatch(value):
            raise ValueError("%s: YAML reads %r as a null, boolean or number, not text; quote it" % (key, value))
        if ": " in value:
            raise ValueError("%s: a plain value containing \": \" is not valid YAML; quote it" % key)
        return value
    return None


def skill_problems(rel, text):
    """Why the skill at REL (.claude/skills/<name>/SKILL.md) cannot be found or invoked, if it can't."""
    folder = rel.split("/")[2]
    front = text[4:text.index("\n---\n", 3)] if text.startswith("---\n") and "\n---\n" in text[3:] else ""
    errs = []
    try:
        name = frontmatter_scalar(front, "name")
        desc = frontmatter_scalar(front, "description")
    except ValueError as e:
        return ["has frontmatter Claude Code cannot read (%s)" % e]
    if (name or "").strip() != folder:
        errs.append("needs `name: %s` in its frontmatter" % folder)
    if not (desc or "").strip():
        errs.append("needs a non-empty `description:`, the only part of a skill loaded at startup")
    return errs


def measure(root, files=None):
    """Sizes of the always-loaded files and the worst single-file load under ROOT."""
    def size(rel):
        return os.path.getsize(os.path.join(root, rel))
    rules = {}
    folder = os.path.join(root, ".claude", "rules")
    for name in sorted(os.listdir(folder)) if os.path.isdir(folder) else []:
        if name.endswith(".md"):
            rel = ".claude/rules/" + name
            with open(os.path.join(root, rel), encoding="utf-8") as f:
                globs = rules_globs(f.read())
            rules[rel] = (size(rel), [glob_regex(g) for g in globs])
    skills = {}
    for path in sorted(glob.glob(os.path.join(root, ".claude", "skills", "*", "SKILL.md"))):
        rel = os.path.relpath(path, root).replace(os.sep, "/")
        with open(path, encoding="utf-8") as f:
            skills[rel] = skill_problems(rel, f.read())
    claude = size("CLAUDE.md")
    worst = (claude, "(no file matches a rules glob)", [])
    worst_with = {}  # rules file → the heaviest load among the files that pull it in
    for path in tracked_files(root) if files is None else files:
        pulled = [rel for rel, (_, rx) in rules.items() if any(r.match(path) for r in rx)]
        load = claude + sum(rules[rel][0] for rel in pulled)
        if load > worst[0]:
            worst = (load, path, pulled)
        for rel in pulled:
            if load > worst_with.get(rel, (0,))[0]:
                worst_with[rel] = (load, path, pulled)
    return {"claude": claude, "rules": {rel: s for rel, (s, _) in rules.items()},
            "unscoped": [rel for rel, (_, rx) in rules.items() if not rx], "worst": worst,
            "worst_with": worst_with, "skills": skills}


def problems(m, budgets=BUDGETS):
    """(file or load, size, budget, what) for every broken budget."""
    out = []
    if m["claude"] > budgets["claude"]:
        out.append(("CLAUDE.md", m["claude"], budgets["claude"], "loads with every request"))
    for rel, size in m["rules"].items():
        if size > budgets["rules"]:
            out.append((rel, size, budgets["rules"], "loads with every file its paths: match"))
    for rel in m["unscoped"]:
        out.append((rel, m["rules"][rel], 0, "has no paths:, so it loads with every request — give it globs"))
    for rel, errs in m.get("skills", {}).items():
        if errs:
            out.append((rel, 0, 0, "; ".join(errs)))
    load, path, pulled = m["worst"]
    if load > budgets["load"]:
        out.append(("load:" + path, load, budgets["load"],
                    "CLAUDE.md + %s all load when %s is read" % (", ".join(pulled), path)))
    return out


def advice(root, target, m, pulled=None):
    """What to run for one broken budget: move_entry.py's largest candidates in the file to shrink
    (for a load, the largest rules file in it)."""
    if target.startswith("load:"):
        pulled = m["worst"][2] if pulled is None else pulled
        target = max(pulled, key=lambda rel: m["rules"][rel], default="CLAUDE.md")
    lines = ["  Don't cut content. " + ("A topic entry belongs in the rules file that loads with its code "
                                        "(CLAUDE.md \"Where things live\"); or m" if target == "CLAUDE.md" else "M")
             + "ove an entry's long form out (it stays verbatim in a History file):",
             "    scripts/doc-split/move_entry.py %s --largest" % target]
    try:
        sys.path.insert(0, os.path.join(root, "scripts", "doc-split"))
        import move_entry
        with open(os.path.join(root, target), encoding="utf-8") as f:
            _, _, items, off = move_entry.document(f.read())
        for it in sorted(move_entry.candidates(items), key=lambda it: -len(it["text"].encode("utf-8")))[:3]:
            lines.append("    scripts/doc-split/move_entry.py %s --line %d   # %d B, %s" % (
                target, it["start"] + 1 + off, len(it["text"].encode("utf-8")), move_entry.label_of(it)[:48]))
    except (ImportError, OSError, ValueError):
        pass
    return lines


def report(root, m, budgets=BUDGETS):
    broken = problems(m, budgets)
    shares = [("CLAUDE.md", m["claude"], budgets["claude"])]
    if m["rules"]:
        rel = max(m["rules"], key=m["rules"].get)
        shares.append((rel.rsplit("/", 1)[1], m["rules"][rel], budgets["rules"]))
    shares.append(("worst load, " + os.path.basename(m["worst"][1]), m["worst"][0], budgets["load"]))
    near = [s for s in shares if s[1] >= NEAR * s[2]]
    print("check-doc-budgets: %s — %s." % ("OVER BUDGET" if broken else "ok", "; ".join(
        "%s %.1f%%" % (label, 100 * size / budget) for label, size, budget in shares)))
    for target, size, budget, what in broken:
        name = target[5:] if target.startswith("load:") else target
        if budget or size:
            print("  %s: %s bytes, budget %s — %s." % (name, format(size, ","), format(budget, ","), what))
        else:
            print("  %s %s." % (name, what))
        if budget:
            print("\n".join(advice(root, target, m)))
    if near and not broken:
        print("  close to a budget: %s. Before adding more there, move an entry out "
              "(scripts/doc-split/move_entry.py FILE --largest)." % ", ".join(label for label, _, _ in near))
    return 1 if broken else 0


def hook(root, stdin, budgets=BUDGETS):
    """PostToolUse: exit 2 with a message for Claude when the edited file breaks a budget."""
    try:
        event = json.loads(stdin)
        path = (event.get("tool_input") or {}).get("file_path")
    except (ValueError, AttributeError):
        return 0, ""
    if not isinstance(path, str):
        return 0, ""
    rel = os.path.relpath(os.path.realpath(path), os.path.realpath(root)).replace(os.sep, "/")
    if rel != "CLAUDE.md" and not re.fullmatch(r"\.claude/(rules/[^/]+\.md|skills/[^/]+/SKILL\.md)", rel):
        return 0, ""
    m = measure(root)
    mine = [p for p in problems(m, budgets) if p[0] == rel]
    # A load counts against this edit only when it includes the edited file: CLAUDE.md is in
    # every load, a rules file only in the loads of the files its globs match.
    load, path, pulled = m["worst"] if rel == "CLAUDE.md" else m["worst_with"].get(rel, (0, "", []))
    if load > budgets["load"]:
        mine.append(("load:" + path, load, budgets["load"],
                     "CLAUDE.md + %s all load when %s is read" % (", ".join(pulled), path)))
    if not mine:
        return 0, ""
    msg = []
    for target, size, budget, what in mine:
        name = target[5:] if target.startswith("load:") else target
        if budget:
            msg.append("Budget exceeded — %s is %s bytes (budget %s): %s." % (
                name, format(size, ","), format(budget, ","), what))
        else:
            msg.append("%s %s." % (name, what))
        if budget:
            msg += advice(root, target, m, pulled if target.startswith("load:") else None)
    msg.append("CI's docs-check job and `make docs-check` fail while this stands. Details: scripts/check-doc-budgets.py")
    return 2, "\n".join(msg)


def self_test():
    ok = True

    def expect(name, got, want):
        nonlocal ok
        if got != want:
            ok = False
            print("FAIL %s: got %r, want %r" % (name, got, want))

    g = glob_regex("**/scanner.go")
    expect("**/ matches a file at the root", bool(g.match("scanner.go")), True)
    expect("**/ matches a nested file", bool(g.match("internal/manifest/scanner.go")), True)
    expect("the name must match whole", bool(g.match("a/scanner.gox")), False)
    expect("* stays inside a segment", bool(glob_regex("internal/manifest/scanner*.go").match(
        "internal/manifest/scanner/x.go")), False)
    expect("a trailing ** reaches every depth", bool(glob_regex("internal/dlna/**").match(
        "internal/dlna/discovery/client.go")), True)
    expect("braces alternate", bool(glob_regex("src/**/*.{ts,tsx}").match("src/a/b.tsx")), True)
    expect("paths: list is read",
           rules_globs('---\npaths:\n  - "**/A.go"\n  - "**/B*.go"\n---\n# x\n'), ["**/A.go", "**/B*.go"])
    expect("no frontmatter, no globs", rules_globs("# x\n"), [])
    expect("comments inside the list do not end it",
           rules_globs('---\npaths:\n  # why A\n  - "**/A.go"\n  # why B\n  - "**/B.go"  # trailing\n---\n'),
           ["**/A.go", "**/B.go"])
    expect("an unindented, unquoted list works", rules_globs("---\npaths:\n- **/A.go\n---\n"), ["**/A.go"])
    expect("another key ends the list", rules_globs('---\npaths:\n  - "a"\nother: 1\n  - "b"\n---\n'), ["a"])

    event_for = lambda p: json.dumps({"tool_name": "Edit", "tool_input": {"file_path": p}})
    with tempfile.TemporaryDirectory() as root:
        os.makedirs(os.path.join(root, ".claude", "rules"))
        with open(os.path.join(root, "CLAUDE.md"), "w", encoding="utf-8") as f:
            f.write("# t\n\n" + "- **A rule that is long enough to move.** " + "x" * 600 + "\n")
        with open(os.path.join(root, ".claude/rules/a.md"), "w", encoding="utf-8") as f:
            f.write('---\npaths:\n  - "**/A.go"\n---\n# a\n\n' + "y" * 300 + "\n")
        with open(os.path.join(root, ".claude/rules/b.md"), "w", encoding="utf-8") as f:
            f.write("# b, no paths\n")
        m = measure(root, files=["src/A.go", "README.md"])
        expect("worst load names the file", m["worst"][1], "src/A.go")
        expect("a rules file without paths: is flagged", [p[0] for p in problems(m)], [".claude/rules/b.md"])
        os.remove(os.path.join(root, ".claude/rules/b.md"))
        os.makedirs(os.path.join(root, ".claude", "skills", "ok"))
        os.makedirs(os.path.join(root, ".claude", "skills", "bad"))
        with open(os.path.join(root, ".claude/skills/ok/SKILL.md"), "w", encoding="utf-8") as f:
            f.write('---\nname: ok\ndescription: "Use it when: a thing happens."\n---\n# ok\n')
        with open(os.path.join(root, ".claude/skills/bad/SKILL.md"), "w", encoding="utf-8") as f:
            f.write("---\nname: other\n---\n# no description\n")
        m = measure(root, files=[])
        expect("a skill with a wrong name and no description is flagged, a good one is not",
               [(p[0], p[3].count("needs")) for p in problems(m)], [(".claude/skills/bad/SKILL.md", 2)])
        scalar = lambda fm: skill_problems(".claude/skills/s/SKILL.md", "---\n%s\n---\n" % fm)
        expect("a quoted name is read as its value", scalar('name: "s"\ndescription: d'), [])
        expect("a single-quoted description works", scalar("name: s\ndescription: 'it''s fine'"), [])
        expect("a folded description works", scalar("name: s\ndescription: >\n  Use it\n  when x.\nx: 1"), [])
        expect("an empty quoted description is flagged", len(scalar('name: s\ndescription: ""')), 1)
        expect("a plain description with \": \" is flagged as unreadable",
               "cannot read" in " ".join(scalar("name: s\ndescription: Use it when: x")), True)
        expect("description: null is not text", "cannot read" in " ".join(scalar("name: s\ndescription: null")), True)
        expect("description: ~ is not text", "cannot read" in " ".join(scalar("name: s\ndescription: ~")), True)
        expect("a numeric or boolean description is not text",
               ["cannot read" in " ".join(scalar("name: s\ndescription: %s" % v)) for v in ("123", "1.5e3", "true", "False")],
               [True] * 4)
        expect("a quoted \"null\" is text", scalar('name: s\ndescription: "null"'), [])
        expect("a plain description that merely starts with a number is text", scalar("name: s\ndescription: 3 ways to x"), [])
        code, msg = hook(root, event_for(os.path.join(root, ".claude/skills/bad/SKILL.md")))
        expect("hook speaks on an edit that breaks a skill", (code, "non-empty `description:`" in msg), (2, True))
        expect("hook stays quiet on a good skill", hook(root, event_for(os.path.join(root, ".claude/skills/ok/SKILL.md")))[0], 0)
        os.remove(os.path.join(root, ".claude/skills/bad/SKILL.md"))
        small = {"claude": 500, "rules": 1000, "load": 100_000}
        expect("CLAUDE.md over its budget", [p[0] for p in problems(measure(root, files=[]), small)], ["CLAUDE.md"])
        event = lambda p: json.dumps({"tool_name": "Edit", "tool_input": {"file_path": p}})
        expect("hook ignores other files", hook(root, event(os.path.join(root, "src/A.go")), small)[0], 0)
        expect("hook ignores unreadable input", hook(root, "not json", small)[0], 0)
        code, msg = hook(root, event(os.path.join(root, "CLAUDE.md")), small)
        expect("hook speaks on an over-budget edit", code, 2)
        expect("hook message names the file and the budget", "CLAUDE.md is" in msg and "budget 500" in msg, True)
        expect("hook stays quiet within budget", hook(root, event(os.path.join(root, "CLAUDE.md")))[0], 0)
        with open(os.path.join(root, ".claude/rules/c.md"), "w", encoding="utf-8") as f:
            f.write('---\npaths:\n  - "**/C.go"\n---\n# c\n')
        tight = {"claude": 100_000, "rules": 100_000, "load": 900}
        files = ["src/A.go", "src/C.go"]
        real_measure = measure
        globals()["measure"] = lambda r, files=files: real_measure(r, files)
        try:
            expect("a load over budget is reported to an edit of a rules file it includes",
                   hook(root, event(os.path.join(root, ".claude/rules/a.md")), tight)[0], 2)
            expect("but not to an edit of a rules file it does not include",
                   hook(root, event(os.path.join(root, ".claude/rules/c.md")), tight)[0], 0)
        finally:
            globals()["measure"] = real_measure
    print("self-test:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


def main():
    if "--self-test" in sys.argv[1:]:
        return self_test()
    if "--hook" in sys.argv[1:]:
        code, msg = hook(REPO, sys.stdin.read())
        if msg:
            print(msg, file=sys.stderr)
        return code
    if sys.argv[1:]:
        sys.exit(__doc__)
    return report(REPO, measure(REPO))


if __name__ == "__main__":
    sys.exit(main())
