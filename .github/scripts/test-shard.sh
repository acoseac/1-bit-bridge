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
# WHAT MUST NOT HAPPEN
#
# A test that runs in NO shard: every shard reports PASS, the gate goes
# green, and the test silently stopped running — the same shape as a fuzz
# target that "looks like it ran and did not". Everything below that reads
# as paranoia is guarding that one outcome, because nothing else in the
# tree would notice it:
#
#   - the listed set is non-empty (the `checked == 0` floor — a `go test
#     -list` that silently returns nothing would otherwise make every shard
#     trivially green);
#   - the name partitions are DISJOINT and their union is the whole list,
#     recomputed as set equality rather than as a count, because a matching
#     count is also what you get when one name lands in two shards and
#     another lands in none;
#   - this shard's own partition is non-empty;
#   - the workflow matrix actually schedules every shard this script
#     believes in — see assert_matrix_is_complete.
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

GROUP="${1:?usage: test-shard.sh <rest|manifest|admin> <index>}"
INDEX="${2:?shard index, 0-based}"

# The two flags that MUST stay identical to the Makefile's `test` target.
# They live here as well only because this script bypasses that target to
# add `-run`; if either changes there, change it here in the same commit.
RACE_FLAGS=(-race -timeout 30m)

# group:shards — which packages are sharded by test name AND how many ways,
# as ONE declaration with three readers: the dispatch at the bottom, the
# exclusion `rest` applies, and the matrix check.
#
# The COUNT lives here rather than in the workflow on purpose. It used to be
# a `shards:` key each matrix leg passed in, which made the workflow and
# this script two places that had to agree about the partition — and they
# could disagree silently, because a leg passing a different count computes
# a different partition of the same names. The workflow now says only WHICH
# group and WHICH index; this file decides how many there are, and
# assert_matrix_is_complete requires the matrix to match.
SHARDED=(manifest:4 admin:4)

cores() { command -v nproc >/dev/null 2>&1 && nproc || sysctl -n hw.ncpu; }

# sharded_count <group> — how many ways that group splits; non-zero exit
# when the group is not sharded at all.
#
# A `group -> count` associative array would read better and is bash 4;
# macOS still ships 3.2 and this script is run by hand there, so the pairs
# are packed into one indexed array instead.
sharded_count() {
  local entry
  for entry in "${SHARDED[@]}"; do
    if [ "${entry%%:*}" = "$1" ]; then
      printf '%s' "${entry##*:}"
      return 0
    fi
  done
  return 1
}

# is_sharded <import path> — true when this package has its own matrix legs.
#
# A glob with NO trailing wildcard, which is what makes it an exact tail
# match: ./internal/adminauth does not match */internal/admin, so it stays
# in the `rest` leg where it belongs.
is_sharded() {
  local pkg="$1" entry
  for entry in "${SHARDED[@]}"; do
    if [[ "$pkg" == */internal/"${entry%%:*}" ]]; then
      return 0
    fi
  done
  return 1
}

# names <pkg> — every name `-run` can actually select, one per line.
#
# DISCOVERY RUNS UNDER RACE_FLAGS, and that is load-bearing rather than
# tidy. `-race` defines the `race` build tag, so a listing taken without it
# describes a DIFFERENT build than the one the shard executes: a
# `//go:build race` test would be absent from the list, land in no
# partition, and silently never run. This repo already carries both tags
# (internal/manifest's racefixture_*_test.go size the compaction fixture by
# build), so it is a single new function away rather than hypothetical. The
# mirror case is harmless by construction: a `//go:build !race` name simply
# is not in the race listing, and the shard that would have owned it never
# asks for it.
#
# It is also strictly less work. The shard has to build the race binary
# regardless; listing without `-race` builds a SECOND, non-race binary that
# nothing then uses.
#
# The listing is captured BEFORE the filter so a package that fails to
# compile fails here, loudly, with go's own message — rather than reaching
# the empty-set floor below and being reported as "no test names", which
# names the symptom and hides the cause. (CodeRabbit on #943.)
names() {
  local out
  if ! out="$(go test "${RACE_FLAGS[@]}" "$1" -list '.*')"; then
    echo "test-shard: could not list tests in $1" >&2
    return 1
  fi
  printf '%s\n' "$out" | grep -E '^(Test|Fuzz|Example)' || true
}

# partition <names> <index> <count> — one shard's slice, round-robin by line.
partition() {
  printf '%s\n' "$1" | awk -v i="$2" -v n="$3" 'NR % n == i'
}

# matrix_indices <workflow> <group> — the `index:` of every matrix leg
# naming that group, ascending.
#
# One leg per line is the matrix's shape and this depends on it. The
# dependency is safe in the direction that matters: a leg split across
# lines yields no index here, the set comes up short, and the caller
# FAILS. It cannot invent coverage that is not there.
#
# `group: <name>` followed by any non-name character or end of line, never
# a trailing comma — that comma exists only because `index` happens to
# follow `group` today, and a comma-dependent match would report "runs
# nowhere" about a perfectly healthy matrix if the keys were reordered.
# POSIX ERE rather than `\b`, a GNU extension this script cannot assume on
# every host it is run from.
# `|| true` because a group with NO legs left is the single most important
# case this reports, and grep exits 1 on no match — under `pipefail` that
# killed the whole script before the caller could say what was wrong. It
# still failed CLOSED, which is the right direction, but a CI failure with
# an empty log names nothing. Verified by deleting all four admin legs: the
# script exited 1 and printed not one word.
matrix_indices() {
  grep -E "group:[[:space:]]*$2([^[:alnum:]_]|$)" "$1" |
    sed -nE 's/.*index:[[:space:]]*([0-9]+).*/\1/p' | sort -n || true
}

