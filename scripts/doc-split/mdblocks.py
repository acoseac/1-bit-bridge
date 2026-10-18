"""Shared pieces of the doc-split tooling: markdown block parser, one-line condenser, and the
argument and path checks every script applies before touching git or the filesystem.

Ported from the iOS repo (acoseac/1-bit, scripts/doc-split/) on 2026-10-18. Two differences:
the long forms live in ops/ (HISTORY_DIR), because docs/ here is the public GitHub Pages site;
and there is no test-count checker, so load_claim_regex returns a pattern that matches nothing.

It also runs on Windows, where the POSIX-only steps of a write degrade: no fcntl (so no
F_FULLFSYNC, a plain fsync instead), no os.O_NOFOLLOW (a symlink is refused by os.path.islink
instead), no os.fchmod before Python 3.13 (the replaced file's mode is not copied), and no
directory sync (os.open refuses a directory there; NTFS journals the rename). Every write is
byte for byte: no platform newline translation.

A *block* is a top-level bullet (with its indented continuation and sub-bullets), a
paragraph, a table, or a fenced code block. Headings are separate items. `text` is the
exact original lines joined with "\n" — the unit the lossless checks compare byte-for-byte.
"""
import argparse
import errno
import fnmatch
import os
import re
import subprocess
import sys
import tempfile

try:
    import fcntl  # POSIX only; sync_fd uses its F_FULLFSYNC on macOS
except ImportError:  # Windows: sync_fd falls back to os.fsync
    fcntl = None

HEAD_RE = re.compile(r"^(#{1,6}) (.*)")
BULLET_RE = re.compile(r"^(?:[-*] |\d+\. )")

# A block's bold title, plus an immediately-following "(PR #…)" parenthetical.
TITLE_RE = re.compile(r"^(?:[-*] |\d+\. )?\*\*(.+?)\*\*(\s*\([^()]{1,160}\))?")
# The entry's own bolded prohibitions, copied verbatim into its one-line form.
DONT_RE = re.compile(r"\*\*((?:[Dd]on['’]t|[Dd]o NOT|[Dd]o not|[Nn]ever|NEVER)\b[^*]{4,600}?)\*\*")
RULES_RE = re.compile(r"\.claude/rules/([a-z0-9-]+\.md)")
DOC_RE = re.compile(r"docs/([A-Za-z0-9_.-]+\.md)")
KEEP_MAX = 450  # blocks at or under this many bytes stay verbatim
# Where the History files live. docs/ is served as a public website (ops/README.md), and the long
# forms are internal working material, which ops/ holds; keep it flat (ops/History-*.md).
HISTORY_DIR = "ops"


def parse(text):
    """Return (lines, items). Items are dicts: type heading|block, kind, start, end, lvl, title, path, gap, text."""
    lines = text.split("\n")
    n = len(lines)
    items, path = [], []
    i, prev_nonblank = 0, -1
    while i < n:
        l = lines[i]
        if l.strip() == "":
            i += 1
            continue
        gap = (i - prev_nonblank - 1) if prev_nonblank >= 0 else 0
        m = HEAD_RE.match(l)
        if m:
            lvl, title = len(m.group(1)), m.group(2).strip()
            path = [p for p in path if p[0] < lvl] + [(lvl, title)]
            items.append({"type": "heading", "kind": "heading", "start": i, "end": i + 1, "lvl": lvl,
                          "title": title, "path": list(path), "gap": gap, "text": l})
            prev_nonblank = i
            i += 1
            continue
        start = i
        if l.startswith("```"):
            i += 1
            while i < n and not lines[i].startswith("```"):
                i += 1
            i += 1
            kind = "code"
        elif BULLET_RE.match(l):
            i += 1
            while i < n and lines[i][:1] in (" ", "\t") and lines[i].strip() != "":
                i += 1
            while i + 1 < n and lines[i].strip() == "" and lines[i + 1].startswith("  "):
                i += 1
                while i < n and lines[i][:1] in (" ", "\t") and lines[i].strip() != "":
                    i += 1
            kind = "bullet"
        elif l.startswith("|"):
            while i < n and lines[i].startswith("|"):
                i += 1
            kind = "table"
        else:
            while (i < n and lines[i].strip() != "" and not HEAD_RE.match(lines[i])
                   and not BULLET_RE.match(lines[i]) and not lines[i].startswith("|")
                   and not (i > start and lines[i].startswith("```"))):
                i += 1
            kind = "para"
        end = min(i, n)  # an unclosed fence runs to the end of the file
        while end > start and lines[end - 1].strip() == "":
            end -= 1
        items.append({"type": "block", "kind": kind, "start": start, "end": end, "path": list(path),
                      "gap": gap, "text": "\n".join(lines[start:end])})
        prev_nonblank = end - 1
    return lines, items


