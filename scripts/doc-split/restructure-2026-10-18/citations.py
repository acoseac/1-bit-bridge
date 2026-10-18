#!/usr/bin/env python3
"""Resolve every "CLAUDE.md …" citation in the code of REV against the working tree.

  citations.py [--base REV] [--all]

Reads every Go, JavaScript, HTML, CSS, shell, PowerShell, workflow and Makefile line of REV that
names CLAUDE.md, takes the phrase it cites from PHRASES (curated, keyed by file and a unique part
of the line), and finds it in REV's CLAUDE.md: the block or heading that holds it is the entry
the citation means. It then finds where that entry is now, verbatim (relative links compared by
the file they reach), in CLAUDE.md, the rules files, the skills or the ops/History-*.md long
forms, and names the always-loaded file that carries its line: a rules file, or CLAUDE.md for a
core long form. A phrase REV's CLAUDE.md does not hold is listed as stale at REV, not failed; an
entry REV held that is now nowhere fails the run, as does a citation line PHRASES does not read.
--all prints every row, not only the stale and the unresolved ones.
"""
import argparse
import glob
import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
from mdblocks import (read_text, safe_rev, git_show, parse, split_frontmatter, rebase_links,  # noqa: E402
                      HISTORY_DIR)

REPO = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
BASE = "6f816e49"
SOURCES = ("*.go", "*.js", "*.mjs", "*.html", "*.css", "*.sh", "*.ps1", "*.yml", "*.yaml", "Makefile", "Dockerfile")
NOW = ("CLAUDE.md", ".claude/rules/*.md", ".claude/skills/*/SKILL.md", HISTORY_DIR + "/History-*.md")

