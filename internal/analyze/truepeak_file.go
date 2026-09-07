package analyze

import (
	"context"
	"strconv"
)

// nativeRateDecodeArgs is decodeArgs WITHOUT the 48 kHz resample: the
// file's own rate reaches the reader. The loudness path needs 48 kHz for
// its K-weighting coefficients; a true-peak measurement of a rendered file
// needs the OPPOSITE — a resample would move the intersample peaks it
// exists to find (and at 44.1 kHz would low-pass them away).
func nativeRateDecodeArgs(srcAbs string, channels int) []string {
	return []string{
		srcAbs,
		"-t", "raw", "-e", "float", "-b", "32", "-L",
		"-c", strconv.Itoa(channels),
		"-",
	}
}

// TruePeakDBTP measures the BS.1770-style true peak of a file sox can read,
// at the file's native rate, and returns it in dBTP. ok is false for a file
// with no signal at all (the meter's own "nothing seen" contract), which a
// caller treats as "no peak to guard against", not as an error.
//
// Exported for the transcode package's DSD render: Stage B of the
// clip-guarded gain measures the decimated intermediate here. The decoder is
// ALWAYS sox — the argument is built for sox's native reader rather than
// dispatched on the extension, so a `.sox` scratch file is read by the
// handler that wrote it. The 4× polyphase interpolator runs at any input
// rate, so a 44.1 kHz intermediate is measured with the same oversampling
// as a 176.4 kHz one.
func TruePeakDBTP(ctx context.Context, srcAbs string, channels int) (dbTP float64, ok bool, err error) {
	if channels < 1 {
		channels = 1
	}
	meter := newTruePeakMeter(channels)
	if _, err := decodeFramesWith(ctx, decoderSox, resolveBin(soxLookPath, "sox"),
		nativeRateDecodeArgs(srcAbs, channels), srcAbs, channels, 0, meter.addFrame); err != nil {
		return 0, false, err
	}
	dbTP, ok = meter.truePeakDB()
	return dbTP, ok, nil
}
