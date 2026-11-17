#!/usr/bin/env bash
#
# One group of the native suite. The `cross-platform` job in
# .github/workflows/gate.yml runs the module's tests without -race on
# windows-latest and macos-latest, each platform as two matrix legs on runners
# of their own: `native-test.sh slow` and `native-test.sh rest`.
#
# WHY TWO GROUPS
#
# go test applies one -timeout to each package's test binary of an
# invocation, so packages that need a longer cap than the rest need an
# invocation of their own. On run 44097405776 (attempt 1, the Windows leg) a
# slow runner took every package a median 1.48x longer than main's run an
# hour before: cmd/bridge 1731.1 s, 96 % of its 30m; internal/backup met the
# 30m cap and failed; internal/manifest 1297.2 s; internal/admin 1078.7 s. On
# main the same leg's cmd/bridge had gone from 1026.9 to 1206.9 s and
# internal/backup from 987.2 to 1184.8 s in one day (2026-11-16 to 11-17). So
# those three run at -timeout 45m, and every other package keeps 30m.
# internal/manifest is among them for its waits derived from the binary's
# deadline: there a late slowdown fails as an assertion before the cap.
#
# The two groups run at the same time, each on a runner of its own: two go
# test invocations on one 4-vCPU runner would contend for its cores and slow
# each other, the cap problem again. The slow group runs its three packages
# at once (-p 3), so the leg is one wave of at most 45 minutes, which the
# job's timeout-minutes covers with its setup; at -p 1 the three would run
# one after another and the job would be cancelled before a 45-minute
# binary's timeout could print its goroutines. The rest takes go's default -p.
#
# THE GROUPS
#
# slow is the literal list below, each of which go list must list; rest is
# every package `go list` lists with the release tags but those three. An
# empty group is an error, never a pass: a leg that runs nothing is a leg
# that stopped testing. scripts/third-party-licenses'
# TestTheNativeLegsRunEveryPackageOnceAndTheSlowOnesAt45m holds the two
# groups, on each platform's own listing, to its literal three and to the
# whole module, and the workflow's matrix to its four legs.
#
# NATIVE_TEST_DRY_RUN=1 prints the go test command, one word a line, instead
# of running it.

set -euo pipefail

GROUP="${1:?usage: native-test.sh <slow|rest>}"

SLOW=(
  github.com/acoseac/1-bit-bridge/cmd/bridge
  github.com/acoseac/1-bit-bridge/internal/backup
  github.com/acoseac/1-bit-bridge/internal/manifest
)

# The release tags, written out as every go command in CI writes them.
listed="$(go list -tags ts_omit_webclient,ts_omit_debugeventbus ./...)"
listed="${listed//$'\r'/}"

is_slow() {
  local s
  for s in "${SLOW[@]}"; do
    if [ "$1" = "$s" ]; then
      return 0
    fi
  done
  return 1
}

is_listed() {
  local p
  while IFS= read -r p; do
    if [ "$p" = "$1" ]; then
      return 0
    fi
  done <<< "$listed"
  return 1
}

pkgs=()
case "$GROUP" in
  slow)
    for s in "${SLOW[@]}"; do
      if ! is_listed "$s"; then
        echo "native-test: $s is in the slow group, and go list does not list it" >&2
        exit 1
      fi
    done
    pkgs=("${SLOW[@]}")
    ;;
  rest)
    while IFS= read -r p; do
      if [ -n "$p" ] && ! is_slow "$p"; then
        pkgs+=("$p")
      fi
    done <<< "$listed"
    ;;
  *)
    echo "native-test: unknown group '$GROUP' (want slow or rest)" >&2
    exit 2
    ;;
esac

if [ "${#pkgs[@]}" -eq 0 ]; then
  echo "native-test: the $GROUP group lists no package" >&2
  exit 1
fi

cmd=(go test -tags ts_omit_webclient,ts_omit_debugeventbus)
if [ "$GROUP" = slow ]; then
  cmd+=(-p 3 -timeout 45m)
else
  cmd+=(-timeout 30m)
fi
cmd+=("${pkgs[@]}")

if [ -n "${NATIVE_TEST_DRY_RUN:-}" ]; then
  printf '%s\n' "${cmd[@]}"
  exit 0
fi
echo "native-test: ${cmd[*]}"
"${cmd[@]}"