# The phrase each citation names, by (file, a unique part of its line). A str is looked for as it
# stands. A (cited, meaning) pair is a citation whose own wording was already stale at the base:
# CITED is looked for first, and MEANING, a phrase of the entry it means, settles where it
# resolves. None is a line that names no section or phrase ("see CLAUDE.md"), listed as such.
PHRASES = {
    (".github/workflows/docker.yml", "republish-a-frozen-tag recovery in CLAUDE.md"): "tag=<ver>` + `ref=main",
    (".github/workflows/fuzz.yml", "Measured on FuzzFoldForMatch (CLAUDE.md)"): "19,003",
    (".github/workflows/gate.yml", "local gate documented in CONTRIBUTING.md / CLAUDE.md"): "make fmt vet test build-all",
    (".github/workflows/gate.yml", "run in CI, on any branch, since they were written. CLAUDE.md cites their"):
        "The DSD decimation was MEASURED and it does not alias",
    (".github/workflows/gate.yml", "and whose numbers CLAUDE.md cites as settling the decimation"):
        "The DSD decimation was MEASURED and it does not alias",
    ("Makefile", "numbers reproduce CLAUDE.md's to the decimal"): "the stopband is -116.8 dB",
    ("cmd/bridge/blank_keepers_test.go", "CLAUDE.md's rule still says to delete it"): "An import keeper is dead code",
    ("cmd/bridge/cited_test_names_test.go", "stale-claim warnings CLAUDE.md"): "have been wrong and been corrected",
    ("cmd/bridge/cited_test_names_test.go", "where CLAUDE.md"): "Put it in a `_`-prefixed dir",
    ("cmd/bridge/cited_test_names_test.go", "`CLAUDE.md` is the file this harness loads on"): None,
    ("cmd/bridge/cited_test_names_test.go", "test declared there satisfies no citation here. CLAUDE.md tells an"):
        "Put it in a `_`-prefixed dir",
    ("cmd/bridge/cited_test_names_test.go", ".#CLAUDE.md"): None,
    ("cmd/bridge/gc_lost_mount_test.go", "(CLAUDE.md, #941)"): "#941",
    ("cmd/bridge/init_rewrite.go", "CLAUDE.md, is the same fact"):
        "Never advertise an endpoint synthesised from this process's own listen port",
    ("cmd/bridge/main.go", "functional index. Per CLAUDE.md PR #126."): "#126",
    ("cmd/bridge/main.go", "CLAUDE.md PR #109"): "#109",
    ("cmd/bridge/main.go", "time. Per CLAUDE.md PR #126."): "#126",
    ("cmd/bridge/main.go", "see CLAUDE.md \"Things that have bitten before\" — public-"): "`Validate()` is a pure shape check",
    ("cmd/bridge/main.go", "are in CLAUDE.md under \"Job pools\""): "A forward sweep's denominator is the TREE",
    ("cmd/bridge/rendition_stamp.go", "(CLAUDE.md, the rule on reapers that"):
        "`ReapOrphans`-style directory reapers must refuse an empty root",
    ("cmd/bridge/retention_sweeper_test.go", "(CLAUDE.md: \"a fixture must be a"):
        "a fixture must be a value the transformation would actually change",
    ("cmd/bridge/root_owner_jobs_test.go", "(CLAUDE.md, dido)"): "as root with sox and ffmpeg on dido",
    ("cmd/bridge/root_owner_test.go", "(CLAUDE.md, dido)"): "skips and dido's container runs",
    ("cmd/bridge/runserve_lifecycle_test.go", "CLAUDE.md's architecture table"): "Architecture at a glance",
    ("cmd/bridge/serve_restart.go", "CLAUDE.md, \"POST"): "`POST /api/restart` must invoke the same cancellation closure",
    ("cmd/bridge/upnp_upstream_wiring.go", "CLAUDE.md's cadence rule"): "A cadence provider needs a rearm",
    ("cmd/bridge/upnp_upstream_wiring.go", "CLAUDE.md's documented \"anything wanting both membership"):
        "anything wanting both membership and liveness",
    ("cmd/bridge/upscale.go", "\"DeleteTrack sidecar-cleanup contract\""): "DeleteTrack sidecar-cleanup contract",
    ("cmd/bridge/upscale.go", "CLAUDE.md \"no"): "no premature abstractions",
    ("deploy/linux/deploy-bridge-vps.sh", "docs/ tree; see CLAUDE.md"): "`docs/` is a PUBLIC WEBSITE",
    ("deploy/linux/deploy-bridge-vps.sh", "(CLAUDE.md: \"keep"): "keep host coordinates in env vars with placeholder defaults",
    ("deploy/windows/firewall-bridge-windows.ps1", "(per CLAUDE.md QUIC PR #271)"): "#271",
    ("internal/admin/console_source_parity_test.go", "at the time; see CLAUDE.md"): "A rate-limited bot's silence is not approval",
    ("internal/admin/console_source_parity_test.go", "CLAUDE.md already records that stripJSNoise"): "stripJSNoise",
    ("internal/admin/handlers_library_meta.go", "(CLAUDE.md admin posture)"): "Admin console is loopback-only",
    ("internal/admin/handlers_login_ticket_test.go", "CLAUDE.md records for the live fixture too"):
        "the browser must use the HOST",
    ("internal/admin/handlers_pairing.go", "per the CLAUDE.md invariant"):
        ("rotated server cert requires re-pairing", "TLS fingerprint is captured once"),
    ("internal/admin/handlers_retention_settings_test.go", "CLAUDE.md names this exact shape"):
        "saves nothing while the page still says",
    ("internal/admin/layout_drawer_test.go", "CLAUDE.md's rule for this class of change"): "drive the console in a browser",
    ("internal/admin/layout_drawer_test.go", "trap CLAUDE.md records for the deletion diffs"): "Deleting scattered CSS or JS",
    ("internal/admin/settings_matrix_doc_test.go", "A CLAUDE.md"): "the WAV/AIFF extractor gap",
    ("internal/admin/templates/library.html", "See CLAUDE.md admin mobile conventions."): "admin mobile conventions",
    ("internal/admin/upload_panel_test.go", "last time (see CLAUDE.md)"): "must normalize CRLF first",
    ("internal/adminauth/hashcost_test.go", "CLAUDE.md records the same trap"): "A guard that scans SOURCE must parse, or strip comments",
    ("internal/adminauth/hashcost_test.go", "CLAUDE.md tells an operator to put a throwaway helper in one"):
        "Put it in a `_`-prefixed dir",
    ("internal/adminauth/hashcost_test.go", "holding the directories CLAUDE.md tells an operator to put a throwaway"):
        "Put it in a `_`-prefixed dir",
    ("internal/analyze/decode.go", "redactSoxErr rule in CLAUDE.md"): "redactSoxErr",
    ("internal/api/files.go", "(see CLAUDE.md v0.1.4)"): "v0.1.4",
    ("internal/api/upscale_delete.go", "invariant in CLAUDE.md (avoids the \"stats flash 0 while"): "stats flash 0",
    ("internal/api/upscale_deleted_event.go", "Mirror-PR pair invariant (see CLAUDE.md)"): "Mirror-PR",
    ("internal/api/upscale_events.go", "Mirror-PR pair invariant (see CLAUDE.md)"): "Mirror-PR",
    ("internal/atlasharvest/state_test.go", "(CLAUDE.md's Windows rule)"): "Windows CI catches wall-clock assumptions",
    ("internal/atlasharvest/stored_base_test.go", "(CLAUDE.md's rule against mtimes)"):
        "Windows CI catches wall-clock assumptions",
    ("internal/atomicwrite/atomicwrite_test.go", "(CLAUDE.md, PR #63)"): "#63",
    ("internal/backup/backup_test.go", "(CLAUDE.md, PR #63)"): "#63",
    ("internal/backup/manifest_atomic_test.go", "the same position CLAUDE.md records for the init-permissions work"):
        "init-permissions",
    ("internal/config/config.go", "Rationale (see CLAUDE.md \"Things"): "`Validate()` is a pure shape check",
    ("internal/config/config.go", "(CLAUDE.md PR #75 invariant)"): "#75",
    ("internal/config/deployment_test.go", "a CLAUDE.md \"operator just runs the"): "operator just runs the bridge",
    ("internal/config/variant_sweep_guard_test.go", "field's docblock and CLAUDE.md quote"):
        "variantSweepMaxDeletePercent` (default 20",
    ("internal/config/variant_sweep_guard_test.go", "the field's docblock and CLAUDE.md say 20"):
        "variantSweepMaxDeletePercent` (default 20",
    ("internal/ctxerr/ctxerr.go", "CLAUDE.md records under \"The CLI and"): "A cancelled pass is not a failed one",
    ("internal/dlna/server.go", "`internal/integrity` (CLAUDE.md"):
        ("logging-convention invariant", "`logging.Component` resolves `slog.Default()` at LOG time"),
    ("internal/doctor/sighting.go", "bullet in CLAUDE.md), so all it shows is that pid is not in it"):
        "The image's `lsof` package is load-bearing",
    ("internal/doctor/stand_in_pid_test.go", "(CLAUDE.md: a test that records a pid of its own"):
        "records a pid of its own choosing forces what the probe says about it",
    ("internal/enrich/cache_key_test.go", "CLAUDE.md's convention for these counters"): "grandfathered bare-int",
    ("internal/fsutil/owner.go", "(CLAUDE.md, \"Atomic writes\""): "Atomic writes: stage, then rename with retry",
    ("internal/fsutil/owner_create_unix_test.go", "(CLAUDE.md, dido)"): "as root with sox and ffmpeg on dido",
    ("internal/fsutil/owner_unix_test.go", "(CLAUDE.md, dido)"): "skips and dido's container runs",
    ("internal/integrity/sidecars.go", "The reset procedure this repo's own CLAUDE.md documents is"):
        "`rm -f bridge.db*` + restart",
    ("internal/integrity/sidecars_test.go", "own CLAUDE.md documents, and `run` takes a tick at boot"):
        "`rm -f bridge.db*` + restart",
    ("internal/manifest/analysis_failure.go", "\"both wrong together\" shape CLAUDE.md records"):
        "two copies can be wrong together",
    ("internal/manifest/dupe_refs.go", "(CLAUDE.md composition-bars discipline)"): "composition-bars",
    ("internal/manifest/extractors.go", "Compilation flag + safety net (CLAUDE.md"): "Track.Compilation",
    ("internal/manifest/fuzz_sacd_test.go", "CLAUDE.md states the rule"):
        "A crash found by the extractor targets is a REAL defect, not a nicety",
    ("internal/manifest/fuzz_sacd_test.go", "CLAUDE.md calls out: it asserts an invariant"): "PROPERTY assertions",
    ("internal/manifest/library_meta.go", "(CLAUDE.md composition-bars discipline)"): "composition-bars",
    ("internal/manifest/name_collisions.go", "(CLAUDE.md's Speller bullet)"): "fsutil.Speller",
    ("internal/manifest/scanner_fixture_test.go", "CLAUDE.md points future authors at these by name"):
        "scanner_fixture_test.go",
    ("internal/manifest/scanner_panic_recovery_test.go", "panic-recover contract documented in CLAUDE.md"):
        "Per-iteration panic recovery",
    ("internal/manifest/scanner_subtree_case_test.go", "This is the CLAUDE.md rule"):
        "\"We could not see this path\" dominates every",
    ("internal/manifest/scanner_walk_error_test.go", "CLAUDE.md flags as the most likely cause"):
        "the bridge lost my library",
    ("internal/manifest/store.go", "(see CLAUDE.md \"enriched_at monotonicity\")"): "`enriched_at` monotonicity",
    ("internal/manifest/store.go", "it carries no `json:` tags (see CLAUDE.md →"): "## Wire-type discipline",
    ("internal/manifest/store.go", "the bridge is in single-root or multi-root mode (see CLAUDE.md"):
        "Single ↔ multi-root storage form flips",
    ("internal/manifest/store.go", "No `json:` tags. It is a rows.Scan target in this file, and CLAUDE.md's"):
        "MUST NOT gain `json:` tags",
    ("internal/manifest/store_trackstat_test.go", "This is the load-bearing assertion behind GetTrackStat. CLAUDE.md"):
        "GetTrackStat",
    ("internal/manifest/variant_failure.go", "see the FavoritedTrackFeatures note in CLAUDE.md"): "FavoritedTrackFeatures",
    ("internal/manifest/variant_failure.go", "(see the prefix-case entry in CLAUDE.md)"): "MUST be a byte range, never `LIKE`",
    ("internal/smartplaylistgen/generate.go", "Mood bands — see CLAUDE.md / plan"): "Mood bands",
    ("internal/tailscale/tailscale.go", "`--peers=false` is the existing CLAUDE.md invariant"): "--peers=false",
    ("internal/tailscale/tailscale.go", "**`exec.LookPath` results MUST be absolute** (existing CLAUDE.md"):
        ("`exec.LookPath` results MUST be absolute", "`exec.LookPath`'s absolute answer"),
    ("internal/transcode/output_fault.go", "(CLAUDE.md, the B17"): "backlog B17",
    ("internal/transcode/output_fault_unix.go", "(CLAUDE.md, the B17 bullet)"): "backlog B17",
    ("internal/transcode/pool.go", "Coalescing publisher (CLAUDE.md: \"Bounded SSE publisher"):
        ("Bounded SSE publisher goroutine for transcode events", "The publisher is one long-lived goroutine"),
    ("internal/transcode/pool_publisher_test.go", "Tests for the bounded coalescing publisher pattern (CLAUDE.md:"):
        ("Bounded SSE publisher goroutine for transcode events", "The publisher is one long-lived goroutine"),
    ("internal/updater/sessions.go", "See iOS CLAUDE.md \"Things that have bitten before\"."): None,
    ("internal/upnpingest/ingest.go", "CLAUDE.md-documented design: a ~24 h time backstop"): "24 h time backstop",
}


