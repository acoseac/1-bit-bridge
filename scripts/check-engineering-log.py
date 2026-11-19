#!/usr/bin/env python3
"""The engineering log: its archive frozen, and every record since in a file of its own.

ops/engineering-log.md holds every record up to 2026-11-18 and is frozen: its size and the SHA-256
of its bytes are pinned below, so no edit of it passes. Every record since is a file of its own,
which its PR adds, so that no PR appends to a file another wrote (backlog B888: every PR appended
to the one log, and each merge left every open PR conflicting at its end):

    ops/engineering-log/<YYYY-MM>/<YYYY-MM-DD>-<entry>--<branch>.md

  <YYYY-MM-DD>  the day the entry is written, the date its heading carries; <YYYY-MM> its month
  <entry>       the backlog item (b888, b728-b9-pr-3b), or a topic for an entry with none; a later
                entry of the same PR adds a word (b601-round-2)
  <branch>      the PR's branch, so two open PRs whose slugs differ never pick one name (two
                branches can fold to one slug, fix/foo and fix-foo, and git then reports the
                second entry of that name as an add/add conflict, never an overwrite); a slug
                longer than 56 characters keeps its first 47, then "-" and the first 8 hex digits
                of the whole slug's SHA-256 (--slug prints it)

Both are slugs: lowercase letters and digits, each other run of characters one "-", none at either
end, so the first "--" after the date ends <entry>. The name is at most 120 characters. An entry is
a regular file of UTF-8 with LF line ends and a final newline, its first line `## <date> — <title>`
with its name's date, and no other `## ` line outside fenced code: a quoted `> ## ` line is none,
and a fence in a list item or a block quote ends where they end (h2_lines). The folder's
README.md is the one other file it holds, and git must track it: it states this layout to every
PR.

  check-engineering-log.py              check the archive, every entry, and that nothing shaped
                                        like one sits anywhere else (`make docs-check` and CI's
                                        docs-check job do this)
  check-engineering-log.py --slug NAME  print the slug a branch NAME takes in an entry's name
  check-engineering-log.py --self-test  pin the freeze, the names, the contents and the listing

The PostToolUse hook (check-doc-budgets.py --hook) hands hook_for each edited path of the log's,
which checks that one path: it lists nothing and asks no git.

It needs Python 3.8 or later (OLDEST) and the standard library alone: the self-test parses this
file as ast does for 3.8, which refuses most later syntax (not all of it), and on Windows a
junction is told by the reparse tag lstat reports from 3.8 on. CI's docs-check job runs it on
the Python it pins, 3.12 (.github/workflows/gate.yml), not on 3.8.
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
OLDEST = (3, 8)  # the oldest Python this file supports, as whose grammar the self-test parses it
ARCHIVE = "ops/engineering-log.md"
FOLDER = "ops/engineering-log/"
README = FOLDER + "README.md"

# The archive as the cut-over left it, its closing pointer included. Only a redaction the owner
# approves changes these two values: a placeholder of the same line count where the text must go,
# both values updated in that PR, and an entry saying why. Review refuses any other change to them.
FROZEN_BYTES = 7_162_775
FROZEN_SHA256 = "8034010bf550376950b3cc1a59c8c8b3e1b0643c9431e98cfa04be9b1935e1ef"

NAME_MAX = 120  # characters in an entry's file name
NAME_SURROGATE = 0x20000000  # a Windows reparse tag's bit for a point naming another path (a link, a junction)
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
LIST_ITEM = re.compile(r" {0,3}(?:[-*+]|([0-9]{1,9})[.)])(?= |$)")  # an ordered item's number its group
BREAK = re.compile(r" {0,3}([-*_])(?: *\1){2,} *")  # a thematic break, which no list item is
ATX = re.compile(r"#{1,6}(?:[ \t]|$)")
UNDERLINE = re.compile(r"(?:=+|-+)[ \t]*")  # under a paragraph, a setext heading's
FENCE_OPEN = re.compile(r"(`{3,}|~{3,})(.*)")
FENCE_CLOSE = re.compile(r"(`{3,}|~{3,})[ \t]*")
# An entry filed anywhere else, by its name alone: a file named like an entry in any folder, and
# any dated Markdown file directly in ops/ that git tracks. Untracked, a dated note there that is
# not named like an entry is no record: git ignores *review*, *audit* and *findings* names in ops/
# for the private notes kept there.
DATED_IN_OPS = re.compile(r"ops/\d{4}-\d{2}-\d{2}[^/]*\.md", re.IGNORECASE)
ENTRY_SHAPED = re.compile(r"\d{4}-\d{2}-\d{2}-[^/]*--[^/]*\.md", re.IGNORECASE)
# The untracked files git is asked for outside ops/: a name holding "--" that ends in .md, in any
# folder and any case, which ENTRY_SHAPED then judges.
SHAPED_ANYWHERE = ":(glob,icase)**/*--*.md"

WAY = ("a record goes in a file of its own, ops/engineering-log/<YYYY-MM>/<YYYY-MM-DD>-<entry>--<branch>.md "
       "(ops/engineering-log/README.md; `python3 scripts/check-engineering-log.py --slug <branch>` "
       "prints a branch's slug)")
# Never a bare checkout of main's archive: a branch from before the cut-over may have appended its
# record to it, and a redaction's bytes belong with its pins.
RESTORE = ("move any record this branch added to it into an entry file of its own, named by its heading's "
           "date, then restore it with `git checkout origin/main -- ops/engineering-log.md`; a redaction the "
           "owner approved changes its bytes and this checker's FROZEN_BYTES and FROZEN_SHA256 in one commit, "
           "so take neither from a side without the other")


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


def going_on(line, containers):
    """(how many of CONTAINERS, outermost first, LINE goes on in, and what is left of it). Each is
    [width, holds]: a block quote's width None, and it goes on where its marker stands; a list
    item's the columns its content starts at, and it goes on where the line is indented by that
    many at least, or is blank, but for an item that holds nothing yet, which a blank line ends."""
    for k, (width, holds) in enumerate(containers):
        if width is None:
            m = QUOTE.match(line)
            if not m:
                return k, line
            line = line[m.end():]
        elif not line.strip(" "):
            if not holds:
                return k, line
        elif len(line) - len(line.lstrip(" ")) < width:
            return k, line
        else:
            line = line[width:]
    return len(containers), line


def starts(rest, tip, inside):
    """(the containers REST opens, outermost first, what is left of it, and what that is: "fence",
    "heading", "break", "underline", "code", "blank" or "text"), as CommonMark's block starts read
    it. TIP says a paragraph is the last block open, INSIDE that it goes on in every container
    the line goes on in: there a list item interrupts it only with something on its line and,
    ordered, numbered 1, a run of `=` or `-` underlines it, and a line indented four spaces or more
    is its text (where TIP alone holds, a lazy line's). A thematic break (`- - -`) opens no item.
    An item's width is its marker's, the up to three spaces before it included, and the spaces
    after it, one where it holds nothing yet or more than four follow."""
    out = []
    while True:
        if not rest.strip(" "):
            return out, rest, "blank"
        indent = len(rest) - len(rest.lstrip(" "))
        if indent > 3:
            return out, rest, "text" if tip and not out else "code"
        body, interrupting = rest[indent:], inside and not out
        m = QUOTE.match(rest)
        if m:
            out.append([None, True])
            rest = rest[m.end():]
            continue
        if ATX.match(body):
            return out, rest, "heading"
        m = FENCE_OPEN.fullmatch(body)
        if m and not (m.group(1)[0] == "`" and "`" in m.group(2)):
            return out, rest, "fence"
        if interrupting and UNDERLINE.fullmatch(body):
            return out, rest, "underline"
        if BREAK.fullmatch(rest):
            return out, rest, "break"
        m = LIST_ITEM.match(rest)
        if not m:
            return out, rest, "text"
        after = rest[m.end():]
        empty = not after.strip(" ")
        if interrupting and (empty or (m.group(1) is not None and int(m.group(1)) != 1)):
            return out, rest, "text"
        spaces = len(after) - len(after.lstrip(" "))
        width = m.end() + (spaces if not empty and spaces <= 4 else 1)
        out.append([width, not empty])
        rest = rest[width:]


def h2_lines(text):
    """The numbers of TEXT's lines that are `## ` lines outside fenced code: after up to three
    spaces, `##` then a space, a tab or the line's end. A line in a block quote begins with its
    `>`, so a quoted `> ## ` is none.

    Fenced code is read with CommonMark's containers, block quotes and list items, and the
    paragraph rules that decide where an item ends. A quote goes on where its marker stands (`>`,
    up to three spaces before it and one after); a list item where a line is indented by its width
    at least, or is blank, but for an item that holds nothing yet, which a blank line ends. A
    paragraph's lazy line, one that starts no block, keeps every container open. A list item
    interrupts a paragraph only with something on its line and, ordered, numbered 1, so `2.` or an
    empty `-` after a paragraph's text is more of that text, and a lone run of `-` or `=` under it
    underlines it; a quote, a heading, a fence and a thematic break always interrupt it. Inside its
    containers, after up to three spaces, a fence opens on three or more backticks or tildes, a
    backtick fence's info holding no backtick, and closes on a line of the same character, at
    least as many of them, with nothing after them but white space; it ends where a container it
    opened in ends, and the line that ends it is read afresh.

    The containers are markdownShellLines' (scripts/third-party-licenses/tags.go), which differs:
    it follows no paragraph, so a line indented less than an item's width ends the item there,
    every marker opens one and a blank line goes on in one; code in a fence opens a container
    there, so a fence's line that begins with a marker (`- ```) closes the fence; more than three
    spaces before a fence or a marker do not make code there; and a thematic break (`- - -`) is
    read as list items. HTML blocks are read by neither: a `## ` line inside one counts here."""
    found, containers, fence, paragraph = [], [], None, False  # fence: (its character, how many)
    for number, line in enumerate(text.split("\n"), 1):
        line = line.replace("\t", "    ")
        k, rest = going_on(line, containers)
        if fence:
            if k == len(containers):
                indent = len(rest) - len(rest.lstrip(" "))
                m = FENCE_CLOSE.fullmatch(rest[indent:]) if indent <= 3 else None
                if m and m.group(1)[0] == fence[0] and len(m.group(1)) >= fence[1]:
                    fence = None
                continue
            fence = None  # a container it opened in has ended, and the fence with it: read the line afresh
        more, rest, kind = starts(rest, paragraph, paragraph and k == len(containers))
        if kind == "text" and paragraph and not more:
            continue  # the paragraph goes on, lazily where a container did not, and every container with it
        if more or kind != "blank":
            for container in containers[:k]:
                container[1] = True  # it holds what this line opens or puts in it
        containers = containers[:k] + more
        paragraph = kind == "text"
        if kind == "fence":
            m = FENCE_OPEN.fullmatch(rest.lstrip(" "))
            fence = (m.group(1)[0], len(m.group(1)))
            continue
        indent = len(line) - len(line.lstrip(" "))
        if indent <= 3 and H2.match(line[indent:]):
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
    not there), "a link", "not a folder" (a component on the way) or "not a regular file". A link
    is a symbolic link, or on Windows any reparse point that names another path: lstat reports a
    junction as a folder, with its reparse tag (Python 3.8 and later), and the open would follow it."""
    parts = rel.split("/")
    st = None
    for k in range(1, len(parts) + 1):
        component = "/".join(parts[:k])
        try:
            st = os.lstat(os.path.join(root, *parts[:k]))
        except (FileNotFoundError, NotADirectoryError):
            return "missing", component, None
        if stat.S_ISLNK(st.st_mode) or getattr(st, "st_reparse_tag", 0) & NAME_SURROGATE:
            return "a link", component, None
        if k < len(parts) and not stat.S_ISDIR(st.st_mode):
            return "not a folder", component, None
    if not stat.S_ISREG(st.st_mode):
        return "not a regular file", rel, None
    return "file", rel, st


def read_bytes(root, rel, st):
    """REL's bytes as they are, no newline translated, from the file path_state saw (ST); None when
    it cannot be opened or read (an OSError at the open, the fstat or any read), or another file
    stands there now. O_NONBLOCK, so a named pipe put there since the lstat is opened without
    waiting for a writer, and then refused as another file."""
    flags = os.O_RDONLY | getattr(os, "O_BINARY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
    try:
        fd = os.open(os.path.join(root, *rel.split("/")), flags)
    except OSError:
        return None
    try:
        now = os.fstat(fd)
        if not stat.S_ISREG(now.st_mode) or not os.path.samestat(now, st):  # an inode number is reused
            return None
        chunks = []
        while True:
            chunk = os.read(fd, 1 << 20)
            if not chunk:
                return b"".join(chunks)
            chunks.append(chunk)
    except OSError:
        return None
    finally:
        os.close(fd)


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
    or a name shaped like an entry outside the folder), "dated" (another dated Markdown file
    directly in ops/: misfiled where git tracks it, a private note where it does not), or None."""
    if rel == ARCHIVE:
        return "archive"
    if rel == README:
        return "readme"
    if rel.startswith(FOLDER):
        return "entry"
    low = rel.lower()
    if low == ARCHIVE or low.startswith(FOLDER) or ENTRY_SHAPED.fullmatch(rel.rsplit("/", 1)[-1]):
        return "misfiled"
    if DATED_IN_OPS.fullmatch(rel):
        return "dated"
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
    Markdown file below a top-level folder named like ops in any case, ignored ones included, and
    for every untracked file in any other folder whose name holds "--", as an entry's does, that
    git does not ignore or ignores by its own name: a folder git ignores whole, as .tmp/ where
    scratch goes, is not entered there). Only a name that could be a record's is listed: one ending
    in .md that does not begin with "." (an editor's lock, the OS's .DS_Store and a backup's ~ are
    no records; committed, git lists them as tracked, and they are refused). check() reads an
    untracked entry git does not ignore as it reads a tracked one; one git ignores it judges by its
    name alone, what an ignored file holds being nobody's to read here."""
    tracked = git_paths(root, ["--cached"], env)

    def record(p):
        return p.lower().endswith(".md") and not p.rsplit("/", 1)[-1].startswith(".")
    tops = sorted(n for n in os.listdir(root) if n.lower() == "ops" and os.path.isdir(os.path.join(root, n)))
    untracked = {}
    if tops:
        ignored = set(git_paths(root, ["--others", "--ignored", "--exclude-standard", "--"] + tops, env))
        untracked = {p: p in ignored for p in git_paths(root, ["--others", "--"] + tops, env) if record(p)}
    for p in git_paths(root, ["--others", "--exclude-standard", "--", SHAPED_ANYWHERE], env):
        if record(p):
            untracked.setdefault(p, False)
    # --directory: a folder ignored whole is printed as itself, "<folder>/", and not entered.
    for p in git_paths(root, ["--others", "--ignored", "--exclude-standard", "--directory", "--", SHAPED_ANYWHERE], env):
        if record(p):
            untracked.setdefault(p, True)
    return tracked, untracked


def check(root, size=None, digest=None, env=None):
    """(problems, entries) for the tree at ROOT: the archive frozen and tracked, the README tracked,
    every tracked path below the folder an entry by name and content (or the README), and every
    untracked one git does not ignore, git listing something there, no ignored one, no misfiled
    name anywhere (a dated note directly in ops/ that is not named like an entry is one only where
    git tracks it), and no two of the log's paths that one volume folding case would take for one
    file."""
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
    if README not in tracked:
        problems.append("%s is not tracked by git: the folder keeps its README, which states the layout "
                        "every entry follows" % README)
    entries, folded = 0, {}
    for rel in tracked:
        kind = classify(rel)
        if kind:
            folded.setdefault(rel.casefold(), set()).add(rel)
        if kind in ("misfiled", "dated"):
            problems.append(misfiled_problem(rel))
        elif kind in ("entry", "readme"):
            problems += ["%s %s" % (rel, p) for p in file_problems(root, rel)]
            entries += kind == "entry"
    for rel, ignored in sorted(untracked.items()):
        kind = classify(rel)
        if kind in (None, "dated"):
            continue
        folded.setdefault(rel.casefold(), set()).add(rel)
        if kind == "misfiled":
            problems.append(misfiled_problem(rel) + (", and git ignores it there" if ignored else ""))
        elif ignored:
            problems.append("%s is ignored by git, so it would never be committed: rename it" % rel)
        elif kind in ("entry", "readme"):  # read as a tracked one is: git add would commit it as it stands
            problems += ["%s %s" % (rel, p) for p in file_problems(root, rel)]
    for rels in folded.values():
        if len(rels) > 1:
            problems.append("%s name one file on a volume that folds case" % " and ".join(sorted(rels)))
    return problems, entries


def hook_for(root, rel):
    """The PostToolUse hook's check of REL, one edited path of the log's, repository-relative as the
    editor named it and in any case (check-doc-budgets.py resolves no link inside the repository,
    so an edited link keeps its own name here). (2, what is wrong and the way through), or (0, "")
    to say nothing. A deleted entry is let be, and so is a dated note in ops/ not named like an
    entry, which may be a private one git ignores; a deleted archive is not. Nothing is listed and
    no git is asked: an edit costs one lstat per component and the read of the one file."""
    kind = classify(rel)
    if kind in (None, "dated"):
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
    import ast
    import errno
    import threading
    from types import SimpleNamespace
    ok = True

    def expect(name, got, want):
        nonlocal ok
        if got != want:
            ok = False
            print("FAIL %s: got %r, want %r" % (name, got, want))

    # The oldest Python the docstring states, as ast parses for it: CPython refuses there what it
    # marks as later (a match statement), not all of it (a parenthesized `with` item passes), and
    # the library calls are 3.8's by reading alone. A check, not a run on 3.8.
    with open(os.path.abspath(__file__), "rb") as f:
        source = f.read()
    try:
        ast.parse(source, feature_version=OLDEST)
        floor = None
    except SyntaxError as e:
        floor = "line %s: %s" % (e.lineno, e.msg)
    expect("this file parses as Python %d.%d's grammar" % OLDEST, floor, None)

    # The slug and its bound.
    expect("a branch's slug", slug("fix/b898-updater-held-binary"), "fix-b898-updater-held-binary")
    expect("a slug lowercases and joins each run with one -", slug("Feat/B888 Log_per--Entry/"), "feat-b888-log-per-entry")
    expect("nothing left is nothing", slug("///"), "")
    expect("two branch names can fold to one slug", (slug("fix/foo"), slug("fix-foo")), ("fix-foo", "fix-foo"))
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
    # List items are containers: each row names the `## ` lines it holds, the heading its first.
    lists = [
        ("a fence under a list item ends with the item", head + "- item\n  ```\n## a\n```\n", [1, 4]),
        ("a list item's indented closer opens no fence", head + "- ```\n  code\n  ```\n## b\n", [1, 5]),
        ("a heading inside a list item's fence is none", head + "- ```\n  ## inside\n  ```\n", [1]),
        ("nor inside an ordered item's", head + "1. ```\n   ## inside\n   ```\n", [1]),
        ("nor past a blank line in the item", head + "- ```\n  a\n\n  ## inside\n  ```\n", [1]),
        ("a heading after a list item's fence closes is one", head + "- ```\n  ## inside\n  ```\n## after\n", [1, 5]),
        ("and one after the item ends, the fence with it", head + "- ```\n  ## inside\n## after\n", [1, 4]),
        ("a marker in a fence opens nothing, and closes nothing", head + "```\n- ```\n## a\n```\n", [1]),
        ("a closer short of the item's width ends it and opens a fence", head + "- ```\n  a\n```\n## x\n", [1]),
        ("a list item in a quote ends with the quote", head + "> - ```\n>   ## inside\n>   ```\n## after\n", [1, 5]),
        ("a heading in a list item outside a fence is one", head + "- item\n  ## sub\n", [1, 3]),
        ("a thematic break opens no list item", head + "- - -\n  ```\n## a\n", [1]),
    ]
    for name, text, want in lists:
        expect(name, h2_lines(text), want)
    # A paragraph decides where an item ends and whether a marker opens one: each row as lists'.
    empty_then_blank = head + "\n-\n\n  ```\ncode\n  ```\n## stray\n"
    two_after_text = head + "\nparagraph\n2. item\n   ```\n## sample\n```\n"
    lazy = head + "\n- foo\nbar\n    ```\n  ## looks\n    ```\n"
    two_as_text = head + "\nThe number is\n2. ```\n   ## stray\n```\n"
    paragraphs = [
        ("an item that holds nothing ends at a blank line, its fence closing outside it", empty_then_blank, [1, 8]),
        ("an ordered item not numbered 1 does not interrupt a paragraph", two_after_text, [1]),
        ("a lazy line keeps its item open", lazy, [1]),
        ("a heading after a 2. that is a paragraph's text is one", two_as_text, [1, 5]),
        ("0. does not interrupt one either", head + "\nparagraph\n0. ```\n   ## inside\n   ```\n", [1, 5]),
        ("an empty bullet does not: under a paragraph it is a setext underline",
         head + "\nparagraph\n-\n  ```\n## after\n```\n", [1]),
        ("a line four spaces in under a paragraph's text is more of it, so 2. after it is too",
         head + "\nparagraph\n    more\n2. ```\n   ## inside\n   ```\n", [1, 6]),
        ("an underline ends the paragraph, so 2. after it opens an item",
         head + "\nparagraph\n===\n2. ```\n   ## inside\n   ```\n", [1]),
        ("after a heading 2. opens an item", head + "2. ```\n   ## inside\n   ```\n", [1]),
        ("and after a blank line", head + "\nparagraph\n\n2. ```\n   ## inside\n   ```\n", [1]),
        ("1. interrupts a paragraph", head + "\nparagraph\n1. ```\n   ## inside\n   ```\n", [1]),
        ("and so does 01.", head + "\nparagraph\n01. ```\n    ## inside\n    ```\n", [1]),
        ("and a bullet", head + "\nparagraph\n- ```\n  ## inside\n  ```\n", [1]),
        ("a line that leaves its item opens one with any marker", head + "\n- foo\n2. bar\n  ```\n## x\n  ```\n", [1]),
        ("an item that holds something goes on past a blank line",
         head + "-\n  foo\n\n  ```\n  ## inside\n  ```\n", [1]),
        ("a fence's closing line is no paragraph's text", head + "```\nx\n```\n2. ```\n   ## inside\n   ```\n", [1]),
    ]
    for name, text, want in paragraphs:
        expect(name, h2_lines(text), want)
    twice = ["holds 2 `## ` lines outside fenced code, not one: each entry is a file of its own"]
    expect("so those entries are refused, and the valid ones accepted",
           [content_problems(good, text.encode("utf-8")) for text in (empty_then_blank, two_after_text, lazy, two_as_text)],
           [twice, [], [], twice])
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
        said = " ".join(archive_problems(root, size - 5, digest))
        expect("and to move a branch's record out before restoring it, a redaction's bytes kept with the pins",
               (0 <= said.find("into an entry file of its own") < said.find("git checkout origin/main"),
                "FROZEN_BYTES and FROZEN_SHA256" in said), (True, True))
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

    ignore = "ops/*review*.md\nops/*audit*.md\n*hidden*\nscratch/\n"

    def checkout(tmp, name, files=None, add=True):
        root = os.path.join(tmp, name)
        env = git_env(root)
        os.makedirs(root)
        git(root, env, "init", "-q")
        files = {".gitignore": ignore, ARCHIVE: base, README: "# Log\n", entry: entry_text} if files is None else files
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
            put(root, "ops/engineering-log/2026-11/2026-11-20-c--b.md", text_for("2026-11-20-c--b.md"))
            put(root, "ops/notes-review.md", "a review\n")
            for junk in (".DS_Store", ".#2026-11-20-c--b.md", "2026-11-20-c--b.md~", "2026-11-20-hidden.swp"):
                put(root, "ops/engineering-log/2026-11/" + junk, "junk\n")
            expect("a good checkout passes, a good untracked entry read and counted with none, no lock or backup read",
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
            expect("the misnamed one is read too, its heading judged against no date, and nothing else is refused",
                   (len(problems), found(problems, "ops/engineering-log/2026-11/2026-11-20-B--x.md",
                                         "does not begin with its heading, `## <YYYY-MM-DD>")), (4, True))

            root, env = checkout(tmp, "untracked-read")
            month = "ops/engineering-log/2026-11/"
            put(root, month + "2026-11-20-crlf--b.md", text_for("2026-11-20-crlf--b.md").replace("\n", "\r\n"))
            put(root, month + "2026-11-20-body--b.md", "Notes, no heading.\n")
            put(root, month + "2026-11-20-hidden--b.md", "junk\r\n")
            linked = link(os.path.join(root, *entry.split("/")), os.path.join(root, *(month + "2026-11-20-link--b.md").split("/")))
            problems, _ = check(root, size, digest, env)
            expect("an untracked entry is read as a tracked one is: its CR is refused, and a body that is no entry's",
                   [found(problems, month + "2026-11-20-crlf--b.md", "carriage return"),
                    found(problems, month + "2026-11-20-body--b.md", "does not begin with its heading")], [True, True])
            expect("an untracked entry git ignores is judged by its name alone, its bytes unread",
                   [p for p in problems if "hidden" in p],
                   ["%s2026-11-20-hidden--b.md is ignored by git, so it would never be committed: rename it" % month])
            if linked:
                expect("an untracked entry that is a link is refused", found(problems, month + "2026-11-20-link--b.md", "is a link"), True)
            expect("and nothing else is refused", len(problems), 5 if linked else 4)

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

            shaped = "docs/2026-11-20-b888--feat-x.md"
            root, env = checkout(tmp, "docs-tracked", {".gitignore": ignore, ARCHIVE: base, README: "# Log\n",
                                                       entry: entry_text, shaped: entry_text})
            expect("a tracked file named like an entry in another folder is misfiled, and nothing else",
                   check(root, size, digest, env)[0], [misfiled_problem(shaped)])

            root, env = checkout(tmp, "docs-untracked")
            hidden, top = "docs/sub/2026-11-20-hidden--feat-x.md", "2026-11-20-b888--feat-x.md"
            for rel in (shaped, hidden, top, "scratch/2026-11-20-b888--feat-x.md", "docs/a--b.md"):
                put(root, rel, entry_text)
            expect("an untracked name like an entry is found in any folder, one git ignores by its own name too, "
                   "and a folder git ignores whole is not entered",
                   check(root, size, digest, env)[0], [misfiled_problem(top), misfiled_problem(shaped),
                                                       misfiled_problem(hidden) + ", and git ignores it there"])

            root, env = checkout(tmp, "dated-notes")
            note, plain_note, named = "ops/2026-11-20-loupe-audit.md", "ops/2026-11-20-notes.md", "ops/2026-11-20-x-audit--feat-x.md"
            for rel in (note, plain_note, named):
                put(root, rel, "a private note\n")
            expect("an untracked dated note in ops/, ignored or not, is let be, and one named like an entry is not",
                   check(root, size, digest, env)[0], [misfiled_problem(named) + ", and git ignores it there"])
            expect("the hook lets the two notes be", [hook_for(root, note), hook_for(root, plain_note)], [(0, ""), (0, "")])
            git(root, env, "add", "--", plain_note)
            expect("a dated note in ops/ that git tracks is misfiled",
                   check(root, size, digest, env)[0], [misfiled_problem(plain_note),
                                                       misfiled_problem(named) + ", and git ignores it there"])

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

            root, env = checkout(tmp, "no-readme", {".gitignore": ignore, ARCHIVE: base, entry: entry_text})
            problems, _ = check(root, size, digest, env)
            expect("a checkout whose README git does not track is refused for that alone",
                   (len(problems), found(problems, README, "is not tracked by git")), (1, True))
            put(root, README, "# Log\n")
            problems, _ = check(root, size, digest, env)
            expect("and so is one whose README is on disk and out of git's index",
                   (len(problems), found(problems, README, "is not tracked by git")), (1, True))
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
        put(root, "docs/2026-11-20-b888--feat-x.md", entry_text)
        expect("the hook refuses a name like an entry in another folder",
               hook_for(root, "docs/2026-11-20-b888--feat-x.md")[0], 2)

    # An OSError once a file is open, at its fstat or a read: the check and the hook say the file
    # could not be read, and no traceback reaches their callers. The failure is the file's alone, so
    # git's pipes read on.
    def failing_on(name, path, run):
        real, real_fstat, want = getattr(os, name), os.fstat, os.stat(path)

        def call(fd, *args):
            if os.path.samestat(real_fstat(fd), want):
                raise OSError(errno.EIO, "a read that fails (self-test)")
            return real(fd, *args)
        setattr(os, name, call)
        try:
            return run()
        except OSError as e:
            return "raised %s" % e
        finally:
            setattr(os, name, real)

    if have_git:
        with tempfile.TemporaryDirectory() as tmp:
            root, env = checkout(tmp, "unreadable")
            pins = FROZEN_BYTES, FROZEN_SHA256
            for name in ("fstat", "read"):
                for rel, words in ((ARCHIVE, "(unread)"), (entry, "%s could not be read" % entry)):
                    path = os.path.join(root, *rel.split("/"))
                    got = failing_on(name, path, lambda: check(root, size, digest, env)[0])
                    expect("the check refuses %s when its %s fails once it is open" % (rel, name),
                           any(words in p for p in got) if isinstance(got, list) else got, True)
                    globals().update(FROZEN_BYTES=size, FROZEN_SHA256=digest)
                    try:
                        got = failing_on(name, path, lambda: hook_for(root, rel))
                    finally:
                        globals().update(FROZEN_BYTES=pins[0], FROZEN_SHA256=pins[1])
                    expect("and so does the hook, by its diagnostic (%s, %s)" % (rel, name),
                           (got[0], words in got[1]) if isinstance(got, tuple) else got, (2, True))

    # A junction: Windows' lstat reports one as a folder, and its reparse tag names another path.
    # None can be made off Windows, so a stat stands in for ops/ there; on Windows a real one is made.
    with tempfile.TemporaryDirectory() as root:
        put(root, ARCHIVE, base)
        put(root, entry, entry_text)
        ops, real_lstat, got = os.path.join(root, "ops"), os.lstat, {}
        for tag in (0xA0000003, 0x9000001A):  # IO_REPARSE_TAG_MOUNT_POINT; a cloud file's, which names none

            def standing_in(path, *args, **kwargs):
                if path == ops:
                    return SimpleNamespace(st_mode=stat.S_IFDIR | 0o755, st_reparse_tag=tag)
                return real_lstat(path, *args, **kwargs)
            os.lstat = standing_in
            try:
                got[tag] = (len(archive_problems(root, size, digest)), hook_for(root, entry)[0])
            finally:
                os.lstat = real_lstat
        expect("a junction on the way to the archive or an entry is refused as a link is", got[0xA0000003], (1, 2))
        expect("a reparse point that names no other path is a folder still", got[0x9000001A], (0, 0))
        if os.name == "nt":  # CI's docs-check runs on Linux: only a Windows run of the self-test gets here
            import _winapi
            put(root, "target/engineering-log.md", base)
            shutil.rmtree(ops)
            _winapi.CreateJunction(os.path.join(root, "target"), ops)
            try:
                expect("a real junction on the way to the archive is refused", len(archive_problems(root, size, digest)), 1)
            finally:
                os.unlink(ops)

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
