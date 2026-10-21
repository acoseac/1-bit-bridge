// Package sysfstest lays out the fake /sys and /proc trees the player's
// identity tests read, under t.TempDir(), from a manifest: a manifest in
// internal/alsa/testdata/sysfs, or text a test writes. A real sysfs is
// symlinks all the way down, and a checkout of a committed symlink is not
// the same file on every system, so no tree is committed: the manifest
// is, and the links are made at test time. Like net/http/httptest it is
// imported only by tests.
//
// A manifest is one directive a line; # starts a comment. Paths are
// slash-separated and relative to the tree's root, with sys/ and proc/ the
// two trees (a SysfsReader's Root is root/sys, a Reader's ProcRoot
// root/proc). A value with a space is Go-quoted.
//
//	dir PATH                    a directory
//	file PATH TEXT…             a file holding the rest of the line and a newline, as sysfs
//	                            prints; a Go-quoted TEXT is written as it unquotes, with no newline
//	link PATH TARGET            a symlink to TARGET, a path from the root, written relative as sysfs's are;
//	                            !TEXT writes TEXT as the link verbatim
//	remove PATH                 PATH and everything below it
//	subsys DIR NAME             DIR/subsystem, a link to sys/bus/NAME
//	platform DIR, pci DIR       a platform or PCI device directory
//	roothub DIR speed=MBITS     a USB root hub (usbN), its speed, and its sys/bus/usb/devices link
//	usbdev DIR vendor=V product=P [serial=S] [serialraw="…"] [class=C]
//	                            a USB device: its ids, its serial as sysfs prints it (or exact bytes),
//	                            its class (09 is a hub), and its sys/bus/usb/devices link
//	usbcard N IFACE             a USB interface IFACE ("1-1.3:1.0") that is sound card N: its
//	                            bInterfaceNumber, sound/cardN with its device link, and sys/class/sound/cardN
//	card N DEVDIR               sound card N under any device directory (HDMI, I2S)
//	proccard N ID NAME [driver=D] [usb=V:P] [pcm=D.S,…]
//	                            card N in proc/asound/cards, its usbid, and its playback substreams
//	procremove N                card N out of proc/asound
//	unplug DIR                  DIR removed with every card, bus/usb/devices link and proc card under it
//
// Links are made after the directories and files of the same manifest, so
// on Windows a link to a directory is a directory link. A test on a
// system that cannot make symlinks is skipped.
package sysfstest

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Build lays out the manifest file at manifestPath under a new
// t.TempDir() and returns the tree's root.
func Build(t testing.TB, manifestPath string) string {
	t.Helper()
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("sysfstest: %v", err)
	}
	return BuildText(t, string(b))
}

// BuildText is Build from manifest text.
func BuildText(t testing.TB, text string) string {
	t.Helper()
	root := t.TempDir()
	Apply(t, root, text)
	return root
}

// Apply lays out manifest text on a tree Build made: a test's next step,
// an unplug or a replug, after a first reading.
func Apply(t testing.TB, root, text string) {
	t.Helper()
	symlinksOrSkip(t)
	b := &builder{t: t, root: root}
	for i, line := range strings.Split(text, "\n") {
		if j := strings.Index(line, "#"); j >= 0 && !strings.Contains(line[:j], `"`) {
			line = line[:j]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := b.directive(line); err != nil {
			t.Fatalf("sysfstest: line %d %q: %v", i+1, strings.TrimSpace(line), err)
		}
	}
	for _, l := range b.links {
		if err := b.makeLink(l); err != nil {
			t.Fatalf("sysfstest: link %s: %v", l.at, err)
		}
	}
	if b.cardsChanged {
		if err := b.writeCards(); err != nil {
			t.Fatalf("sysfstest: proc/asound/cards: %v", err)
		}
	}
}

// symlinksOrSkip skips a test on a system that cannot make a symlink
// (Windows without the privilege to).
func symlinksOrSkip(t testing.TB) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Skipf("sysfstest: this system cannot make a symlink: %v", err)
	}
}

