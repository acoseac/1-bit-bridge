#!/usr/bin/env python3
"""Move one entry's long form out of an always-loaded file, losing nothing.

  move_entry.py FILE --largest [N]
  move_entry.py FILE --line L [--to DOC | --new DOC [--title TITLE]] [--text LINE] [--dry-run]

FILE is CLAUDE.md or .claude/rules/<name>.md, the files Claude Code loads on its own. The entry
whose first line is L moves, byte for byte, into a History file under a `##` heading naming its
section. FILE keeps one line in its place: the entry's bold title, its own bolded "Don't…/Never…"
sentences and `→ History-….md` — or LINE, a hand-written replacement, checked the way
validate_overrides.py checks polished lines (same shape, same pointer suffix, every "Don't" kept,
no invented symbols).

Where the entry goes:
  --to ops/History-X.md      an existing History file
  --new ops/History-Y.md     a new one, created with a short header (--title names its topic)
  neither                    for a rules file, ops/History-rules-<name>.md (created if missing);
                             for CLAUDE.md, the History file the other entries of the same
                             section already point to (refused when there are none, or a tie)

The History file is written, synced to disk (its directory too) and read back first, and FILE
changes only once the entry is verified there — an interruption, even a power cut, can leave the
entry in both files, never in neither.
"""
import argparse
import collections
import datetime
import errno
import os
import re
import stat
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from mdblocks import (parse, split_frontmatter, movable, condense_entry, read_text, write_text,  # noqa: E402
                      load_claim_regex, docs_in, HISTORY_DIR)

REPO = os.path.abspath(os.path.join(HERE, "..", ".."))
POINTER_RE = re.compile(r"→ (History-[A-Za-z0-9-]+\.md)")
HISTORY_NAME_RE = re.compile(r"History-[A-Za-z0-9][A-Za-z0-9-]*\.md")
PLACEHOLDER = "History-X.md"  # the orientation paragraph's example, not a real target


class MoveError(Exception):
    """A move that must not happen; the message says why."""


def source_of(path, root):
    """(path relative to ROOT, rules name or None) for CLAUDE.md or .claude/rules/<name>.md."""
    real, top = os.path.realpath(path), os.path.realpath(root)
    rel = os.path.relpath(real, top).replace(os.sep, "/")
    if rel == "CLAUDE.md" and os.path.isfile(real):
        return rel, None
    m = re.fullmatch(r"\.claude/rules/([a-z0-9][a-z0-9-]*)\.md", rel)
    if m and os.path.isfile(real):
        return rel, m.group(1)
    raise MoveError("FILE must be CLAUDE.md or .claude/rules/<name>.md in this repository: %s" % path)


def document(text):
    """(frontmatter, lines, items, line offset) of a file, headings and blocks as mdblocks parses them."""
    fm, body = split_frontmatter(text)
    lines, items = parse(body)
    return fm, lines, items, fm.count("\n")


def blocks(text):
    return [it["text"] for it in document(text)[2] if it["type"] == "block"]


def headings(text):
    return [it["text"] for it in document(text)[2] if it["type"] == "heading"]


def label_of(item):
    return " › ".join(t for lvl, t in item["path"] if lvl >= 2) or "(top of file)"


def candidates(items):
    """Entries that can move: long enough to leave, not a table or code, and not already a pointer."""
    return [it for it in items if it["type"] == "block" and movable(it) and not POINTER_RE.search(it["text"])]


def default_destination(items, item, name, root):
    """The History file an entry goes to when neither --to nor --new is given."""
    if name:
        return "History-rules-%s.md" % name
    own = [p for p in item["path"] if p[0] >= 2]
    for depth in dict.fromkeys((len(own), 1)):  # its own section first, then its whole `##` section
        scope = own[:depth]
        if not scope:
            continue
        near = [it for it in items if it is not item and it["type"] == "block"
                and [p for p in it["path"] if p[0] >= 2][:depth] == scope]
        votes = collections.Counter(t for it in near for t in POINTER_RE.findall(it["text"])
                                    if t != PLACEHOLDER and os.path.isfile(os.path.join(root, HISTORY_DIR, t)))
        ranked = votes.most_common(2)
        if ranked and (len(ranked) == 1 or ranked[0][1] > ranked[1][1]):
            return ranked[0][0]
        if ranked:
            raise MoveError("the entries around it point to %s equally — choose one with --to"
                            % " and ".join(t for t, _ in ranked))
    raise MoveError("no entry in its section points to a History file — choose one with --to or --new")


