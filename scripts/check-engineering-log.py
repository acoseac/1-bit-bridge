#!/usr/bin/env python3
"""The engineering log: its archive frozen, and every record since in a file of its own.

ops/engineering-log.md holds every record up to 2026-11-18 and is frozen: its size and the SHA-256
of its bytes are pinned below, so no edit of it passes. Every record since is a file of its own, so
that two PRs never write one file (backlog B888: every PR appended to the one log, and each merge
left every open PR conflicting at its end):

    ops/engineering-log/<YYYY-MM>/<YYYY-MM-DD>-<entry>--<branch>.md

  <YYYY-MM-DD>  the day the entry is written, the date its heading carries; <YYYY-MM> its month
  <entry>       the backlog item (b888, b728-b9-pr-3b), or a topic for an entry with none; a later
                entry of the same PR adds a word (b601-round-2)
  <branch>      the PR's branch, so two open PRs never pick one name; a slug longer than 56
                characters keeps its first 47, then "-" and the first 8 hex digits of the whole
                slug's SHA-256 (--slug prints it)

Both are slugs: lowercase letters and digits, each other run of characters one "-", none at either
end, so the first "--" after the date ends <entry>. The name is at most 120 characters. An entry is
a regular file of UTF-8 with LF line ends and a final newline, its first line `## <date> — <title>`
with its name's date, and no other `## ` line outside fenced code. The folder's README.md is the
one other file it holds.

  check-engineering-log.py              check the archive, every entry, and that nothing shaped
                                        like one sits anywhere else (`make docs-check` and CI's
                                        docs-check job do this)
  check-engineering-log.py --slug NAME  print the slug a branch NAME takes in an entry's name
  check-engineering-log.py --self-test  pin the freeze, the names, the contents and the listing

The PostToolUse hook (check-doc-budgets.py --hook) hands hook_for each edited path of the log's,
which checks that one path: it lists nothing and asks no git.
"""
import datetime
import hashlib
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ARCHIVE = "ops/engineering-log.md"
FOLDER = "ops/engineering-log/"
README = FOLDER + "README.md"

# The archive as the cut-over left it, its closing pointer included. Only a redaction the owner
# approves changes these two values: a placeholder of the same line count where the text must go,
# both values updated in that PR, and an entry saying why. Review refuses any other change to them.
FROZEN_BYTES = 7_148_762
FROZEN_SHA256 = "9acb68e40606e19a09105ede3671e10864c4f0d826a5e4c88546d1010ae9c62e"

NAME_MAX = 120  # characters in an entry's file name
BRANCH_MAX = 56  # characters in its branch slug; slug() shortens a longer one
BRANCH_KEEP = 47  # what a shortened slug keeps of the whole one, before "-" and the hash
HASH_DIGITS = 8

SLUG = r"[a-z0-9]+(?:-[a-z0-9]+)*"
# An entry's path below the folder: the month, a date in that month, <entry>, "--", <branch>. Its
# digits are ASCII's: Python's \d also takes every other script's, which int() reads as a date.
ENTRY_PATH = re.compile(r"([0-9]{4}-[0-9]{2})/(\1-[0-9]{2})-(%s)--(%s)\.md" % (SLUG, SLUG))
HEADING = re.compile(r"## ([0-9]{4}-[0-9]{2}-[0-9]{2}) — (\S.*)")
H2 = re.compile(r"##(?:[ \t]|$)")
QUOTE = re.compile(r" {0,3}> ?")
FENCE_OPEN = re.compile(r"(`{3,}|~{3,})(.*)")
FENCE_CLOSE = re.compile(r"(`{3,}|~{3,})[ \t]*")
# An entry filed anywhere else, by its name alone: a dated Markdown file directly in ops/ (where git
# ignores *review*, *audit* and *findings*), or a file named like an entry in any folder.
DATED_IN_OPS = re.compile(r"ops/\d{4}-\d{2}-\d{2}[^/]*\.md", re.IGNORECASE)
ENTRY_SHAPED = re.compile(r"\d{4}-\d{2}-\d{2}-[^/]*--[^/]*\.md", re.IGNORECASE)

WAY = ("a record goes in a file of its own, ops/engineering-log/<YYYY-MM>/<YYYY-MM-DD>-<entry>--<branch>.md "
       "(ops/engineering-log/README.md; `python3 scripts/check-engineering-log.py --slug <branch>` "
       "prints a branch's slug)")
RESTORE = "restore the archive with `git checkout origin/main -- ops/engineering-log.md`"