// builder is one Apply in progress.
type builder struct {
	t            testing.TB
	root         string
	links        []pendingLink
	cardsChanged bool
}

// pendingLink is a symlink to make once the directives' files exist.
type pendingLink struct {
	at     string // the link, from the root
	target string // the target, from the root, or the verbatim text
	raw    bool
}

// cardsDir keeps one file per proc card, which writeCards turns into
// proc/asound/cards. The Reader lists no directory of proc/asound, so it
// never sees it.
const cardsDir = "proc/asound/.sysfstest-cards"

func (b *builder) abs(p string) string { return filepath.Join(b.root, filepath.FromSlash(p)) }

func (b *builder) mkdir(p string) error { return os.MkdirAll(b.abs(p), 0o755) }

func (b *builder) write(p string, data []byte) error {
	if err := b.mkdir(path.Dir(p)); err != nil {
		return err
	}
	return os.WriteFile(b.abs(p), data, 0o644)
}

func (b *builder) link(at, target string) {
	b.links = append(b.links, pendingLink{at: at, target: target})
}

func (b *builder) makeLink(l pendingLink) error {
	if err := b.mkdir(path.Dir(l.at)); err != nil {
		return err
	}
	text := l.target
	if !l.raw {
		rel, err := filepath.Rel(filepath.Dir(b.abs(l.at)), b.abs(l.target))
		if err != nil {
			return err
		}
		text = rel
	}
	_ = os.Remove(b.abs(l.at))
	return os.Symlink(text, b.abs(l.at))
}

// directive runs one manifest line.
func (b *builder) directive(line string) error {
	f, err := tokens(line)
	if err != nil {
		return err
	}
	need := func(n int) error {
		if len(f) < n {
			return fmt.Errorf("%s takes %d fields", f[0], n-1)
		}
		return nil
	}
	switch f[0] {
	case "dir":
		if err := need(2); err != nil {
			return err
		}
		return b.mkdir(f[1])
	case "file":
		if err := need(2); err != nil {
			return err
		}
		rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "file"))
		rest = strings.TrimSpace(strings.TrimPrefix(rest, rawField(line, 1)))
		if strings.HasPrefix(rest, `"`) {
			s, err := strconv.Unquote(rest)
			if err != nil {
				return err
			}
			return b.write(f[1], []byte(s))
		}
		return b.write(f[1], []byte(rest+"\n"))
	case "link":
		if err := need(3); err != nil {
			return err
		}
		if strings.HasPrefix(f[2], "!") {
			b.links = append(b.links, pendingLink{at: f[1], target: strings.TrimPrefix(f[2], "!"), raw: true})
			return nil
		}
		b.link(f[1], f[2])
		return nil
	case "remove":
		if err := need(2); err != nil {
			return err
		}
		return os.RemoveAll(b.abs(f[1]))
	case "subsys":
		if err := need(3); err != nil {
			return err
		}
		return b.subsys(f[1], f[2])
	case "platform", "pci":
		if err := need(2); err != nil {
			return err
		}
		if err := b.mkdir(f[1]); err != nil {
			return err
		}
		return b.subsys(f[1], f[0])
	case "roothub":
		if err := need(2); err != nil {
			return err
		}
		opts, err := options(f[2:])
		if err != nil {
			return err
		}
		return b.usbDevice(f[1], map[string]string{"speed": opts["speed"], "class": "09"})
	case "usbdev":
		if err := need(2); err != nil {
			return err
		}
		opts, err := options(f[2:])
		if err != nil {
			return err
		}
		return b.usbDevice(f[1], opts)
	case "usbcard":
		if err := need(3); err != nil {
			return err
		}
		return b.usbCard(f[1], f[2])
	case "card":
		if err := need(3); err != nil {
			return err
		}
		return b.card(f[1], f[2])
	case "proccard":
		if err := need(4); err != nil {
			return err
		}
		opts, err := options(f[4:])
		if err != nil {
			return err
		}
		return b.procCard(f[1], f[2], f[3], opts)
	case "procremove":
		if err := need(2); err != nil {
			return err
		}
		return b.procRemove(f[1])
	case "unplug":
		if err := need(2); err != nil {
			return err
		}
		return b.unplug(f[1])
	}
	return fmt.Errorf("no directive %q", f[0])
}

