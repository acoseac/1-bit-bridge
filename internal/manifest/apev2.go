package manifest

// The APEv2 tag a WavPack or Monkey's Audio file carries at its end (backlog
// B489). The dhowden/tag this bridge pins does not read one: its dispatcher
// finds no magic it knows at the start of these files and reads the ID3v1 tag
// at their end, so it is only the fallback here, for a file with no APEv2 tag.
//
// The tag is a footer, 32 bytes at the file's end or just before an ID3v1 tag
// ("APETAGEX", the version, the size of the items and the footer, the item
// count, the flags, 8 reserved bytes), the items before it, and, when its
// flags say so, a header of the same shape before them. Each item is its
// value's size, its flags (bits 1 and 2 its type: 0 UTF-8 text, 1 binary, 2 an
// external locator), its key (2 to 255 printable ASCII characters and a NUL)
// and the value; a text value holds several values separated by NULs.
//
// Version 2000 tags are read. A version 1000 tag (APEv1, whose text is not
// defined as UTF-8) is not, and a file holding one reads as holding none: its
// ID3v1 tag, when it has one, is read instead.
//
// Every length is checked against what the file holds and against a cap of
// its own before anything is read on its account: the tag (apeTagMaxBytes),
// the item count (apeTagMaxItems), a key, and each value, by what it is read
// for (apeTextValueMax, lyrics.MaxBodyBytes for lyrics, maxArtworkBytes for
// the front cover). The items are read one at a time, a value only when it is
// one this reader keeps, so a tag costs its kept values and no more.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
)

const (
	apeTagFrameBytes = 32
	apeTagVersion2   = 2000
	apeID3v1Bytes    = 128

	// apeTagMaxBytes caps a tag's declared size: room for a front cover at
	// maxArtworkBytes beside a back cover and the text, and far below what a
	// length read out of a file could claim.
	apeTagMaxBytes = 64 << 20
	// apeTagMaxItems caps the items read; a real tag holds tens.
	apeTagMaxItems = 1024
	// apeTextValueMax caps a text value this reader keeps, but lyrics.
	apeTextValueMax = 64 << 10
	// apeCoverNameMax bounds the file name a cover item holds before its
	// NUL and its image.
	apeCoverNameMax = 1 << 10

	apeTagFlagHasHeader = 1 << 31
	apeTagFlagIsHeader  = 1 << 29
	apeItemTypeMask     = 0x6
	apeItemTypeText     = 0x0
	apeItemTypeBinary   = 0x2
)

// apeTagItem is one item this reader kept: its key as the tag holds it, and
// its values (a text item's, split on NUL, each trimmed, the empty dropped).
type apeTagItem struct {
	key    string
	values []string
}

// apeTagData is what an APEv2 tag holds that the extraction reads: the text
// items in the tag's order, and the front cover's image.
type apeTagData struct {
	items []apeTagItem
	cover []byte
}

// errAPETagMalformed is a tag whose own lengths do not fit: it is not read,
// and the file reads as holding none.
var errAPETagMalformed = errors.New("apev2: a tag whose lengths do not fit the file")

// readAPEv2 finds an APEv2 tag at the end of r, whose size is size, and reads
// it; logPath names the file in a line about an item passed over. found is
// false when the file holds none this reader takes (no footer, a
// version 1000 tag, or one whose lengths do not fit), and err a read that
// failed, which the extractor's source has noted when it did not complete.
func readAPEv2(r io.ReaderAt, size int64, logPath string) (tag apeTagData, found bool, err error) {
	footerAt, ok, err := findAPETagFooter(r, size)
	if err != nil || !ok {
		return apeTagData{}, false, err
	}
	var footer [apeTagFrameBytes]byte
	if err := readAtExactly(r, footer[:], footerAt); err != nil {
		return apeTagData{}, false, tagReadErr(err)
	}
	tagSize := int64(binary.LittleEndian.Uint32(footer[12:16]))
	count := binary.LittleEndian.Uint32(footer[16:20])
	flags := binary.LittleEndian.Uint32(footer[20:24])
	itemsLen := tagSize - apeTagFrameBytes
	itemsAt := footerAt - itemsLen
	if tagSize < apeTagFrameBytes || tagSize > apeTagMaxBytes || itemsAt < 0 {
		return apeTagData{}, false, nil
	}
	if flags&apeTagFlagHasHeader != 0 && itemsAt < apeTagFrameBytes {
		return apeTagData{}, false, nil
	}
	tag, err = readAPEv2Items(r, itemsAt, footerAt, count, logPath)
	if err != nil {
		return apeTagData{}, false, tagReadErr(err)
	}
	return tag, true, nil
}

// tagReadErr is a read's failure as readAPEv2 answers it: the file ending
// before a length said it would is the tag's malformation, which is no tag;
// any other failure is the read's.
func tagReadErr(err error) error {
	if errors.Is(err, errTruncatedHeader) || errors.Is(err, errAPETagMalformed) {
		return nil
	}
	return err
}