def curated(rel, line):
    """The PHRASES key for this citation line: the one whose part of the line it holds (the longest
    when two match)."""
    keys = [k for k in PHRASES if k[0] == rel and k[1] in line]
    return max(keys, key=lambda k: len(k[1])) if keys else None


def sources_at(sha):
    """{path: text} of REV's tracked files that may carry a citation (the doc-split tooling excepted)."""
    out = subprocess.run(["git", "-C", REPO, "ls-tree", "-r", "-z", "--name-only", "--end-of-options", sha],
                         capture_output=True, text=True, check=True).stdout
    names = [p for p in out.split("\0") if p and not p.startswith("scripts/doc-split/")
             and (p.endswith(tuple(x[1:] for x in SOURCES if x.startswith("*"))) or p in SOURCES)]
    return {p: git_show(REPO, sha, p) for p in names}


def pattern(phrase):
    """PHRASE as a regex over text with `**` and backticks removed: case-folded, any whitespace
    run for a space, and word boundaries at its ends, so `B17` does not match B170."""
    bare = phrase.replace("**", "").replace("`", "")
    body = r"\s+".join(re.escape(t) for t in bare.split())
    before = r"(?<![\w#])" if re.match(r"[\w#]", bare) else ""
    after = r"(?!\w)" if re.search(r"\w$", bare) else ""
    return re.compile(before + body + after, re.I)


