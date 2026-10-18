#!/usr/bin/env python3
"""rules-for.py — print the .claude/rules/*.md files that govern a path.

    python3 scripts/rules-for.py PATH...     # repo-relative, cwd-relative or absolute
    python3 scripts/rules-for.py --changed   # every file this branch changes against
                                             # origin/main, staged or not, plus untracked
    python3 scripts/rules-for.py --self-test

Each rules file opens with a `paths:` list of globs and is binding for every
file a glob matches. Claude Code loads a rules file on its own when it reads a
matching file; other agents do not (AGENTS.md: Cursor loads AGENTS.md and
CLAUDE.md only), so they run this before they change a file, and `--changed`
before they commit. Ported from the iOS repo on 2026-10-18.

Glob semantics, as the rules files use them: `**/` matches any number of
directories (none included), a trailing `**` matches everything below, and `*`
and `?` stay inside one path segment. No rules file uses braces or character
classes. A pattern that does stops the script (exit 2) rather than being
guessed at, so the script is extended before it can answer wrongly.

Exit status: 0 on success (a path no rules file governs is not an error),
2 when a rules file or an argument cannot be read.
"""
import os
import re
import subprocess
import sys

RULES_DIR = os.path.join(".claude", "rules")
LIST_ITEM = re.compile(r"""^\s*-\s*(?:"([^"]*)"|'([^']*)'|([^"'#\s][^#]*?))\s*(?:#.*)?$""")
TOP_LEVEL_KEY = re.compile(r"^([A-Za-z_][\w-]*):")


class RulesError(Exception):
    pass


def repo_root():
    """The checkout this script lives in, so a worktree answers for itself."""
    here = os.path.dirname(os.path.abspath(__file__))
    try:
        out = subprocess.run(["git", "-C", here, "rev-parse", "--show-toplevel"],
                             capture_output=True, text=True, check=True).stdout.strip()
        if out:
            return out
    except (OSError, subprocess.CalledProcessError):
        pass
    return os.path.dirname(here)


def read_globs(path):
    """The `paths:` globs in a rules file's front matter, in order."""
    with open(path, encoding="utf-8") as f:
        lines = f.read().splitlines()
    if not lines or lines[0].strip() != "---":
        raise RulesError(f"{path}: no front matter")
    globs, in_paths, closed = [], False, False
    for line in lines[1:]:
        if line.strip() == "---":
            closed = True
            break
        key = TOP_LEVEL_KEY.match(line)
        if key:
            in_paths = key.group(1) == "paths"
            if in_paths and line.split(":", 1)[1].strip():
                raise RulesError(f"{path}: an inline `paths:` value is not supported")
            continue
        if not in_paths or not line.strip() or line.lstrip().startswith("#"):
            continue
        item = LIST_ITEM.match(line)
        if not item:
            raise RulesError(f"{path}: cannot read this `paths:` line: {line!r}")
        globs.append(next(g for g in item.groups() if g is not None).strip())
    if not closed:
        raise RulesError(f"{path}: front matter is not closed")
    return globs


def glob_regex(glob):
    if any(c in glob for c in "[]{}"):
        raise RulesError(f"unsupported glob syntax in {glob!r}: extend rules-for.py first")
    out, i, n = [], 0, len(glob)
    while i < n:
        if glob.startswith("**/", i):
            out.append("(?:.*/)?")
            i += 3
        elif glob.startswith("**", i):
            if i + 2 != n or (i > 0 and glob[i - 1] != "/"):
                raise RulesError(f"`**` must be a whole path segment in {glob!r}")
            out.append(".*")
            i += 2
        elif glob[i] == "*":
            out.append("[^/]*")
            i += 1
        elif glob[i] == "?":
            out.append("[^/]")
            i += 1
        else:
            out.append(re.escape(glob[i]))
            i += 1
    return re.compile("^" + "".join(out) + "$")


def load_rules(root):
    """[(rules file name, [(glob, compiled)])], sorted by name."""
    folder = os.path.join(root, RULES_DIR)
    rules = []
    for name in sorted(os.listdir(folder)):
        if name.endswith(".md"):
            globs = read_globs(os.path.join(folder, name))
            rules.append((name, [(g, glob_regex(g)) for g in globs]))
    return rules


def governing(rules, rel):
    return [name for name, pats in rules if any(rx.match(rel) for _, rx in pats)]


def repo_relative(root, path):
    """A path as the globs see it: relative to the root, with forward slashes."""
    real_root = os.path.realpath(root)
    rel = os.path.relpath(os.path.realpath(os.path.abspath(path)), real_root)
    if rel == os.pardir or rel.startswith(os.pardir + os.sep):
        # Not under the root from here: read it as already repo-relative.
        rel = os.path.normpath(path)
        if os.path.isabs(rel) or rel == os.pardir or rel.startswith(os.pardir + os.sep):
            raise RulesError(f"{path}: not inside {root}")
    return rel.replace(os.sep, "/")