def split_frontmatter(raw):
    """(frontmatter_including_closing_marker, body). Frontmatter is kept byte-for-byte."""
    if raw.startswith("---\n"):
        end = raw.index("\n---\n", 4) + 5
        return raw[:end], raw[end:]
    return "", raw


def movable(block):
    """True when the block leaves the always-loaded file (long, and not a table or code)."""
    return block["kind"] not in ("table", "code") and len(block["text"].encode("utf-8")) + 1 > KEEP_MAX


def condense_entry(block, hist_name, self_rules=None, known_docs=None):
    """One-line form of a moved block: whole bold title (or first sentence), the entry's own
    bolded Don't/Never sentences verbatim, and `→ hist_name` plus rules/doc refs. KNOWN_DOCS, when
    given, limits the doc refs to those names (this repo's docs/*.md: an entry here also cites the
    iOS repo's docs/BridgeProtocol.md, which is no doc of this repo).
    Returns (line, used_fallback_title, donts_in_line)."""
    flat = re.sub(r"\s+", " ", block["text"])
    m = TITLE_RE.match(flat)
    if m:
        title = re.sub(r"\s+", " ", "**%s**%s" % (m.group(1).strip(), m.group(2) or ""))
        plain = m.group(1)
    else:
        body = re.sub(r"^(?:[-*] |\d+\. )", "", flat)
        mm = re.search(r"^(.{40,280}?(?<!e\.g)(?<!i\.e)(?<! vs)\.)(?:\s|$)", body)
        title = mm.group(1) if mm else body[:260].rstrip() + "…"
        if title.count("`") % 2:
            title += "`"
        if title.count("**") % 2:
            title += "**"
        plain = title
    donts = []
    for d in DONT_RE.finditer(block["text"]):
        s = re.sub(r"\s+", " ", d.group(1)).strip()
        if s not in donts and s not in plain:
            donts.append(s)
    rules = sorted({r for r in RULES_RE.findall(block["text"]) if r != self_rules})
    docs = [x for x in dict.fromkeys(DOC_RE.findall(block["text"]))
            if not x.startswith("History-") and (known_docs is None or x in known_docs)][:2]
    line = ("- " if block["kind"] == "bullet" else "") + title
    for d in donts:
        line += " **%s**" % d
    line += " → " + hist_name
    if rules:
        line += " · rules: " + ", ".join(rules)
    if docs:
        line += " · doc: " + ", ".join(docs)
    return line, m is None, donts


def docs_in(repo):
    """The names of the top-level docs/*.md in REPO's working tree: the doc refs a line may carry."""
    folder = os.path.join(repo, "docs")
    return {f for f in os.listdir(folder) if f.endswith(".md")} if os.path.isdir(folder) else set()


def section_names(path):
    """(top '##' title or '', sub '###' title or '')"""
    top = next((t for lvl, t in path if lvl == 2), "")
    sub = next((t for lvl, t in path if lvl == 3), "")
    return top, sub


