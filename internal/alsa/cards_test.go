package alsa

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseCardsOnEveryCapture reads the cards file of every tree: the
// Pi's two HDMI cards and the USB DAC at index 5, whose id is padded to
// the kernel's 15 columns inside the brackets.
func TestParseCardsOnEveryCapture(t *testing.T) {
	want := []Card{
		{Index: 0, ID: "vc4hdmi0", Driver: "vc4-hdmi", Name: "vc4-hdmi-0", LongName: "vc4-hdmi-0"},
		{Index: 1, ID: "vc4hdmi1", Driver: "vc4-hdmi", Name: "vc4-hdmi-1", LongName: "vc4-hdmi-1"},
		{Index: 5, ID: "Audio", Driver: "USB-Audio", Name: "iFi (by AMR) HD USB Audio",
			LongName: "iFi (by AMR) iFi (by AMR) HD USB Audio at usb-xhci-hcd.0-1, high speed"},
	}
	for _, c := range captures {
		t.Run(c.name, func(t *testing.T) {
			cards, probs := ParseCards(readFixture(t, c.name, "asound/cards"))
			if !reflect.DeepEqual(cards, want) {
				t.Errorf("cards = %+v\nwant    %+v", cards, want)
			}
			if len(probs) != 0 {
				t.Errorf("problems on a captured file: %q", probs)
			}
		})
	}
}

// TestParseCardsShapes covers the shapes the captures do not: an index of
// two digits (no leading space), an id that fills its 15 columns, an
// empty short name, the no-cards line, CRLF line ends, and the damaged
// files a parse has to report rather than read.
func TestParseCardsShapes(t *testing.T) {
	tests := []struct {
		name  string
		in    string
		want  []Card
		probs []string
	}{
		{
			name: "no soundcards",
			in:   "--- no soundcards ---\n",
			want: []Card{},
		},
		{
			name: "two-digit index, a 15-column id, an empty short name",
			in: "10 [sndrpihifiberry]: HifiberryDacp - \n" +
				"                      snd_rpi_hifiberry_dacplus\n",
			want: []Card{{Index: 10, ID: "sndrpihifiberry", Driver: "HifiberryDacp", LongName: "snd_rpi_hifiberry_dacplus"}},
		},
		{
			name: "CRLF line ends read as the kernel wrote them",
			in:   " 2 [DAC            ]: USB-Audio - DAC\r\n                      DAC at usb-1, high speed\r\n",
			want: []Card{{Index: 2, ID: "DAC", Driver: "USB-Audio", Name: "DAC", LongName: "DAC at usb-1, high speed"}},
		},
		{
			name: "a head cut off before its long name",
			in:   " 0 [vc4hdmi0       ]: vc4-hdmi - vc4-hdmi-0\n                      vc4-hdmi-0\n 1 [vc4hdmi1       ]: vc4-hdmi - vc4-hdmi-1\n",
			want: []Card{
				{Index: 0, ID: "vc4hdmi0", Driver: "vc4-hdmi", Name: "vc4-hdmi-0", LongName: "vc4-hdmi-0"},
				{Index: 1, ID: "vc4hdmi1", Driver: "vc4-hdmi", Name: "vc4-hdmi-1"},
			},
			probs: []string{"line 3: card 1 has no long-name line"},
		},
		{
			name: "a repeated index keeps the first entry",
			in:   " 0 [A              ]: d - first\n                      one\n 0 [B              ]: d - second\n                      two\n",
			want: []Card{{Index: 0, ID: "A", Driver: "d", Name: "first", LongName: "one"}},
			probs: []string{
				"line 3: card 0 is listed twice; the first entry is kept",
				"line 4: a long-name line with no card before it: \"                      two\"",
			},
		},
		{
			name: "garbage between entries",
			in:   "xyzzy\n 3 [C              ]: drv\n                      long\n",
			want: []Card{{Index: 3, ID: "C", Driver: "drv", LongName: "long"}},
			probs: []string{
				"line 1: not a card entry: \"xyzzy\"",
				"line 2: card 3: no \" - \" between the driver and the name in \"drv\"",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cards, probs := ParseCards([]byte(tc.in))
			if !reflect.DeepEqual(cards, tc.want) {
				t.Errorf("cards = %+v\nwant    %+v", cards, tc.want)
			}
			if tc.probs == nil {
				tc.probs = []string{}
			}
			if !reflect.DeepEqual(probs, tc.probs) {
				t.Errorf("problems = %q\nwant       %q", probs, tc.probs)
			}
		})
	}
}

// TestParseCardsBoundsItsProblems feeds a file that fails on every line:
// the problems stop at maxProblems and then count the rest, and none
// quotes more than maxQuoted bytes of its line.
func TestParseCardsBoundsItsProblems(t *testing.T) {
	in := strings.Repeat("x"+strings.Repeat("y", 500)+"\n", 100)
	cards, probs := ParseCards([]byte(in))
	if len(cards) != 0 {
		t.Errorf("cards from garbage: %+v", cards)
	}
	if len(probs) != maxProblems+1 || probs[maxProblems] != "92 more not shown" {
		t.Fatalf("problems = %d entries ending %q, want %d and \"92 more not shown\"", len(probs), probs[len(probs)-1], maxProblems+1)
	}
	for _, p := range probs {
		if len(p) > 2*maxQuoted+64 {
			t.Errorf("a problem of %d bytes quotes more than its bound: %.80q…", len(p), p)
		}
	}
}
