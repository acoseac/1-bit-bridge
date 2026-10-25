package playback

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/player/decode"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// fixtureFiles is a Resolver over named files' bytes.
type fixtureFiles map[string][]byte

func (f fixtureFiles) Open(_ context.Context, ref engine.Reference) (Content, error) {
	b, ok := f[ref.String()]
	if !ok {
		return Content{}, errors.New("no such fixture")
	}
	return Content{Data: bytes.NewReader(b), Size: int64(len(b))}, nil
}

// noUnit is a Locator a test that never opens a device passes.
type noUnit struct{}

func (noUnit) Locate(context.Context) (Unit, error) {
	return Unit{}, errors.New("no unit in this test")
}

// oneOfEachContainer is a committed golden of every container the
// decoders reconstruct from, by the container decode reads in it.
var oneOfEachContainer = map[format.Container]string{
	format.ContainerFLAC: filepath.Join("goldens", "decorrelated_16bit_44100.flac"),
	format.ContainerWAV:  filepath.Join("goldens", "decorrelated_16bit_44100.wav"),
	format.ContainerAIFF: filepath.Join("goldens", "decorrelated_16bit_44100.aiff"),
	format.ContainerALAC: filepath.Join("alac-goldens", "afconvert_16bit_44100.m4a"),
	format.ContainerDSF:  filepath.Join("dsd-goldens", "lsb_dsd64_whole_blocks.dsf"),
	format.ContainerDFF:  filepath.Join("dsd-goldens", "dsd64.dff"),
}

// TestTheContainersThePlayerPlaysAreTheOnesItsOpenTakes pins the one source
// of GET /v1/outputs' codecs for kind bridge (B470 PR 5b): PlayedContainers
// is flac, wav, aiff and alac, Plays says the same of every container, and
// the open's own refusal agrees on real content of each container
// format.Containers names: content of a container the list holds is never
// refused as dsd_not_in_this_release, and content of any other is, before
// any of it plays.
func TestTheContainersThePlayerPlaysAreTheOnesItsOpenTakes(t *testing.T) {
	want := []format.Container{format.ContainerFLAC, format.ContainerWAV, format.ContainerAIFF, format.ContainerALAC}
	if got := PlayedContainers(); !slices.Equal(got, want) {
		t.Errorf("PlayedContainers() = %q, want %q", got, want)
	}
	files := fixtureFiles{}
	for c, rel := range oneOfEachContainer {
		b, err := os.ReadFile(filepath.Join("..", "decode", "testdata", rel))
		if err != nil {
			t.Fatal(err)
		}
		files[string(c)] = b
	}
	b, err := New(Config{Resolver: files, Locator: noUnit{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range format.Containers() {
		if _, ok := oneOfEachContainer[c]; !ok {
			t.Errorf("no fixture of container %s: the cross-check does not cover it", c)
			continue
		}
		listed := slices.Contains(PlayedContainers(), c)
		if Plays(c) != listed {
			t.Errorf("Plays(%s) = %v, and PlayedContainers lists it: %v", c, Plays(c), listed)
		}
		ref, err := engine.NewReference(string(c))
		if err != nil {
			t.Fatal(err)
		}
		o := &occ{id: 1, entry: 1, ref: ref, done: make(chan struct{})}
		b.open(context.Background(), o)
		refusedDSD := o.refused != nil && o.refused.Reason == ReasonDSD
		switch {
		case listed && o.refused != nil:
			t.Errorf("%s, which the player lists, was refused at its open: %+v", c, *o.refused)
		case !listed && !refusedDSD:
			t.Errorf("%s, which the player does not list, was not refused as %s: %+v", c, ReasonDSD, o.refused)
		}
		if listed {
			if got := o.pcm.Container; got != c {
				t.Errorf("the fixture of %s decoded as %s", c, got)
			}
		}
		if o.dec != nil {
			_ = o.dec.Close()
		}
	}
	// What decode refuses before the backend asks (lossy, DST) is no
	// container the player lists: it never has one.
	aac, err := os.ReadFile(filepath.Join("..", "decode", "testdata", "alac-goldens", "afconvert_aac_44100.m4a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decode.Open(bytes.NewReader(aac)); err == nil {
		t.Error("decode opened an AAC .m4a: the player's alac would then name AAC too")
	}
}
