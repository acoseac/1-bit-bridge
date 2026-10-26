package alsapcm

import (
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// allParams is an rmask that asks the kernel to refine every parameter.
const allParams = ^uint32(0)

// anyParams returns hw_params that allow everything: every mask full, every
// interval 0 to the largest value, as alsa-lib's snd_pcm_hw_params_any
// starts a negotiation.
func anyParams() hwParams {
	var hw hwParams
	for i := range hw.masks {
		for j := range hw.masks[i].bits {
			hw.masks[i].bits[j] = ^uint32(0)
		}
	}
	for i := range hw.intervals {
		hw.intervals[i] = interval{min: 0, max: ^uint32(0)}
	}
	hw.rmask = allParams
	hw.info = ^uint32(0)
	return hw
}

// maskOf is the mask of parameter p, one of hwParamAccess…hwParamSubformat.
func (hw *hwParams) maskOf(p int) *mask { return &hw.masks[p-hwParamFirstMask] }

// intervalOf is the interval of parameter p, one of hwParamSampleBits…
// hwParamTickTime.
func (hw *hwParams) intervalOf(p int) *interval { return &hw.intervals[p-hwParamFirstInterval] }

// setMask leaves v alone in parameter p's mask.
func (hw *hwParams) setMask(p int, v uint32) {
	m := hw.maskOf(p)
	*m = mask{}
	m.bits[v/32] = 1 << (v % 32)
}

// setInteger makes parameter p exactly the integer v.
func (hw *hwParams) setInteger(p int, v uint32) {
	*hw.intervalOf(p) = interval{min: v, max: v, flags: intervalInteger}
}

// setRange makes parameter p any integer from lo to hi.
func (hw *hwParams) setRange(p int, lo, hi uint32) {
	*hw.intervalOf(p) = interval{min: lo, max: hi, flags: intervalInteger}
}

// values lists the values a mask holds, ascending.
func (m mask) values() []uint32 {
	var out []uint32
	for i, w := range m.bits {
		for w != 0 {
			out = append(out, uint32(i*32+bits.TrailingZeros32(w)))
			w &= w - 1
		}
	}
	return out
}

// single returns the value a mask holds when it holds exactly one.
func (m mask) single() (uint32, bool) {
	v := m.values()
	if len(v) != 1 {
		return 0, false
	}
	return v[0], true
}

// lo is the least integer an interval holds, its open end excluded.
func (iv interval) lo() uint32 {
	if iv.flags&intervalOpenMin != 0 && iv.min < ^uint32(0) {
		return iv.min + 1
	}
	return iv.min
}

// hi is the greatest integer an interval holds, its open end excluded.
func (iv interval) hi() uint32 {
	if iv.flags&intervalOpenMax != 0 && iv.max > 0 {
		return iv.max - 1
	}
	return iv.max
}

// exact returns the value of an interval the kernel committed: closed, not
// empty, its two ends equal. The kernel's own runtime takes the minimum
// (params_rate and its siblings), so a committed value is never read any
// other way.
func (iv interval) exact() (uint32, bool) {
	if iv.flags&(intervalOpenMin|intervalOpenMax|intervalEmpty) != 0 || iv.min != iv.max {
		return 0, false
	}
	return iv.min, true
}

// raw renders an interval as the kernel holds it, its ends and whether each
// is open: "[44100, 44100]", "(44099, 44100]". A read-back names what was
// committed this way, because the kernel's own runtime takes the minimum
// whatever the flags say.
func (iv interval) raw() string {
	open, end := "[", "]"
	if iv.flags&intervalOpenMin != 0 {
		open = "("
	}
	if iv.flags&intervalOpenMax != 0 {
		end = ")"
	}
	s := fmt.Sprintf("%s%d, %d%s", open, iv.min, iv.max, end)
	if iv.flags&intervalEmpty != 0 {
		s += " empty"
	}
	return s
}

// String renders an interval as the integers it holds: "44100..384000",
// or the one value.
func (iv interval) String() string {
	if iv.flags&intervalEmpty != 0 || iv.lo() > iv.hi() {
		return "none"
	}
	if iv.lo() == iv.hi() {
		return strconv.FormatUint(uint64(iv.lo()), 10)
	}
	return fmt.Sprintf("%d..%d", iv.lo(), iv.hi())
}

// sampleFormat is an ALSA format this backend packs: its snd_pcm_format_t
// and the bytes one sample takes.
type sampleFormat struct {
	code  uint32
	bytes int
}

// sampleFormats are the formats of the design's PCM table, each the
// format package's packer writes.
var sampleFormats = map[format.ALSAFormat]sampleFormat{
	format.S16LE:  {code: formatS16LE, bytes: 2},
	format.S243LE: {code: formatS243LE, bytes: 3},
	format.S24LE:  {code: formatS24LE, bytes: 4},
	format.S32LE:  {code: formatS32LE, bytes: 4},
}

// formatName names an ALSA format code this backend packs, or numbers one
// it does not.
func formatName(code uint32) string {
	for name, f := range sampleFormats {
		if f.code == code {
			return string(name)
		}
	}
	return "format " + strconv.FormatUint(uint64(code), 10)
}

// formatsOf renders a format mask: the formats this backend packs by name,
// then the others by number.
func formatsOf(m mask) string {
	var known, other []string
	for _, v := range m.values() {
		// A refine leaves the bits past the last format the kernel defines
		// as the request set them; they are not formats (the GO bar's
		// refusal listed 64 to 255).
		if v > formatLast {
			break
		}
		name := formatName(v)
		if strings.HasPrefix(name, "format ") {
			other = append(other, strconv.FormatUint(uint64(v), 10))
		} else {
			known = append(known, name)
		}
	}
	out := strings.Join(known, " ")
	if len(known) == 0 {
		out = "none of S16_LE S24_3LE S24_LE S32_LE"
	}
	if len(other) > 0 {
		out += " (and formats " + strings.Join(other, ", ") + ", which this backend does not pack)"
	}
	return out
}

// framesIn is how many frames at rate fill d, rounded down, at most the
// largest uint32.
func framesIn(rate uint32, d time.Duration) uint32 {
	n := uint64(rate) * uint64(d) / uint64(time.Second)
	if n > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(n)
}

// clamp limits v to lo..hi.
func clamp(v, lo, hi uint32) uint32 {
	return min(max(v, lo), hi)
}
