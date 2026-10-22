package alsa

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// writeTree lays files out under root, by their slash-separated paths.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// copyTree copies a test case's /proc tree into a temporary directory, so
// a test can change one file of a real capture.
func copyTree(t *testing.T, name string) string {
	t.Helper()
	src, dst := procRootOf(name), t.TempDir()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		writeTree(t, dst, map[string]string{filepath.ToSlash(rel): string(b)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// strPtr returns a pointer to its argument.
func strPtr(s string) *string { return &s }

// mpdOwner is the owner every playing capture's substream resolves to:
// MPD's output thread, 6584, in the mpd process, 6551.
var mpdOwner = &Owner{TID: 6584, Thread: strPtr("output:alsa"), PID: intPtr(6551), Process: strPtr("mpd")}

// TestReaderOnEveryCapture reads each captured tree whole: the three cards,
// the DAC's USB id, its one device with stream0 and one substream, and the
// owner of a substream that plays.
func TestReaderOnEveryCapture(t *testing.T) {
	for _, c := range captures {
		t.Run(c.name, func(t *testing.T) {
			snap, err := Reader{ProcRoot: procRootOf(c.name)}.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.Problems) != 0 {
				t.Errorf("snapshot problems: %q", snap.Problems)
			}
			if len(snap.Cards) != 3 {
				t.Fatalf("%d cards, want 3", len(snap.Cards))
			}
			for _, hdmi := range snap.Cards[:2] {
				// Only card 5's files were captured: the HDMI cards are in
				// the cards file and have no directory in the tree.
				if hdmi.USBID != nil || len(hdmi.Devices) != 0 || len(hdmi.Problems) != 0 {
					t.Errorf("card %d = %+v; want no USB id, no device and no problem", hdmi.Index, hdmi)
				}
			}
			dac := snap.Cards[2]
			if dac.Index != 5 || dac.USBID == nil || *dac.USBID != (USBID{0x20b1, 0x3009}) || len(dac.Problems) != 0 {
				t.Fatalf("card 5 = %+v (usb id %v, problems %q)", dac.Card, dac.USBID, dac.Problems)
			}
			if len(dac.Devices) != 1 || dac.Devices[0].Device != 0 || dac.Devices[0].Stream == nil || len(dac.Devices[0].Substreams) != 1 {
				t.Fatalf("card 5's devices = %+v; want device 0 with its stream and one substream", dac.Devices)
			}
			d := dac.Devices[0]
			if p := d.Stream.Playback; p == nil || (p.Running != nil) != c.running {
				t.Errorf("stream0's playback = %+v; want it running: %v", p, c.running)
			}
			s := d.Substreams[0]
			if s.Subdevice != 0 || s.HWParams == nil || s.Status == nil {
				t.Fatalf("substream = %+v", s)
			}
			if !c.running {
				if !s.HWParams.Closed || !s.Status.Closed || s.Owner != nil {
					t.Errorf("idle substream = hw %+v, status %+v, owner %+v; want closed with no owner", s.HWParams, s.Status, s.Owner)
				}
				return
			}
			if s.HWParams.Format != c.format || deref(s.HWParams.Rate) != c.rate || s.Status.State != "RUNNING" {
				t.Errorf("substream = %s at %d, %s; want %s at %d, RUNNING", s.HWParams.Format, deref(s.HWParams.Rate), s.Status.State, c.format, c.rate)
			}
			if !reflect.DeepEqual(s.Owner, mpdOwner) {
				t.Errorf("owner = %+v, want %+v", describeOwner(s.Owner), describeOwner(mpdOwner))
			}
		})
	}
}

// describeOwner renders an owner's pointers for a failure message.
func describeOwner(o *Owner) string {
	if o == nil {
		return "nil"
	}
	str := func(s *string) string {
		if s == nil {
			return "<nil>"
		}
		return *s
	}
	return strings.Join([]string{"tid", strconv.Itoa(o.TID), "thread", str(o.Thread), "pid", strconv.Itoa(deref(o.PID)), "process", str(o.Process)}, " ")
}

// TestReaderResolvesTheOwnerThroughTheThreadsTgid pins the attribution.
// owner_pid is the THREAD that opened the device: on the captured box it is
// MPD's output thread, 6584, whose comm reads "output:alsa". The process is
// the thread's Tgid, 6551, whose comm reads "mpd". A reader that took
// owner_pid for the process would name it "output:alsa". A process that
// opens the device from its main thread (aplay, in the synthetic tree) is
// its own Tgid, and both names are its.
func TestReaderResolvesTheOwnerThroughTheThreadsTgid(t *testing.T) {
	snap, err := Reader{ProcRoot: procRootOf("dsd256-native")}.Read()
	if err != nil {
		t.Fatal(err)
	}
	o := snap.Cards[2].Devices[0].Substreams[0].Owner
	if o == nil || o.TID != 6584 || deref(o.PID) != 6551 || o.Thread == nil || *o.Thread != "output:alsa" ||
		o.Process == nil || *o.Process != "mpd" {
		t.Errorf("owner = %s; want thread 6584 \"output:alsa\" in process 6551 \"mpd\"", describeOwner(o))
	}

	snap, err = Reader{ProcRoot: procRootOf("i2s-and-hdmi")}.Read()
	if err != nil {
		t.Fatal(err)
	}
	o = snap.Cards[1].Devices[0].Substreams[0].Owner
	want := &Owner{TID: 812, Thread: strPtr("aplay"), PID: intPtr(812), Process: strPtr("aplay")}
	if !reflect.DeepEqual(o, want) {
		t.Errorf("single-threaded owner = %s, want %s", describeOwner(o), describeOwner(want))
	}
}

// TestReaderLeavesAnOwnerItCannotNameUnknown takes the dsd256 capture and
// removes, one at a time, what attribution reads. Each leaves the part it
// could not read nil and the rest of the reading whole, with no error and
// no problem: an owner nobody can name is still a substream playing.
func TestReaderLeavesAnOwnerItCannotNameUnknown(t *testing.T) {
	tests := []struct {
		name   string
		change func(t *testing.T, root string)
		want   Owner
	}{
		{
			name:   "no /proc entry for the thread",
			change: func(t *testing.T, root string) { removeAll(t, root, "6584", "6551") },
			want:   Owner{TID: 6584},
		},
		{
			name: "the thread's status has no Tgid line",
			change: func(t *testing.T, root string) {
				writeTree(t, root, map[string]string{"6584/status": "Name:\toutput:alsa\nPid:\t6584\n"})
			},
			want: Owner{TID: 6584, Thread: strPtr("output:alsa")},
		},
		{
			name:   "the process has no comm",
			change: func(t *testing.T, root string) { removeAll(t, root, "6551") },
			want:   Owner{TID: 6584, Thread: strPtr("output:alsa"), PID: intPtr(6551)},
		},
		{
			name: "owner_pid 0: outside this pid namespace",
			change: func(t *testing.T, root string) {
				st := readFixture(t, "dsd256-native", "asound/card5/pcm0p/sub0/status")
				writeTree(t, root, map[string]string{
					"asound/card5/pcm0p/sub0/status": strings.Replace(string(st), ": 6584", ": 0", 1),
					// A /proc/0 the reader must not consult.
					"0/comm": "swapper\n",
				})
			},
			want: Owner{TID: 0},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := copyTree(t, "dsd256-native")
			tc.change(t, root)
			snap, err := Reader{ProcRoot: root}.Read()
			if err != nil {
				t.Fatal(err)
			}
			card := snap.Cards[2]
			if len(card.Problems) != 0 {
				t.Errorf("an unnamed owner became a problem: %q", card.Problems)
			}
			s := card.Devices[0].Substreams[0]
			if s.Owner == nil || !reflect.DeepEqual(*s.Owner, tc.want) {
				t.Errorf("owner = %s, want %s", describeOwner(s.Owner), describeOwner(&tc.want))
			}
			if s.HWParams == nil || s.HWParams.Format != "DSD_U32_BE" || s.Status == nil || s.Status.State != "RUNNING" {
				t.Errorf("the rest of the reading is not whole: hw %+v, status %+v", s.HWParams, s.Status)
			}
		})
	}
	t.Run("an unreadable comm is unused when status has Name", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a POSIX permission bit a non-root reader honours")
		}
		root := copyTree(t, "dsd256-native")
		if err := os.Chmod(filepath.Join(root, "6584", "comm"), 0); err != nil {
			t.Fatal(err)
		}
		snap, err := Reader{ProcRoot: root}.Read()
		if err != nil {
			t.Fatal(err)
		}
		o := snap.Cards[2].Devices[0].Substreams[0].Owner
		if !reflect.DeepEqual(o, mpdOwner) || len(snap.Cards[2].Problems) != 0 {
			t.Errorf("owner = %s (problems %q), want %s", describeOwner(o), snap.Cards[2].Problems, describeOwner(mpdOwner))
		}
	})
	t.Run("an unreadable status falls back to comm", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a POSIX permission bit a non-root reader honours")
		}
		root := copyTree(t, "dsd256-native")
		if err := os.Chmod(filepath.Join(root, "6584", "status"), 0); err != nil {
			t.Fatal(err)
		}
		snap, err := Reader{ProcRoot: root}.Read()
		if err != nil {
			t.Fatal(err)
		}
		o := snap.Cards[2].Devices[0].Substreams[0].Owner
		// Tgid was in the status that could not be read, so the process
		// stays unknown. The thread's name is its comm. Neither is a problem.
		want := &Owner{TID: 6584, Thread: strPtr("output:alsa")}
		if !reflect.DeepEqual(o, want) || len(snap.Cards[2].Problems) != 0 {
			t.Errorf("owner = %s (problems %q), want %s", describeOwner(o), snap.Cards[2].Problems, describeOwner(want))
		}
	})
}

