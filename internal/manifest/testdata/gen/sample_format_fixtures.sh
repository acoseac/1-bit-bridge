#!/bin/sh
# Generate the sample-format fixtures in ../wav/ and ../aifc/ (backlog B513):
# the headers a real writer puts on integer and floating-point PCM, which the
# manifest's sampleFormat is read from.
#
#   wav/ (afconvert, Apple, macOS)
#     af_f64.wav    WAVE_FORMAT_EXTENSIBLE, 64 bits, the subformat
#                   KSDATAFORMAT_SUBTYPE_IEEE_FLOAT: float.
#     af_i16.wav, af_i32.wav
#                   WAVE_FORMAT_EXTENSIBLE, 16 and 32 bits, the subformat
#                   KSDATAFORMAT_SUBTYPE_PCM: integer.
#   aifc/ (afconvert)
#     af_fl64.aifc  AIFF-C, compression type "fl64": float.
#     af_be16.aiff  a plain AIFF (FORM type AIFF, an 18-byte COMM): integer.
#
# The other shapes are already here: ../wav/float.wav (format tag 3) and
# ../wav/s24.wav (extensible, the PCM subformat) from ffmpeg, and
# ../aifc/af_fl32.aifc, af_in24.aifc, af_twos.aifc and ff_sowt.aifc
# (compressed_pcm_fixtures.sh).
#
# The source is ../wav/s24.wav: ffmpeg's 0.05 s of a 441 Hz tone, mono, 44.1 kHz,
# 24 bits. Every byte is afconvert's, the 4 KB FLLR chunk it pads each file with
# included (compressed_pcm_fixtures.sh drops it; here it stays, a chunk the
# header walks must step over between the format chunk and the audio).
#
# Requires macOS (afconvert). Made on Darwin 27.0.0.
#
# Usage: sh sample_format_fixtures.sh (from any directory)
set -eu

cd "$(dirname "$0")"
aifc=../aifc
wav=../wav
src=$wav/s24.wav

afconvert -f WAVE -d LEF64 "$src" "$wav/af_f64.wav"
afconvert -f WAVE -d LEI16 "$src" "$wav/af_i16.wav"
afconvert -f WAVE -d LEI32 "$src" "$wav/af_i32.wav"
afconvert -f AIFC -d BEF64 "$src" "$aifc/af_fl64.aifc"
afconvert -f AIFF -d BEI16 "$src" "$aifc/af_be16.aiff"
ls -la "$wav" "$aifc"