def slug(text):
    """TEXT as an entry's name takes it: lowercased, each run of characters other than a-z and 0-9
    one "-", none at either end. One longer than BRANCH_MAX keeps its first BRANCH_KEEP characters
    (no "-" at their end), then "-" and the first HASH_DIGITS hex digits of the whole slug's
    SHA-256, so two long branches that begin alike keep apart. "" when nothing is left."""
    s = re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")
    if len(s) > BRANCH_MAX:
        s = s[:BRANCH_KEEP].rstrip("-") + "-" + hashlib.sha256(s.encode("utf-8")).hexdigest()[:HASH_DIGITS]
    return s


def name_problems(sub):
    """Why SUB, a path below ops/engineering-log/, is not an entry's name ([] when it is one)."""
    m = ENTRY_PATH.fullmatch(sub)
    if not m:
        hint = ""
        if sub != sub.lower():
            hint = " (it has a capital letter)"
        elif "/" not in sub:
            hint = " (it sits outside a month folder)"
        elif re.fullmatch(r"([0-9]{4}-[0-9]{2})/([0-9]{4}-[0-9]{2})-[0-9]{2}-.*", sub) and sub[:7] != sub[8:15]:
            hint = " (its folder is not its date's month)"
        return ["is not named <YYYY-MM>/<YYYY-MM-DD>-<entry>--<branch>.md: the month folder of its "
                "date, then <entry> and <branch> in lowercase letters and digits, words joined by "
                "one \"-\", the two parts by \"--\"%s" % hint]
    out = []
    try:
        datetime.date(int(m.group(2)[:4]), int(m.group(2)[5:7]), int(m.group(2)[8:]))
    except ValueError:
        out.append("is dated %s, which is no day of the calendar" % m.group(2))
    base = sub.rsplit("/", 1)[-1]
    if len(base) > NAME_MAX:
        out.append("has a name of %d characters, more than %d: shorten <entry>" % (len(base), NAME_MAX))
    if len(m.group(4)) > BRANCH_MAX:
        out.append("has a branch part of %d characters, more than %d: use the slug `--slug <branch>` "
                   "prints" % (len(m.group(4)), BRANCH_MAX))
    return out


def quote_depth(line, limit=None):
    """(how many block quote markers came off LINE, at most LIMIT of them, and what is left)."""
    depth = 0
    while limit is None or depth < limit:
        m = QUOTE.match(line)
        if not m:
            break
        depth, line = depth + 1, line[m.end():]
    return depth, line


def h2_lines(text):
    """The numbers of TEXT's lines that are `## ` headings outside fenced code: after up to three
    spaces, `##` then a space, a tab or the line's end, in no block quote.

    The fences are markdownShellLines' (scripts/third-party-licenses/tags.go): a line's block quote
    markers (`>`, up to three spaces before each and one after) come off first, then up to three
    spaces; a fence opens on three or more backticks or tildes, a backtick fence's info holding no
    backtick, and closes on a line of the same character, at least as many of them, with nothing
    after them but white space; a fence opened in a block quote ends where that quote ends, and the
    line that ends it is read afresh. List items are no containers here: a fence on a list item's
    own line is not seen."""
    found, fence = [], None  # fence: (its character, how many, the quote depth it opened at)
    for number, line in enumerate(text.split("\n"), 1):
        line = line.replace("\t", "    ")
        if fence:
            depth, rest = quote_depth(line, fence[2])
            if depth == fence[2]:
                indent = len(rest) - len(rest.lstrip(" "))
                m = FENCE_CLOSE.fullmatch(rest[indent:]) if indent <= 3 else None
                if m and m.group(1)[0] == fence[0] and len(m.group(1)) >= fence[1]:
                    fence = None
                continue
            fence = None  # the block quote it opened in has ended, and the fence with it
        depth, rest = quote_depth(line)
        indent = len(rest) - len(rest.lstrip(" "))
        if indent > 3:
            continue
        rest = rest[indent:]
        m = FENCE_OPEN.fullmatch(rest)
        if m and not (m.group(1)[0] == "`" and "`" in m.group(2)):
            fence = (m.group(1)[0], len(m.group(1)), depth)
        elif depth == 0 and H2.match(rest):
            found.append(number)
    return found


