package manifest

import (
	"slices"
	"testing"
)

// TestAVolumeRootIsOneElementHoweverACleanPathSpellsIt: a path's volume and
// its leading separator are its first element, which every comparison of a
// path with a root takes whole (caseLook.under): a drive's root, C:\, or a
// share's, \\server\share\, which a clean path spells with its separator or
// without it (filepath.Clean leaves \\server\share bare). A root configured
// at a share's root without its separator held nothing below it, whose paths
// spell the element with one (B651's ninth round, Codex on round 8). C:
// alone, a drive's current folder, which filepath.Clean spells C:., stays
// apart from C:\. The volumes are handed to volumeElements, pathElements'
// logic, as Windows' filepath splits them, so every host runs this; on
// Windows, TestAVolumeRootHoldsThePathsBelowIt asks pathElements itself.
func TestAVolumeRootIsOneElementHoweverACleanPathSpellsIt(t *testing.T) {
	for _, c := range []struct {
		vol, rest string
		sep       byte
		want      []string
	}{
		{`\\server\share`, ``, '\\', []string{`\\server\share\`}},
		{`\\server\share`, `\`, '\\', []string{`\\server\share\`}},
		{`\\server\share`, `\Album`, '\\', []string{`\\server\share\`, `Album`}},
		{`\\server\share`, `\Album\Disc 1`, '\\', []string{`\\server\share\`, `Album`, `Disc 1`}},
		{`\\?\UNC\server\share`, ``, '\\', []string{`\\?\UNC\server\share\`}},
		{`C:`, `\`, '\\', []string{`C:\`}},
		{`C:`, `\Album`, '\\', []string{`C:\`, `Album`}},
		{`C:`, `.`, '\\', []string{`C:`}},
		{`C:`, `Album`, '\\', []string{`C:`, `Album`}},
		{``, `/`, '/', []string{`/`}},
		{``, `/music/usb`, '/', []string{`/`, `music`, `usb`}},
		{``, `music/usb`, '/', []string{`music`, `usb`}},
	} {
		if got := volumeElements(c.vol, c.rest, c.sep); !slices.Equal(got, c.want) {
			t.Errorf("the elements of %q and %q are %q, want %q", c.vol, c.rest, got, c.want)
		}
	}
}
