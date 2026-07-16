package manifest

import "testing"

// TestIsLossyCodec pins the lossy set + the case/whitespace folding +
// the documented fail-open on empty codec (legacy rows stay
// upscale-eligible on geometry alone).
func TestIsLossyCodec(t *testing.T) {
	lossy := []string{"MP3", "mp3", " AAC ", "OGG", "OPUS", "WMA", "opus"}
	for _, c := range lossy {
		if !IsLossyCodec(c) {
			t.Errorf("IsLossyCodec(%q) = false, want true", c)
		}
	}
	notLossy := []string{"FLAC", "ALAC", "WAV", "AIFF", "PCM", "DSF", "DFF", "", "  "}
	for _, c := range notLossy {
		if IsLossyCodec(c) {
			t.Errorf("IsLossyCodec(%q) = true, want false", c)
		}
	}
}