def history_name(arg, root):
    """The file name of a --to/--new argument: History-<Name>.md directly in ROOT's HISTORY_DIR
    (ops/: flat, so every History file is found the same way), given as a bare name, an
    ops/-relative path (./ allowed) or an absolute path."""
    if os.path.isabs(arg):
        folder, name = os.path.split(os.path.realpath(arg))
        folder = HISTORY_DIR if folder == os.path.realpath(os.path.join(root, HISTORY_DIR)) else folder
    else:
        folder, name = os.path.split(os.path.normpath(arg).replace(os.sep, "/"))
    if folder not in ("", HISTORY_DIR) or not HISTORY_NAME_RE.fullmatch(name):
        raise MoveError("a History file must be %s/History-<Name>.md, directly in %s/: %s"
                        % (HISTORY_DIR, HISTORY_DIR, arg))
    return name


def history_header(source_rel, hist, title, date):
    what = title or "entries moved from `%s`" % source_rel
    return ("# Long form: %s\n\n"
            "> Moved verbatim from `%s` on %s so it stops loading on its own. **These entries are "
            "still binding.** The source keeps each one as a single line ending `→ %s`; change an "
            "entry here and its line there in the same commit. Each `##` names the section the "
            "entries came from." % (what, source_rel, date, hist))


def with_entry(hist_text, label, block_text):
    """HIST_TEXT with BLOCK_TEXT appended to the end of its `## LABEL` section (created if absent)."""
    lines, items = parse(hist_text.rstrip("\n"))
    heads = [it for it in items if it["type"] == "heading" and it["lvl"] <= 2]
    mine = [k for k, it in enumerate(heads) if it["lvl"] == 2 and it["title"] == label]
    if not mine:
        return "\n".join(lines) + "\n\n## " + label + "\n\n" + block_text + "\n"
    k = mine[-1]
    stop = heads[k + 1]["start"] if k + 1 < len(heads) else len(lines)
    end = stop
    while end > heads[k]["start"] + 1 and lines[end - 1].strip() == "":
        end -= 1
    tail = [""] + lines[stop:] if stop < len(lines) else []
    return "\n".join(lines[:end] + [""] + block_text.split("\n") + tail) + "\n"


def plan(root, source_path, line_no, to=None, new=None, title=None, text=None, date=None):
    """Everything a move writes, computed and verified in memory. Returns a dict with the source
    and History paths, their old and new texts, the one-line replacement and the entry."""
    rel, name = source_of(source_path, root)
    src_path = os.path.join(root, rel)
    src_old = read_text(src_path)
    _, _, items, off = document(src_old)
    item = next((it for it in items if it["type"] == "block" and it["start"] + 1 + off == line_no), None)
    if item is None:
        raise MoveError("no entry starts on line %d of %s (see --largest for the line numbers)" % (line_no, rel))
    if item["kind"] in ("table", "code"):
        raise MoveError("line %d starts a %s; tables and code stay where they are" % (line_no, item["kind"]))
    if POINTER_RE.search(item["text"]):
        raise MoveError("line %d is already a one-line pointer" % line_no)
    if to and new:
        raise MoveError("use --to or --new, not both")
    hist = history_name(to or new, root) if (to or new) else default_destination(items, item, name, root)
    hist_path = os.path.join(root, HISTORY_DIR, hist)
    exists = os.path.isfile(hist_path)
    if to and not exists:
        raise MoveError("--to %s does not exist; use --new to create it" % to)
    if new and exists:
        raise MoveError("--new %s already exists; use --to to add to it" % new)
    if os.path.islink(hist_path):
        raise MoveError("%s is a symlink" % hist_path)

    mechanical, fallback, donts = condense_entry(item, hist, self_rules=name + ".md" if name else None,
                                                 known_docs=docs_in(root))
    one_liner = text if text is not None else mechanical
    import validate_overrides  # noqa: E402 — shares the polished-line checks, loaded only when needed
    errs = validate_overrides.problems({"block": item, "mechanical_line": mechanical, "donts": donts}, one_liner)
    if errs:
        raise MoveError("the one-line replacement would be rejected: " + "; ".join(errs))

    lines = src_old.split("\n")
    first, last = item["start"] + off, item["end"] + off
    src_new = "\n".join(lines[:first] + [one_liner] + lines[last:])
    hist_old = read_text(hist_path) if exists else None
    base = hist_old if exists else history_header(rel, hist, title, date or datetime.date.today().isoformat())
    hist_new = with_entry(base, label_of(item), item["text"])
    move = {"rel": rel, "src_path": src_path, "src_old": src_old, "src_new": src_new, "hist": hist,
            "hist_path": hist_path, "hist_old": hist_old, "hist_new": hist_new, "item": item,
            "line": one_liner, "fallback": fallback and text is None, "label": label_of(item)}
    verify(move)
    return move