func (b *builder) subsys(dir, name string) error {
	if err := b.mkdir(dir); err != nil {
		return err
	}
	if err := b.mkdir("sys/bus/" + name); err != nil {
		return err
	}
	b.link(dir+"/subsystem", "sys/bus/"+name)
	return nil
}

// usbDevice makes a USB device or root hub directory: its attributes, its
// subsystem and its sys/bus/usb/devices link.
func (b *builder) usbDevice(dir string, opts map[string]string) error {
	if err := b.subsys(dir, "usb"); err != nil {
		return err
	}
	attrs := map[string]string{"speed": opts["speed"], "idVendor": opts["vendor"], "idProduct": opts["product"], "serial": opts["serial"]}
	class := opts["class"]
	if class == "" {
		class = "00"
	}
	attrs["bDeviceClass"] = class
	for name, v := range attrs {
		if v == "" {
			continue
		}
		if err := b.write(dir+"/"+name, []byte(v+"\n")); err != nil {
			return err
		}
	}
	if raw, ok := opts["serialraw"]; ok {
		if err := b.write(dir+"/serial", []byte(raw)); err != nil {
			return err
		}
	}
	b.link("sys/bus/usb/devices/"+path.Base(dir), dir)
	return nil
}

// usbCard makes the interface iface, a child of a usbdev directory, sound
// card n.
func (b *builder) usbCard(n, iface string) error {
	name := path.Base(iface)
	colon := strings.LastIndex(name, ":")
	dot := strings.LastIndex(name, ".")
	if colon < 0 || dot < colon {
		return fmt.Errorf("%s is not an interface name (B-C:x.y)", name)
	}
	num, err := strconv.Atoi(name[dot+1:])
	if err != nil {
		return err
	}
	if err := b.subsys(iface, "usb"); err != nil {
		return err
	}
	if err := b.write(iface+"/bInterfaceNumber", []byte(fmt.Sprintf("%02x\n", num))); err != nil {
		return err
	}
	b.link("sys/bus/usb/devices/"+name, iface)
	return b.card(n, iface)
}

// card makes devdir/sound/cardN, its device link and sys/class/sound/cardN.
func (b *builder) card(n, devdir string) error {
	cardDir := devdir + "/sound/card" + n
	if err := b.mkdir(cardDir); err != nil {
		return err
	}
	b.link(cardDir+"/device", devdir)
	b.link("sys/class/sound/card"+n, cardDir)
	return nil
}

