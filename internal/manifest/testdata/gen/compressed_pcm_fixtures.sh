#!/bin/sh
# Generate the compressed AIFF-C and WAV fixtures in ../aifc/ and ../wav/
# (backlog B124, B154).
#
# Real writers, because the defect lived in what they write: an AIFF-C names its
# encoding in the COMM chunk's compression type, a WAV in the fmt chunk's format
# tag, and the bridge read neither, so every one was "AIFF" or "WAV", lossless.
#
#   aifc/ (afconvert, Apple, macOS; and ffmpeg's AIFF muxer)
#     af_ulaw.aifc, af_alaw.aifc   "ulaw", "alaw": COMM says 8 bits and the
#                                  true frame count.
#     af_ima4.aifc                 "ima4": COMM says 0 bits, and numSampleFrames
#                                  counts PACKETS of 64 frames (0.2 s long, so
#                                  its duration clears the floor).
#     af_ima4_96k.aifc             the same at 96 kHz, a rate the Hi-Res bucket
#                                  takes for a lossless file.
#     af_twos.aifc, af_in24.aifc, af_fl32.aifc
#                                  linear PCM ("twos", "in24", "fl32").
#     ff_ima4.aifc                 ffmpeg's adpcm_ima_qt: "ima4", COMM says 4 bits.
#     ff_sowt.aifc                 ffmpeg's pcm_s16le: "sowt".
#   wav/ (ffmpeg; sox for GSM)
#     ima_adpcm.wav, ms_adpcm.wav  format tags 0x0011, 0x0002.
#     ima_adpcm_96k.wav, ms_adpcm_96k.wav
#                                  the same at 96 kHz, which ffmpeg writes with a
#                                  WAVE_FORMAT_EXTENSIBLE header (tag 0xFFFE, the
#                                  code in the subformat GUID).
#     alaw.wav, mulaw.wav          tags 0x0006, 0x0007.
#     mp2.wav                      tag 0x0050.
#     gsm.wav                      tag 0x0031 (sox, 8 kHz).
#     g726.wav                     tag 0x0045, a code the bridge does not name.
#     float.wav, s24.wav           linear: tag 0x0003, and 0xFFFE with the PCM
#                                  subformat (ffmpeg's 24-bit header).
#
# afconvert pads each file with a 4 KB FLLR chunk; it is dropped and the FORM
# size recomputed (the recipe the iOS app's AIFCFixtures uses); every other
# byte is the writer's. Mono, 44.1 kHz unless named, 0.05 s but for
# af_ima4.aifc.
#
# Requires macOS (afconvert), ffmpeg, sox and python3. Made with ffmpeg 9.0.2 and
# sox 14.4.2.
#
# Usage (from this directory): sh compressed_pcm_fixtures.sh
set -eu

aifc=../aifc
wav=../wav
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$aifc" "$wav"

tone() { # tone <seconds> <rate> <file>
	ffmpeg -hide_banner -loglevel error -y -f lavfi -i "sine=frequency=441:duration=$1:sample_rate=$2" \
		-ac 1 -c:a pcm_s16le -map_metadata -1 -fflags +bitexact "$3"
}
tone 0.05 44100 "$tmp/t44.wav"
tone 0.2 44100 "$tmp/t44_long.wav"
tone 0.05 96000 "$tmp/t96.wav"
tone 0.05 8000 "$tmp/t8k.wav"

afconvert -f AIFC -d ulaw "$tmp/t44.wav" "$tmp/af_ulaw.aifc"
afconvert -f AIFC -d alaw "$tmp/t44.wav" "$tmp/af_alaw.aifc"
afconvert -f AIFC -d ima4 "$tmp/t44_long.wav" "$tmp/af_ima4.aifc"
afconvert -f AIFC -d ima4 "$tmp/t96.wav" "$tmp/af_ima4_96k.aifc"
afconvert -f AIFC -d BEI16 "$tmp/t44.wav" "$tmp/af_twos.aifc"
afconvert -f AIFC -d BEI24 "$tmp/t44.wav" "$tmp/af_in24.aifc"
afconvert -f AIFC -d BEF32 "$tmp/t44.wav" "$tmp/af_fl32.aifc"
ffmpeg -hide_banner -loglevel error -y -i "$tmp/t44_long.wav" -c:a adpcm_ima_qt -fflags +bitexact -f aiff "$tmp/ff_ima4.aifc"
ffmpeg -hide_banner -loglevel error -y -i "$tmp/t44.wav" -c:a pcm_s16le -fflags +bitexact -f aiff "$tmp/ff_sowt.aifc"

for f in "$tmp"/*.aifc; do
	python3 - "$f" "$aifc/$(basename "$f")" <<'EOF'
import struct
import sys

data = open(sys.argv[1], 'rb').read()
assert data[:4] == b'FORM' and data[8:12] == b'AIFC', sys.argv[1]
chunks, pos = [], 12
while pos + 8 <= len(data):
    cid = data[pos:pos + 4]
    size = struct.unpack('>I', data[pos + 4:pos + 8])[0]
    end = pos + 8 + size + (size & 1)
    if cid != b'FLLR':
        chunks.append(data[pos:end])
    pos = end
body = b'AIFC' + b''.join(chunks)
open(sys.argv[2], 'wb').write(b'FORM' + struct.pack('>I', len(body)) + body)
EOF
done

w() { # w <name> <source> <ffmpeg codec> [args]
	name=$1 src=$2 codec=$3
	shift 3
	ffmpeg -hide_banner -loglevel error -y -i "$src" -c:a "$codec" "$@" -map_metadata -1 -fflags +bitexact -f wav "$wav/$name"
}
w ima_adpcm.wav "$tmp/t44.wav" adpcm_ima_wav
w ms_adpcm.wav "$tmp/t44.wav" adpcm_ms
w ima_adpcm_96k.wav "$tmp/t96.wav" adpcm_ima_wav
w ms_adpcm_96k.wav "$tmp/t96.wav" adpcm_ms
w alaw.wav "$tmp/t44.wav" pcm_alaw
w mulaw.wav "$tmp/t44.wav" pcm_mulaw
w mp2.wav "$tmp/t44.wav" mp2
w g726.wav "$tmp/t8k.wav" adpcm_g726
w float.wav "$tmp/t44.wav" pcm_f32le
w s24.wav "$tmp/t44.wav" pcm_s24le
sox "$tmp/t8k.wav" -e gsm-full-rate "$wav/gsm.wav"
ls -la "$aifc" "$wav"