// removeAll removes entries of a tree.
func removeAll(t *testing.T, root string, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReaderReadsCardsWithNoStreamFile reads the synthetic tree of a Pi
// with HDMI and an I2S DAC HAT, neither of which is USB: no usbid and no
// stream file, the HDMI card's substream closed and the HAT's playing for
// aplay. Without a stream file there is no altset to report, and the
// substream's own files still say what the device receives.
func TestReaderReadsCardsWithNoStreamFile(t *testing.T) {
	snap, err := Reader{ProcRoot: procRootOf("i2s-and-hdmi")}.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Cards) != 2 {
		t.Fatalf("%d cards, want 2", len(snap.Cards))
	}
	for _, c := range snap.Cards {
		if c.USBID != nil || len(c.Problems) != 0 || len(c.Devices) != 1 || c.Devices[0].Stream != nil || len(c.Devices[0].Substreams) != 1 {
			t.Fatalf("card %d = %+v; want one device, no stream, no USB id, no problem", c.Index, c)
		}
	}
	hdmi := snap.Cards[0].Devices[0].Substreams[0]
	if !hdmi.HWParams.Closed || !hdmi.Status.Closed || hdmi.Owner != nil {
		t.Errorf("HDMI substream = %+v %+v %+v; want closed with no owner", hdmi.HWParams, hdmi.Status, hdmi.Owner)
	}
	hat := snap.Cards[1].Devices[0].Substreams[0]
	if hat.HWParams.Format != "S32_LE" || deref(hat.HWParams.Rate) != 192000 || deref(hat.HWParams.Channels) != 2 ||
		hat.Status.State != "RUNNING" || hat.Owner == nil || hat.Owner.Process == nil || *hat.Owner.Process != "aplay" {
		t.Errorf("HAT substream = %+v %+v %s", hat.HWParams, hat.Status, describeOwner(hat.Owner))
	}
}