# ---- argument and path checks ----
#
# People and Claude sessions both run these scripts, so every command-line value is checked
# before it reaches git or the filesystem: a revision can never be read as a git option, a
# rules-file name can only be one of the files in .claude/rules, and generated output can never
# land inside the repository or on top of files these scripts did not write (for example
# ~/.claude/rules, which Claude Code loads in every session). A staging directory is reused only
# when it carries STAGE_MARKER, i.e. when these scripts made it, and no write follows a symlink.

REV_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._/~^-]{0,99}")
SHA_RE = re.compile(r"[0-9a-f]{40}")
DATE_RE = re.compile(r"20\d\d-[01]\d-[0-3]\d")
STAGE_MARKER = ".doc-split-stage"


def read_text(path):
    with open(path, encoding="utf-8") as f:
        return f.read()


def open_for_write(path):
    """PATH opened for writing as UTF-8 text, refusing to follow a symlink there: O_NOFOLLOW where
    the platform has it, and where it does not (Windows) an os.path.islink check first, which
    refuses a link that is there when this is called. The descriptor is opened with O_BINARY where
    the platform has it, and the returned UTF-8 text stream uses newline="", so no newline is
    translated and the file holds exactly the text written to it."""
    nofollow = getattr(os, "O_NOFOLLOW", 0)
    if not nofollow and os.path.islink(path):
        sys.exit("refusing to write through a symlink: %s" % path)
    flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | nofollow | getattr(os, "O_BINARY", 0)
    try:
        fd = os.open(path, flags, 0o666)
    except OSError as e:
        if e.errno == getattr(errno, "ELOOP", None):
            sys.exit("refusing to write through a symlink: %s" % path)
        raise
    return os.fdopen(fd, "w", encoding="utf-8", newline="")


# What a file system without F_FULLFSYNC answers (an SMB mount says ENOTSUP). Any other failure
# is real, and falling back to fsync would report a flush that never happened.
NO_FULLFSYNC = tuple(getattr(errno, name) for name in ("ENOTSUP", "EOPNOTSUPP", "ENOTTY") if hasattr(errno, name))

# Whether a rename is made durable by syncing its directory. Windows opens no directory with
# os.open (PermissionError) and journals the rename itself (NTFS), so there sync_dir does nothing.
SYNCS_DIRECTORIES = os.name != "nt"


def sync_fd(fd):
    """Flush FD to the storage device. On macOS a plain fsync stops at the drive's own cache, which
    may reorder writes, so F_FULLFSYNC comes first; only a file system that lacks it falls back to
    fsync. Every other error is raised. Where there is no fcntl (Windows) or no F_FULLFSYNC (Linux),
    this is fsync."""
    if fcntl is not None and hasattr(fcntl, "F_FULLFSYNC"):
        try:
            fcntl.fcntl(fd, fcntl.F_FULLFSYNC)
            return
        except OSError as e:
            if e.errno not in NO_FULLFSYNC:
                raise
    os.fsync(fd)


def sync_dir(path):
    """Make a rename inside the directory PATH durable: syncing the renamed file alone leaves its
    new directory entry unwritten. Nothing where SYNCS_DIRECTORIES is false (Windows)."""
    if not SYNCS_DIRECTORIES:
        return
    fd = os.open(path, os.O_RDONLY)
    try:
        sync_fd(fd)
    finally:
        os.close(fd)


