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

repo=$(cd "$(dirname "$0")/../.." && pwd)
tag=$(sed -n 's/^const uapiKernel = "\(v[0-9][0-9.]*\)"$/\1/p' "$repo/internal/alsapcm/uapi.go")
if [ -z "$tag" ]; then
	echo "check.sh: no 'const uapiKernel = \"vX.Y\"' line in internal/alsapcm/uapi.go" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/include/sound"
header="$work/include/sound/asound.h"

if [ -n "${ALSA_UAPI_HEADER:-}" ]; then
	cp "$ALSA_UAPI_HEADER" "$header"
	echo "asound.h: $ALSA_UAPI_HEADER (ALSA_UAPI_HEADER), compared as the header of $tag"
else
	url="https://raw.githubusercontent.com/torvalds/linux/$tag/include/uapi/sound/asound.h"
	curl -fsSL --retry 3 -o "$header" "$url"
	echo "asound.h: $url"
fi
# The blob id is the file's identity in the kernel's own git history:
# `git ls-tree <tag> include/uapi/sound/asound.h` in any kernel clone shows
# the same id for the same bytes.
echo "  git blob $(git hash-object "$header"), sha256 $(sha256sum "$header" | cut -d' ' -f1), $(wc -l <"$header") lines"
echo "compiler: $(cc --version | head -1), for $(uname -m)"

# The header in the kernel's source tree still carries the annotations
# `make headers_install` strips: __user (a user-space pointer) and __force
# (a sparse cast). Both mean nothing to a C compiler.
cc -std=gnu11 -Wall -Werror -D__user= -D__force= -DUAPI_KERNEL="\"$tag\"" \
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
