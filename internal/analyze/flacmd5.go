package analyze

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- FLAC's embedded audio checksum IS MD5 (spec-mandated); integrity comparison only, not a security primitive.
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

// FLAC audio-MD5 verification: decode the file to the EXACT byte stream
// the FLAC spec hashes (interleaved, little-endian, signed, native bit
// depth, native rate, native channels) and compare against the MD5 the
// encoder stored in STREAMINFO.
//
// THE TRAP THIS FILE EXISTS TO AVOID (consult 2026-08-03): hashing a
// decoder's convenience output false-flags good files — the analysis
// pipeline's f32le/48kHz stream is precisely NOT the hashed stream, and
// generic float/planar normalization shifts bit alignment. So the decode
// here pins the output format to the STREAMINFO bit depth with NO
// resample and NO channel mapping, and only bit depths whose byte packing
// is unambiguous across decoders (8/16/24/32) are verified — 12/20-bit
// FLAC (rare) reports "cannot verify" (empty state) rather than risking a
// false mismatch from shift conventions.
//
// FAILURE DIRECTION: any decode error, tool absence, short read, or
// unsupported layout yields the EMPTY state ("cannot verify"), never
// "mismatch" — a mismatch is only reported when a CLEAN, complete decode
// hashed differently from a nonzero stored checksum. Note a mismatch is
// not proof of corruption: some tag editors rewrite FLAC without
// updating STREAMINFO's MD5 (the known `sox WARN` treadmill class), so
// consumers phrase it as "modified or corrupt", not "corrupt".

const (
	// AudioMD5Verified: decoded audio hashed exactly to STREAMINFO's MD5.
	AudioMD5Verified = "verified"
	// AudioMD5Mismatch: clean complete decode, different hash — the file's
	// audio no longer matches what its encoder checksummed.
	AudioMD5Mismatch = "mismatch"
)

type flacStreamInfo struct {
	channels      int
	bitsPerSample int
	totalSamples  int64
	storedMD5     [16]byte
	hasMD5        bool
}

// readFLACStreamInfo reads just the 34-byte STREAMINFO block (the spec
// requires it first). Hand-rolled bounded parse — the house habit
// (internal/manifest's extractors document the same 34-byte layout; this
// copy exists because analyze cannot import manifest without a cycle, and
// the parse is 30 lines against a spec-frozen layout).
func readFLACStreamInfo(path string) (flacStreamInfo, error) {
	f, err := os.Open(path) // #nosec G304 -- path comes from the scanner's own library walk.
	if err != nil {
		return flacStreamInfo{}, err
	}
	defer f.Close()
	header := make([]byte, 8)
	if _, err := io.ReadFull(f, header); err != nil {
		return flacStreamInfo{}, err
	}
	if string(header[0:4]) != "fLaC" {
		return flacStreamInfo{}, fmt.Errorf("not a FLAC stream")
	}
	if header[4]&0x7F != 0 {
		return flacStreamInfo{}, fmt.Errorf("first metadata block is not STREAMINFO")
	}
	blockLen := int(header[5])<<16 | int(header[6])<<8 | int(header[7])
	if blockLen != 34 {
		return flacStreamInfo{}, fmt.Errorf("STREAMINFO length %d != 34", blockLen)
	}
	body := make([]byte, 34)
	if _, err := io.ReadFull(f, body); err != nil {
		return flacStreamInfo{}, err
	}
	info := flacStreamInfo{
		channels:      int((body[12]>>1)&0x07) + 1,
		bitsPerSample: int(body[12]&0x01)<<4 | int(body[13]>>4) + 1,
		totalSamples: int64(body[13]&0x0F)<<32 | int64(body[14])<<24 |
			int64(body[15])<<16 | int64(body[16])<<8 | int64(body[17]),
	}
	copy(info.storedMD5[:], body[18:34])
	info.hasMD5 = info.storedMD5 != [16]byte{}
	return info, nil
}

// verifiableBitDepth reports whether the STREAMINFO depth has an
// unambiguous decoder byte packing (whole bytes) for the hash stream.
func verifiableBitDepth(bits int) bool {
	switch bits {
	case 8, 16, 24, 32:
		return true
	default:
		return false
	}
}

// flacMD5DecodeCommand builds the native-passthrough decode for the hash:
// sox with an explicit raw signed-LE output at the source depth (no -r,
// no -c → native rate/channels), or the ffmpeg twin with the matching
// pcm_sNN codec. Both decode FLAC losslessly, so the streams are
// byte-identical — pinned by the round-trip test.
func flacMD5DecodeCommand(tool decoderTool, srcAbs string, bits int) (string, []string) {
	if tool == decoderFFmpeg {
		var codec, format string
		switch bits {
		case 8:
			codec, format = "pcm_s8", "s8"
		case 16:
			codec, format = "pcm_s16le", "s16le"
		case 24:
			codec, format = "pcm_s24le", "s24le"
		default:
			codec, format = "pcm_s32le", "s32le"
		}
		return resolveBin(ffmpegLookPath, "ffmpeg"), []string{
			"-nostdin", "-hide_banner", "-loglevel", "error",
			"-i", srcAbs,
			"-map", "0:a:0",
			"-c:a", codec,
			"-f", format,
			"-",
		}
	}
	return resolveBin(soxLookPath, "sox"), []string{
		srcAbs,
		"-t", "raw", "-e", "signed", "-b", strconv.Itoa(bits), "-L",
		"-",
	}
}

// verifyFLACAudioMD5 returns AudioMD5Verified / AudioMD5Mismatch, or ""
// when verification is not possible (no stored checksum, odd bit depth,
// tool failure, decode error). See the file docblock for the failure
// direction.
func verifyFLACAudioMD5(ctx context.Context, srcAbs string, tool decoderTool) string {
	info, err := readFLACStreamInfo(srcAbs)
	if err != nil || !info.hasMD5 || !verifiableBitDepth(info.bitsPerSample) {
		return ""
	}
	// totalSamples == 0 is legal ("unknown", streamed encodes) but makes
	// completeness unverifiable — and a CLEAN EXIT is not proof of a
	// complete decode (sox exits 0 after a FLAC LOST_SYNC on a truncated
	// file; the analysis decode defends with a duration probe, this path
	// defends with the exact expected byte count below). Without a length
	// to check against, refuse to verify rather than risk hashing half a
	// file into a false mismatch.
	if info.totalSamples <= 0 || info.channels < 1 {
		return ""
	}
	expectedBytes := info.totalSamples * int64(info.channels) * int64(info.bitsPerSample/8)
	name, args := flacMD5DecodeCommand(tool, srcAbs, info.bitsPerSample)
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return ""
	}
	if err := cmd.Start(); err != nil {
		return ""
	}
	processReleased := false
	defer func() {
		if !processReleased {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	hasher := md5.New() // #nosec G401 -- see file docblock: FLAC's spec checksum, not a security primitive.
	copied, copyErr := io.Copy(hasher, stdout)
	waitErr := cmd.Wait()
	processReleased = true
	// Only a CLEAN decode of EXACTLY the STREAMINFO-declared sample count
	// may compare — a truncated decode hashes differently by construction
	// (and sox exits 0 on truncation), so anything short or long reads as
	// "cannot verify", never "mismatch".
	if copyErr != nil || waitErr != nil || copied != expectedBytes {
		return ""
	}
	var digest [16]byte
	copy(digest[:], hasher.Sum(nil))
	if digest == info.storedMD5 {
		return AudioMD5Verified
	}
	return AudioMD5Mismatch
}
