#!/usr/bin/env bash
#
# One shard of the race-detector suite.
#
# `make test` runs `go test -p N -race ./...` and is the local gate; this is
# the SAME flags, split so CI's wall clock stops being one package's
# sequential runtime. Measured on the 2026-09-21 run of gate.yml (895s wall,
# ubuntu-latest, 4 cores):
#
#   internal/manifest  760s      internal/api         72s
#   internal/admin     668s      internal/analyze     66s
#   cmd/bridge         218s      internal/adminauth   40s
#   internal/transcode 137s      everything else     <10s each
#
# Total work is ~1,970 CPU-seconds and `-p $(nproc)` already packs it onto
# four cores well — the job is not slow because the scheduler is bad, it is
# slow because ONE package takes 760s and nothing can start its second half
# early. `t.Parallel()` appears in 0 of internal/manifest's 138 test files
# and 0 of internal/admin's 131, so those two run strictly sequentially
# inside a single binary and MORE CORES DO NOTHING for them. Splitting the
# package across processes is the only lever that does, short of a faster
# core.
#
# So: `manifest` and `admin` are sharded by test name, and every other
# package rides one `rest` shard that keeps `-p $(nproc)`.
#
# WHY THE PARTITION IS GUARDED
#
# The failure mode this must not have is a test that runs in NO shard:
# every shard reports PASS, the gate goes green, and the test silently
# stopped running — the same shape as a fuzz target that "looks like it ran
# and did not". Three things are asserted on every invocation, all of them
# pure text and free:
#
#   - the listed set is non-empty (the `checked == 0` floor — a `go test
#     -list` that silently returns nothing would otherwise make every shard
#     trivially green);
#   - the shards are DISJOINT and their union is the whole list, recomputed
#     here rather than assumed from `NR % SHARDS`;
#   - this shard's own partition is non-empty.
#
# FUZZ TARGETS ARE IN THE PARTITION ON PURPOSE. Without `-fuzz` a target
# runs its seed corpus as an ordinary test, which is how `make test`
# absorbs 39 of them for free; dropping `Fuzz*` from the name list here
# would retire every seed corpus from CI without a single red X.
# `Benchmark*` is excluded because `go test` does not run benchmarks
# without `-bench`, so including them would only pad the regex.
#
# WHY THE REGEX IS ANCHORED
#
# `-run` matches each `/`-separated part of a test name with an UNANCHORED
# regexp, so a bare `TestFoo` alternative also matches `TestFooBar` — which
# would run that test in two shards (wasted minutes, and a confusing log).
# `^(A|B|C)$` pins the top-level name; subtests still run, because a
# pattern with no `/` part places no constraint on the parts below it.

set -euo pipefail

GROUP="${1:?usage: test-shard.sh <manifest|admin|rest> <index> <shards>}"
INDEX="${2:?shard index, 0-based}"
SHARDS="${3:?shard count}"

# The two flags that MUST stay identical to the Makefile's `test` target.
# They live here as well only because this script bypasses that target to
# add `-run`; if either changes there, change it here in the same commit.
RACE_FLAGS=(-race -timeout 30m)

# The packages sharded by test name, as ONE list with two readers: the
# dispatch at the bottom, and the exclusion `rest` applies. Two copies of
# this could disagree, and the way they would disagree is a package that
# runs in no shard at all.
SHARDED=(manifest admin)

cores() { command -v nproc >/dev/null 2>&1 && nproc || sysctl -n hw.ncpu; }

# names <pkg> — every name `-run` can actually select, one per line.
#
# The listing is captured BEFORE the filter so a package that fails to
# compile fails here, loudly, with go's own message — rather than reaching
# the empty-set floor below and being reported as "no test names", which
# names the symptom and hides the cause.
names() {
  local out
  if ! out="$(go test "$1" -list '.*')"; then
    echo "test-shard: could not list tests in $1" >&2
    return 1
  fi
  printf '%s\n' "$out" | grep -E '^(Test|Fuzz|Example)' || true
}

# partition <index> — this shard's slice of stdin, round-robin by line.
partition() {
  awk -v i="$1" -v n="$SHARDS" 'NR % n == i'
}

# is_sharded <import path> — true when this package has its own matrix legs.
# Exact tail match, never a substring: ./internal/adminauth must not be
# swallowed by the entry meant for ./internal/admin.
is_sharded() {
  local pkg="$1" g
  for g in "${SHARDED[@]}"; do
    [ "${pkg##*/internal/}" = "$g" ] && [ "$pkg" != "${pkg%/internal/$g}" ] && return 0
  done
  return 1
}

