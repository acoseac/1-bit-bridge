package alsapcm

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestTheCardsOpenSubstreamsAreReadFromTheirStatus pins the reading the
// probe's Linux seam takes (openSubstreamsIn, B728 B4 round 2) on a tree laid
// out as /proc/asound is: each pcm<D><p|c>/sub<S> whose status file's first
// line is anything but "closed" is open, playback and capture alike, an
// error line among them; a closed one, an entry that is no PCM device's or
// substream's, a substream whose status is gone, and another card's are
// not; and a card that is not there is the reading's error.
func TestTheCardsOpenSubstreamsAreReadFromTheirStatus(t *testing.T) {
	root := t.TempDir()
	for rel, data := range map[string]string{
		"card5/pcm0p/sub0/status":    "state: RUNNING\nowner_pid   : 6584\ntrigger_time: 1000.000000000\n",
		"card5/pcm0p/sub1/status":    "closed\n",
		"card5/pcm0c/sub0/status":    "state: OPEN\nowner_pid   : 7001\n",
		"card5/pcm1p/sub0/status":    "error -19\n",
		"card5/pcm1p/info":           "card: 5\ndevice: 1\n",
		"card5/pcm2p/sub0/hw_params": "closed\n",
		"card5/pcmXp/sub0/status":    "state: RUNNING\n",
		"card5/pcm3p/subA/status":    "state: RUNNING\n",
		"card5/stream0":              "FiiO QX13\n",
		"card6/pcm0p/sub0/status":    "state: RUNNING\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := openSubstreamsIn(root, 5)
	if err != nil {
		t.Fatalf("openSubstreamsIn: %v", err)
	}
	if want := []string{"pcm0c/sub0", "pcm0p/sub0", "pcm1p/sub0"}; !slices.Equal(got, want) {
		t.Errorf("card 5's open substreams: %q; want %q", got, want)
	}
	if _, err := openSubstreamsIn(root, 7); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a card that is not there: %v; want the reading's error", err)
	}
}
