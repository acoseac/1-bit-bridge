package alsa

import (
	"reflect"
	"testing"
)

func TestCapabilitiesFromTheIFiCapture(t *testing.T) {
	s, probs := ParseStream(readFixture(t, "dsd256-native", "asound/card5/stream0"))
	if len(probs) != 0 || s.Playback == nil {
		t.Fatalf("parse: %v playback %v", probs, s.Playback != nil)
	}
	got := CapabilitiesFromAltsets(s.Playback.Altsets)
	want := Capabilities{
		MaxPCMRate:     384000,
		PCMBitDepths:   []int{16, 24, 32},
		NativeDSD:      true,
		NativeDSDRates: []int{1411200, 1536000, 2822400, 3072000, 5644800, 6144000, 11289600, 12288000},
		DSDFormats:     []string{"DSD_U32_BE"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("iFi capabilities\n got %#v\nwant %#v", got, want)
	}
}

func TestCapabilitiesFromTheSynthetic768AndDSD512(t *testing.T) {
	s, probs := ParseStream(readFixture(t, "pcm768-dsd512", "asound/card0/stream0"))
	if len(probs) != 0 || s.Playback == nil {
		t.Fatalf("parse: %v playback %v", probs, s.Playback != nil)
	}
	got := CapabilitiesFromAltsets(s.Playback.Altsets)
	want := Capabilities{
		MaxPCMRate:     768000,
		PCMBitDepths:   []int{16, 24, 32},
		NativeDSD:      true,
		NativeDSDRates: []int{22579200, 24576000},
		DSDFormats:     []string{"DSD_U32_BE"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pcm768-dsd512\n got %#v\nwant %#v", got, want)
	}
}

func TestCapabilitiesTable(t *testing.T) {
	bits := func(n int) *int { return &n }
	cases := []struct {
		name string
		in   []Altset
		want Capabilities
	}{
		{
			name: "empty",
			want: Capabilities{PCMBitDepths: []int{}, NativeDSDRates: []int{}, DSDFormats: []string{}},
		},
		{
			name: "dsd bits are not a pcm depth",
			in: []Altset{{
				Formats: []string{"SPECIAL", "DSD_U32_BE"},
				Rates:   []int{44100},
				Bits:    bits(32),
			}},
			want: Capabilities{
				PCMBitDepths:   []int{},
				NativeDSD:      true,
				NativeDSDRates: []int{1411200},
				DSDFormats:     []string{"DSD_U32_BE"},
			},
		},
		{
			name: "u16 and u8 multipliers",
			in: []Altset{
				{Formats: []string{"DSD_U16_LE"}, Rates: []int{88200}},
				{Formats: []string{"DSD_U8"}, Rates: []int{176400}},
			},
			want: Capabilities{
				PCMBitDepths:   []int{},
				NativeDSD:      true,
				NativeDSDRates: []int{1411200},
				DSDFormats:     []string{"DSD_U16_LE", "DSD_U8"},
			},
		},
		{
			name: "unknown dsd format lists and invents no rate",
			in:   []Altset{{Formats: []string{"DSD_FOO"}, Rates: []int{44100}}},
			want: Capabilities{
				PCMBitDepths:   []int{},
				NativeDSD:      true,
				NativeDSDRates: []int{},
				DSDFormats:     []string{"DSD_FOO"},
			},
		},
		{
			name: "pcm rate range",
			in:   []Altset{{Formats: []string{"S24_3LE"}, RateRange: &RateRange{Min: 8000, Max: 96000}}},
			want: Capabilities{
				MaxPCMRate:     96000,
				PCMBitDepths:   []int{24},
				NativeDSDRates: []int{},
				DSDFormats:     []string{},
			},
		},
		{
			name: "mixed altset is pcm and dsd",
			in:   []Altset{{Formats: []string{"S32_LE", "DSD_U32_LE"}, Rates: []int{48000}}},
			want: Capabilities{
				MaxPCMRate:     48000,
				PCMBitDepths:   []int{32},
				NativeDSD:      true,
				NativeDSDRates: []int{1536000},
				DSDFormats:     []string{"DSD_U32_LE"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CapabilitiesFromAltsets(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestNativeDSDRate(t *testing.T) {
	cases := []struct {
		format string
		frame  int
		want   int
		ok     bool
	}{
		{"DSD_U32_BE", 384000, 12288000, true},
		{"DSD_U32_LE", 705600, 22579200, true},
		{"DSD_U16_BE", 88200, 1411200, true},
		{"DSD_U8", 176400, 1411200, true},
		{"DSD_FOO", 44100, 0, false},
		{"S32_LE", 768000, 0, false},
		{"SPECIAL", 352800, 0, false},
	}
	for _, tc := range cases {
		got, ok := NativeDSDRate(tc.format, tc.frame)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NativeDSDRate(%s, %d) = %d, %v; want %d, %v", tc.format, tc.frame, got, ok, tc.want, tc.ok)
		}
	}
}

func TestResolveALSADevice(t *testing.T) {
	cards := []Card{{Index: 0, ID: "vc4hdmi0"}, {Index: 5, ID: "Audio"}, {Index: 6, ID: "Audio2"}}
	ok := []struct {
		device string
		card   int
		pcm    int
	}{
		{"hw:5", 5, 0},
		{"hw:5,0", 5, 0},
		{"hw:5,1", 5, 1},
		{"plughw:5,0", 5, 0},
		{"hw:CARD=Audio,DEV=0", 5, 0},
		{"hw:CARD=Audio", 5, 0},
		{"hw:Audio", 5, 0},
		{"hw:Audio,1", 5, 1},
		{"plughw:CARD=Audio2,DEV=2", 6, 2},
	}
	for _, tc := range ok {
		card, pcm, resolved := ResolveALSADevice(tc.device, cards)
		if !resolved || card != tc.card || pcm != tc.pcm {
			t.Errorf("ResolveALSADevice(%q) = %d, %d, %v; want %d, %d, true", tc.device, card, pcm, resolved, tc.card, tc.pcm)
		}
	}
	for _, device := range []string{"", "default", "volumio", "hw:AudioX", "hw:CARD=Aud", "hw:9", "hw:CARD=missing"} {
		if _, _, resolved := ResolveALSADevice(device, cards); resolved {
			t.Errorf("ResolveALSADevice(%q) resolved", device)
		}
	}
	both := []Card{{Index: 1, ID: "Audio"}, {Index: 2, ID: "Audio"}}
	if _, _, resolved := ResolveALSADevice("hw:CARD=Audio", both); resolved {
		t.Fatal("two cards with the same id resolved")
	}
}