def changed_files(root):
    def git(*args):
        return subprocess.run(["git", "-C", root, *args], capture_output=True,
                              text=True, check=True).stdout.splitlines()
    try:
        base = git("merge-base", "HEAD", "origin/main")[0]
    except (subprocess.CalledProcessError, IndexError):
        print("rules-for: no origin/main to compare with; listing uncommitted files only",
              file=sys.stderr)
        base = "HEAD"
    names = set(git("diff", "--name-only", base))
    names |= set(git("ls-files", "--others", "--exclude-standard"))
    return sorted(n for n in names if n)


def report(rules, rels):
    needed = []
    for rel in rels:
        names = governing(rules, rel)
        print(rel)
        if names:
            for name in names:
                print(f"    {RULES_DIR}/{name}")
                if name not in needed:
                    needed.append(name)
        else:
            print("    (no rules file; CLAUDE.md alone governs it)")
    if needed:
        print("\nRead in full before changing these files: "
              + ", ".join(f"{RULES_DIR}/{n}" for n in needed))


def self_test(root):
    passed = 0

    def check(ok, what):
        nonlocal passed
        if not ok:
            print(f"rules-for self-test FAILED: {what}", file=sys.stderr)
            sys.exit(1)
        passed += 1

    # Glob semantics.
    for glob, path, want in [
        ("**/*_test.go", "scanner_test.go", True),
        ("**/*_test.go", "internal/manifest/scanner_test.go", True),
        ("**/*_test.go", "internal/manifest/scanner.go", False),
        ("internal/manifest/scanner*.go", "internal/manifest/scanner_dupes.go", True),
        ("internal/manifest/scanner*.go", "internal/manifest/store.go", False),
        ("internal/manifest/scanner*.go", "internal/manifest/scanner/x.go", False),
        ("internal/dlna/**", "internal/dlna/discovery/client.go", True),
        ("internal/dlna/**", "internal/dlnax/server.go", False),
        ("**/testdata/**", "internal/transcode/testdata/flac/a.flac", True),
        ("internal/fsutil/owner*.go", "internal/fsutil/owner_unix.go", True),
        ("internal/fsutil/owner*.go", "internal/fsutil/lock.go", False),
        ("PROTOCOL.md", "PROTOCOL.md", True),
        ("PROTOCOL.md", "docs/PROTOCOL.md", False),
        ("a?c.md", "abc.md", True),
        ("a?c.md", "a/c.md", False),
        ("x.md", "xxmd", False),
    ]:
        check(bool(glob_regex(glob).match(path)) == want, f"{glob!r} on {path!r} should be {want}")
    for bad in ("**/{a,b}.swift", "**/[ab].swift", "a/**b", "a**"):
        try:
            glob_regex(bad)
            check(False, f"{bad!r} should be refused")
        except RulesError:
            passed += 1

    # The real rules files still have the shape this script reads.
    rules = load_rules(root)
    check(len(rules) > 0, "no rules files found")
    for name, pats in rules:
        check(len(pats) > 0, f"{name}: no `paths:` globs, so its front matter changed shape")

    # Pins against the real tree. Update them when a rules file or glob moves.
    def rules_for(rel):
        return governing(rules, rel)
    check("scanner.md" in rules_for("internal/manifest/scanner.go"), "scanner.go is governed by scanner.md")
    check("scanner.md" in rules_for("internal/manifest/extractors.go") and
          "jobs.md" not in rules_for("internal/manifest/extractors.go"),
          "extractors.go loads scanner.md and not jobs.md: internal/manifest is globbed by file name")
    check("jobs.md" in rules_for("internal/transcode/pool.go"), "transcode/pool.go is governed by jobs.md")
    check("wire.md" in rules_for("PROTOCOL.md"), "PROTOCOL.md is governed by wire.md")
    check("dlna-upnp.md" in rules_for("internal/dlna/discovery/client.go"),
          "dlna/discovery/client.go is governed by dlna-upnp.md")
    check("admin-console.md" in rules_for("internal/admin/static/app.js"), "app.js is governed by admin-console.md")
    check("build-ci-tests.md" in rules_for("internal/manifest/scanner_test.go"),
          "a test file loads build-ci-tests.md")
    check("build-ci-tests.md" not in rules_for("internal/manifest/scanner.go"),
          "a production file does not load build-ci-tests.md")
    check(rules_for("AGENTS.md") == [], "AGENTS.md is governed by no rules file")
    check(repo_relative(root, os.path.join(root, "AGENTS.md")) == "AGENTS.md",
          "an absolute path inside the root becomes repo-relative")
    print(f"rules-for self-test: {passed} checks passed")


def main(argv):
    root = repo_root()
    try:
        if argv == ["--self-test"]:
            self_test(root)
            return 0
        rules = load_rules(root)
        if argv == ["--changed"]:
            rels = changed_files(root)
            if not rels:
                print("rules-for: nothing changed")
                return 0
        elif argv and not any(a.startswith("--") for a in argv):
            rels = [repo_relative(root, a) for a in argv]
        else:
            print(__doc__.strip().split("\n\n")[1], file=sys.stderr)
            return 2
        report(rules, rels)
        return 0
    except (RulesError, OSError, subprocess.CalledProcessError) as error:
        print(f"rules-for: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