// findAPETagFooter is where an APEv2 footer is: in the file's last 32
// bytes, or the 32 before an ID3v1 tag. A footer of another version is
// none.
func findAPETagFooter(r io.ReaderAt, size int64) (int64, bool, error) {
	for _, at := range []int64{size - apeTagFrameBytes, size - apeID3v1Bytes - apeTagFrameBytes} {
		if at < 0 {
			continue
		}
		if at != size-apeTagFrameBytes {
			// Only before an ID3v1 tag.
			var id3 [3]byte
			if err := readAtExactly(r, id3[:], size-apeID3v1Bytes); err != nil {
				return 0, false, tagReadErr(err)
			}
			if string(id3[:]) != "TAG" {
				continue
			}
		}
		var head [12]byte
		if err := readAtExactly(r, head[:], at); err != nil {
			return 0, false, tagReadErr(err)
		}
		if string(head[0:8]) != "APETAGEX" {
			continue
		}
		if binary.LittleEndian.Uint32(head[8:12]) != apeTagVersion2 {
			return 0, false, nil
		}
		return at, true, nil
	}
	return 0, false, nil
}

// readAPEv2Items reads up to count items from itemsAt, stopping at end (the
// footer) or at an item that does not fit, keeping what was read before it.
func readAPEv2Items(r io.ReaderAt, itemsAt, end int64, count uint32, logPath string) (apeTagData, error) {
	var tag apeTagData
	pos := itemsAt
	for i := uint32(0); i < count && i < apeTagMaxItems && pos < end; i++ {
		var head [8]byte
		if end-pos < 8 {
			break
		}
		if err := readAtExactly(r, head[:], pos); err != nil {
			return apeTagData{}, err
		}
		valueLen := int64(binary.LittleEndian.Uint32(head[0:4]))
		itemFlags := binary.LittleEndian.Uint32(head[4:8])
		key, keyLen, err := readAPEItemKey(r, pos+8, end)
		if err != nil {
			return apeTagData{}, err
		}
		if keyLen == 0 {
			break // not a key the format allows: the tag past here is not read
		}
		valueAt := pos + 8 + keyLen + 1
		if valueLen > end-valueAt {
			break
		}
		if err := tag.keep(r, key, itemFlags, valueAt, valueLen, logPath); err != nil {
			return apeTagData{}, err
		}
		pos = valueAt + valueLen
	}
	return tag, nil
}

// readAPEItemKey reads the key at at: 2 to 255 printable ASCII characters
// and a NUL, inside end. keyLen 0 is a key the format does not allow.
func readAPEItemKey(r io.ReaderAt, at, end int64) (string, int64, error) {
	var buf [256]byte
	n := int64(len(buf))
	if end-at < n {
		n = end - at
	}
	if n <= 0 {
		return "", 0, nil
	}
	if err := readAtExactly(r, buf[:n], at); err != nil {
		return "", 0, err
	}
	nul := bytes.IndexByte(buf[:n], 0)
	if nul < 2 {
		return "", 0, nil
	}
	for _, c := range buf[:nul] {
		if c < 0x20 || c > 0x7e {
			return "", 0, nil
		}
	}
	return string(buf[:nul]), int64(nul), nil
}

// keep reads one item's value when it is one the extraction reads: a text
// item within its cap, or the front cover within maxArtworkBytes. Every other
// item (an external locator, another binary, a text value past its cap) is
// passed over by its length, unread.
func (tag *apeTagData) keep(r io.ReaderAt, key string, flags uint32, at, n int64, logPath string) error {
	switch flags & apeItemTypeMask {
	case apeItemTypeText:
		limit := int64(apeTextValueMax)
		if apeLyricsKey(key) != "" {
			limit = lyrics.MaxBodyBytes * 2
		}
		if n > limit {
			scanLogger.Warn("apev2: a text item larger than its cap; skipping",
				"path", logPath, "key", key, "bytes", n, "cap", limit)
			return nil
		}
		buf := make([]byte, n)
		if err := readAtExactly(r, buf, at); err != nil {
			return err
		}
		var values []string
		for _, v := range strings.Split(string(buf), "\x00") {
			if v = strings.TrimSpace(v); v != "" {
				values = append(values, v)
			}
		}
		tag.items = append(tag.items, apeTagItem{key: key, values: values})
	case apeItemTypeBinary:
		if !strings.EqualFold(key, "Cover Art (Front)") || tag.cover != nil {
			return nil
		}
		if n > maxArtworkBytes+apeCoverNameMax {
			scanLogger.Warn("apev2: front cover larger than the artwork cap; skipping",
				"path", logPath, "bytes", n, "cap", maxArtworkBytes)
			return nil
		}
		buf := make([]byte, n)
		if err := readAtExactly(r, buf, at); err != nil {
			return err
		}
		nul := bytes.IndexByte(buf[:min(len(buf), apeCoverNameMax+1)], 0)
		if nul < 0 {
			return nil
		}
		tag.cover = buf[nul+1:]
	}
	return nil
}

// apeLyricsKey classifies a lyrics item's key as the FLAC comment walk does
// a Vorbis comment's: "synced" for a synchronised document's names, "plain"
// for the rest of lyrics' names, and "" for any other key.
func apeLyricsKey(key string) string {
	switch strings.ToLower(key) {
	case "syncedlyrics", "synced lyrics", "lrc":
		return "synced"
	case "lyrics", "x-lyrics", "unsyncedlyrics", "unsynced lyrics":
		return "plain"
	}
	return ""
}
