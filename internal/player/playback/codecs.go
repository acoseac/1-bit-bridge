package playback

import (
	"slices"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// Plays reports whether this release's backend plays content in container
// c as PCM it decodes: every container a decoder reconstructs PCM from
// (FLAC, WAV, AIFF, ALAC in M4A), and no DSD container (DSF, DFF), whose
// content opens as DSD and plays native, as DoP or converted to PCM by the
// effective DSD policy, or is refused by its word (B728); what GET
// /v1/outputs lists beside these while that policy plays a DSD rate is
// PlayerPolicy.PlaysDSD's. It is a container's question alone: content of a
// container it takes may still be refused for what it holds (float PCM, a
// rate, a depth, a transport the DAC does not take), and a lossy file has no
// container here (decode refuses it first). B470 PR 5b.
func Plays(c format.Container) bool {
	return slices.Contains(format.Containers(), c) && !c.IsDSD()
}

// PlayedContainers is every container Plays takes, in format.Containers'
// order: GET /v1/outputs' codecs for the bridge's own player (kind bridge),
// which cmd/bridge hands the route.
func PlayedContainers() []format.Container {
	var out []format.Container
	for _, c := range format.Containers() {
		if Plays(c) {
			out = append(out, c)
		}
	}
	return out
}