def line_of(phrase, text):
    """The 1-based line where PHRASE first occurs in TEXT, or None. Removing `**` and backticks
    removes no line break, so the line counted in the stripped text is the file's."""
    flat = text.replace("**", "").replace("`", "")
    m = pattern(phrase).search(flat)
    return flat.count("\n", 0, m.start()) + 1 if m else None


def items_of(text):
    """[(first line, last line, item)] for the headings and blocks of TEXT, 1-based."""
    fm, body = split_frontmatter(text)
    off = fm.count("\n")
    _, items = parse(body)
    return [(it["start"] + 1 + off, max(it["end"], it["start"] + 1) + off, it) for it in items]


def now_index():
    """{root-relative block text: (file, line)} and {heading title: (file, line)} over today's files."""
    blocks, heads = {}, {}
    for pat in NOW:
        for path in sorted(glob.glob(os.path.join(REPO, pat))):
            rel = os.path.relpath(path, REPO)
            for first, _, it in items_of(read_text(path)):
                if it["type"] == "heading":
                    heads.setdefault(it["title"], (rel, first))
                else:
                    blocks.setdefault(rebase_links(it["text"], os.path.dirname(rel), ""), (rel, first))
    return blocks, heads


def carrier(rel):
    """The always-loaded file that holds the line of an entry whose text is in REL."""
    name = os.path.basename(rel)
    if rel.startswith(HISTORY_DIR + "/History-rules-"):
        return ".claude/rules/" + name[len("History-rules-"):]
    if rel.startswith(HISTORY_DIR + "/History-"):
        return "CLAUDE.md"
    return rel


