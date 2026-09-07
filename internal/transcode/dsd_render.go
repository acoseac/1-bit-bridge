package transcode

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// DSD → PCM renditions.
//
// A DSD source (DSF / DSDIFF) cannot be played by anything that is not a
// DoP-capable DAC on a wired route. The iOS app converts on the phone when
// it has to; these renditions let the bridge do that decode ONCE and serve
// the result as an ordinary FLAC sidecar, in two tiers:
//
//	optimized-dsd-<schema>-<44100|48000>-16   compact: CarPlay / wireless / speaker
//	pcm-<schema>-<176400|192000>-24           faithful: a wired DAC that cannot take this DSD rate
//
// The compact tier rides the existing optimize family — every `optimized-`
// prefix check on both sides admits it unchanged — with a `-dsd` infix so
// the DSD recipe can be versioned independently of the PCM optimize recipe
// (see DSDRenditionSchemaVersion). The faithful tier is a NEW family: it is
// decimation, never upscaling, and `upscaled-` is a taken prefix with its
// own client-side semantics.
//
// Nothing here decodes. The two-stage chain lives in RunSox; this file is
// identity, targets and eligibility, all pure.

// VariantPrefixPCM is the faithful-tier family prefix (no trailing dash;
// the ID convention is `<prefix>-<schema>-<rate>-<bits>`).
const VariantPrefixPCM = "pcm"

// VariantPrefixOptimizedDSD is the compact tier's prefix. It deliberately
// STARTS WITH VariantPrefixOptimized so `HasPrefix("optimized-")` and
// `LIKE 'optimized-%'` — the routing checks on both repos — admit it with
// no change; the `-dsd` infix is what lets the recipe version on its own.
const VariantPrefixOptimizedDSD = VariantPrefixOptimized + "-dsd"

// DSDRenditionSchemaVersion versions the DSD decode recipe (ffmpeg decode
// at unity → sox rate [→ sinc] → true-peak-measured clip-guarded gain →
// FLAC). It is INDEPENDENT of VariantSchemaVersion on purpose: a change to
// the decimation filter after the alias measurement must not invalidate
// every PCM optimize on every bridge, and vice versa. Bump this, not that,
// when the DSD chain changes what it emits.
const DSDRenditionSchemaVersion = "v1"

// JobKindPCMRender is the faithful-tier job kind. Wire value "pcm" is what
// POST /v1/upscale, the batch and delete endpoints and the CLI carry.
const JobKindPCMRender JobKind = "pcm"

// TargetRateForPCMRender picks the faithful rendition's rate: the DSD
// family's 4× base (176.4 kHz for the 44.1k family, 192 kHz for the 48k
// family). Nothing is ever picked for an off-family rate — a DSD header
// whose rate is not a multiple of either base is a header we do not trust,
// and rendering it at a guessed rate produces a sidecar at the wrong pitch.
// Returns 0 for such a rate (ineligible), which every caller treats as
// "skip", never as "render at 0 Hz".
func TargetRateForPCMRender(dsdRate int) int {
	if dsdRate <= 0 {
		return 0
	}
	if dsdRate%44100 == 0 {
		return 176400
	}
	if dsdRate%48000 == 0 {
		return 192000
	}
	return 0
}

// ResolveTargetRateForPCMRender is the checked form — the sibling of
// ResolveTargetRateForOptimize for the faithful tier.
func ResolveTargetRateForPCMRender(dsdRate int) (int, error) {
	if dsdRate <= 0 {
		return 0, fmt.Errorf("DSD source rate must be positive, got %d", dsdRate)
	}
	t := TargetRateForPCMRender(dsdRate)
	if t == 0 {
		return 0, fmt.Errorf("DSD source rate %d Hz is in neither the 44.1k nor the 48k family", dsdRate)
	}
	return t, nil
}

// DSDRenderCaps folds policy and capability into the one value every DSD
// gate receives, so a gate cannot consult one without the other:
//
//   - Enabled   — the operator flag (upscale.dsdRender.enabled). Default
//     OFF: flipping it starts a sweep that reads the whole DSD library and
//     adds a toolchain requirement, so it stays an explicit act.
//   - DecodeDSD — ffmpeg carries all four dsd_* decoders (FFmpegInfo.HasDSD).
//   - DecodeDST — ffmpeg carries the dst decoder; without it DST-compressed
//     DSDIFF is ineligible rather than a mid-job failure.
//
// The zero value grants nothing, which is what an unwired call site gets.
type DSDRenderCaps struct {
	Enabled   bool
	DecodeDSD bool
	DecodeDST bool
}

// Active reports whether DSD renditions can be produced at all.
func (c DSDRenderCaps) Active() bool {
	return c.Enabled && c.DecodeDSD
}