def write_text(path, text):
    """Replace PATH with TEXT atomically and durably: write a sibling temporary file, set its mode,
    sync it, rename it over PATH, then sync the directory. An interruption leaves the old file or
    the new one, never a truncated one, and a file written after this returns cannot reach the disk
    before it. A symlink at PATH is refused rather than replaced; an existing file keeps its
    permissions (where there is os.fchmod: on Windows before Python 3.13 the new file has the
    default mode). TEXT goes to disk as its UTF-8 bytes, with no newline translation."""
    if os.path.islink(path):
        sys.exit("refusing to write through a symlink: %s" % path)
    if os.path.exists(path):
        mode = os.stat(path).st_mode & 0o7777
    else:
        umask = os.umask(0)
        os.umask(umask)
        mode = 0o666 & ~umask
    folder = os.path.dirname(os.path.abspath(path))
    fd, tmp = tempfile.mkstemp(dir=folder, prefix=".doc-split-")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(text.encode("utf-8"))
            f.flush()
            fchmod = getattr(os, "fchmod", None)
            if fchmod is not None:
                fchmod(f.fileno(), mode)
            sync_fd(f.fileno())
        os.replace(tmp, path)
    except BaseException:
        if os.path.exists(tmp):
            os.remove(tmp)
        raise
    sync_dir(folder)


LINK_RE = re.compile(r"(\]\()([^)\s]+)(\))")


def rebase_links(text, from_dir, to_dir):
    """TEXT with each relative Markdown link target, written for a file in FROM_DIR, rewritten for a
    file in TO_DIR (both relative to the repository root, "" for the root), so it still points at
    the same file. URLs, absolute paths and #anchors are left alone."""
    def fix(m):
        target = m.group(2)
        if re.match(r"(?:[a-z][a-z0-9+.-]*:|#|/)", target, re.I):
            return m.group(0)
        path, sep, anchor = target.partition("#")
        resolved = os.path.normpath(os.path.join(from_dir, path))
        return m.group(1) + os.path.relpath(resolved, to_dir or ".").replace(os.sep, "/") + sep + anchor + m.group(3)
    return LINK_RE.sub(fix, text)


def rules_names(repo):
    """Stems of the rules files in the working tree: the only NAME values the scripts accept."""
    folder = os.path.join(repo, ".claude", "rules")
    if not os.path.isdir(folder):
        return []
    return sorted(f[:-3] for f in os.listdir(folder) if f.endswith(".md"))


def iso_date(value):
    """argparse type for --date: the value is written into the generated files."""
    if not DATE_RE.fullmatch(value):
        raise argparse.ArgumentTypeError("expected a date such as 2026-09-24, got %r" % value)
    return value


def safe_rev(repo, rev):
    """Resolve REV to a full commit id. `--end-of-options` keeps git from reading the value as
    an option, and only the resolved id is passed on to `git show`."""
    if not REV_RE.fullmatch(rev):
        sys.exit("not a revision: %r" % rev)
    r = subprocess.run(["git", "-C", repo, "rev-parse", "--verify", "--quiet", "--end-of-options",
                        rev + "^{commit}"], capture_output=True, text=True)
    sha = r.stdout.strip()
    if r.returncode or not SHA_RE.fullmatch(sha):
        sys.exit("not a commit in this repository: %r" % rev)
    return sha


def git_show(repo, sha, relpath):
    """The file at RELPATH in commit SHA (a full id from safe_rev)."""
    if not SHA_RE.fullmatch(sha) or relpath.startswith(("/", "-")) or ".." in relpath.split("/"):
        sys.exit("refusing git show %s:%s" % (sha, relpath))
    return subprocess.run(["git", "-C", repo, "show", "--end-of-options", "%s:%s" % (sha, relpath)],
                          capture_output=True, text=True, check=True).stdout


def outside_repo(path, repo):
    """PATH resolved; exits when it is the repository or anything inside it."""
    real, top = os.path.realpath(path), os.path.realpath(repo)
    if real == top or real.startswith(top + os.sep):
        sys.exit("refusing %s: generated output must go outside the repository" % path)
    return real


def _walk_stage(real):
    """(relative path, full path, is_dir) for everything under REAL, top-down."""
    for folder, subdirs, files in os.walk(real):
        rel = os.path.relpath(folder, real)
        prefix = "" if rel == "." else rel.replace(os.sep, "/") + "/"
        for name in subdirs:
            yield prefix + name, os.path.join(folder, name), True
        for name in files:
            yield prefix + name, os.path.join(folder, name), False