def resolve(phrase, base_text, base_items, blocks, heads):
    """(base line, base label, now location or None) for PHRASE, or None when the base lacks it."""
    line = line_of(phrase, base_text)
    if line is None:
        return None
    item = next((it for first, last, it in base_items if first <= line <= last), None)
    if item is None:
        return line, "?", None
    label = " › ".join(t for lvl, t in item["path"] if lvl >= 2) or "(top)"
    if item["type"] == "heading":
        return line, label, heads.get(item["title"])
    return line, label, blocks.get(item["text"])


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--base", default=BASE)
    ap.add_argument("--all", action="store_true", help="print every row, not only stale and unresolved ones")
    a = ap.parse_args()
    sha = safe_rev(REPO, a.base)
    base_text = git_show(REPO, sha, "CLAUDE.md")
    base_items = items_of(base_text)
    blocks, heads = now_index()
    rows, unread, used = [], [], set()
    for rel, text in sorted(sources_at(sha).items()):
        for k, line in enumerate(text.split("\n")):
            if "CLAUDE.md" not in line:
                continue
            key = curated(rel, line)
            if key is None:
                unread.append("%s:%d: %s" % (rel, k + 1, line.strip()[:120]))
                continue
            used.add(key)
            entry = PHRASES[key]
            where = "%s:%d" % (rel, k + 1)
            if entry is None:
                rows.append((where, "(names no section or phrase)", "-", "-", "-"))
                continue
            cited, meaning = entry if isinstance(entry, tuple) else (entry, entry)
            found, label = resolve(cited, base_text, base_items, blocks, heads), cited
            if found is None and meaning != cited:
                found = resolve(meaning, base_text, base_items, blocks, heads)
                label = "%s (its wording was stale; it means: %s)" % (cited, meaning)
            if found is None:
                rows.append((where, label, "STALE at base", "-", "-"))
                continue
            line_no, section, now = found
            rows.append((where, label, "L%d (%s)" % (line_no, section[:60]),
                         "%s:%d" % now if now else "UNRESOLVED", carrier(now[0]) if now else "-"))
    unresolved = [r for r in rows if r[3] == "UNRESOLVED"]
    for where, phrase, b, n, c in rows:
        if a.all or n == "UNRESOLVED" or b == "STALE at base" or phrase.startswith("("):
            print("%s\n    cites: %s\n    base: %s\n    now:  %s   (its line loads from %s)" % (where, phrase, b, n, c))
    missing = sorted(set(PHRASES) - used)
    print("\n%d citation lines at %s: %d resolve to an entry now, %d name no phrase, %d stale at the base "
          "(listed, not failed), %d UNRESOLVED; %d lines PHRASES does not read; %d PHRASES rows match no line" % (
              len(rows), a.base, sum(1 for r in rows if r[3] not in ("UNRESOLVED", "-")),
              sum(1 for r in rows if r[1].startswith("(")), sum(1 for r in rows if r[2] == "STALE at base"),
              len(unresolved), len(unread), len(missing)))
    for u in unread:
        print("  UNREAD", u)
    for m in missing:
        print("  UNUSED", m)
    sys.exit(1 if unresolved or unread or missing else 0)


if __name__ == "__main__":
    main()
