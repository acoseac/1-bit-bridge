#!/usr/bin/env bash
# kernel-check.sh holds the DAC profile database's kernel-seeded claims
# (internal/player/dacprofile/profiles.json) to the Linux kernel's own USB
# audio DSD quirks, at the kernel tag scripts/alsa-abi pins:
#
#   1. fetch sound/usb/quirks.c at the tag internal/alsapcm/uapi.go pins
#      (const uapiKernel), or take the copy named by DACPROFILE_KERNEL_QUIRKS;
#   2. run TestTheKernelSeedIsThePinnedTags with it, which reads the file's
#      DSD format quirk and flags table and compares each device-specific
#      fact with the database's kernel-seeded profiles, both ways, and fail
#      unless that test ran and passed.
#
# CI runs it on linux/amd64 (the dac-profiles job in
# .github/workflows/gate.yml). It needs curl and Go. Nothing it fetches is
# kept: the file is kernel source, read in a temporary directory and
# deleted, and the test prints the facts it read (USB IDs, DSD formats and
# how the driver finds the altsetting), never the file's text.
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
	cp "$DACPROFILE_KERNEL_QUIRKS" "$quirks"
	echo "quirks.c: $DACPROFILE_KERNEL_QUIRKS (DACPROFILE_KERNEL_QUIRKS), compared as the file of $tag"
else
	url="https://raw.githubusercontent.com/torvalds/linux/$tag/sound/usb/quirks.c"
	# The retries are alsa-abi's: a 429 is retried as transient, honouring
	# Retry-After, an attempt that stalls past its bound is retried, and no
	# retry starts after 5 minutes; a 404 or another failure is not retried.
	fetch=(curl --proto '=https' --tlsv1.2 -fsSL --connect-timeout 15 --max-time 60 --retry 8 --retry-max-time 300 -o "$quirks")
	if [ -s "$auth" ]; then
		"${fetch[@]}" --config "$auth" "$url"
	else
		"${fetch[@]}" "$url"
	fi
	echo "quirks.c: $url"
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