// TestReaderReportsAGarbledStreamAndKeepsTheRest reads the synthetic tree
// whose stream0 is garbled: its problems land on the card under the
// file's path, and the card's id, the substream and its (unnamed) owner
// are read as if the stream file were whole.
func TestReaderReportsAGarbledStreamAndKeepsTheRest(t *testing.T) {
	snap, err := Reader{ProcRoot: procRootOf("garbled-stream0")}.Read()
	if err != nil {
		t.Fatal(err)
	}
	c := snap.Cards[0]
	_, streamProbs := ParseStream(readFixture(t, "garbled-stream0", "asound/card0/stream0"))
	if !reflect.DeepEqual(c.Problems, prefixed("card0/stream0", streamProbs)) || len(c.Problems) != 6 {
		t.Errorf("card problems = %q; want stream0's six, prefixed", c.Problems)
	}
	if c.USBID == nil || c.USBID.String() != "1234:abcd" {
		t.Errorf("usb id = %v", c.USBID)
	}
	s := c.Devices[0].Substreams[0]
	if s.HWParams.Format != "S32_LE" || s.Status.State != "RUNNING" || !reflect.DeepEqual(s.Owner, &Owner{TID: 4242}) {
		t.Errorf("substream = %+v %+v %s", s.HWParams, s.Status, describeOwner(s.Owner))
	}
}