def content_problems(sub, data):
    """Why DATA, the bytes of SUB below ops/engineering-log/, is not an entry's (or the README's)."""
    out = []
    if b"\r" in data:
        out.append("holds a carriage return: its lines end in LF alone")
    if not data.endswith(b"\n"):
        out.append("does not end in a newline")
    try:
        text = data.decode("utf-8").replace("\r\n", "\n")
    except UnicodeDecodeError:
        return out + ["is not UTF-8"]
    if sub == "README.md":
        return out
    m = ENTRY_PATH.fullmatch(sub)
    date = m.group(2) if m else "<YYYY-MM-DD>"
    first = HEADING.fullmatch(text.split("\n", 1)[0])
    if not first or first.group(1) != date:
        out.append("does not begin with its heading, `## %s — <title>`, the date its name's" % date)
    count = len(h2_lines(text))
    if count != 1:
        out.append("holds %d `## ` lines outside fenced code, not one: each entry is a file of its own" % count)
    return out


def path_state(root, rel):
    """How REL, a path below ROOT spelled with "/", stands, each of its components asked by lstat so
    that no link is followed: ("file", REL, stat) for a regular file reached through folders alone,
    else (why, the component at fault, None), why one of "missing" (it, or a folder on its way, is
    not there), "a link", "not a folder" (a component on the way) or "not a regular file"."""
    parts = rel.split("/")
    st = None
    for k in range(1, len(parts) + 1):
        component = "/".join(parts[:k])
        try:
            st = os.lstat(os.path.join(root, *parts[:k]))
        except (FileNotFoundError, NotADirectoryError):
            return "missing", component, None
        if stat.S_ISLNK(st.st_mode):
            return "a link", component, None
        if k < len(parts) and not stat.S_ISDIR(st.st_mode):
            return "not a folder", component, None
    if not stat.S_ISREG(st.st_mode):
        return "not a regular file", rel, None
    return "file", rel, st


