package alsa

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Capabilities is what one USB stream's playback altsets can be fed.
// PCM numbers come only from altsets that name a PCM format. A native
// DSD rate is that altset's frame rate times the format's width: 32 for
// DSD_U32_*, 16 for DSD_U16_*, 8 for DSD_U8. A DSD format this does not
// know a width for is listed and adds no rate. Lists are empty, never
// nil.
type Capabilities struct {
	MaxPCMRate     int
	PCMBitDepths   []int
	NativeDSD      bool
	NativeDSDRates []int
	DSDFormats     []string
}

// pcmFormatDepth matches a PCM sample format's significant bits. S24_3LE
// is 24. DSD_ and SPECIAL do not match: they do not start with S or U.
var pcmFormatDepth = regexp.MustCompile(`^[SU](\d+)`)

// CapabilitiesFromAltsets derives a stream's playback capabilities from
// every altset. A DSD altset's Bits line is not a PCM depth, and its
// frame rates are not PCM rates.
func CapabilitiesFromAltsets(altsets []Altset) Capabilities {
	out := Capabilities{
		PCMBitDepths:   []int{},
		NativeDSDRates: []int{},
		DSDFormats:     []string{},
	}
	depths := map[int]struct{}{}
	rates := map[int]struct{}{}
	seenFormat := map[string]struct{}{}
	for _, a := range altsets {
		dsd := a.DSDFormat()
		pcm := false
		for _, name := range a.Formats {
			m := pcmFormatDepth.FindStringSubmatch(name)
			if m == nil {
				continue
			}
			n, err := strconv.Atoi(m[1])
			if err != nil || n <= 0 {
				continue
			}
			pcm = true
			depths[n] = struct{}{}
		}
		frames := altsetFrameRates(a)
		if pcm {
			for _, r := range frames {
				if r > out.MaxPCMRate {
					out.MaxPCMRate = r
				}
			}
		}
		if dsd == "" {
			continue
		}
		out.NativeDSD = true
		if _, ok := seenFormat[dsd]; !ok {
			seenFormat[dsd] = struct{}{}
			out.DSDFormats = append(out.DSDFormats, dsd)
		}
		for _, frame := range frames {
			if native, ok := NativeDSDRate(dsd, frame); ok {
				rates[native] = struct{}{}
			}
		}
	}
	for n := range depths {
		out.PCMBitDepths = append(out.PCMBitDepths, n)
	}
	for n := range rates {
		out.NativeDSDRates = append(out.NativeDSDRates, n)
	}
	slices.Sort(out.PCMBitDepths)
	slices.Sort(out.NativeDSDRates)
	return out
}

// altsetFrameRates is the altset's discrete rates plus both ends of a
// continuous range. A zero is not a rate.
func altsetFrameRates(a Altset) []int {
	var out []int
	for _, r := range a.Rates {
		if r > 0 {
			out = append(out, r)
		}
	}
	if a.RateRange != nil {
		if a.RateRange.Min > 0 {
			out = append(out, a.RateRange.Min)
		}
		if a.RateRange.Max > 0 {
			out = append(out, a.RateRange.Max)
		}
	}
	return out
}

// NativeDSDRate converts one altset frame rate into a native DSD rate.
// The kernel lists the USB frame rate; the DSD rate is that times the
// number of DSD bits packed into a frame. A format whose width this does
// not know returns false, so the caller does not invent a multiplier.
func NativeDSDRate(format string, frameRate int) (int, bool) {
	if frameRate <= 0 {
		return 0, false
	}
	var mul int
	switch {
	case strings.HasPrefix(format, "DSD_U32_"):
		mul = 32
	case strings.HasPrefix(format, "DSD_U16_"):
		mul = 16
	case format == "DSD_U8" || strings.HasPrefix(format, "DSD_U8_"):
		mul = 8
	default:
		return 0, false
	}
	if frameRate > int(^uint(0)>>1)/mul {
		return 0, false
	}
	return frameRate * mul, true
}

// devicePlugins are the ALSA plugin names whose device string names a
// card directly. A bare PCM name is not one of these; ResolveOutputDevice
// follows that name through asound.conf, and does not guess a card.
var devicePlugins = map[string]struct{}{
	"hw": {}, "plughw": {}, "plug": {}, "front": {},
}

var deviceByCard = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*):CARD=([^,]+)(?:,DEV=(\d+))?$`)
var deviceByIndex = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*):(\d+)(?:,(\d+))?$`)
var deviceByID = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*):([A-Za-z][\w.-]*)(?:,(\d+))?$`)

// ResolveALSADevice maps an MPD audio_output device string onto a card
// and a PCM device. ok is false when the string does not name a card, the
// card is not in the list, or more than one card has that id. A match on
// a card id is exact: Audio does not match Audio2.
func ResolveALSADevice(device string, cards []Card) (cardIndex, pcmDevice int, ok bool) {
	device = strings.TrimSpace(device)
	if device == "" {
		return 0, 0, false
	}
	if m := deviceByCard.FindStringSubmatch(device); m != nil {
		if _, allowed := devicePlugins[m[1]]; !allowed {
			return 0, 0, false
		}
		pcm, good := atoiOptional(m[3])
		if !good {
			return 0, 0, false
		}
		idx, found := uniqueCardID(cards, m[2])
		if !found {
			return 0, 0, false
		}
		return idx, pcm, true
	}
	if m := deviceByIndex.FindStringSubmatch(device); m != nil {
		if _, allowed := devicePlugins[m[1]]; !allowed {
			return 0, 0, false
		}
		idx, err := strconv.Atoi(m[2])
		if err != nil || !cardIndexExists(cards, idx) {
			return 0, 0, false
		}
		pcm, good := atoiOptional(m[3])
		if !good {
			return 0, 0, false
		}
		return idx, pcm, true
	}
	if m := deviceByID.FindStringSubmatch(device); m != nil {
		if _, allowed := devicePlugins[m[1]]; !allowed {
			return 0, 0, false
		}
		pcm, good := atoiOptional(m[3])
		if !good {
			return 0, 0, false
		}
		idx, found := uniqueCardID(cards, m[2])
		if !found {
			return 0, 0, false
		}
		return idx, pcm, true
	}
	return 0, 0, false
}

func atoiOptional(s string) (int, bool) {
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func cardIndexExists(cards []Card, idx int) bool {
	for _, c := range cards {
		if c.Index == idx {
			return true
		}
	}
	return false
}

func uniqueCardID(cards []Card, id string) (int, bool) {
	found := -1
	for _, c := range cards {
		if c.ID != id {
			continue
		}
		if found >= 0 {
			return 0, false
		}
		found = c.Index
	}
	if found < 0 {
		return 0, false
	}
	return found, true
}