def verify(move):
    """Raise MoveError unless the move keeps every block, heading and test-count claim."""
    entry, line = move["item"]["text"], move["line"]
    before_src, after_src = collections.Counter(blocks(move["src_old"])), collections.Counter(blocks(move["src_new"]))
    before_src[entry] -= 1
    before_src[line] += 1
    if +before_src != +after_src:
        raise MoveError("the source would not parse back to the same entries plus the one-liner")
    if headings(move["src_old"]) != headings(move["src_new"]):
        raise MoveError("the source's headings would change")
    before_hist = collections.Counter(blocks(move["hist_old"] or ""))
    after_hist = collections.Counter(blocks(move["hist_new"]))
    if any(after_hist[t] < n for t, n in before_hist.items()):
        raise MoveError("the History file would lose an entry it already holds")
    if after_hist[entry] != before_hist[entry] + 1:
        raise MoveError("the entry would not land in the History file exactly once")
    claim = load_claim_regex(REPO)
    count = lambda s: collections.Counter((m.group(1), int(m.group(3))) for m in claim.finditer(s or ""))
    if count(move["src_old"]) + count(move["hist_old"]) != count(move["src_new"]) + count(move["hist_new"]):
        raise MoveError("a test-count claim would be lost or duplicated")


def apply(move):
    """Write the History file, read it back, then the source."""
    write_text(move["hist_path"], move["hist_new"])
    if read_text(move["hist_path"]) != move["hist_new"]:
        raise MoveError("%s did not read back as written; the source is unchanged" % move["hist_path"])
    write_text(move["src_path"], move["src_new"])


def list_largest(root, source_path, n):
    rel, name = source_of(source_path, root)
    text = read_text(os.path.join(root, rel))
    _, _, items, off = document(text)
    found = sorted(candidates(items), key=lambda it: -len(it["text"].encode("utf-8")))[:n]
    print("%s: %d bytes. The %d largest entries that can move:" % (rel, len(text.encode("utf-8")), len(found)))
    for it in found:
        line = it["start"] + 1 + off
        try:
            dest = default_destination(items, it, name, root)
        except MoveError:
            dest = "(needs --to or --new)"
        print("  L%-5d %6d B → %-32s %s" % (line, len(it["text"].encode("utf-8")), dest, label_of(it)[:60]))
        print("         %s" % re.sub(r"\s+", " ", it["text"])[:110])
        print("         scripts/doc-split/move_entry.py %s --line %d" % (rel, line))
    if not found:
        print("  none: every long entry is already a one-line pointer. Shorten a pointer line's")
        print("  \"Don't\" list, or split a section into its own rules file.")