def read_bytes(root, rel, st):
    """REL's bytes as they are, no newline translated, from the file path_state saw (ST); None when
    it cannot be opened, or another file stands there now. O_NONBLOCK, so a named pipe put there
    since the lstat is opened without waiting for a writer, and then refused as another file."""
    flags = os.O_RDONLY | getattr(os, "O_BINARY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
    try:
        fd = os.open(os.path.join(root, *rel.split("/")), flags)
    except OSError:
        return None
    with os.fdopen(fd, "rb") as f:
        now = os.fstat(f.fileno())
        if not stat.S_ISREG(now.st_mode) or not os.path.samestat(now, st):  # an inode number is reused
            return None
        return f.read()


def archive_problems(root, size=None, digest=None):
    """Why the archive under ROOT is not the frozen one: anything but a regular file reached through
    no link, another size, or other bytes of that size ([] when it is the one SIZE and DIGEST pin,
    FROZEN_BYTES and FROZEN_SHA256 unless given). The size is asked first, to say how it moved."""
    size = FROZEN_BYTES if size is None else size
    digest = FROZEN_SHA256 if digest is None else digest
    frozen = "%s is frozen at %s bytes (sha256 %s…)" % (ARCHIVE, format(size, ","), digest[:12])
    state, at, st = path_state(root, ARCHIVE)
    if state != "file":
        return ["%s, and %s is %s: %s; %s." % (frozen, at, "not there" if state == "missing" else state, WAY, RESTORE)]
    if st.st_size != size:
        moved = st.st_size - size
        return ["%s and is now %s: it %s by %s byte%s. Never edit it: %s; %s." % (
            frozen, format(st.st_size, ","), "grew" if moved > 0 else "shrank", format(abs(moved), ","),
            "" if abs(moved) == 1 else "s", WAY, RESTORE)]
    data = read_bytes(root, ARCHIVE, st)
    got = hashlib.sha256(data).hexdigest() if data is not None else "(unread)"
    if got != digest:
        return ["%s and holds other bytes of that size now (sha256 %s…). Never edit it: %s; %s." % (
            frozen, got[:12], WAY, RESTORE)]
    return []


def classify(rel):
    """What REL, a repository path spelled with "/", is to the log: "archive", "readme", "entry"
    (anything below the folder, by its exact path), "misfiled" (the log's paths in another case,
    or a name shaped like an entry outside the folder), or None."""
    if rel == ARCHIVE:
        return "archive"
    if rel == README:
        return "readme"
    if rel.startswith(FOLDER):
        return "entry"
    low = rel.lower()
    if low == ARCHIVE or low.startswith(FOLDER) or DATED_IN_OPS.fullmatch(rel) or \
            ENTRY_SHAPED.fullmatch(rel.rsplit("/", 1)[-1]):
        return "misfiled"
    return None


def misfiled_problem(rel):
    if rel.lower() == ARCHIVE or rel.lower().startswith(FOLDER):
        return ("%s: the log's paths are lowercase, ops/engineering-log.md and ops/engineering-log/; "
                "this one differs in case, which a volume that folds case takes for the other" % rel)
    return ("%s: an entry is filed below ops/engineering-log/<YYYY-MM>/, never directly in ops/ (where "
            "git ignores *review*, *audit* and *findings* names) or in another folder" % rel)


def file_problems(root, rel):
    """Why REL, the README or an entry by its exact path, is not one: its name, how it stands on
    disk, and its bytes."""
    sub = rel[len(FOLDER):]
    out = [] if rel == README else name_problems(sub)
    state, at, st = path_state(root, rel)
    if state == "missing":
        return out + ["is listed by git and missing from the tree"]
    if state != "file":
        return out + ["stands where %s is %s: an entry is a regular file in real folders, as git keeps "
                      "it" % (at, state)]
    if not rel.lower().endswith(".md"):
        return out  # no Markdown, so no record: its name says all there is
    data = read_bytes(root, rel, st)
    if data is None:
        return out + ["could not be read"]
    return out + content_problems(sub, data)


def git_paths(root, args, env=None):
    """The paths `git ls-files -z ARGS` prints under ROOT, as git wrote them (any byte kept)."""
    r = subprocess.run(["git", "-C", root, "ls-files", "-z"] + list(args), capture_output=True, env=env)
    if r.returncode:
        raise OSError("git ls-files %s: %s" % (" ".join(args), r.stderr.decode("utf-8", "replace").strip()))
    return [p.decode("utf-8", "surrogateescape") for p in r.stdout.split(b"\0") if p]


def listing(root, env=None):
    """(every path git tracks, repository-wide; {path: whether git ignores it} for every untracked
    Markdown file below a top-level folder named like ops in any case, ignored ones included).
    Untracked paths are judged by their names alone, what an ignored file holds being nobody's to
    read here, and only a name that could be a record's: one ending in .md that does not begin
    with "." (an editor's lock, the OS's .DS_Store and a backup's ~ are no records; committed,
    git lists them as tracked, and they are refused)."""
    tracked = git_paths(root, ["--cached"], env)
    tops = sorted(n for n in os.listdir(root) if n.lower() == "ops" and os.path.isdir(os.path.join(root, n)))
    untracked = {}
    if tops:
        ignored = set(git_paths(root, ["--others", "--ignored", "--exclude-standard", "--"] + tops, env))
        untracked = {p: p in ignored for p in git_paths(root, ["--others", "--"] + tops, env)
                     if p.lower().endswith(".md") and not p.rsplit("/", 1)[-1].startswith(".")}
    return tracked, untracked


def check(root, size=None, digest=None, env=None):
    """(problems, entries) for the tree at ROOT: the archive frozen and tracked, every tracked path
    below the folder an entry by name and content (or the README), git listing something there,
    every untracked one's name, no misfiled name anywhere, and no two of the log's paths that one
    volume folding case would take for one file."""
    try:
        tracked, untracked = listing(root, env)
    except OSError as e:
        return ["git could not list the tree (%s), so the log's names went unchecked" % e], 0
    problems = archive_problems(root, size, digest)
    if ARCHIVE not in tracked:
        problems.append("%s is not tracked by git" % ARCHIVE)
    if not any(p.startswith(FOLDER) for p in tracked):
        problems.append("git lists nothing below %s, where the README stands from the cut-over on: "
                        "git failed, or this is no checkout of this tree" % FOLDER)
    entries, folded = 0, {}
    for rel in tracked:
        kind = classify(rel)
        if kind:
            folded.setdefault(rel.casefold(), set()).add(rel)
        if kind == "misfiled":
            problems.append(misfiled_problem(rel))
        elif kind in ("entry", "readme"):
            problems += ["%s %s" % (rel, p) for p in file_problems(root, rel)]
            entries += kind == "entry"
    for rel, ignored in sorted(untracked.items()):
        kind = classify(rel)
        if not kind:
            continue
        folded.setdefault(rel.casefold(), set()).add(rel)
        if kind == "misfiled":
            problems.append(misfiled_problem(rel) + (", and git ignores it there" if ignored else ""))
        elif ignored:
            problems.append("%s is ignored by git, so it would never be committed: rename it" % rel)
        elif kind == "entry":
            problems += ["%s %s" % (rel, p) for p in name_problems(rel[len(FOLDER):])]
    for rels in folded.values():
        if len(rels) > 1:
            problems.append("%s name one file on a volume that folds case" % " and ".join(sorted(rels)))
    return problems, entries


def hook_for(root, rel):
    """The PostToolUse hook's check of REL, one edited path of the log's, repository-relative as the
    editor named it and in any case (check-doc-budgets.py resolves no link inside the repository,
    so an edited link keeps its own name here). (2, what is wrong and the way through), or (0, "")
    to say nothing. A deleted entry is let be, a deleted archive is not. Nothing is listed and no
    git is asked: an edit costs one lstat per component and the read of the one file."""
    kind = classify(rel)
    if kind is None:
        return 0, ""
    problems = []
    state, at, _ = path_state(root, rel)
    if rel.lower() == ARCHIVE:
        if rel != ARCHIVE and state != "missing":
            problems.append(misfiled_problem(rel))
        problems += archive_problems(root)
    elif state == "missing":
        return 0, ""
    elif kind == "misfiled":
        problems.append(misfiled_problem(rel))
    else:
        problems += ["%s %s" % (rel, p) for p in file_problems(root, rel)]
    if not problems:
        return 0, ""
    return 2, "\n".join(problems + ["The engineering log: %s. Details: scripts/check-engineering-log.py" % WAY])


def self_test():
    import threading
    ok = True

    def expect(name, got, want):
        nonlocal ok
        if got != want:
            ok = False
            print("FAIL %s: got %r, want %r" % (name, got, want))

    # The slug and its bound.
    expect("a branch's slug", slug("fix/b898-updater-held-binary"), "fix-b898-updater-held-binary")
    expect("a slug lowercases and joins each run with one -", slug("Feat/B888 Log_per--Entry/"), "feat-b888-log-per-entry")
    expect("nothing left is nothing", slug("///"), "")
    at_bound = "a" * 20 + "-" + "b" * 35
    expect("a slug of 56 characters is kept", slug(at_bound), at_bound)
    long_one = slug("feat/" + "x" * 52)
    expect("one of 57 is cut to 47, - and 8 hex digits", (len(long_one), long_one[:48], bool(re.fullmatch(
        r"[0-9a-f]{8}", long_one[48:]))), (56, "feat-" + "x" * 42 + "-", True))
    expect("the shortening is deterministic", slug("feat/" + "x" * 52), long_one)
    expect("two long branches that begin alike keep apart", slug("feat/" + "x" * 52 + "y") != long_one, True)
    expect("a cut that ends in - leaves no --", "--" in slug("a" * 46 + "/" + "b" * 20), False)
    expect("a shortened slug passes the bound", name_problems("2026-11/2026-11-20-b1--%s.md" % long_one), [])

    # The names.
    good = "2026-11/2026-11-18-b888--feat-b888-log-per-entry.md"
    at_most = "2026-11/2026-11-20-%s--%s.md" % ("e" * 48, "b" * BRANCH_MAX)
    expect("the longest name has 120 characters", len(at_most.split("/")[1]), NAME_MAX)
    for sub in (good, "2026-11/2026-11-15-b898-pr-1-round-2--fix-b898-updater-held-binary.md",
                "2026-11/2026-11-18-b618fu--fix-bridge-lows2-1118.md",
                "2026-10/2026-10-25-ci-ubuntu-2604--fix-ci-ubuntu-2604.md", "2028-02/2028-02-29-b1--main.md", at_most):
        expect("%s is a name" % sub, name_problems(sub), [])
    refused = {
        "a capital letter": "2026-11/2026-11-18-B888--feat-x.md",
        "an underscore": "2026-11/2026-11-18-b888_x--feat-x.md",
        "a single - between the parts": "2026-11/2026-11-18-b888-feat-x.md",
        "an empty entry": "2026-11/2026-11-18---feat-x.md",
        "an empty branch": "2026-11/2026-11-18-b888--.md",
        "---": "2026-11/2026-11-18-b888---feat-x.md",
        "a - at a part's end": "2026-11/2026-11-18-b888--feat-x-.md",
        "a second --": "2026-11/2026-11-18-b888--feat--x.md",
        "another month's folder": "2026-12/2026-11-30-b888--feat-x.md",
        "a day that is not": "2026-02/2026-02-30-b888--feat-x.md",
        "the folder's top": "2026-11-18-b888--feat-x.md",
        "a folder below the month": "2026-11/x/2026-11-18-b888--feat-x.md",
        ".txt": "2026-11/2026-11-18-b888--feat-x.txt",
        "121 characters": "2026-11/2026-11-20-%s--%s.md" % ("e" * 49, "b" * BRANCH_MAX),
        "a branch part of 57": "2026-11/2026-11-20-e--%s.md" % ("b" * 57),
        "a README in a month folder": "2026-11/README.md",
    }
    for why, sub in refused.items():
        expect("%s is refused" % why, bool(name_problems(sub)), True)
    other_digits = "".join(chr(0x660 + int(c)) if c.isdigit() else c for c in "2026-11/2026-11-18") + "-b888--feat-x.md"
    expect("a date in another script's digits is refused", bool(name_problems(other_digits)), True)

    # The contents.
    head = "## 2026-11-18 — B888: the log\n"
    rows = [
        ("an entry", head + "\n### What\n\nText.\n", 0),
        ("a heading inside a fence", head + "```\n## not one\n```\n", 0),
        ("a heading inside a tilde fence whose info holds a backtick", head + "~~~ a`b\n## not one\n~~~\n", 0),
        ("a heading in a block quote", head + "> ## quoted\n", 0),
        ("a heading indented four spaces", head + "    ## code\n", 0),
        ("a closer with text after it closes nothing", head + "```\n## a\n``` not a closer\n## b\n```\n", 0),
        ("a shorter closer closes nothing", head + "````\n```\n## a\n````\n", 0),
        ("a tilde closes no backtick fence", head + "```\n~~~\n## a\n```\n", 0),
        ("a fence in a quote that goes on", head + "> ```\n>\n> ## a\n> ```\n", 0),
        ("a fence in a nested quote ends with its quote", head + "> > ```\n> ## a\n", 0),
        ("another date", "## 2026-11-17 — B888\n", 1),
        ("a level-one heading", "# 2026-11-18 — B888\n", 1),
        ("a hyphen for the dash", "## 2026-11-18 - B888\n", 1),
        ("no title", "## 2026-11-18 — \n", 1),
        ("a second heading", head + "\n## 2026-11-18 — again\n", 1),
        ("a second heading three spaces in", head + "   ## again\n", 1),
        ("a closer with white space after it closes", head + "```\n## a\n```  \n## b\n", 1),
        ("a fence in a quote ends where the quote ends", head + "> ```\n> ## a\n## b\n", 1),
        ("a blank line ends a quote and its fence", head + "> ```\n\n## b\n", 1),
        ("a backtick info with a backtick opens nothing", head + "``` a`b\n## a\n", 1),
        ("a fence four spaces in is no fence", head + "    ```\n## a\n", 1),
        ("CRLF ends", head.replace("\n", "\r\n"), 1),
        ("no final newline", head[:-1], 1),
    ]
    for name, text, want in rows:
        expect(name, min(len(content_problems(good, text.encode("utf-8"))), 1), want)
    expect("text that is not UTF-8", content_problems(good, head.encode("utf-8") + b"\xff\n"), ["is not UTF-8"])
    expect("the README needs no heading", content_problems("README.md", b"# The engineering log\n"), [])

    def put(root, rel, data):
        path = os.path.join(root, *rel.split("/"))
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(data if isinstance(data, bytes) else data.encode("utf-8"))

    def link(target, path):
        try:
            os.symlink(target, path)
            return True
        except (OSError, NotImplementedError):
            print("self-test: no symlinks here, so a link row was not run")
            return False

    # The freeze, on a fixture archive pinned by its own digest.
    base = ("# Fixture archive\n" + "".join("\n## 2026-01-%02d — entry %d\n\ntext %d\n" % (d, d, d)
                                             for d in range(1, 9))).encode("utf-8")
    size, digest = len(base), hashlib.sha256(base).hexdigest()
    lines = base.split(b"\n")  # its last element is the empty one after the final newline
    same_size = base.replace(b"entry 3", b"entry 4", 1)
    swapped = b"\n".join(lines[:2] + [lines[4], lines[3], lines[2]] + lines[5:])
    balanced = b"\n".join(lines[:2] + [b"text 9"] + lines[2:-2] + lines[-1:])
    lone_cr = base.replace(b"\n", b"\r")
    archives = {
        "one byte changed, the same size and lines": same_size,
        "two lines swapped": swapped,
        "a line put in and one of its length taken out": balanced,
        "one byte cut": base[:-1],
        "one line cut": b"\n".join(lines[:-2]) + b"\n",
        "one line added": base + b"more\n",
        "CRLF ends": base.replace(b"\n", b"\r\n"),
        "each LF a lone CR, the same size": lone_cr,
    }
    expect("the mutations are what they say", [(len(d), d.count(b"\n")) for d in (same_size, swapped, balanced)] +
           [len(lone_cr), swapped != base, balanced != base], [(size, base.count(b"\n"))] * 3 + [size, True, True])
    with tempfile.TemporaryDirectory() as tmp:
        root = os.path.join(tmp, "root")
        put(root, ARCHIVE, base)
        expect("the fixture archive passes", archive_problems(root, size, digest), [])
        expect("a message says how the archive grew", "grew by 5 bytes" in " ".join(archive_problems(root, size - 5, digest)), True)
        expect("and how it shrank", "shrank by 5 bytes" in " ".join(archive_problems(root, size + 5, digest)), True)
        for name, data in archives.items():
            put(root, ARCHIVE, data)
            expect("the archive refuses %s" % name, len(archive_problems(root, size, digest)), 1)
        put(root, ARCHIVE, base)
        archive = os.path.join(root, *ARCHIVE.split("/"))
        os.remove(archive)
        expect("the archive refuses its absence", len(archive_problems(root, size, digest)), 1)
        os.mkdir(archive)
        expect("the archive refuses a folder in its place", len(archive_problems(root, size, digest)), 1)
        os.rmdir(archive)
        if hasattr(os, "mkfifo"):
            os.mkfifo(archive)
            expect("the archive refuses a named pipe in its place", len(archive_problems(root, size, digest)), 1)
            os.remove(archive)
            put(root, ARCHIVE, base)
            st = os.lstat(archive)
            os.remove(archive)
            os.mkfifo(archive)
            got = []
            reader = threading.Thread(target=lambda: got.append(read_bytes(root, ARCHIVE, st)), daemon=True)
            reader.start()
            reader.join(5)
            expect("a named pipe put in place since the lstat is not waited on", got, [None])
            if reader.is_alive():  # a writer's open lets the waiting open go
                os.close(os.open(archive, os.O_WRONLY | os.O_NONBLOCK))
                reader.join(5)
            os.remove(archive)
        put(tmp, "copy/engineering-log.md", base)
        if link(os.path.join(tmp, "copy", "engineering-log.md"), archive):
            expect("the archive refuses a link to an identical copy", len(archive_problems(root, size, digest)), 1)
            os.remove(archive)
            shutil.rmtree(os.path.join(root, "ops"))
            if link(os.path.join(tmp, "copy"), os.path.join(root, "ops")):
                expect("the archive refuses a folder on its way that is a link",
                       len(archive_problems(root, size, digest)), 1)

    # The listing, in fixture checkouts git lists as it would this one.
    def git_env(home):
        env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
        env.update(GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull, HOME=home, XDG_CONFIG_HOME=home,
                   GIT_CEILING_DIRECTORIES=os.path.dirname(home))
        return env

    def git(root, env, *args, data=None):
        r = subprocess.run(["git", "-C", root] + list(args), input=data, capture_output=True, env=env)
        if r.returncode:
            raise OSError("git %s: %s" % (" ".join(args), r.stderr.decode("utf-8", "replace")))
        return r.stdout

    entry = "ops/engineering-log/" + good
    entry_text = head + "\nText.\n"

    def text_for(rel):
        return "## %s — fixture\n\nText.\n" % rel.rsplit("/", 1)[-1][:10]

    def checkout(tmp, name, files=None, add=True):
        root = os.path.join(tmp, name)
        env = git_env(root)
        os.makedirs(root)
        git(root, env, "init", "-q")
        files = {".gitignore": "ops/*review*.md\n*hidden*\n", ARCHIVE: base, README: "# Log\n",
                 entry: entry_text} if files is None else files
        for rel, data in files.items():
            put(root, rel, data)
        if add and files:
            git(root, env, "add", "--", *files)
        return root, env

    def found(problems, rel, words):
        return any(p.startswith(rel) and words in p for p in problems)

    try:
        git(tempfile.gettempdir(), None, "--version")
        have_git = True
    except OSError:
        have_git = False
        ok = False
        print("FAIL the listing rows need git on PATH")
    if have_git:
        with tempfile.TemporaryDirectory() as tmp:
            root, env = checkout(tmp, "good")
            put(root, "ops/engineering-log/2026-11/2026-11-20-c--b.md", (head + "x").replace("\n", "\r\n"))
            put(root, "ops/notes-review.md", "a review\n")
            for junk in (".DS_Store", ".#2026-11-20-c--b.md", "2026-11-20-c--b.md~", "2026-11-20-hidden.swp"):
                put(root, "ops/engineering-log/2026-11/" + junk, "junk\n")
            expect("a good checkout passes, an untracked entry judged by its name alone, no lock or backup read",
                   check(root, size, digest, env), ([], 1))
            put(root, "ops/engineering-log/2026-11/2026-11-20-B--x.md", entry_text)
            put(root, "ops/engineering-log/2026-11/2026-11-20-hidden--x.md", entry_text)
            put(root, "ops/2026-11-20-b888-review--feat-x.md", entry_text)
            problems, _ = check(root, size, digest, env)
            expect("an untracked misnamed entry is refused by its name",
                   found(problems, "ops/engineering-log/2026-11/2026-11-20-B--x.md", "capital"), True)
            expect("an ignored entry in the folder is refused",
                   found(problems, "ops/engineering-log/2026-11/2026-11-20-hidden--x.md", "ignored"), True)
            expect("an ignored entry directly in ops/ is found and refused",
                   found(problems, "ops/2026-11-20-b888-review--feat-x.md", "git ignores it there"), True)
            expect("nothing else is refused", len(problems), 3)

            root, env = checkout(tmp, "tracked")
            crlf = "ops/engineering-log/2026-11/2026-11-20-crlf--b.md"
            put(root, "ops/2026-11-20-b888--feat-x.md", entry_text)
            put(root, crlf, text_for(crlf).replace("\n", "\r\n"))
            put(root, "ops/engineering-log/2026-11/.DS_Store", "junk\n")
            git(root, env, "add", "--", "ops/2026-11-20-b888--feat-x.md", crlf, "ops/engineering-log/2026-11/.DS_Store")
            blob = git(root, env, "hash-object", "-w", "--stdin", data=entry_text.encode("utf-8")).decode().strip()
            git(root, env, "update-index", "--add", "--cacheinfo", "100644,%s,%s" % (blob, "OPS/" + entry[4:]))
            problems, _ = check(root, size, digest, env)
            expect("a tracked dated file directly in ops/ is misfiled",
                   found(problems, "ops/2026-11-20-b888--feat-x.md", "never directly in ops/"), True)
            expect("a tracked entry is read: its CR is refused", found(problems, crlf, "carriage return"), True)
            expect("a tracked path in another case is misfiled", found(problems, "OPS/" + entry[4:], "lowercase"), True)
            expect("and with its twin it names one file where case folds",
                   any("OPS/%s and %s name one file" % (entry[4:], entry) in p for p in problems), True)
            expect("a tracked file that is no entry is refused by its name",
                   found(problems, "ops/engineering-log/2026-11/.DS_Store", "is not named"), True)
            expect("nothing else is refused", len(problems), 5)

            root, env = checkout(tmp, "other-case", {"Ops/engineering-log/2026-11/2026-11-20-a--b.md": entry_text},
                                 add=False)
            problems, _ = check(root, size, digest, env)
            expect("an untracked file below a top-level folder named like ops in another case is found",
                   found(problems, "Ops/engineering-log/2026-11/2026-11-20-a--b.md", "lowercase"), True)
            expect("and an empty checkout fails closed", found(problems, "git lists nothing below", ""), True)

            root, env = checkout(tmp, "untracked-archive", {README: "# Log\n"})
            put(root, ARCHIVE, base)
            expect("an archive git does not track is refused",
                   [p for p in check(root, size, digest, env)[0]], ["%s is not tracked by git" % ARCHIVE])
            plain = os.path.join(tmp, "plain")
            put(plain, ARCHIVE, base)
            expect("a tree git cannot list fails closed",
                   "git could not list" in " ".join(check(plain, size, digest, git_env(plain))[0]), True)

    # The hook's own check of one path.
    with tempfile.TemporaryDirectory() as root:
        put(root, entry, entry_text)
        expect("the hook lets a good entry be", hook_for(root, entry), (0, ""))
        expect("and a deleted one", hook_for(root, "ops/engineering-log/2026-11/2026-11-20-gone--b.md"), (0, ""))
        put(root, "ops/engineering-log/2026-11/2026-11-20-B--x.md", entry_text)
        expect("the hook refuses a misnamed entry", hook_for(root, "ops/engineering-log/2026-11/2026-11-20-B--x.md")[0], 2)
        expect("the hook refuses a missing archive", hook_for(root, ARCHIVE)[0], 2)
        expect("the hook says nothing of another path", hook_for(root, "ops/README.md"), (0, ""))

    print("self-test:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


def main():
    args = sys.argv[1:]
    if args == ["--self-test"]:
        return self_test()
    if len(args) == 2 and args[0] == "--slug":
        s = slug(args[1])
        if not s:
            sys.exit("check-engineering-log: %r leaves no slug" % args[1])
        print(s)
        return 0
    if args:
        sys.exit(__doc__)
    problems, entries = check(REPO)
    if problems:
        print("check-engineering-log: FAILED.")
        for p in problems:
            print("  " + p)
        return 1
    print("check-engineering-log: ok — the archive frozen at %s bytes (sha256 %s), %d %s since." % (
        format(FROZEN_BYTES, ","), FROZEN_SHA256, entries, "entry" if entries == 1 else "entries"))
    return 0


if __name__ == "__main__":
    sys.exit(main())
