#!/bin/sh
# stamp-install.sh VERSION [SOURCE [DESTINATION]]
#
# Writes the installer a release attaches: SOURCE (scripts/install.sh) with
# its one unstamped line, STAMPED_VERSION='', made STAMPED_VERSION='VERSION',
# at DESTINATION (install.sh, at the repository root). goreleaser's
# before-hook runs it with the release's version, so a released installer
# downloads only from its own release's tag; the file is gitignored, as
# release-meta.json is, and release.extra_files attaches it.
#
# It writes nothing unless VERSION is a release version (X.Y.Z, optionally
# -suffix) and SOURCE has exactly one unstamped line, and it fails unless the
# file it wrote differs from SOURCE in exactly that line.
set -eu

version=${1:-}
source_file=${2:-scripts/install.sh}
destination=${3:-install.sh}

case $version in
'' | *[!0-9A-Za-z.-]*)
	printf 'stamp-install.sh: %s is not a release version\n' "$version" >&2
	exit 2
	;;
esac
if ! printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$'; then
	printf 'stamp-install.sh: %s is not a release version\n' "$version" >&2
	exit 2
fi

unstamped="^[[:space:]]*STAMPED_VERSION=''\$"
count=$(grep -c "$unstamped" "$source_file") || count=0
if [ "$count" != 1 ]; then
	printf "stamp-install.sh: %s must have one unstamped STAMPED_VERSION='' line, and it has %s\n" "$source_file" "$count" >&2
	exit 1
fi

staged=$destination.stamping.$$
trap 'rm -f "$staged"' EXIT
sed "s/^\([[:space:]]*\)STAMPED_VERSION=''\$/\1STAMPED_VERSION='$version'/" "$source_file" >"$staged"

# Exactly the one line changed, and it is the stamp.
differing=$(awk 'NR == FNR { line[FNR] = $0; lines = FNR; next }
	{ if (line[FNR] != $0) changed++; seen = FNR }
	END { if (seen != lines) changed += 1000; print changed + 0 }' "$source_file" "$staged")
stamped=$(grep -c "^[[:space:]]*STAMPED_VERSION='$version'\$" "$staged") || stamped=0
if [ "$differing" != 1 ] || [ "$stamped" != 1 ]; then
	printf 'stamp-install.sh: stamping %s changed %s lines, not the one\n' "$source_file" "$differing" >&2
	exit 1
fi
mv -f "$staged" "$destination"
trap - EXIT
printf 'stamp-install.sh: %s is %s for release %s\n' "$destination" "$source_file" "$version"
