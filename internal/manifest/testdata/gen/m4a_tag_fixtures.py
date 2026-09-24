#!/usr/bin/env python3
"""Generate the M4A tag fixtures in ../m4a/ (ExtractorVersion 15).

The same four files, byte for byte, ship embedded in the iOS app's
`AVTagFixtures.swift`: one set of real files pins both indexing paths, the
bridge's dhowden/tag reader and the phone's AVFoundation enrich. Regenerating
here changes the bytes (afconvert stamps the current time into mvhd / tkhd /
mdhd), so a regeneration must be copied to BOTH repos or neither.

WHY REAL FILES. The defects these pin — `gnre` never read, the freeform
locale kept on every `----` value — live in the gap between what a tagger
writes and what a reader expects, and a hand-built box tree only ever
contains what its author already knew to put there.

Requires macOS (afconvert) and mutagen 1.48. mutagen READS `gnre` but refuses
to WRITE it, so its renderer is patched below to emit iTunes' form: an
implicit (class 0) `data` atom holding a big-endian uint16 of the ID3v1
index PLUS ONE. Verify a regeneration at the byte level, not through
mutagen, which reports a `gnre` it read back as a `©gen` text value: the
`gnre` data payload must end `0012` (Rock) / `000e` (Pop).

Usage (from this directory):
    python3 m4a_tag_fixtures.py && mv itunes_alac.m4a picard_alac.m4a \
        itunes_aac_pop.m4a both_genres_alac.m4a ../m4a/
"""
import shutil
import struct
import subprocess
import wave

from mutagen.mp4 import MP4, MP4FreeForm, MP4Tags


def _render_gnre(self, key, value):
    return self._MP4Tags__render_data(key, 0, 0, [struct.pack('>H', v) for v in value])


MP4Tags._MP4Tags__atoms[b'gnre'] = (MP4Tags._MP4Tags__atoms[b'gnre'][0], _render_gnre)

# 0.05 s of stereo 44.1 kHz silence: the smallest file every reader accepts.
SAMPLE_RATE = 44100
with wave.open('base.wav', 'wb') as w:
    w.setnchannels(2)
    w.setsampwidth(2)
    w.setframerate(SAMPLE_RATE)
    w.writeframes(b'\x00\x00\x00\x00' * int(SAMPLE_RATE * 0.05))
subprocess.run(['afconvert', '-f', 'm4af', '-d', 'alac', 'base.wav', 'alac.m4a'], check=True)
subprocess.run(['afconvert', '-f', 'm4af', '-d', 'aac', '-b', '96000', 'base.wav', 'aac.m4a'], check=True)


def tag(src, dst, tags):
    shutil.copy(src, dst)
    f = MP4(dst)
    if f.tags is None:
        f.add_tags()
    for k, v in tags.items():
        if k.startswith('----:'):
            f.tags[k] = [MP4FreeForm(v.encode())]
        else:
            f.tags[k] = v if isinstance(v, list) else [v]
    f.save(padding=lambda info: 0)


common = {'\xa9nam': 'Bohemian Rhapsody', '\xa9ART': 'Queen', 'aART': 'Queen',
          '\xa9alb': 'A Night at the Opera'}
# iTunes / Music: a STANDARD genre as `gnre` alone — the reported case.
tag('alac.m4a', 'itunes_alac.m4a', {
    **common, 'gnre': 18, '\xa9day': '1975-11-21T08:00:00Z',
    'trkn': [(11, 12)], 'disk': [(1, 1)], '\xa9wrt': 'Freddie Mercury',
    '\xa9lyr': 'Is this the real life?\nIs this just fantasy?'})
# Picard: `©gen` text plus MusicBrainz / ORIGINALDATE freeforms.
tag('alac.m4a', 'picard_alac.m4a', {
    **common, '\xa9gen': 'Rock', '\xa9day': '2011-03-14', 'trkn': [(11, 12)], 'disk': [(1, 1)],
    '----:com.apple.iTunes:MusicBrainz Album Artist Id': '0383dadf-2a4e-4d10-a46a-e9e041da8eb3',
    '----:com.apple.iTunes:MusicBrainz Artist Id': '0383dadf-2a4e-4d10-a46a-e9e041da8eb3',
    '----:com.apple.iTunes:MusicBrainz Album Id': '4b8a2d4c-1c7a-4a9e-9d6f-3c2b1a0f9e8d',
    '----:com.apple.iTunes:ORIGINALDATE': '1975-11-21'})
# AAC with the other genre the report named.
tag('aac.m4a', 'itunes_aac_pop.m4a', {
    '\xa9nam': 'Hello', '\xa9ART': 'Lionel Richie', '\xa9alb': "Can't Slow Down",
    'gnre': 14, '\xa9day': '1983'})
# Both genre atoms: the text one must win.
tag('alac.m4a', 'both_genres_alac.m4a', {**common, 'gnre': 18, '\xa9gen': 'Classic Rock'})