// procCard records card n in proc/asound: its cards entry, its usbid and
// its playback substreams.
func (b *builder) procCard(n, id, name string, opts map[string]string) error {
	idx, err := strconv.Atoi(n)
	if err != nil {
		return err
	}
	driver := opts["driver"]
	if driver == "" {
		driver = "USB-Audio"
	}
	entry := fmt.Sprintf("%2d [%-15s]: %s - %s\n%22s%s\n", idx, id, driver, name, "", name)
	if err := b.write(fmt.Sprintf("%s/%03d", cardsDir, idx), []byte(entry)); err != nil {
		return err
	}
	b.cardsChanged = true
	dir := "proc/asound/card" + n
	if err := b.mkdir(dir); err != nil {
		return err
	}
	if usb := opts["usb"]; usb != "" {
		if err := b.write(dir+"/usbid", []byte(usb+"\n")); err != nil {
			return err
		}
	}
	if pcm := opts["pcm"]; pcm != "" {
		for _, ds := range strings.Split(pcm, ",") {
			d, s, ok := strings.Cut(ds, ".")
			if !ok {
				return fmt.Errorf("pcm %q is not D.S", ds)
			}
			sub := dir + "/pcm" + d + "p/sub" + s
			for _, f := range []string{"hw_params", "status"} {
				if err := b.write(sub+"/"+f, []byte("closed\n")); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (b *builder) procRemove(n string) error {
	idx, err := strconv.Atoi(n)
	if err != nil {
		return err
	}
	_ = os.Remove(b.abs(fmt.Sprintf("%s/%03d", cardsDir, idx)))
	b.cardsChanged = true
	return os.RemoveAll(b.abs("proc/asound/card" + n))
}

// writeCards writes proc/asound/cards from the card entries, by index.
func (b *builder) writeCards() error {
	entries, err := os.ReadDir(b.abs(cardsDir))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	var out strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(b.abs(cardsDir), name))
		if err != nil {
			return err
		}
		out.Write(data)
	}
	if len(names) == 0 {
		out.WriteString("--- no soundcards ---\n")
	}
	return b.write("proc/asound/cards", []byte(out.String()))
}

// unplug removes dir and what points into it: each sound card under it,
// with its proc card, and each sys/bus/usb/devices link.
func (b *builder) unplug(dir string) error {
	inside := func(linkDir, text string) bool {
		target := filepath.Clean(filepath.Join(linkDir, text))
		rel, err := filepath.Rel(b.abs(dir), target)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	for _, d := range []string{"sys/class/sound", "sys/bus/usb/devices"} {
		entries, err := os.ReadDir(b.abs(d))
		if err != nil {
			continue
		}
		for _, e := range entries {
			at := filepath.Join(b.abs(d), e.Name())
			text, err := os.Readlink(at)
			if err != nil || !inside(b.abs(d), text) {
				continue
			}
			if err := os.Remove(at); err != nil {
				return err
			}
			if n, ok := strings.CutPrefix(e.Name(), "card"); ok && d == "sys/class/sound" {
				if err := b.procRemove(n); err != nil {
					return err
				}
			}
		}
	}
	return os.RemoveAll(b.abs(dir))
}

// tokens splits a line at spaces, keeping a Go-quoted value whole and
// unquoting a field that is one; a key="value" field keeps its quotes for
// options to unquote.
func tokens(line string) ([]string, error) {
	var out []string
	for i := 0; i < len(line); {
		if line[i] == ' ' || line[i] == '\t' || line[i] == '\r' {
			i++
			continue
		}
		j := i
		for j < len(line) && line[j] != ' ' && line[j] != '\t' && line[j] != '\r' {
			if line[j] == '"' {
				k := j + 1
				for k < len(line) && line[k] != '"' {
					if line[k] == '\\' {
						k++
					}
					k++
				}
				if k >= len(line) {
					return nil, fmt.Errorf("an unclosed quote")
				}
				j = k
			}
			j++
		}
		tok := line[i:j]
		if strings.HasPrefix(tok, `"`) {
			s, err := strconv.Unquote(tok)
			if err != nil {
				return nil, err
			}
			tok = s
		}
		out = append(out, tok)
		i = j
	}
	return out, nil
}

// rawField returns line's n-th field as written, quotes included.
func rawField(line string, n int) string {
	f := strings.Fields(line)
	if n < len(f) {
		return f[n]
	}
	return ""
}

// options reads key=value fields, unquoting a Go-quoted value.
func options(fields []string) (map[string]string, error) {
	m := map[string]string{}
	for _, f := range fields {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return nil, fmt.Errorf("%q is not key=value", f)
		}
		if strings.HasPrefix(v, `"`) {
			s, err := strconv.Unquote(v)
			if err != nil {
				return nil, err
			}
			v = s
		}
		m[k] = v
	}
	return m, nil
}
