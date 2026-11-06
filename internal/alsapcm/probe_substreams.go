package alsapcm

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The silent probe's look at the card's other substreams (B728 B4 round 2).
// A USB device's interfaces can share a clock, and while one of them runs
// snd-usb-audio holds every interface on that clock to its rate (its rate
// rule asks the clock's running rate), so a refine beside it refuses, with
// EINVAL, rates the DAC takes. A probe therefore reads which of the card's
// substreams are open, before its first refine and again before its close,
// and keeps no refusal from a run beside another.

// asoundRoot is where the kernel lists its cards' PCM substreams.
const asoundRoot = "/proc/asound"

// The entries of a card's /proc/asound directory a reading of its open
// substreams walks: each PCM device's directory, playback (pcm<D>p) and
// capture (pcm<D>c), and each of its substreams (sub<S>).
var (
	pcmDirName = regexp.MustCompile(`^pcm[0-9]+[pc]$`)
	subDirName = regexp.MustCompile(`^sub[0-9]+$`)
)

// maxStatusBytes bounds a read of a substream's status file, a dozen short
// lines.
const maxStatusBytes = 4096

// openSubstreamsIn reads root/card<card> as the kernel lists its PCM
// substreams: each pcm<D><p|c>/sub<S> whose status file's first line is
// anything but "closed", which is what the kernel writes for a substream
// with no runtime, is open, named "pcm<D><p|c>/sub<S>". A device or a
// substream gone by the time its directory or its status is read was not
// open; any other failure is the reading's error. A kernel built without
// the verbose ALSA procfs lists no substream at all, which the probe tells
// by its own being absent (sharedWith).
func openSubstreamsIn(root string, card int) ([]string, error) {
	dir := filepath.Join(root, "card"+strconv.Itoa(card))
	pcms, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var open []string
	for _, p := range pcms {
		if !pcmDirName.MatchString(p.Name()) {
			continue
		}
		subs, err := os.ReadDir(filepath.Join(dir, p.Name()))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, sub := range subs {
			if !subDirName.MatchString(sub.Name()) {
				continue
			}
			first, err := firstLine(filepath.Join(dir, p.Name(), sub.Name(), "status"))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if first != "closed" {
				open = append(open, p.Name()+"/"+sub.Name())
			}
		}
	}
	return open, nil
}

// firstLine is a small file's first line, trimmed, read at most
// maxStatusBytes.
func firstLine(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxStatusBytes))
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line), nil
}

// sharedWith reads the card's open substreams and says why a refusal of the
// probe's run cannot be kept: each open substream but own, the probe's own,
// by its name; or that the reading could not be taken, or did not show own
// open (a kernel whose /proc lists no substream), so that it cannot tell.
// Nothing said means the probe's own was the card's only open substream.
func sharedWith(s sys, card int, own string) []string {
	open, err := s.openSubstreams(card)
	if err != nil {
		return []string{"the card's substreams could not be read: " + err.Error()}
	}
	var out []string
	seen := false
	for _, name := range open {
		if name == own {
			seen = true
			continue
		}
		out = append(out, name)
	}
	if !seen {
		out = append(out, "the card's substreams did not show the probe's own, "+own+", open")
	}
	return out
}