def self_test():
    """Pin the no-loss guarantees on a synthetic tree: nothing here touches the repository."""
    import shutil
    import tempfile
    ok = True

    def expect(name, cond):
        nonlocal ok
        if not cond:
            ok = False
            print("FAIL", name)

    long_a = "- **Rule A stays binding.** **Never undo rule A.** " + "a" * 600
    long_b = "- **Rule B stays binding.** " + "b" * 600
    long_c = "- **Rule C stays binding.** " + "c" * 600
    claude = "\n".join(["# t", "", "## One", "", "- **X** → History-One.md", "- **Y** → History-One.md",
                         "- **Z** → History-Two.md", long_a, "", "## Tie", "", "- **P** → History-One.md",
                         "- **Q** → History-Two.md", long_b, "", "## Empty", "", long_c, ""])
    root = tempfile.mkdtemp()
    try:
        os.makedirs(os.path.join(root, HISTORY_DIR))
        os.makedirs(os.path.join(root, ".claude", "rules"))
        with open(os.path.join(root, "CLAUDE.md"), "w", encoding="utf-8") as f:
            f.write(claude)
        for name in ("History-One.md", "History-Two.md"):
            with open(os.path.join(root, HISTORY_DIR, name), "w", encoding="utf-8") as f:
                f.write("# %s\n\n## One\n\n- earlier entry\n\n## Other\n\n- another\n" % name)
        line_of = lambda text, needle: next(i + 1 for i, l in enumerate(text.split("\n")) if l.startswith(needle))

        m = plan(root, os.path.join(root, "CLAUDE.md"), line_of(claude, "- **Rule A"))
        expect("default destination follows the neighbours' majority", m["hist"] == "History-One.md")
        expect("its own Don't stays in the one-liner", "**Never undo rule A.**" in m["line"])
        apply(m)
        one = read_text(os.path.join(root, HISTORY_DIR, "History-One.md"))
        expect("the entry lands at the end of its own section",
               one.index(long_a) > one.index("- earlier entry") and one.index(long_a) < one.index("## Other"))
        expect("the source keeps only the pointer", long_a not in read_text(os.path.join(root, "CLAUDE.md")))

        cur = read_text(os.path.join(root, "CLAUDE.md"))
        for needle, want in (("- **Rule B", "equally"), ("- **Rule C", "no entry in its section")):
            try:
                plan(root, os.path.join(root, "CLAUDE.md"), line_of(cur, needle))
                expect("refused: %s" % needle, False)
            except MoveError as e:
                expect("refusal explains itself: %s" % needle, want in str(e))

        m = plan(root, os.path.join(root, "CLAUDE.md"), line_of(cur, "- **Rule C"), new=HISTORY_DIR + "/History-New.md",
                 title="a new topic", date="2026-01-01")
        apply(m)
        new = read_text(os.path.join(root, HISTORY_DIR, "History-New.md"))
        expect("--new writes a header and the entry", new.startswith("# Long form: a new topic") and long_c in new)

        cur = read_text(os.path.join(root, "CLAUDE.md"))
        m = plan(root, os.path.join(root, "CLAUDE.md"), line_of(cur, "- **Rule B"), to=HISTORY_DIR + "/History-Two.md")
        before = read_text(os.path.join(root, "CLAUDE.md"))
        global write_text
        real_write = write_text

        def failing_write(path, text):  # the History write fails; any other write goes through
            if os.sep + HISTORY_DIR + os.sep in path:
                raise OSError("disk full")
            real_write(path, text)
        write_text = failing_write
        try:
            apply(m)
        except OSError:
            pass
        finally:
            write_text = real_write
        expect("a failed History write leaves the source untouched", read_text(os.path.join(root, "CLAUDE.md")) == before)

        import mdblocks
        target = os.path.join(root, HISTORY_DIR, "History-One.md")
        os.chmod(target, 0o644)
        kept = read_text(target)
        real_sync = mdblocks.sync_fd

        def failing_sync(fd):
            raise OSError("power cut")
        mdblocks.sync_fd = failing_sync
        try:
            mdblocks.write_text(target, "half-written")
            expect("an interrupted write raises", False)
        except OSError:
            pass
        finally:
            mdblocks.sync_fd = real_sync
        leftovers = lambda: [n for n in os.listdir(os.path.dirname(target)) if n.startswith(".doc-split-")]
        expect("an interrupted write leaves the old file whole", read_text(target) == kept)
        expect("and no temporary file behind", not leftovers())

        # F_FULLFSYNC: a failed drive flush must fail the write; only "not supported" falls back.
        real_fcntl, real_fsync, fsyncs = mdblocks.fcntl, mdblocks.os.fsync, []

        class Fcntl:
            F_FULLFSYNC = 51

            def __init__(self, err):
                self.err = err

            def fcntl(self, fd, cmd):
                raise OSError(self.err, os.strerror(self.err))

        def counting_fsync(fd):
            fsyncs.append(fd)
            real_fsync(fd)
        mdblocks.os.fsync = counting_fsync
        try:
            mdblocks.fcntl = Fcntl(errno.EIO)
            try:
                mdblocks.write_text(target, "unflushed")
                expect("a failed drive flush raises", False)
            except OSError:
                pass
            expect("a failed drive flush leaves the old file whole", read_text(target) == kept)
            expect("and no temporary file behind it either", not leftovers())
            expect("and is not papered over with fsync", not fsyncs)
            del fsyncs[:]
            mdblocks.fcntl = Fcntl(mdblocks.NO_FULLFSYNC[0])
            try:
                mdblocks.write_text(target, "fell back\n")
            except OSError:
                pass
            expect("a file system without F_FULLFSYNC still writes", read_text(target) == "fell back\n")
            expect("syncing the file and its folder with fsync instead",
                   len(fsyncs) == (2 if mdblocks.SYNCS_DIRECTORIES else 1))
        finally:
            mdblocks.fcntl, mdblocks.os.fsync = real_fcntl, real_fsync
        mdblocks.write_text(target, kept + "\n")
        if os.name != "nt":  # Windows keeps a read-only flag, not a mode
            expect("a replaced file keeps its permissions", os.stat(target).st_mode & 0o777 == 0o644)

        # The History file must be on disk, directory entry included, before the source changes.
        folder = os.stat(os.path.dirname(target)).st_ino
        events, real_replace, real_fchmod = [], mdblocks.os.replace, getattr(mdblocks.os, "fchmod", None)

        def sync(fd):
            st = os.fstat(fd)
            if stat.S_ISDIR(st.st_mode):
                events.append("sync its folder" if st.st_ino == folder else "sync another folder")
            else:
                events.append("sync the file")
            real_sync(fd)

        def rename(src, dst):
            events.append("rename")
            real_replace(src, dst)

        def set_mode(fd, mode):
            events.append("set the mode")
            real_fchmod(fd, mode)
        mdblocks.sync_fd, mdblocks.os.replace = sync, rename
        if real_fchmod is not None:
            mdblocks.os.fchmod = set_mode
        try:
            mdblocks.write_text(target, kept)
        finally:
            mdblocks.sync_fd, mdblocks.os.replace = real_sync, real_replace
            if real_fchmod is not None:
                mdblocks.os.fchmod = real_fchmod
        want = ((["set the mode"] if real_fchmod is not None else []) + ["sync the file", "rename"]
                + (["sync its folder"] if mdblocks.SYNCS_DIRECTORIES else []))
        expect("a write sets the mode, syncs, renames, then syncs its folder: %s" % events, events == want)

        # Windows, simulated: there is no fcntl module, no os.O_NOFOLLOW and (before Python 3.13) no
        # os.fchmod, os.open refuses a directory, and a text-mode write turns "\n" into "\r\n". Each
        # write must still land byte for byte, and a symlink must still be refused.
        import subprocess
        probe = subprocess.run([sys.executable, "-B", "-c", "import sys; sys.modules['fcntl'] = None; "
                                "sys.path.insert(0, %r); import mdblocks" % HERE], capture_output=True, text=True)
        expect("mdblocks imports where there is no fcntl module (Windows): %s"
               % (probe.stderr.strip().splitlines() or [""])[-1], probe.returncode == 0)

        absent = object()  # a patch value meaning "this attribute does not exist here"

        def lands(label, text, *patches):
            """Write TEXT to target with each (object, name, value) patched in, the value `absent`
            deleting the attribute; restore, then expect the exact bytes on disk."""
            saved = [(obj, name, getattr(obj, name, None), hasattr(obj, name)) for obj, name, _ in patches]
            try:
                for obj, name, value in patches:
                    if value is absent:
                        if hasattr(obj, name):
                            delattr(obj, name)
                    else:
                        setattr(obj, name, value)
                mdblocks.write_text(target, text)
                with open(target, "rb") as f:
                    got = f.read()
                why = "" if got == text.encode("utf-8") else "wrote %r" % got[:40]
            except BaseException as e:  # SystemExit included: a refusal is a failure here
                why = "%s: %s" % (type(e).__name__, e)
            finally:
                for obj, name, value, present in saved:
                    if present:
                        setattr(obj, name, value)
                    elif hasattr(obj, name):
                        delattr(obj, name)
            expect("%s%s" % (label, " (%s)" % why if why else ""), not why)

        fsyncs = []

        def counting(fd):
            fsyncs.append(fd)
            real_fsync(fd)
        lands("a write where there is no fcntl module syncs with fsync (Windows)", "no fcntl\n",
              (mdblocks, "fcntl", None), (mdblocks.os, "fsync", counting))
        expect("and fsync ran", bool(fsyncs))
        lands("a write where there is no os.fchmod still lands (Windows, Python 3.12 and older)", "no fchmod\n",
              (mdblocks.os, "fchmod", absent))

        def no_directories(path, flags, *args):
            if os.path.isdir(path):
                raise PermissionError(errno.EACCES, "Permission denied", path)
            return real_os_open(path, flags, *args)
        real_os_open = os.open
        lands("a write where os.open refuses a directory still lands (Windows)", "no folder sync\n",
              (mdblocks.os, "open", no_directories), (mdblocks, "SYNCS_DIRECTORIES", False))

        real_fdopen = os.fdopen

        def crlf_fdopen(fd, mode="r", *args, **kwargs):
            if "b" not in mode:
                kwargs.setdefault("newline", "\r\n")  # what a text-mode write does on Windows
            return real_fdopen(fd, mode, *args, **kwargs)
        lands("a write keeps its bytes where a text write turns \\n into \\r\\n (Windows)", "one\ntwo\n",
              (mdblocks.os, "fdopen", crlf_fdopen))

        tsv = os.path.join(root, "out.tsv")
        with open(tsv, "wb") as f:
            f.write(b"before\n")  # so the link below has a target even when the write fails
        nofollow = getattr(os, "O_NOFOLLOW", None)
        try:
            if nofollow is not None:
                del os.O_NOFOLLOW
            os.fdopen = crlf_fdopen
            with mdblocks.open_for_write(tsv) as f:
                f.write("a\tb\n")
            with open(tsv, "rb") as f:
                why = "" if f.read() == b"a\tb\n" else "the bytes changed"
        except BaseException as e:
            why = "%s: %s" % (type(e).__name__, e)
        finally:
            os.fdopen = real_fdopen
            if nofollow is not None:
                os.O_NOFOLLOW = nofollow
        expect("open_for_write writes where there is no O_NOFOLLOW (Windows), byte for byte%s"
               % (" (%s)" % why if why else ""), not why)
        link = os.path.join(root, "link.tsv")
        try:
            os.symlink(tsv, link)
        except (OSError, NotImplementedError):
            link = None  # a Windows account without the symlink privilege: nothing to refuse
        if link:
            with open(tsv, "rb") as f:
                held = f.read()
            try:
                if nofollow is not None:
                    del os.O_NOFOLLOW
                try:
                    mdblocks.open_for_write(link).close()
                    refused = "it wrote through the link"
                except SystemExit:
                    refused = ""
                except BaseException as e:
                    refused = "%s: %s" % (type(e).__name__, e)
            finally:
                if nofollow is not None:
                    os.O_NOFOLLOW = nofollow
            expect("and still refuses a symlink there%s" % (" (%s)" % refused if refused else ""), not refused)
            with open(tsv, "rb") as f:
                expect("leaving the link's target as it was", f.read() == held)

        bad = "- **Rule B stays binding.** → History-Two.md"
        try:
            plan(root, os.path.join(root, "CLAUDE.md"), line_of(cur, "- **Rule B"), to=HISTORY_DIR + "/History-Two.md",
                 text=bad.replace("B stays", "B, invented `Symbol`, stays"))
            expect("a hand-written line with an invented symbol is refused", False)
        except MoveError:
            pass
    finally:
        shutil.rmtree(root)
    print("self-test:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


def main():
    if "--self-test" in sys.argv[1:]:
        sys.exit(self_test())
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file", help="CLAUDE.md or .claude/rules/<name>.md")
    ap.add_argument("--largest", nargs="?", type=int, const=5, metavar="N", help="list the N largest movable entries")
    ap.add_argument("--line", type=int, metavar="L", help="the first line of the entry to move")
    ap.add_argument("--to", metavar="DOC", help="an existing ops/History-*.md to add it to")
    ap.add_argument("--new", metavar="DOC", help="a new ops/History-*.md to create for it")
    ap.add_argument("--title", help="topic for the header of a --new file")
    ap.add_argument("--text", help="a hand-written one-line replacement (checked like a polished line)")
    ap.add_argument("--dry-run", action="store_true", help="show the move without writing anything")
    a = ap.parse_args()
    try:
        if a.largest is not None:
            list_largest(REPO, a.file, a.largest)
            return
        if a.line is None:
            ap.error("give --line L (or --largest to see the candidates)")
        move = plan(REPO, a.file, a.line, a.to, a.new, a.title, a.text)
    except MoveError as e:
        sys.exit("move_entry: " + str(e))
    size = lambda s: len(s.encode("utf-8"))
    print("%s L%d (%d B) → %s/%s, under ## %s" % (move["rel"], a.line, size(move["item"]["text"]),
                                                   HISTORY_DIR, move["hist"], move["label"]))
    print("  leaves: %s" % move["line"])
    if move["fallback"]:
        print("  note: the entry has no bold title, so this line is its first sentence; pass --text to write a better one")
    print("  %s: %d → %d bytes" % (move["rel"], size(move["src_old"]), size(move["src_new"])))
    if a.dry_run:
        print("  (dry run: nothing written)")
        return
    apply(move)
    print("  done — the entry is in %s/%s verbatim and was verified before %s changed" % (
        HISTORY_DIR, move["hist"], move["rel"]))


if __name__ == "__main__":
    main()