// TestReaderReportsAFileItCannotReadAndReadsTheRest grows the dsd256
// capture's stream0 past maxFileBytes, which no real one approaches: the
// file is a problem on its card and absent from the reading, and the
// substream beside it is read in full.
func TestReaderReportsAFileItCannotReadAndReadsTheRest(t *testing.T) {
	root := copyTree(t, "dsd256-native")
	writeTree(t, root, map[string]string{"asound/card5/stream0": strings.Repeat("x", maxFileBytes+1)})
	snap, err := Reader{ProcRoot: root}.Read()
	if err != nil {
		t.Fatal(err)
	}
	c := snap.Cards[2]
	if want := []string{"card5/stream0: longer than 1048576 bytes"}; !reflect.DeepEqual(c.Problems, want) {
		t.Errorf("problems = %q, want %q", c.Problems, want)
	}
	// stream0 still names device 0, and pcm0p is there: the device stays,
	// without a stream.
	if len(c.Devices) != 1 || c.Devices[0].Stream != nil || c.Devices[0].Substreams[0].HWParams.Format != "DSD_U32_BE" {
		t.Errorf("devices = %+v", c.Devices)
	}
}

// TestReaderWithoutACardsFileIsNoALSA: a tree with no asound/cards is not
// a reading with no cards but no reading at all, and the error says it
// does not exist, which is what a host without ALSA looks like.
func TestReaderWithoutACardsFileIsNoALSA(t *testing.T) {
	_, err := Reader{ProcRoot: t.TempDir()}.Read()
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Read on an empty tree = %v, want an fs.ErrNotExist", err)
	}
}

// TestReaderDevicesComeFromPCMDirectoriesAndStreamFiles: a device is one
// with a playback directory (pcmNp) or a stream file that plays, ordered by
// number, not by name; a capture-only stream and a name the kernel would
// not write (pcm03p) are not devices.
func TestReaderDevicesComeFromPCMDirectoriesAndStreamFiles(t *testing.T) {
	root := t.TempDir()
	play := "X : USB Audio\n\nPlayback:\n  Status: Stop\n"
	writeTree(t, root, map[string]string{
		"asound/cards":                      " 0 [X              ]: USB-Audio - X\n                      X at usb-1\n",
		"asound/card0/pcm0p/sub0/status":    "closed\n",
		"asound/card0/stream1":              play,
		"asound/card0/stream2":              "X : USB Audio\n\nCapture:\n  Status: Stop\n",
		"asound/card0/pcm10p/sub0/status":   "closed\n",
		"asound/card0/stream10":             play,
		"asound/card0/pcm03p/sub0/status":   "closed\n",
		"asound/card0/pcm0c/sub0/status":    "closed\n",
		"asound/card0/pcm0p/sub1/status":    "closed\n",
		"asound/card0/pcm0p/sub10/status":   "closed\n",
		"asound/card0/pcm0p/sub2/hw_params": "closed\n",
	})
	snap, err := Reader{ProcRoot: root}.Read()
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, d := range snap.Cards[0].Devices {
		got = append(got, d.Device)
	}
	if !reflect.DeepEqual(got, []int{0, 1, 10}) {
		t.Fatalf("devices = %v, want [0 1 10]", got)
	}
	var subs []int
	for _, s := range snap.Cards[0].Devices[0].Substreams {
		subs = append(subs, s.Subdevice)
	}
	if !reflect.DeepEqual(subs, []int{0, 1, 2, 10}) {
		t.Errorf("device 0's substreams = %v, want [0 1 2 10]", subs)
	}
	if snap.Cards[0].Devices[1].Stream == nil || len(snap.Cards[0].Devices[1].Substreams) != 0 {
		t.Errorf("device 1 = %+v; want its stream and no substream", snap.Cards[0].Devices[1])
	}
}

