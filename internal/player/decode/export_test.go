package decode

import (
	"io"

	alac "github.com/mycophonic/saprobe-alac"
)

// SniffsALAC reports whether content inspection classifies rs as ALAC in
// MP4. PR 1a's sniff tests use it: since PR 1c a stub file the sniffer calls
// ALAC opens the ALAC demuxer, which refuses it for the tables it lacks, so
// the refusal no longer shows what the sniffer decided.
func SniffsALAC(rs io.ReadSeeker) (bool, error) {
	res, err := sniff(rs)
	return res.kind == sniffALAC, err
}

// ALACPackets is the packet decoder the ALAC demuxer drives.
type ALACPackets = alacPackets

// OpenALAC opens rs with the ALAC demuxer alone. The sniffer runs first in
// Open and refuses some malformed files itself; the demuxer's own refusals
// of those are tested through this.
func OpenALAC(rs io.ReadSeeker) (Decoder, error) { return openALAC(rs) }

// OpenALACWith opens rs with newPackets in place of saprobe-alac's
// constructor.
func OpenALACWith(rs io.ReadSeeker, newPackets func(alac.PacketConfig) (ALACPackets, error)) (Decoder, error) {
	return openALACWith(rs, newPackets)
}

// ALACLibraryPanics returns how many packets d's packet decoder panicked on,
// or -1 when d is not an ALAC decoder.
func ALACLibraryPanics(d Decoder) int {
	if a, ok := d.(*alacDecoder); ok {
		return a.libraryPanics
	}
	return -1
}