# assert_matrix_is_complete — the failure this whole arrangement can hide.
#
# `rest` SKIPS every sharded package on the promise that other legs run it.
# Checking that the group is MENTIONED somewhere is not that promise:
# delete just `manifest` index 2 and the mention survives, `rest` still
# excludes internal/manifest, and that partition's 208 tests run nowhere
# behind nine green checks. Verified by deleting exactly that leg and
# watching the mention-only check pass (CodeRabbit on #943).
#
# So: require the exact index set 0..n-1, which also rejects a duplicate
# index and a leg naming a group this script does not shard.
#
# Skipped when the workflow file is absent, so the script still works when
# run by hand from somewhere else; in CI it is always there.
assert_matrix_is_complete() {
  local wf=".github/workflows/gate.yml" entry g n want got
  [ -f "$wf" ] || return 0
  for entry in "${SHARDED[@]}"; do
    g="${entry%%:*}"
    n="${entry##*:}"
    want="$(seq 0 $((n - 1)))"
    got="$(matrix_indices "$wf" "$g")"
    if [ "$got" != "$want" ]; then
      echo "test-shard: ${wf} does not schedule all $n shards of internal/${g}." >&2
      echo "            want indices: $(printf '%s' "$want" | paste -sd, -)" >&2
      echo "            got:          $(printf '%s' "$got" | paste -sd, -)" >&2
      echo "            'rest' skips internal/${g} on the promise those legs run" >&2
      echo "            it, so a missing one runs nowhere and nothing goes red." >&2
      exit 1
    fi
  done
}

run_rest() {
  # One leg, so index 0 is the only one that means anything. A matrix that
  # grew `{ group: rest, index: 1 }` would otherwise run all 44 packages a
  # second time, silently, and only show up as a slower gate.
  assert_index_is_a_shard 1 "the rest shard"
  assert_matrix_is_complete
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

# assert_index_is_a_shard <count> <what> — INDEX must be one of the exact
# strings 0..count-1.
#
# A STRING comparison against the admitted set, never `[ "$INDEX" -lt … ]`.
# INDEX is caller-supplied, and `[` with `-lt` is arithmetic: it errors on
# anything non-numeric, and `set -e` does not fire inside an `if` condition,
# so the guard silently evaluated FALSE and execution carried on into the
# partition. Measured before fixing — `test-shard.sh manifest abc` printed
# two lines of raw `[: abc: integer expression expected` and then failed
# with "shard abc/4 ... is empty (more shards than tests?)", which is a
# confident wrong diagnosis (CodeRabbit on #943). It did fail CLOSED, which
# is the right direction, but naming the symptom while hiding the cause is
# what this file's own docblocks warn about twice already.
#
# The set comparison also disposes of three things a numeric check has to
# handle separately: `007` (bash `test` reads a leading zero as OCTAL while
# awk reads it as decimal, so the bounds check and the partition would
# disagree), a 21-digit value (which bash rejects as not an integer at all),
# and a negative.
assert_index_is_a_shard() {
  local count="$1" what="$2" i
  for ((i = 0; i < count; i++)); do
    [ "$INDEX" = "$i" ] && return 0
  done
  echo "test-shard: index '$INDEX' is not one of $(seq 0 $((count - 1)) | paste -sd, -) for $what" >&2
  exit 1
}

run_sharded() {
  local pkg="$1" shards="$2" all_names part total mine rebuilt i
  assert_index_is_a_shard "$shards" "$pkg"
  all_names="$(names "$pkg")"

  # Floor: a silently-empty list would make this shard — and every other —
  # pass without running anything.
  total="$(printf '%s\n' "$all_names" | grep -c . || true)"
  if [ "${total:-0}" -lt 1 ]; then
    echo "test-shard: no test names listed for $pkg" >&2
    exit 1
  fi

  # Disjoint + covering, as SET EQUALITY rather than as a count. Counting
  # instead would be the weaker check that looks like this one: a sum that
  # matches is also what you get when one name lands in two shards and
  # another lands in none, which is exactly the bug worth catching.
  rebuilt="$(for ((i = 0; i < shards; i++)); do
    partition "$all_names" "$i" "$shards"
  done | sort)"
  if [ "$rebuilt" != "$(printf '%s\n' "$all_names" | sort)" ]; then
    echo "test-shard: the $shards partitions of $pkg are not a partition of its $total names" >&2
    exit 1
  fi

  part="$(partition "$all_names" "$INDEX" "$shards")"
  mine="$(printf '%s\n' "$part" | grep -c . || true)"
  if [ "${mine:-0}" -lt 1 ]; then
    echo "test-shard: shard $INDEX/$shards of $pkg is empty (more shards than tests?)" >&2
    exit 1
  fi

  echo "test-shard: $pkg shard $INDEX/$shards — $mine of $total names"
  go test "$pkg" "${RACE_FLAGS[@]}" -run "^($(printf '%s\n' "$part" | paste -sd'|' -))\$"
}

if [ "$GROUP" = rest ]; then
  run_rest
  exit 0
fi
if shards="$(sharded_count "$GROUP")"; then
  run_sharded "./internal/$GROUP/" "$shards"
  exit 0
fi
echo "test-shard: unknown group '$GROUP' (want rest, or one of: ${SHARDED[*]%%:*})" >&2
exit 1