// TestRawReturnsEveryFileTheReadingReads lists the files a raw dump of the
// dsd256 capture holds, in order, each byte for byte as it is on disk:
// the card files, the substream's four, and the /proc entries its owner
// was named from. The thread's name is its status Name, so its comm is
// not read; the process has no status in this tree, so its comm is the
// fallback. A file read twice is listed once.
func TestRawReturnsEveryFileTheReadingReads(t *testing.T) {
	files, err := Reader{ProcRoot: procRootOf("dsd256-native")}.Raw()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"asound/cards", "asound/card5/usbid", "asound/card5/stream0",
		"asound/card5/pcm0p/sub0/info", "asound/card5/pcm0p/sub0/hw_params",
		"asound/card5/pcm0p/sub0/sw_params", "asound/card5/pcm0p/sub0/status",
		"6584/status", "6551/comm",
	}
	var got []string
	for _, f := range files {
		got = append(got, f.Path)
		if f.Err != nil {
			t.Errorf("%s: %v", f.Path, f.Err)
			continue
		}
		if disk := readFixture(t, "dsd256-native", f.Path); !bytes.Equal(f.Data, disk) {
			t.Errorf("%s differs from the file on disk", f.Path)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("raw files =\n%q\nwant\n%q", got, want)
	}

	files, err = Reader{ProcRoot: procRootOf("i2s-and-hdmi")}.Raw()
	if err != nil {
		t.Fatal(err)
	}
	var comms, statuses int
	for _, f := range files {
		switch f.Path {
		case "812/comm":
			comms++
		case "812/status":
			statuses++
		}
	}
	if comms != 0 || statuses != 1 {
		t.Errorf("812/comm listed %d times and 812/status %d; the process name is that status Name", comms, statuses)
	}
}

// linkSymlinkedPCM turns the symlinked-pcm fixture's real directories into
// the tree a reading should walk: stream0, pcm0p and sub0 are symlinks,
// and Audio points at card0 the way /proc/asound/<id> points at cardN.
func linkSymlinkedPCM(t *testing.T, root string) {
	t.Helper()
	card := filepath.Join(root, "asound", "card0")
	links := [][2]string{
		{"stream0.body", filepath.Join(card, "stream0")},
		{"pcm0-real", filepath.Join(card, "pcm0p")},
		{"sub0-real", filepath.Join(card, "pcm1p", "sub0")},
		// A directory is not a stream file, even through a symlink.
		{"pcm0-real", filepath.Join(card, "stream1")},
		{"card0", filepath.Join(root, "asound", "Audio")},
	}
	for _, l := range links {
		if err := os.Symlink(l[0], l[1]); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReaderFollowsSymlinkedPCMAndSubdirectories: DirEntry.IsDir does not
// follow a symlink, so a tree that links pcmNp and subM would otherwise
// read as having no playback device. An in-root link, including the card
// id pointing at cardN, is the target's own file.
func TestReaderFollowsSymlinkedPCMAndSubdirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege this test does not have")
	}
	root := copyTree(t, "symlinked-pcm")
	linkSymlinkedPCM(t, root)
	snap, err := Reader{ProcRoot: root}.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Cards) != 1 || len(snap.Cards[0].Problems) != 0 {
		t.Fatalf("card = %+v problems %q", snap.Cards[0].Card, snap.Cards[0].Problems)
	}
	if snap.Cards[0].USBID == nil || snap.Cards[0].USBID.String() != "1234:abcd" {
		t.Fatalf("usb id = %v", snap.Cards[0].USBID)
	}
	devs := snap.Cards[0].Devices
	if len(devs) != 2 {
		t.Fatalf("devices = %+v; want 0 (linked pcm and stream) and 1 (linked sub)", devs)
	}
	if devs[0].Device != 0 || devs[0].Stream == nil || devs[0].Stream.Playback == nil ||
		len(devs[0].Substreams) != 1 || devs[0].Substreams[0].HWParams == nil || !devs[0].Substreams[0].HWParams.Closed {
		t.Errorf("device 0 = %+v; want its stream and the linked substream closed", devs[0])
	}
	if devs[1].Device != 1 || devs[1].Stream != nil || len(devs[1].Substreams) != 1 ||
		devs[1].Substreams[0].Status == nil || !devs[1].Substreams[0].Status.Closed {
		t.Errorf("device 1 = %+v; want the linked substream and no stream", devs[1])
	}
	// The id symlink resolves inside the root. A read through it is the
	// card's own stream, not a refusal.
	asound, err := openTree(filepath.Join(root, "asound"), maxLinks, maxHops)
	if err != nil {
		t.Fatal(err)
	}
	defer asound.close()
	if b, err := asound.readFile("Audio/stream0", maxFileBytes, nil); err != nil || !bytes.Equal(b, readFixture(t, "symlinked-pcm", "asound/card0/stream0.body")) {
		t.Errorf("Audio/stream0 through Audio -> card0: %v", err)
	}
}