# assert_matrix_runs_the_sharded_packages — the one failure this whole
# arrangement can hide.
#
# `rest` SKIPS everything in SHARDED on the promise that other legs run it.
# Delete those legs from the workflow and leave this list alone and the
# package runs NOWHERE: nine green checks, a green gate, and 668s of tests
# that quietly stopped existing. Nothing else in the tree would notice, so
# the leg that does the excluding is the one that checks the promise.
#
# Skipped when the workflow file is absent, so the script still works when
# run by hand from somewhere else; in CI it is always there.
assert_matrix_runs_the_sharded_packages() {
  local wf=".github/workflows/gate.yml" g
  [ -f "$wf" ] || return 0
  for g in "${SHARDED[@]}"; do
    if ! grep -qE "group:[[:space:]]*${g}[[:space:]]*," "$wf"; then
      echo "test-shard: internal/${g} is excluded from the 'rest' shard, but no" >&2
      echo "            matrix leg in ${wf} runs it — its tests would run nowhere." >&2
      exit 1
    fi
  done
}

run_rest() {
  assert_matrix_runs_the_sharded_packages
  local all=() skipped=0 pkg
  while IFS= read -r pkg; do
    if is_sharded "$pkg"; then
      skipped=$((skipped + 1))
      continue
    fi
    all+=("$pkg")
  done < <(go list ./...)
  if [ ${#all[@]} -eq 0 ]; then
    echo "test-shard: go list ./... produced no packages" >&2
    exit 1
  fi
  # Every sharded package must have been SEEN and skipped. A typo in
  # SHARDED would otherwise skip nothing, which is merely wasteful here but
  # means the named group's own legs are testing a package that does not
  # exist.
  if [ "$skipped" -ne ${#SHARDED[@]} ]; then
    echo "test-shard: skipped $skipped packages, expected ${#SHARDED[@]} (${SHARDED[*]})" >&2
    exit 1
  fi
  echo "test-shard: rest — ${#all[@]} packages on $(cores) cores, ${#SHARDED[@]} sharded elsewhere"
  go test -p "$(cores)" "${RACE_FLAGS[@]}" "${all[@]}"
}

run_sharded() {
  local pkg="$1" all_names part i
  all_names="$(names "$pkg")"

  # Floor: a silently-empty list would make this shard — and every other —
  # pass without running anything.
  local total
  total="$(printf '%s\n' "$all_names" | grep -c . || true)"
  if [ "${total:-0}" -lt 1 ]; then
    echo "test-shard: no test names listed for $pkg" >&2
    exit 1
  fi

  # Disjoint + covering, as SET EQUALITY rather than as a count.
  #
  # Counting instead would be the weaker check that looks like this one: a
  # sum that matches is also what you get when one name lands in two shards
  # and another lands in none, which is exactly the bug worth catching.
  # Rebuild the union from the same partition function every shard uses and
  # compare it, sorted, against the whole list.
  local rebuilt
  rebuilt="$(for ((i = 0; i < SHARDS; i++)); do
    printf '%s\n' "$all_names" | partition "$i"
  done | sort)"
  if [ "$rebuilt" != "$(printf '%s\n' "$all_names" | sort)" ]; then
    echo "test-shard: the $SHARDS partitions of $pkg are not a partition of its $total names" >&2
    exit 1
  fi

  part="$(printf '%s\n' "$all_names" | partition "$INDEX")"
  local mine
  mine="$(printf '%s\n' "$part" | grep -c . || true)"
  if [ "${mine:-0}" -lt 1 ]; then
    echo "test-shard: shard $INDEX/$SHARDS of $pkg is empty (more shards than tests?)" >&2
    exit 1
  fi

  echo "test-shard: $pkg shard $INDEX/$SHARDS — $mine of $total names"
  go test "$pkg" "${RACE_FLAGS[@]}" -run "^($(printf '%s\n' "$part" | paste -sd'|' -))\$"
}

if [ "$GROUP" = rest ]; then
  run_rest
  exit 0
fi
for g in "${SHARDED[@]}"; do
  if [ "$GROUP" = "$g" ]; then
    run_sharded "./internal/$g/"
    exit 0
  fi
done
echo "test-shard: unknown group '$GROUP' (want rest, or one of: ${SHARDED[*]})" >&2
exit 1
