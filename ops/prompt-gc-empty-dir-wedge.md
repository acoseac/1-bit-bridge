# Prompt: `bridge upscale --gc` permanently wedges once its forward sweep empties the variants directory

> Paste everything below the line into a fresh session in `~/dev/1-bit-bridge`.
> Delete this file once the fix has merged — it is a one-shot work order, not a
> standing doc.

---

Repo: `~/dev/1-bit-bridge`. Read `CLAUDE.md` first — in particular
**"Things that have bitten before" → "Job pools"** and
**"The CLI and the serve wiring (`cmd/bridge`)"**. Work on a `feat/<topic>`
branch, PR to `main`, `make check` in the inner loop and
`make fmt vet test build-all` before pushing. Grep `ops/engineering-log.md` by
symbol before changing anything a rule names, and append your measurement and
your decision there when you are done. No `Co-Authored-By` trailer and no
"Generated with Claude Code" footer.

## The bug

`runGC` in [`cmd/bridge/upscale.go`](cmd/bridge/upscale.go) runs the sweeps in
this order:

1. forward sweep — unlinks files under `outputDir` that no `track_variants`
   row references;
2. `gcCheckOutputDirBeforeReverseSweep` — refuses to reap ROWS when
   `integrity.VariantsDirSweepBlockReason(outputDir)` reports the directory
   missing or empty, on the grounds that a cleanly unmounted volume looks
   exactly like that (PR #207 / the 2026-07-21 review's M15);
3. reverse sweep — reaps rows whose sidecar is gone.

On the **legacy hash-flat layout** (`<outputDir>/<hash>-<variantID>.flac`, every
sidecar directly under the root, no subdirectories) step 1 can remove every
file, which leaves the directory *genuinely* empty. Step 2 then reads that as
an unmounted volume and refuses, so the rows survive — and re-running does not
help, because the directory is still empty. It refuses again, having removed
nothing. **Those rows can never be reaped by `--gc`.**

Measured, twice in a row on each of `main` and `feat/gc-mass-orphan-guard`
(PR #940), with **no override flags**:

```
run 1: rc=1 rows_left=5
  stdout: GC forward sweep: removed 5 orphan file(s), kept 0 known sidecar(s), 0 failure(s).
  stderr: GC reverse sweep: variants directory is empty ("…") but 5 variant row(s) exist;
          refusing to delete rows en masse (likely a disconnected mount or filesystem issue
          — restore access and re-run).
run 2: rc=1 rows_left=5
  stdout: GC forward sweep: removed 0 orphan file(s), kept 0 known sidecar(s), 0 failure(s).
  stderr: (identical refusal)
```

The source-mirrored layout hides it: `filepath.WalkDir` never removes
directories, so an emptied subtree still leaves dirents behind and
`integrity.dirIsEmpty` reads non-empty.

**Reachability.** No flags are needed. At five rows and five flat sidecars both
of #940's new guards are under their floor of ten and never fire, so a plain
`bridge upscale --gc` reaches it. (An earlier note of mine said it required
`--allow-mass-delete --allow-mass-orphans` — that is how it was first
reproduced, not what it requires. Do not repeat that claim.) It predates #940;
that PR's guard only ever refuses *more*, so it neither causes nor reduces this.

## What to build

**The directory being empty *because this run just emptied it* is explained,
and is not evidence of an unmounted volume.** Tell the reverse guard what the
forward sweep removed, and let it proceed on that basis.

Suggested shape, but use your judgement:

- `gcCheckOutputDirBeforeReverseSweep` takes the forward sweep's `removed`
  count (`runGCForwardSweep` already returns it) and skips the mount-loss
  refusal when `removed > 0`.
- Keep the refusal intact for `removed == 0`, which is the case it was written
  for: the directory read empty and this run did nothing to make it so.
- A **missing** directory is not the same as an empty one. Decide explicitly
  whether an emptied-by-us directory that has also been *unlinked* should still
  proceed, and say which in a comment. `VariantsDirSweepBlockReason` returns a
  distinct reason string for each; do not collapse them.

Check the sibling callers before you change the shared helper:
`integrity.VariantsDirSweepBlockReason` is also used by
`internal/integrity.VariantWatcher.tick` and by
`gcRefuseEmptyKnownSetOverPopulatedDir`. The watcher's probe runs *before* its
sweep and the watcher never deletes files, so it should not need this — but
verify that rather than assuming it, and if you do change the helper's
signature, fix every caller in the same commit.

## Tests

- A red-first test driving the real `runGC`, twice in a row, over a flat-layout
  fixture with **no override flags**: run 1 must remove the files *and* reap the
  rows; run 2 must be a clean no-op at `rc=0`. The two-run shape is the point —
  a single run that merely exits 0 does not prove the wedge is gone.
- Keep a positive control for the guard that still matters: an
  `outputDir` that was *already* empty before the gc started, with rows in the
  catalog, must still refuse. Assert on the refusal text naming the mount, so a
  fix that just deletes the guard fails.
- Negative-control every load-bearing assertion, red-first, `-count=1`, and
  **commit before each control** — `git checkout --` takes uncommitted work with
  it, which this repo's log records being learned the hard way more than once.

## Scope

Fix the reverse guard only. Do **not** change the forward sweep, the
mass-orphan guard, the relocation guard, or `analyze --gc`; #940 has just
reworked all of those and a conflict there costs more than it saves. If #940 is
still open, either base on it or rebase onto `main` once it lands — the
surrounding code was restructured there (`gcTakeInventory` /
`runGCForwardSweep(inv)`), so a patch written against the older shape will not
apply cleanly.

## Worth considering while you are in here

Whether an operator with wedged rows has *any* way out today. `bridge manifest
clear-missing` and `bridge restore` both refuse while a bridge answers on the
admin port, and neither targets `track_variants`. If the answer is "no way
out", say so in the PR body — it changes how urgent this is — but do not build
a new escape hatch in the same PR.