// TestReaderRefusesAFileThatLeavesTheRoot: a stream symlink to a file
// outside the tree is not read, and --raw does not receive its bytes. The
// link is absolute, which a tree refuses whatever it names.
func TestReaderRefusesAFileThatLeavesTheRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege this test does not have")
	}
	const secret = "OUTSIDE-ROOT-SECRET"
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte(secret+"\nPlayback:\n  Status: Stop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTree(t, root, map[string]string{
		"asound/cards":                      " 0 [X              ]: USB-Audio - X\n                      X at usb-1\n",
		"asound/card0/pcm0p/sub0/hw_params": "closed\n",
		"asound/card0/pcm0p/sub0/status":    "closed\n",
	})
	if err := os.Symlink(outside, filepath.Join(root, "asound", "card0", "stream0")); err != nil {
		t.Fatal(err)
	}
	// The substream's hw_params also points outside, so the refusal is
	// every read, not only stream files.
	hp := filepath.Join(root, "asound", "card0", "pcm0p", "sub0", "hw_params")
	if err := os.Remove(hp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, hp); err != nil {
		t.Fatal(err)
	}
	snap, err := Reader{ProcRoot: root}.Read()
	if err != nil {
		t.Fatal(err)
	}
	files, err := Reader{ProcRoot: root}.Raw()
	if err != nil {
		t.Fatal(err)
	}
	var refused []string
	for _, f := range files {
		if bytes.Contains(f.Data, []byte(secret)) {
			t.Errorf("raw %s holds the file outside the root", f.Path)
		}
		if errors.Is(f.Err, errOutsideRoot) {
			refused = append(refused, f.Path)
		}
	}
	if !reflect.DeepEqual(refused, []string{"asound/card0/stream0", "asound/card0/pcm0p/sub0/hw_params"}) {
		t.Errorf("refused = %q", refused)
	}
	c := snap.Cards[0]
	if c.Devices == nil || len(c.Devices) != 1 || c.Devices[0].Stream != nil {
		t.Errorf("devices = %+v; the outside stream is not a stream", c.Devices)
	}
	for _, p := range c.Problems {
		if strings.Contains(p, secret) {
			t.Errorf("a problem quotes the outside file: %s", p)
		}
	}
	if !strings.Contains(strings.Join(c.Problems, "\n"), "card0/stream0: an absolute link is refused") {
		t.Errorf("problems = %q; want the read refused", c.Problems)
	}
}

// swapCase is one path a reading opens, swapped for a link out of the tree
// once the reader has looked at it and before it opens or lists it.
type swapCase struct {
	name string
	// at is the path the swap waits for, as a raw dump names it, and swap
	// the path in the tree replaced by the link: at itself, or a directory
	// above it.
	at, swap string
	// outside is what the link's target holds: a file's text under "", a
	// directory's files under their paths in it.
	outside map[string]string
	// problem opens the problem card 5 reports for the refusal, "" where
	// none is reported (an owner that cannot be named is unknown).
	problem string
}

