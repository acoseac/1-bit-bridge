#!/usr/bin/env python3
"""Generate the ID3v2 user-text fixtures in ../id3/ (ExtractorVersion 19, backlog B116).

Each file carries the tag MusicBrainz Picard writes, written the way Picard's
formats/id3.py `_save` writes it, through mutagen (the library Picard writes
with): the recording id as a UFID frame owned by http://musicbrainz.org (ASCII
bytes, no terminator), the release id and the release TRACK id, the release
group and the artists as TXXX frames named 'MusicBrainz Album Id', 'MusicBrainz
Release Track Id', 'MusicBrainz Release Group Id' and 'MusicBrainz Artist Id',
ReplayGain as TXXX frames named in upper case (Picard's __rtranslate_freetext_ci),
any other name (originalyear) as TXXX:<name>, and originaldate as TDOR (mutagen's
update_to_v23 turns it into TORY). Picard's defaults are ID3v2.4 and UTF-16; the
other two MP3s are its "write ID3v2.3" and UTF-8 options, and the DSF, AIFF and
WAV carry the default tag in the places Picard puts it (mutagen's DSF metadata
pointer, the AIFF "ID3 " chunk, the WAV "id3 " chunk). ffmpeg_v23.mp3 is a second
writer: ffmpeg's id3v2 muxer names a TXXX frame by the metadata key it was given,
in ISO-8859-1, and these keys spell the ReplayGain names in lower case, as writers
other than Picard do.

WHY REAL FILES. The defect these pin lives in the gap between what a tagger
writes and what a reader expects: every writer here ends each TXXX value with a
terminator, which dhowden keeps (`"<id>\x00"`), and a UTF-16 value after the
first keeps its byte order mark. A hand-built frame contains what its author
knew to put there.

Requires ffmpeg and mutagen 1.47 (on the dev Mac, /usr/bin/python3 has it).

Usage (from this directory):
    python3 id3_txxx_fixtures.py && mv picard_*.* ffmpeg_*.mp3 ../id3/
"""
import os
import shutil
import struct
import subprocess

from mutagen import id3
from mutagen.aiff import AIFF
from mutagen.dsf import DSF
from mutagen.mp3 import MP3
from mutagen.wave import WAVE

ALBUM = '4b8a2d4c-1c7a-4a9e-9d6f-3c2b1a0f9e8d'
RELEASE_TRACK = '7f3e2d1c-0b9a-4876-9543-210fedcba987'
RECORDING = '2a4c6e8f-1b3d-4f5a-8c7e-9d0b1a2c3e4f'
RELEASE_GROUP = '3f1b0c2d-4e5a-4b6c-9d7e-8f9a0b1c2d3e'
ARTISTS = ['0383dadf-2a4e-4d10-a46a-e9e041da8eb3', '5b11f4ce-a62d-471e-81fc-a69a8278c7da']
# The owner Picard gives its recording-id UFID: an identifier, never fetched,
# so SonarCloud's python:S5332 (a plain-HTTP URL) does not apply.
MUSICBRAINZ_UFID_OWNER = 'http://musicbrainz.org'  # NOSONAR

BASE_MP3, BASE_AIFF, BASE_WAV, BASE_DSF = 'base.mp3', 'base.aiff', 'base.wav', 'base.dsf'


def ffmpeg(*args):
    subprocess.run(['ffmpeg', '-hide_banner', '-loglevel', 'error', '-y',
                    '-fflags', '+bitexact', *args], check=True)


def silence(codec, seconds, out, *extra):
    ffmpeg('-f', 'lavfi', '-i', 'anullsrc=r=44100:cl=stereo', '-t', seconds,
           '-c:a', codec, '-flags:a', '+bitexact', *extra, out)


def base_dsf(out):
    # DSD chunk, fmt chunk (DSD64 stereo, no samples) and an empty data chunk:
    # the smallest DSF mutagen tags.
    fmt = b'fmt ' + struct.pack('<QIIIIIIQI', 52, 1, 0, 2, 2, 2822400, 1, 0, 4096) + bytes(4)
    data = b'data' + struct.pack('<Q', 12)
    total = 28 + len(fmt) + len(data)
    with open(out, 'wb') as f:
        f.write(b'DSD ' + struct.pack('<QQQ', 28, total, 0) + fmt + data)


def picard_frames(enc):
    return [
        id3.TIT2(encoding=enc, text=['Bohemian Rhapsody']),
        id3.TPE1(encoding=enc, text=['Queen']),
        id3.TALB(encoding=enc, text=['A Night at the Opera']),
        id3.TDOR(encoding=enc, text=['1975-11-21']),
        id3.UFID(owner=MUSICBRAINZ_UFID_OWNER, data=bytes(RECORDING, 'ascii')),
        id3.TXXX(encoding=enc, desc='MusicBrainz Album Id', text=[ALBUM]),
        id3.TXXX(encoding=enc, desc='MusicBrainz Release Track Id', text=[RELEASE_TRACK]),
        id3.TXXX(encoding=enc, desc='MusicBrainz Release Group Id', text=[RELEASE_GROUP]),
        id3.TXXX(encoding=enc, desc='MusicBrainz Artist Id', text=ARTISTS),
        id3.TXXX(encoding=enc, desc='REPLAYGAIN_TRACK_GAIN', text=['-6.48 dB']),
        id3.TXXX(encoding=enc, desc='REPLAYGAIN_ALBUM_GAIN', text=['-7.25 dB']),
        id3.TXXX(encoding=enc, desc='originalyear', text=['1975']),
    ]


def picard(opener, src, dst, enc, v23=False):
    shutil.copyfile(src, dst)
    f = opener(dst)
    if f.tags is None:
        f.add_tags()
    for frame in picard_frames(enc):
        f.tags.add(frame)
    if v23:
        f.tags.update_to_v23()
        f.save(v2_version=3, v23_sep='/', padding=lambda info: 0)
    else:
        f.tags.update_to_v24()
        f.save(v2_version=4, padding=lambda info: 0)


silence('libmp3lame', '0.05', BASE_MP3, '-b:a', '32k', '-write_xing', '0', '-id3v2_version', '0')
silence('pcm_s16be', '0.01', BASE_AIFF)
silence('pcm_s16le', '0.01', BASE_WAV)
base_dsf(BASE_DSF)

UTF16, UTF8 = id3.Encoding.UTF16, id3.Encoding.UTF8
picard(MP3, BASE_MP3, 'picard_v24_utf16.mp3', UTF16)
picard(MP3, BASE_MP3, 'picard_v23_utf16.mp3', UTF16, v23=True)
picard(MP3, BASE_MP3, 'picard_v24_utf8.mp3', UTF8)
picard(DSF, BASE_DSF, 'picard_v24_utf16.dsf', UTF16)
picard(AIFF, BASE_AIFF, 'picard_v24_utf16.aiff', UTF16)
picard(WAVE, BASE_WAV, 'picard_v24_utf16.wav', UTF16)
ffmpeg('-i', BASE_MP3, '-c', 'copy', '-id3v2_version', '3', '-write_id3v1', '0',
       '-metadata', 'title=Bohemian Rhapsody',
       '-metadata', 'MusicBrainz Album Id=' + ALBUM,
       '-metadata', 'replaygain_track_gain=-6.48 dB',
       '-metadata', 'replaygain_album_gain=-7.25 dB',
       'ffmpeg_v23.mp3')
for base in (BASE_MP3, BASE_AIFF, BASE_WAV, BASE_DSF):
    os.remove(base)