// IsDSDSource reports whether a source is a DSD container the render
// pipeline knows: codec "DSF" / "DFF" (the extractors' stamps), with the
// on-disk extension as the fallback for legacy rows scanned before the
// codec column was populated — the same fallback OptimizeEligible carries.
func IsDSDSource(codec, sourcePath string) bool {
	switch strings.ToUpper(strings.TrimSpace(codec)) {
	case "DSF", "DFF":
		return true
	case "":
		ext := strings.ToLower(filepath.Ext(sourcePath))
		return ext == ".dsf" || ext == ".dff"
	default:
		return false
	}
}

// DSDRenderEligible is the Go half of the DSD eligibility rule; the SQL
// half is manifest's dsdRenderEligibleSQL and the two are held in lockstep
// by the admin eligibility test. A source qualifies when every term holds:
//
//   - caps.Active(): the flag is on AND ffmpeg decodes DSD;
//   - isDSD (the manifest's own flag) AND IsDSDSource (codec / extension) —
//     both, so a forged isDSD on an MP3 row cannot reach the decoder;
//   - the rate is in a family TargetRateForPCMRender knows (the compact
//     tier's TargetRateForOptimize is total, so this is the stricter of the
//     two and is applied to both — an off-family DSD header is refused
//     outright);
//   - DST-compressed (compression == "DST") only when caps.DecodeDST;
//   - not an SACD virtual track (`<iso>/st/NN.dff`): the bridge has no
//     demuxer for those, and handing the container to ffmpeg renders the
//     whole disc image, not the track.
func DSDRenderEligible(sourcePath, codec string, isDSD bool, sourceRate int, compression string, caps DSDRenderCaps) bool {
	if !caps.Active() {
		return false
	}
	if !isDSD || !IsDSDSource(codec, sourcePath) {
		return false
	}
	if TargetRateForPCMRender(sourceRate) == 0 {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(compression), "DST") && !caps.DecodeDST {
		return false
	}
	if manifest.IsSACDVirtualPath(sourcePath) {
		return false
	}
	return true
}

// PCMRenderEligible is the faithful tier's gate. It is DSDRenderEligible by
// another name today; it exists as its own symbol so the two tiers can
// diverge (a rate cap, say) without touching every call site.
func PCMRenderEligible(sourcePath, codec string, isDSD bool, sourceRate int, compression string, caps DSDRenderCaps) bool {
	return DSDRenderEligible(sourcePath, codec, isDSD, sourceRate, compression, caps)
}

// OptimizeEligibleFor is the compact tier's gate: the PCM rule
// (OptimizeEligible, unchanged and still exported for the three gates and
// the lockstep test that call it) OR the DSD rule. A DSD source is eligible
// for a compact rendition whenever it is eligible for a faithful one — the
// compact tier is strictly cheaper.
func OptimizeEligibleFor(sourcePath, codec string, sourceRate, sourceBits int, isDSD bool, compression string, caps DSDRenderCaps) bool {
	if OptimizeEligible(sourcePath, codec, sourceRate, sourceBits) {
		return true
	}
	return DSDRenderEligible(sourcePath, codec, isDSD, sourceRate, compression, caps)
}

// The DSD variant-ID memos. Same shape and same reason as optimizeIDCache:
// VariantID is on the manifest-scan hot path, the rate × bits cross-product
// is tiny, and each family keeps its OWN map so a lookup can never hand one
// family the other's string.
var (
	dsdIDCacheOnce      sync.Once
	optimizedDSDIDCache map[[2]int]string
	pcmIDCache          map[[2]int]string
)

func initDSDIDCaches() {
	optimizedDSDIDCache = map[[2]int]string{
		{44100, 16}: fmt.Sprintf("%s-%s-44100-16", VariantPrefixOptimizedDSD, DSDRenditionSchemaVersion),
		{48000, 16}: fmt.Sprintf("%s-%s-48000-16", VariantPrefixOptimizedDSD, DSDRenditionSchemaVersion),
	}
	pcmIDCache = map[[2]int]string{
		{176400, 24}: fmt.Sprintf("%s-%s-176400-24", VariantPrefixPCM, DSDRenditionSchemaVersion),
		{192000, 24}: fmt.Sprintf("%s-%s-192000-24", VariantPrefixPCM, DSDRenditionSchemaVersion),
	}
}

func lookupCachedOptimizedDSDVariantID(rate, bits int) (string, bool) {
	dsdIDCacheOnce.Do(initDSDIDCaches)
	if rate < 0 || bits < 0 {
		return "", false
	}
	id, ok := optimizedDSDIDCache[[2]int{rate, bits}]
	return id, ok
}

func lookupCachedPCMVariantID(rate, bits int) (string, bool) {
	dsdIDCacheOnce.Do(initDSDIDCaches)
	if rate < 0 || bits < 0 {
		return "", false
	}
	id, ok := pcmIDCache[[2]int{rate, bits}]
	return id, ok
}