// swapAfterCheck lays the dsd256 capture out, and returns a Reader of it
// that swaps tc.swap for a link to a copy of tc.outside the first time the
// reader comes to tc.at, and a func that fails the test when it never came.
// The link is absolute, or relative and climbing out of the tree when
// relative is set; both leave the root.
func swapAfterCheck(t *testing.T, tc swapCase, relative bool) (Reader, func()) {
	t.Helper()
	root := copyTree(t, "dsd256-native")
	target := filepath.Join(t.TempDir(), "outside")
	if text, ok := tc.outside[""]; ok {
		if err := os.WriteFile(target, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		writeTree(t, target, tc.outside)
	}
	at := filepath.Join(root, filepath.FromSlash(tc.swap))
	link := target
	if relative {
		rel, err := filepath.Rel(filepath.Dir(at), target)
		if err != nil {
			t.Fatal(err)
		}
		link = rel
	}
	var once sync.Once
	var swapped bool
	var swapErr error
	r := Reader{ProcRoot: root, beforeOpen: func(path string) {
		if path != tc.at {
			return
		}
		once.Do(func() {
			swapped = true
			if swapErr = os.Rename(at, at+".before"); swapErr == nil {
				swapErr = os.Symlink(link, at)
			}
		})
	}}
	return r, func() {
		t.Helper()
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if !swapped {
			t.Fatalf("the reading never came to %s", tc.at)
		}
	}
}

// TestReaderRefusesALinkSwappedInAfterItsCheck: what the reader opens and
// lists is what its root holds at the open, never a path it looked at
// before. A file, a directory above a file, a directory it lists, an
// owner's status and the cards file, each swapped for a link out of the
// tree after the look and before the open, are refused: neither the
// reading nor --raw holds anything of the outside, and nothing the outside
// directory lists becomes part of the reading.
func TestReaderRefusesALinkSwappedInAfterItsCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink needs a privilege this test does not have")
	}
	const secret = "OUTSIDE-ROOT-SECRET"
	for _, tc := range []swapCase{
		{name: "a file", at: "asound/card5/stream0", swap: "asound/card5/stream0",
			outside: map[string]string{"": secret + " : USB Audio\n\nPlayback:\n  Status: Stop\n"}, problem: "card5/stream0: "},
		{name: "a directory above a file", at: "asound/card5/pcm0p/sub0/hw_params", swap: "asound/card5/pcm0p/sub0",
			outside: map[string]string{"hw_params": secret + "\n", "status": secret + "\n", "sw_params": secret + "\n", "info": secret + "\n"},
			problem: "card5/pcm0p/sub0/hw_params: "},
		{name: "a directory it lists", at: "asound/card5/pcm0p", swap: "asound/card5/pcm0p",
			outside: map[string]string{"sub7/hw_params": secret + "\n", "sub7/status": secret + "\n"}, problem: "card5/pcm0p: "},
		{name: "an owner's status", at: "6584/status", swap: "6584/status",
			outside: map[string]string{"": "Name:\t" + secret + "\nTgid:\t6551\n"}},
		{name: "the cards file", at: "asound/cards", swap: "asound/cards",
			outside: map[string]string{"": " 7 [Secret         ]: USB-Audio - " + secret + "\n                      " + secret + " at usb-1\n"}},
	} {
		for _, relative := range []bool{false, true} {
			name := tc.name
			if relative {
				name += ", a relative link"
			}
			t.Run(name, func(t *testing.T) {
				r, swapped := swapAfterCheck(t, tc, relative)
				snap, err := r.Read()
				swapped()
				if (err != nil) != (tc.at == "asound/cards") {
					t.Fatalf("the reading's error is %v", err)
				}
				if b, _ := json.Marshal(snap); bytes.Contains(b, []byte(secret)) {
					i := bytes.Index(b, []byte(secret))
					t.Errorf("the reading holds the outside: …%s…", b[max(0, i-80):min(len(b), i+len(secret)+20)])
				}
				for _, c := range snap.Cards {
					for _, d := range c.Devices {
						for _, s := range d.Substreams {
							if s.Subdevice == 7 {
								t.Errorf("card %d device %d has substream 7, which only the outside directory lists", c.Index, d.Device)
							}
						}
					}
					if probs := strings.Join(c.Problems, "\n"); c.Index == 5 && tc.problem != "" && !strings.Contains(probs, tc.problem) {
						t.Errorf("card 5's problems do not report the refusal at %s:\n%s", tc.at, probs)
					}
				}
				r, swapped = swapAfterCheck(t, tc, relative)
				files, _ := r.Raw()
				swapped()
				for _, f := range files {
					if bytes.Contains(f.Data, []byte(secret)) {
						t.Errorf("raw %s holds the outside", f.Path)
					}
					if f.Path == tc.at && f.Err == nil {
						t.Errorf("raw %s was read", f.Path)
					}
				}
			})
		}
	}
}

