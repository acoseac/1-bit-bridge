#!/usr/bin/env bash
# kernel-check.sh holds the DAC profile database's kernel-seeded claims
# (internal/player/dacprofile/profiles.json) to the Linux kernel's own USB
# audio DSD quirks, at the kernel tag scripts/alsa-abi pins:
#
#   1. fetch sound/usb/quirks.c at the tag internal/alsapcm/uapi.go pins
#      (const uapiKernel), and copy what it fetched to the path
#      DACPROFILE_KERNEL_QUIRKS_SAVE names, when it names one; or take the
#      copy named by DACPROFILE_KERNEL_QUIRKS;
#   2. run TestTheKernelSeedIsThePinnedTags with it, which reads the file's
#      DSD format quirk and flags table and compares each device-specific
#      fact with the database's kernel-seeded profiles, both ways, and fail
#      unless that test ran and passed.
#
# CI runs it on linux/amd64 (the dac-profiles job in
# .github/workflows/gate.yml). It needs curl and Go. The file is kernel
# source, read in a temporary directory and deleted, and the test prints the
# facts it read (USB IDs, DSD formats and how the driver finds the
# altsetting), never the file's text. What it fetches is kept only in CI's
# Actions cache, keyed by the tag; never in the repository, a build, a
# release archive, a log or the database: the job saves a fetched copy there
# through DACPROFILE_KERNEL_QUIRKS_SAVE, and a later run of the job reads
# that copy, unmodified, through DACPROFILE_KERNEL_QUIRKS.
set -euo pipefail

work=$(unset GITHUB_TOKEN; mktemp -d)
trap 'rm -rf "$work"' EXIT
# In CI the workflow's token authenticates the download, as alsa-abi's does:
# awk writes it from the environment into a curl config file only this user
# can read, and it leaves the environment at once, on failure too.
auth="$work/curl-auth"
(umask 077 && awk 'BEGIN { t = ENVIRON["GITHUB_TOKEN"]; if (t != "") printf "header = \"Authorization: Bearer %s\"\n", t }' >"$auth") ||
	{ unset GITHUB_TOKEN; echo "kernel-check.sh: could not write the download's curl config" >&2; exit 1; }
unset GITHUB_TOKEN

repo=$(cd "$(dirname "$0")/../.." && pwd)
tag=$(sed -n 's/^const uapiKernel = "\(v[0-9][0-9.]*\)"$/\1/p' "$repo/internal/alsapcm/uapi.go")
if [ -z "$tag" ]; then
	echo "kernel-check.sh: no 'const uapiKernel = \"vX.Y\"' line in internal/alsapcm/uapi.go" >&2
	exit 1
fi

quirks="$work/quirks.c"
if [ -n "${DACPROFILE_KERNEL_QUIRKS:-}" ]; then
	# A copy named here is read and never saved:
	# DACPROFILE_KERNEL_QUIRKS_SAVE is read only after a fetch.
	cp "$DACPROFILE_KERNEL_QUIRKS" "$quirks"
	echo "quirks.c: $DACPROFILE_KERNEL_QUIRKS (DACPROFILE_KERNEL_QUIRKS), compared as the file of $tag"
else
	url="https://raw.githubusercontent.com/torvalds/linux/$tag/sound/usb/quirks.c"
	# The retries are alsa-abi's: a 429 is retried as transient, honouring
	# Retry-After, an attempt that stalls past its bound is retried, and no
	# retry starts after 5 minutes; a 404 or another failure is not retried.
	# So is the reading of a failure: curl prints the last attempt's HTTP
	# status alone on stdout (-w), the file going to -o, and a 429 that
	# outlasts the retries is named as the host's limit, which asks for a
	# rerun and not a fix. The status is read with the assignment guarded:
	# under set -e a bare failure would end the script before it is read.
	# As there, only an exit of 0 with a final 200 is a download.
	fetch=(curl --proto '=https' --tlsv1.2 -fsSL --connect-timeout 15 --max-time 60 --retry 8 --retry-max-time 300 -o "$quirks" -w '%{http_code}')
	rc=0
	if [ -s "$auth" ]; then
		code=$("${fetch[@]}" --config "$auth" "$url") || rc=$?
	else
		code=$("${fetch[@]}" "$url") || rc=$?
	fi
	if [ "$rc" -ne 0 ] || [ "$code" != 200 ]; then
		rm -f "$quirks"
		if [ "$code" = 429 ]; then
			echo "kernel-check.sh: the upstream host rate-limited the download of $url (HTTP 429) after curl's retries; this is not a definitions difference: rerun the job" >&2
		elif [ -z "$code" ] || [ "$code" = 000 ]; then
			echo "kernel-check.sh: the download of $url failed with curl exit status $rc and no HTTP status" >&2
		else
			echo "kernel-check.sh: the download of $url failed with curl exit status $rc and HTTP status $code" >&2
		fi
		exit 1
	fi
	echo "quirks.c: $url"
	if [ -n "${DACPROFILE_KERNEL_QUIRKS_SAVE:-}" ]; then
		# The copy is made right after the fetch, before the comparison, so
		# that the save can be tried offline. CI keeps it only when the job
		# succeeds (actions/cache saves at the job's end, post-if: success()),
		# and a run by hand sets no save variable. As alsa-abi's does, the
		# save refuses a path that is a link, dangling or not, or that is
		# there and is no regular file, before anything is written, and writes
		# the copy beside its path and renames it into place, each step in the
		# one condition: whichever fails prints the line below, the temporary
		# file it made is removed and a file already at the path is kept; a
		# directory a successful mkdir -p made may remain.
		save_dir=$(dirname "$DACPROFILE_KERNEL_QUIRKS_SAVE")
		save_tmp=""
		if [ -L "$DACPROFILE_KERNEL_QUIRKS_SAVE" ] ||
			{ [ -e "$DACPROFILE_KERNEL_QUIRKS_SAVE" ] && [ ! -f "$DACPROFILE_KERNEL_QUIRKS_SAVE" ]; } ||
			! mkdir -p "$save_dir" || ! save_tmp=$(mktemp "$save_dir/.quirks.c.XXXXXX") ||
			! cp "$quirks" "$save_tmp" || ! mv -f "$save_tmp" "$DACPROFILE_KERNEL_QUIRKS_SAVE"; then
			[ -z "$save_tmp" ] || rm -f "$save_tmp" || true
			echo "kernel-check.sh: could not save the file to $DACPROFILE_KERNEL_QUIRKS_SAVE" >&2
			exit 1
		fi
		echo "quirks.c: saved to $DACPROFILE_KERNEL_QUIRKS_SAVE (DACPROFILE_KERNEL_QUIRKS_SAVE)"
	fi
fi
# The blob id is the file's identity in the kernel's own git history:
# `git ls-tree <tag> sound/usb/quirks.c` in any kernel clone shows the same
# id for the same bytes.
echo "  git blob $(git hash-object "$quirks"), sha256 $(sha256sum "$quirks" | cut -d' ' -f1), $(wc -l <"$quirks") lines"

cd "$repo"
go test -tags ts_omit_webclient,ts_omit_debugeventbus ./internal/player/dacprofile -run '^TestTheKernelSeedIsThePinnedTags$' -count=1 -v \
	-args -kernel-quirks="$quirks" | tee "$work/go-test.txt"
# A skipped test also exits 0, so the pass line is what proves the
# comparison ran.
if ! grep -q -- '--- PASS: TestTheKernelSeedIsThePinnedTags' "$work/go-test.txt"; then
	echo "kernel-check.sh: TestTheKernelSeedIsThePinnedTags did not run and pass" >&2
	exit 1
fi