def _matches(rel, patterns):
    """REL matches one of PATTERNS at the same depth: fnmatch's `*` also crosses "/", so without
    the depth check docs/*.md would match docs/sub/x.md."""
    return any(fnmatch.fnmatchcase(rel, p) and rel.count("/") == p.count("/") for p in patterns)


def _first_stray(real, allowed):
    """The first entry an earlier run of this script could not have written, or None."""
    parents = {"/".join(p.split("/")[:k]) for p in allowed for k in range(1, p.count("/") + 1)}
    for rel, full, is_dir in _walk_stage(real):
        if os.path.islink(full):
            return rel
        if is_dir and not _matches(rel, parents):
            return rel
        if not is_dir and rel != STAGE_MARKER and not _matches(rel, allowed):
            return rel
    return None


def staging_dir(path, repo, allowed):
    """A directory for generated output, created and marked if needed. It must be outside the
    repository and either new, empty, or an earlier run's staging directory: one that carries
    STAGE_MARKER and holds only files matching ALLOWED (fnmatch patterns relative to it), with
    no symlinks anywhere inside it. PATH itself must not be a symlink either, or it could lead to
    another stage; its parents may be (on macOS /tmp is one), since the stage checks run on the
    resolved directory."""
    if os.path.islink(path.rstrip("/") or "/"):
        sys.exit("refusing %s: it is a symlink; pass the directory it points to" % path)
    real = outside_repo(path, repo)
    if os.path.lexists(real) and not os.path.isdir(real):
        sys.exit("refusing %s: not a directory" % path)
    if os.path.isdir(real) and os.listdir(real):
        if not os.path.isfile(os.path.join(real, STAGE_MARKER)):
            sys.exit("refusing %s: it is not empty and these scripts did not make it" % path)
        stray = _first_stray(real, allowed)
        if stray:
            sys.exit("refusing %s: it holds %s, which this script does not write" % (path, stray))
    os.makedirs(real, exist_ok=True)
    write_text(os.path.join(real, STAGE_MARKER),
               "Output of scripts/doc-split. Those scripts reuse this directory only while this file is here.\n")
    return real


def clear_stage(real, allowed):
    """Delete an earlier run's output (the files matching ALLOWED) from a staging_dir result,
    so a run whose output set shrank leaves nothing stale behind."""
    for rel, full, is_dir in list(_walk_stage(real)):
        if not is_dir and _matches(rel, allowed):
            os.remove(full)


def output_file(path, repo, suffix):
    """A file for generated output: named *SUFFIX, outside the repository, not a directory and
    not a symlink (write it with open_for_write, which also refuses one)."""
    if os.path.islink(path):
        sys.exit("refusing %s: it is a symlink" % path)
    real = outside_repo(path, repo)
    if not real.endswith(suffix) or os.path.isdir(real):
        sys.exit("refusing %s: expected a %s file" % (path, suffix))
    return real


def existing_file(path, suffix):
    real = os.path.realpath(path)
    if not real.endswith(suffix) or not os.path.isfile(real):
        sys.exit("expected an existing %s file: %s" % (suffix, path))
    return real


def existing_dir(path):
    real = os.path.realpath(path)
    if not os.path.isdir(real):
        sys.exit("expected an existing directory: %s" % path)
    return real


# A pattern with the iOS claim regex's three groups (suite, wording, count) that never matches.
NO_CLAIMS = re.compile(r"(?!)(.)(.)(\d)")


def load_claim_regex(repo):
    """The test-count claim pattern the scripts share. A STUB here: the iOS repo reads CLAIM from
    its scripts/check-doc-test-counts.py, which enforces `XTests` (N cases) claims, and this repo
    has no such checker, so nothing is read and NO_CLAIMS is returned. The claim checks in
    verify_moves.py, verify_rules.py, move_entry.py and validate_overrides.py stay in place and
    find nothing. If the repo gains a checker, load its CLAIM here as the iOS copy does."""
    return NO_CLAIMS
