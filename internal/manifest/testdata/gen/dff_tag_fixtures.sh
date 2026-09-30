#!/bin/sh
# Generate the DSDIFF tag fixtures in ../dff/ (ExtractorVersion 20, backlog B140).
#
# Two real writers, because the defect lived in the gap between what they write
# and what the bridge read:
#
#   TagLib 2 writes a DIIN (Edited Master Information) chunk as the DSDIFF 1.5
#   specification lays it out: DITI (title) and DIAR (artist), each a 4-byte
#   big-endian count and the text, in ISO-8859-1 (taglib_dff_diin.cpp). ffmpeg,
#   TagLib and MediaInfo all read that layout; the bridge read a 1-byte length
#   no writer produces until v20, so it read no DIIN a real file carries.
#   mutagen (the library Picard writes with) tags a DSDIFF file with an "ID3 "
#   chunk appended after the audio, which the bridge did not read at all.
#
#   taglib_diin.dff     TagLib's DIIN alone: a title with ISO-8859-1 letters
#                       (0xE9, 0xE0, 0xE8) and an artist.
#   picard_id3.dff      mutagen's ID3v2.4 tag in UTF-16, Picard's default, and no
#                       DIIN: title, artist, album, genre, track 3/12, date 2019.
#   diin_then_id3.dff   TagLib's DIIN, then mutagen's ID3 chunk holding a title and
#                       an album but no artist: the tag answers where it has a
#                       value and the DIIN where it has none.
#   id3_then_diin.dff   the same two chunks written in the other order (mutagen
#                       first, then TagLib, which appends its DIIN after the ID3
#                       chunk), so the answer cannot depend on chunk order.
#
# The base file under the tags is written here: FRM8/DSD with FVER 1.5, PROP/SND
# (FS 2822400, CHNL two channels, CMPR "DSD "), and a DSD chunk of 256 bytes of
# the DSD silence pattern 0x69. Its duration is below the plausible floor, so
# none is stamped: these fixtures are about tags.
#
# Requires python3 with mutagen 1.47, TagLib 2 (headers and pkg-config) and g++.
# Made in a Debian 13 container: apt-get install python3-mutagen libtag-dev
# pkg-config g++ (taglib 2.0.2, mutagen 1.47.0).
#
# Usage (from this directory): sh dff_tag_fixtures.sh
set -eu

out=../dff
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$out"

python3 - "$tmp/base.dff" <<'EOF'
import struct
import sys


def chunk(cid, body):
    out = cid + struct.pack('>Q', len(body)) + body
    return out + (b'\x00' if len(body) & 1 else b'')


prop = b'SND ' + chunk(b'FS  ', struct.pack('>I', 2822400)) \
    + chunk(b'CHNL', struct.pack('>H', 2) + b'SLFTSRGT') \
    + chunk(b'CMPR', b'DSD ' + bytes([14]) + b'not compressed' + b'\x00')
form = b'DSD ' + chunk(b'FVER', struct.pack('>I', 0x01050000)) + chunk(b'PROP', prop) \
    + chunk(b'DSD ', b'\x69' * 256)
with open(sys.argv[1], 'wb') as f:
    f.write(b'FRM8' + struct.pack('>Q', len(form)) + form)
EOF

g++ -std=c++17 -O1 -o "$tmp/taglib_dff_diin" taglib_dff_diin.cpp $(pkg-config --cflags --libs taglib)

# picard_id3 <file> <title> <artist or ""> <album> [genre track date]
picard_id3() {
	python3 - "$@" <<'EOF'
import sys

from mutagen.dsdiff import DSDIFF
from mutagen.id3 import TALB, TCON, TDRC, TIT2, TPE1, TRCK

path, title, artist, album = sys.argv[1:5]
extra = sys.argv[5:]
d = DSDIFF(path)
if d.tags is None:
    d.add_tags()
d.tags.add(TIT2(encoding=1, text=title))
if artist:
    d.tags.add(TPE1(encoding=1, text=artist))
d.tags.add(TALB(encoding=1, text=album))
if extra:
    genre, track, date = extra
    d.tags.add(TCON(encoding=1, text=genre))
    d.tags.add(TRCK(encoding=1, text=track))
    d.tags.add(TDRC(encoding=1, text=date))
d.save(v2_version=4, padding=lambda info: 0)
EOF
}

cp "$tmp/base.dff" "$out/taglib_diin.dff"
"$tmp/taglib_dff_diin" "$out/taglib_diin.dff" "Prélude à la nuit, première" "Ensemble DIIN"

cp "$tmp/base.dff" "$out/picard_id3.dff"
picard_id3 "$out/picard_id3.dff" "Picard Title" "Picard Artist" "Picard Album" "Picard Genre" "3/12" "2019"

cp "$tmp/base.dff" "$out/diin_then_id3.dff"
"$tmp/taglib_dff_diin" "$out/diin_then_id3.dff" "DIIN Title" "DIIN Artist"
picard_id3 "$out/diin_then_id3.dff" "ID3 Title" "" "ID3 Album"

cp "$tmp/base.dff" "$out/id3_then_diin.dff"
picard_id3 "$out/id3_then_diin.dff" "ID3 Title" "" "ID3 Album"
"$tmp/taglib_dff_diin" "$out/id3_then_diin.dff" "DIIN Title" "DIIN Artist"
