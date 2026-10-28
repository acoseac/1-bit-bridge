package alsa

import "testing"

func TestQualificationGainDescriptors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		b         []byte
		gain, bad bool
	}{{"none", []byte{9, 4, 0, 0, 0, 1, 1, 0, 0}, false, false}, {"feature", []byte{9, 4, 0, 0, 0, 1, 1, 0, 0, 3, 0x24, 6}, true, false}, {"unrelated", []byte{9, 4, 0, 0, 0, 3, 1, 0, 0, 3, 0x24, 6}, false, false}, {"unknown_audio_protocol", []byte{9, 4, 0, 0, 0, 1, 1, 0x30, 0}, false, true}, {"short", []byte{9, 4}, false, true}, {"missing", nil, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			got, e := descriptorGain(tc.b)
			if got != tc.gain || (e != nil) != tc.bad {
				t.Fatalf("gain=%v error=%v", got, e)
			}
		})
	}
}