// TestReaderNamesTheOwnerFromOneStatusRead: Name and Tgid are one file.
// comm is a different read and is only the fallback when Name is absent.
// When the thread is the process, that same Name is the process name.
func TestReaderNamesTheOwnerFromOneStatusRead(t *testing.T) {
	t.Run("names come from status, not comm", func(t *testing.T) {
		root := copyTree(t, "dsd256-native")
		writeTree(t, root, map[string]string{
			"6584/status": "Name:\tfrom-status\nTgid:\t6551\n",
			"6584/comm":   "from-comm\n",
			"6551/status": "Name:\tmpd-status\nTgid:\t6551\n",
			"6551/comm":   "mpd-comm\n",
		})
		snap, err := Reader{ProcRoot: root}.Read()
		if err != nil {
			t.Fatal(err)
		}
		o := snap.Cards[2].Devices[0].Substreams[0].Owner
		want := &Owner{TID: 6584, Thread: strPtr("from-status"), PID: intPtr(6551), Process: strPtr("mpd-status")}
		if !reflect.DeepEqual(o, want) {
			t.Errorf("owner = %s, want %s", describeOwner(o), describeOwner(want))
		}
		files, err := Reader{ProcRoot: root}.Raw()
		if err != nil {
			t.Fatal(err)
		}
		var procs []string
		for _, f := range files {
			if strings.HasPrefix(f.Path, "65") {
				procs = append(procs, f.Path)
			}
		}
		if !reflect.DeepEqual(procs, []string{"6584/status", "6551/status"}) {
			t.Errorf("owner files = %q; comm is not read when status has Name", procs)
		}
	})
	t.Run("the process name is that Name when the thread is the process", func(t *testing.T) {
		root := copyTree(t, "i2s-and-hdmi")
		writeTree(t, root, map[string]string{
			"812/status": "Name:\taplay-status\nTgid:\t812\n",
			"812/comm":   "aplay-comm\n",
		})
		snap, err := Reader{ProcRoot: root}.Read()
		if err != nil {
			t.Fatal(err)
		}
		o := snap.Cards[1].Devices[0].Substreams[0].Owner
		want := &Owner{TID: 812, Thread: strPtr("aplay-status"), PID: intPtr(812), Process: strPtr("aplay-status")}
		if !reflect.DeepEqual(o, want) {
			t.Errorf("owner = %s, want %s", describeOwner(o), describeOwner(want))
		}
	})
	t.Run("comm is the fallback when status has no Name", func(t *testing.T) {
		root := copyTree(t, "dsd256-native")
		writeTree(t, root, map[string]string{
			"6584/status": "Tgid:\t6551\nPid:\t6584\n",
			"6584/comm":   "output:alsa\n",
		})
		removeAll(t, root, "6551/status")
		snap, err := Reader{ProcRoot: root}.Read()
		if err != nil {
			t.Fatal(err)
		}
		o := snap.Cards[2].Devices[0].Substreams[0].Owner
		want := &Owner{TID: 6584, Thread: strPtr("output:alsa"), PID: intPtr(6551), Process: strPtr("mpd")}
		if !reflect.DeepEqual(o, want) {
			t.Errorf("owner = %s, want %s", describeOwner(o), describeOwner(want))
		}
		if len(snap.Cards[2].Problems) != 0 {
			t.Errorf("a fallback name became a problem: %q", snap.Cards[2].Problems)
		}
	})
}
