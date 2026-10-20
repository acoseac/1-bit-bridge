package decode

const (
	// maxHeaderChunkBody is the largest fmt / COMM / similar descriptor we
	// will allocate. Real PCM descriptors are tens of bytes.
	maxHeaderChunkBody = 4096
	// maxNonAudioHeaderScan bounds bytes spent walking chunks before the
	// audio payload, including after fmt/COMM has been seen.
	maxNonAudioHeaderScan = 8 << 20
	// maxReadChunkBytes caps a single ReadFrames scratch allocation so a
	// large dst cannot force a multi-megabyte ReadFull.
	maxReadChunkBytes = 64 << 10
	// maxFLACMetaBody is the largest FLAC metadata body we will parse into
	// memory (SEEKTABLE). Larger blocks are skipped by seeking.
	maxFLACMetaBody = 256 << 10
	// maxSampleRateHz is an upper bound for rates we accept for PCM open.
	maxSampleRateHz = 768000
	// maxMP4Boxes bounds ISO-BMFF walks.
	maxMP4Boxes = 256
	// maxMP4BoxDepth caps nested box recursion inside a moov buffer.
	maxMP4BoxDepth = 8
	// maxMoovBytes is the largest moov/moof payload we will load to find stsd.
	maxMoovBytes = 4 << 20
)

func acceptRate(rate int) error {
	if rate <= 0 || rate > maxSampleRateHz {
		return refuse(ReasonUnsupportedLayout, "sample rate %d Hz is not supported", rate)
	}
	return nil
}

// framesPerRead returns how many sample frames fit in one cancellable read
// chunk for the given frame size in bytes.
func framesPerRead(frameBytes, framesWant int) int {
	if frameBytes <= 0 || framesWant <= 0 {
		return 0
	}
	maxFrames := maxReadChunkBytes / frameBytes
	if maxFrames < 1 {
		maxFrames = 1
	}
	if framesWant < maxFrames {
		return framesWant
	}
	return maxFrames
}
