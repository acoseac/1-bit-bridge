#!/usr/bin/env bash
# check.sh holds internal/alsapcm's Go definitions of the kernel's ALSA PCM
# interface to the kernel's own header, on the architecture it runs on:
#
#   1. fetch include/uapi/sound/asound.h at the kernel tag
#      internal/alsapcm/uapi.go pins (const uapiKernel), and copy what it
#      fetched to the path ALSA_UAPI_HEADER_SAVE names, when it names one;
#      or take the copy named by ALSA_UAPI_HEADER;
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
# asm/), curl and Go. Nothing it builds is kept, and nothing here is part of
# the bridge. The header it fetches is kept only in CI's Actions cache,
# keyed by the tag; never in the repository, a build, a release archive, a
# log or the database: the job saves a fetched copy there through
# ALSA_UAPI_HEADER_SAVE, and a later run of the job compares that copy,
# unmodified, through ALSA_UAPI_HEADER.
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
	# A copy named here is compared and never saved: ALSA_UAPI_HEADER_SAVE is
	# read only after a fetch.
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
	#
	# curl prints the last attempt's HTTP status alone on stdout (-w), the
	# header going to its file (-o), so a failed download says which failure
	# it was. A 429 that outlasts the retries is the host's limit, which the
	# token raises and does not lift: on 2026-11-27 each of main's three
	# downloads of a kernel file got one on every retry for about four
	# minutes, the job's token sent. Its line says so, so that a rerun that
	# passes is not taken for a fix. The status is read with the assignment
	# guarded: under set -e a bare failure would end the script before it is
	# read. Only an exit of 0 with a final 200 is a download: -f fails a
	# status of 400 or more alone, and curl exits 0 after a 204 or a 206,
	# which hold no whole file.
	fetch=(curl --proto '=https' --tlsv1.2 -fsSL --connect-timeout 15 --max-time 60 --retry 8 --retry-max-time 300 -o "$header" -w '%{http_code}')
	rc=0
	if [ -s "$auth" ]; then
		code=$("${fetch[@]}" --config "$auth" "$url") || rc=$?
	else
		code=$("${fetch[@]}" "$url") || rc=$?
	fi
	if [ "$rc" -ne 0 ] || [ "$code" != 200 ]; then
		rm -f "$header"
		if [ "$code" = 429 ]; then
			echo "check.sh: the upstream host rate-limited the download of $url (HTTP 429) after curl's retries; this is not a definitions difference: rerun the job" >&2
		elif [ -z "$code" ] || [ "$code" = 000 ]; then
			echo "check.sh: the download of $url failed with curl exit status $rc and no HTTP status" >&2
		else
			echo "check.sh: the download of $url failed with curl exit status $rc and HTTP status $code" >&2
		fi
		exit 1
	fi
	echo "asound.h: $url"
	if [ -n "${ALSA_UAPI_HEADER_SAVE:-}" ]; then
		# The copy is made right after the fetch, before the comparison, so
		# that the save can be tried offline. CI keeps it only when the job
		# succeeds (actions/cache saves at the job's end, post-if: success()),
		# and a run by hand sets no save variable. A path that is a link,
		# dangling or not, or that is there and is no regular file, is
		# refused before anything is written: mv -f would move the copy into
		# a directory with a status of 0, a save that did not happen, and
		# would replace a link or a fifo. The copy is written beside its
		# path and renamed into place, so the path never holds part of one.
		# Each step is in the one condition, so whichever fails prints the
		# line below, the temporary file it made is removed and a file
		# already at the path is kept; a directory a successful mkdir -p
		# made may remain.
		save_dir=$(dirname "$ALSA_UAPI_HEADER_SAVE")
		save_tmp=""
		if [ -L "$ALSA_UAPI_HEADER_SAVE" ] ||
			{ [ -e "$ALSA_UAPI_HEADER_SAVE" ] && [ ! -f "$ALSA_UAPI_HEADER_SAVE" ]; } ||
			! mkdir -p "$save_dir" || ! save_tmp=$(mktemp "$save_dir/.asound.h.XXXXXX") ||
			! cp "$header" "$save_tmp" || ! mv -f "$save_tmp" "$ALSA_UAPI_HEADER_SAVE"; then
			[ -z "$save_tmp" ] || rm -f "$save_tmp" || true
			echo "check.sh: could not save the header to $ALSA_UAPI_HEADER_SAVE" >&2
			exit 1
		fi
		echo "asound.h: saved to $ALSA_UAPI_HEADER_SAVE (ALSA_UAPI_HEADER_SAVE)"
	fi
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
go test -tags ts_omit_webclient,ts_omit_debugeventbus ./internal/alsapcm -run '^TestUAPIMatchesTheKernelHeader$' -count=1 -v \
	-args -abi-probe="$work/probe.txt" | tee "$work/go-test.txt"
# A skipped test also exits 0, so the pass line is what proves the
# comparison ran.
if ! grep -q -- '--- PASS: TestUAPIMatchesTheKernelHeader' "$work/go-test.txt"; then
	echo "check.sh: TestUAPIMatchesTheKernelHeader did not run and pass" >&2
	exit 1
fi
