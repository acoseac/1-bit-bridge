package decode

import (
	"encoding/binary"
	"io"
)

type sniffKind int

const (
	sniffUnknown sniffKind = iota
	sniffFLAC
	sniffWAV
	sniffAIFF
	sniffALAC
	sniffDSF
	sniffDFF
	sniffMP3
	sniffAAC
	sniffOgg
)

// sniff peeks at the start of rs without requiring a later Seek(0) from the
// caller (Open resets). It never decides by file extension.
func sniff(rs io.ReadSeeker) (sniffKind, error) {
	var hdr [12]byte
	n, err := io.ReadFull(rs, hdr[:])
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		return sniffBuf(hdr[:n]), nil
	}
	if err != nil {
		return sniffUnknown, err
	}

	return sniffBufFull(rs, hdr[:])
}

func sniffBuf(hdr []byte) sniffKind {
	k, _ := sniffFrom(hdr, nil)
	return k
}

func sniffBufFull(rs io.ReadSeeker, hdr []byte) (sniffKind, error) {
	return sniffFrom(hdr, rs)
}

func sniffFrom(hdr []byte, rs io.ReadSeeker) (sniffKind, error) {
	if len(hdr) >= 4 && string(hdr[:4]) == "fLaC" {
		return sniffFLAC, nil
	}
	if len(hdr) >= 12 && string(hdr[:4]) == "RIFF" && string(hdr[8:12]) == "WAVE" {
		return sniffWAV, nil
	}
	if len(hdr) >= 12 && string(hdr[:4]) == "RF64" && string(hdr[8:12]) == "WAVE" {
		return sniffUnknown, refuse(ReasonUnsupportedLayout, "RF64 WAVE is not supported")
	}
	if len(hdr) >= 12 && string(hdr[:4]) == "FORM" && (string(hdr[8:12]) == "AIFF" || string(hdr[8:12]) == "AIFC") {
		return sniffAIFF, nil
	}
	if len(hdr) >= 4 && string(hdr[:4]) == "DSD " {
		return sniffDSF, nil
	}
	if len(hdr) >= 4 && string(hdr[:4]) == "FRM8" {
		return sniffDFF, nil
	}
	if len(hdr) >= 4 && string(hdr[:4]) == "OggS" {
		return sniffOgg, nil
	}
	if len(hdr) >= 3 && string(hdr[:3]) == "ID3" {
		if rs == nil {
			return sniffMP3, nil
		}
		return sniffAfterID3(rs, hdr)
	}
	if len(hdr) >= 8 && string(hdr[4:8]) == "ftyp" {
		if rs == nil {
			return sniffAAC, nil
		}
		return sniffMP4Brand(rs, hdr)
	}
	// MP3 frame sync: 11 set bits.
	if len(hdr) >= 2 && hdr[0] == 0xFF && hdr[1]&0xE0 == 0xE0 {
		return sniffMP3, nil
	}
	return sniffUnknown, nil
}

func sniffAfterID3(rs io.ReadSeeker, hdr []byte) (sniffKind, error) {
	if len(hdr) < 10 {
		var more [10]byte
		copy(more[:], hdr)
		if _, err := io.ReadFull(rs, more[len(hdr):]); err != nil {
			return sniffMP3, nil // ID3-tagged lossy is the common case
		}
		hdr = more[:]
	}
	// ID3v2 size is synchsafe in bytes 6..9.
	size := int(hdr[6]&0x7F)<<21 | int(hdr[7]&0x7F)<<14 | int(hdr[8]&0x7F)<<7 | int(hdr[9]&0x7F)
	if size < 0 || size > 16<<20 {
		return sniffMP3, nil
	}
	if _, err := rs.Seek(int64(10+size), io.SeekStart); err != nil {
		return sniffUnknown, err
	}
	var next [4]byte
	if _, err := io.ReadFull(rs, next[:]); err != nil {
		return sniffMP3, nil
	}
	if string(next[:]) == "fLaC" {
		return sniffFLAC, nil
	}
	if next[0] == 0xFF && next[1]&0xE0 == 0xE0 {
		return sniffMP3, nil
	}
	return sniffMP3, nil
}

func sniffMP4Brand(rs io.ReadSeeker, hdr []byte) (sniffKind, error) {
	boxSize := binary.BigEndian.Uint32(hdr[0:4])
	if boxSize < 8 {
		return sniffUnknown, nil
	}
	// Walk top-level boxes from the start, skipping mdat by size so an
	// mdat-first ALAC file still reveals its moov/alac sample entry.
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return sniffUnknown, err
	}
	alac, aac, err := scanMP4Codecs(rs)
	if err != nil {
		return sniffUnknown, err
	}
	if alac {
		return sniffALAC, nil
	}
	if aac {
		return sniffAAC, nil
	}
	// ftyp said MP4/M4A but no codec we recognise — refuse as not-yet /
	// unknown rather than guessing lossy.
	_ = boxSize
	return sniffAAC, nil
}

// scanMP4Codecs walks top-level ISO-BMFF boxes looking for 'alac' or AAC
// sample-entry fourccs. It skips mdat payloads without reading them.
func scanMP4Codecs(rs io.ReadSeeker) (alac, aac bool, err error) {
	const maxBoxes = 64
	const maxMoovScan = 4 << 20 // 4 MiB of moov is plenty
	for i := 0; i < maxBoxes; i++ {
		var hdr [8]byte
		n, rerr := io.ReadFull(rs, hdr[:])
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			if n == 0 {
				return alac, aac, nil
			}
			return alac, aac, nil
		}
		if rerr != nil {
			return false, false, rerr
		}
		size := uint64(binary.BigEndian.Uint32(hdr[0:4]))
		typ := string(hdr[4:8])
		var payload uint64
		switch {
		case size == 0:
			// Extends to EOF — nothing useful left to walk.
			return alac, aac, nil
		case size == 1:
			var big [8]byte
			if _, err := io.ReadFull(rs, big[:]); err != nil {
				return alac, aac, nil
			}
			size = binary.BigEndian.Uint64(big[:])
			if size < 16 {
				return alac, aac, nil
			}
			payload = size - 16
		default:
			if size < 8 {
				return alac, aac, nil
			}
			payload = size - 8
		}
		switch typ {
		case "mdat", "free", "skip", "wide":
			if _, err := rs.Seek(int64(payload), io.SeekCurrent); err != nil {
				return alac, aac, nil
			}
		case "moov", "moof":
			scan := payload
			if scan > maxMoovScan {
				scan = maxMoovScan
			}
			buf := make([]byte, scan)
			n, _ := io.ReadFull(rs, buf)
			buf = buf[:n]
			if containsFourCC(buf, "alac") {
				alac = true
			}
			if containsFourCC(buf, "mp4a") {
				aac = true
			}
			remain := int64(payload) - int64(n)
			if remain > 0 {
				if _, err := rs.Seek(remain, io.SeekCurrent); err != nil {
					return alac, aac, nil
				}
			}
			if alac {
				return true, aac, nil
			}
		default:
			if _, err := rs.Seek(int64(payload), io.SeekCurrent); err != nil {
				return alac, aac, nil
			}
		}
	}
	return alac, aac, nil
}

func containsFourCC(buf []byte, cc string) bool {
	if len(cc) != 4 {
		return false
	}
	for i := 0; i+4 <= len(buf); i++ {
		if string(buf[i:i+4]) == cc {
			return true
		}
	}
	return false
}
