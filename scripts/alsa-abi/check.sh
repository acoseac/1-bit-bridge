#!/usr/bin/env bash
# check.sh holds internal/alsapcm's Go definitions of the kernel's ALSA PCM
# interface to the kernel's own header, on the architecture it runs on:
#
#   1. fetch include/uapi/sound/asound.h at the kernel tag
#      internal/alsapcm/uapi.go pins (const uapiKernel), or take the copy
#      named by ALSA_UAPI_HEADER;
#   2. compile probe.c against it with this machine's C compiler and run
#      it, which prints every size, offset, bit, constant and ioctl number
#      the backend relies on, as the compiler made them;
#   3. run TestUAPIMatchesTheKernelHeader, which compares each with the Go
#      definitions compiled for this same architecture, and fail unless it
#      ran and passed.
#
# CI runs it on linux/amd64 and on linux/arm64 (the alsa-abi job in
# .github/workflows/gate.yml). It needs Linux, a C compiler with the
# distribution's kernel headers (linux-libc-dev, for linux/types.h and
# asm/), curl and Go. Nothing it fetches or builds is kept: the header is
# not vendored, and nothing here is part of the bridge.
set -euo pipefail

work=$(unset GITHUB_TOKEN; mktemp -d)
trap 'rm -rf "$work"' EXIT
# In CI the workflow's token authenticates the header's download (below),
# which the raw host gives a higher rate limit. awk, the one program here
# that sees it, writes it from the environment straight into a curl config
# file in $work, which only this user can read, and it leaves the
# environment at once, on failure too: nothing else this script runs
# inherits it, no command line carries it, and no shell expansion shows it
# in a trace (bash -x).
auth="$work/curl-auth"
(umask 077 && awk 'BEGIN { t = ENVIRON["GITHUB_TOKEN"]; if (t != "") printf "header = \"Authorization: Bearer %s\"\n", t }' >"$auth") ||
	{ unset GITHUB_TOKEN; echo "check.sh: could not write the download's curl config" >&2; exit 1; }
unset GITHUB_TOKEN

repo=$(cd "$(dirname "$0")/../.." && pwd)
tag=$(sed -n 's/^const uapiKernel = "\(v[0-9][0-9.]*\)"$/\1/p' "$repo/internal/alsapcm/uapi.go")
if [ -z "$tag" ]; then
	echo "check.sh: no 'const uapiKernel = \"vX.Y\"' line in internal/alsapcm/uapi.go" >&2
	exit 1
fi

mkdir -p "$work/include/sound"
header="$work/include/sound/asound.h"

if [ -n "${ALSA_UAPI_HEADER:-}" ]; then
	cp "$ALSA_UAPI_HEADER" "$header"
	echo "asound.h: $ALSA_UAPI_HEADER (ALSA_UAPI_HEADER), compared as the header of $tag"
else
	url="https://raw.githubusercontent.com/torvalds/linux/$tag/include/uapi/sound/asound.h"
	# GitHub's raw host rate-limits unauthenticated requests, and a shared CI
	# runner can find its limit spent: four 429s in 13 seconds failed the
	# job on 2026-10-21. curl retries a 429 as transient, honouring the
	# server's Retry-After, and otherwise waits 1 s and doubles the wait, and
	# it retries an attempt that stalls past its own bound (15 s to connect,
	# 60 s in all). No retry starts after 5 minutes, so the download gives up
	# within 6, well inside the job's 15. A 404 or another failure is not
	# retried.
	fetch=(curl --proto '=https' --tlsv1.2 -fsSL --connect-timeout 15 --max-time 60 --retry 8 --retry-max-time 300 -o "$header")
	if [ -s "$auth" ]; then
		"${fetch[@]}" --config "$auth" "$url"
	else
		"${fetch[@]}" "$url"
	fi
	echo "asound.h: $url"
fi
# The blob id is the file's identity in the kernel's own git history:
# `git ls-tree <tag> include/uapi/sound/asound.h` in any kernel clone shows
# the same id for the same bytes.
echo "  git blob $(git hash-object "$header"), sha256 $(sha256sum "$header" | cut -d' ' -f1), $(wc -l <"$header") lines"
echo "compiler: $(cc --version | head -1), for $(uname -m)"

# The header in the kernel's source tree still carries the annotations
# `make headers_install` strips: __user (a user-space pointer) and __force
# (a sparse cast), which mean nothing to a C compiler, and __packed, which it
# rewrites as __attribute__((packed)) (without that, the compiler reads
# `} __packed;` as a declaration and the UMP structures fail to compile).
cc -std=gnu11 -Wall -Werror -D__user= -D__force= '-D__packed=__attribute__((packed))' -DUAPI_KERNEL="\"$tag\"" \
	-I "$work/include" -o "$work/probe" "$repo/scripts/alsa-abi/probe.c"
"$work/probe" >"$work/probe.txt"
echo "the probe printed $(wc -l <"$work/probe.txt") facts:"
sed 's/^/  /' "$work/probe.txt"

cd "$repo"
go test ./internal/alsapcm -run '^TestUAPIMatchesTheKernelHeader$' -count=1 -v \
	-args -abi-probe="$work/probe.txt" | tee "$work/go-test.txt"
# A skipped test also exits 0, so the pass line is what proves the
# comparison ran.
if ! grep -q -- '--- PASS: TestUAPIMatchesTheKernelHeader' "$work/go-test.txt"; then
	echo "check.sh: TestUAPIMatchesTheKernelHeader did not run and pass" >&2
	exit 1
fi
