#!/usr/bin/env python3
"""Measure a markdown file by heading and by top-level block (sizes and first-line labels only).

  measure.py FILE [--headings] [--blocks N] [--tsv OUT.tsv]

  --headings  bytes per heading: "own" runs to the next heading of any level, "total" to the
              next heading of the same or a higher level
  --blocks N  the N largest blocks: first line number, bytes, kind, section, start of the text
  --tsv OUT   every block to OUT.tsv and every heading to OUT.headings.tsv, outside the repository

Blocks are the ones mdblocks.parse sees, i.e. the units the condensation moves.
"""
import argparse
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from mdblocks import parse, read_text, existing_file, output_file, open_for_write  # noqa: E402

REPO = os.path.abspath(os.path.join(HERE, "..", ".."))
BINS = ((20000, None), (10000, 20000), (5000, 10000), (2000, 5000), (1000, 2000), (0, 1000))


def measure(text):
    """(total_bytes, line_count, headings, blocks). A heading row is (line, level, own_bytes,
    total_bytes, title); a block row is (first_line, last_line, kind, section, bytes, label)."""
    lines, items = parse(text)
    size = [len(line.encode("utf-8")) + 1 for line in lines]
    size[-1] -= 1  # the last line has no newline after it
    heads = [it for it in items if it["type"] == "heading"]
    hrows = []
    for k, h in enumerate(heads):
        own_end = heads[k + 1]["start"] if k + 1 < len(heads) else len(lines)
        total_end = next((x["start"] for x in heads[k + 1:] if x["lvl"] <= h["lvl"]), len(lines))
        hrows.append((h["start"] + 1, h["lvl"], sum(size[h["start"]:own_end]),
                      sum(size[h["start"]:total_end]), h["title"]))
    brows = []
    for b in (it for it in items if it["type"] == "block"):
        section = " > ".join(t[:40] for _, t in b["path"])
        brows.append((b["start"] + 1, b["end"], b["kind"], section, sum(size[b["start"]:b["end"]]),
                      re.sub(r"\s+", " ", lines[b["start"]])[:110]))
    return sum(size), len(lines), hrows, brows


def headings_path(blocks_path):
    return output_file(blocks_path[: -len(".tsv")] + ".headings.tsv", REPO, ".tsv")


def write_tsv(blocks_path, hrows, brows):
    heads_path = headings_path(blocks_path)
    with open_for_write(blocks_path) as f:
        f.write("start\tend\tkind\tbytes\tsection\tlabel\n")
        for b in brows:
            f.write("%d\t%d\t%s\t%d\t%s\t%s\n" % (b[0], b[1], b[2], b[4], b[3], b[5]))
    with open_for_write(heads_path) as f:
        f.write("line\tlvl\town\ttotal\ttitle\n")
        for r in hrows:
            f.write("%d\t%d\t%d\t%d\t%s\n" % r)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("file")
    ap.add_argument("--headings", action="store_true", help="bytes per heading")
    ap.add_argument("--blocks", type=int, default=0, metavar="N", help="the N largest blocks")
    ap.add_argument("--tsv", metavar="OUT.tsv", help="write every block and heading as TSV")
    a = ap.parse_args()
    source = existing_file(a.file, ".md")
    tsv = output_file(a.tsv, REPO, ".tsv") if a.tsv else None
    if tsv:
        headings_path(tsv)  # both destinations are checked before any work

    total, n_lines, hrows, brows = measure(read_text(source))
    print(f"FILE {a.file}: {total} bytes, {n_lines} lines, {len(hrows)} headings, {len(brows)} blocks")
    if a.headings:
        print("line\tlvl\town\ttotal\ttitle")
        for r in hrows:
            print(f"{r[0]}\t{r[1]}\t{r[2]}\t{r[3]}\t{r[4][:95]}")
    print("\nblock size distribution:")
    for lo, hi in BINS:
        sel = [b for b in brows if b[4] >= lo and (hi is None or b[4] < hi)]
        rng = f">={lo}" if hi is None else f"{lo}-{hi}"
        print(f"  {rng:>12}: {len(sel):4d} blocks, {sum(x[4] for x in sel):8d} bytes")
    if a.blocks:
        print(f"\n{a.blocks} largest blocks:")
        for b in sorted(brows, key=lambda x: -x[4])[: a.blocks]:
            print(f"  L{b[0]}\t{b[4]}\t{b[2]}\t[{b[3][:70]}]\t{b[5][:90]}")
    if tsv:
        write_tsv(tsv, hrows, brows)


if __name__ == "__main__":
    main()
