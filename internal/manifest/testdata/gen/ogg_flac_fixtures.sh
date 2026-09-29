#!/bin/sh
# Generate the Ogg FLAC fixtures in ../ogg/ (ExtractorVersion 18, backlog B102).
#
# Two real muxers, because what dhowden could not read was what they write:
#
#   ffmpeg_pages.oga     ffmpeg's Ogg FLAC muxer: the mapping packet declaring one
#                        header packet, a VORBIS_COMMENT block flagged last, then the
#                        audio in 20 ms pages, so that four pages of audio follow the
#                        headers (the test that no audio page is read needs them).
#   libflac_picture.oga  the reference encoder (flac --ogg): a VORBIS_COMMENT block with
#                        two ARTIST values, a PICTURE block holding a 16x16 JPEG and a
#                        PADDING block flagged last, one header packet to a page.
#
# Requires ffmpeg and flac (made with ffmpeg 9.0.2 and flac 1.5.0). Regenerating
# changes the bytes, since both encoders stamp their version into the vendor string.
#
# Usage (from this directory): sh ogg_flac_fixtures.sh
set -eu

out=../ogg
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$out"

ffmpeg -hide_banner -loglevel error -y \
	-f lavfi -i "sine=frequency=440:duration=0.5:sample_rate=8000" -ac 1 \
	-c:a flac -page_duration 20000 -fflags +bitexact -flags:a +bitexact \
	-metadata title="Ogg Title" -metadata artist="Ogg Artist" \
	-metadata album="Ogg Album" -metadata album_artist="Ogg Album Artist" \
	-metadata date=2019 -metadata track=7 -metadata disc=2 -metadata genre=Ambient \
	-metadata MUSICBRAINZ_TRACKID=0a1b2c3d-0000-4000-8000-000000000001 \
	"$out/ffmpeg_pages.oga"

ffmpeg -hide_banner -loglevel error -y \
	-f lavfi -i "sine=frequency=440:duration=0.2:sample_rate=8000" -ac 1 \
	-c:a pcm_s16le -map_metadata -1 -fflags +bitexact "$tmp/tone.wav"
ffmpeg -hide_banner -loglevel error -y \
	-f lavfi -i color=c=red:s=16x16 -frames:v 1 \
	-map_metadata -1 -fflags +bitexact -flags +bitexact "$tmp/cover.jpg"
flac --ogg -s -f -P 64 \
	-T "TITLE=Libflac Title" -T "ARTIST=First Artist" -T "ARTIST=Second Artist" \
	-T "ALBUM=Libflac Album" -T "DATE=2021-05-04" -T "TRACKNUMBER=3" -T "COMPILATION=1" \
	--picture="$tmp/cover.jpg" -o "$out/libflac_picture.oga" "$tmp/tone.wav"
