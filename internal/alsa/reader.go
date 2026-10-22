package alsa

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// DefaultProcRoot is the host's /proc, which a zero Reader reads.
const DefaultProcRoot = "/proc"

// Reader takes readings of a /proc tree: the host's by default, or one
// captured from another box, which is how the tests run on every OS. It
// reads its two roots through a tree each, so nothing it opens, lists or
// prints lies outside them, whatever the trees do while it reads.
type Reader struct {
	// ProcRoot is the /proc tree a substream's owner is looked up in; ""
	// means DefaultProcRoot.
	ProcRoot string
	// AsoundRoot is the ALSA tree; "" means ProcRoot's asound directory.
	AsoundRoot string
	// beforeOpen, when set, is called with a file's or a directory's path
	// in a raw dump (asound/card5/stream0, 6584/status) after the path is
	// resolved and before it is opened or listed: the window a test swaps a
	// link in.
	beforeOpen func(path string)
}

// Snapshot is one reading of every card.
type Snapshot struct {
	ProcRoot   string        `json:"procRoot"`
	AsoundRoot string        `json:"asoundRoot"`
	Cards      []CardReading `json:"cards"`
	// Problems are what the cards file itself could not say.
	Problems []string `json:"problems"`
}

// CardReading is one card and what its directory says about it.
type CardReading struct {
	Card
	// USBID is the card's USB vendor and product id, nil for a card that
	// is not USB, and for one whose usbid the kernel leaves empty, which
	// it does while the device is being disconnected.
	USBID *USBID `json:"usbId"`
	// Devices are the card's PCM devices that play, by device number: one
	// with a pcmNp directory or a streamN file with a Playback section.
	Devices []Device `json:"devices"`
	// Problems are what the card's files could not say, each opening with
	// the file's path under the ALSA root ("card5/stream0: line 9: …").
	Problems []string `json:"problems"`
}

// Device is one PCM device of a card: hw:<card>,<device>.
type Device struct {
	Device int `json:"device"`
	// Stream is the device's streamN file, which only a USB card has.
	Stream *Stream `json:"stream"`
	// Substreams are the device's playback substreams, by number.
	Substreams []Substream `json:"substreams"`
}

// Substream is one playback substream (pcmNp/subM): what it was opened
// with, its state, and who holds it.
type Substream struct {
	Subdevice int `json:"subdevice"`
	// HWParams and Status are nil when their file is absent or could not
	// be read (the card's problems then say which).
	HWParams *HWParams `json:"hwParams"`
	Status   *Status   `json:"status"`
	// Owner is nil when the status names no owner: the substream is
	// closed, or its status could not be read.
	Owner *Owner `json:"owner"`
}

// Owner is the thread that holds a substream open and the process it
// belongs to. A name or a pid that cannot be read is nil, never an error:
// /proc may hide another user's processes (hidepid), the thread may have
// exited since, or the owner may sit outside this pid namespace (an
// owner_pid of 0). The reading is whole either way.
type Owner struct {
	// TID is the status file's owner_pid, a thread id: MPD opens the
	// device from its output thread, "output:alsa", not its main one.
	TID int `json:"tid"`
	// Thread is the thread's name: the Name line of /proc/<tid>/status,
	// which is the same read as its Tgid. comm is the fallback when that
	// status has no Name line.
	Thread *string `json:"thread"`
	// PID is the process the thread belongs to: the Tgid line of that
	// same /proc/<tid>/status.
	PID *int `json:"pid"`
	// Process is the process's name. When the thread is the process, it
	// is the same Name; otherwise it is the Name line of
	// /proc/<pid>/status, with comm the same fallback.
	Process *string `json:"process"`
}

// RawFile is one file a reading read, verbatim, for capturing a box's
// state as fixtures.
type RawFile struct {
	// Path is where the file sits in a /proc tree, with forward slashes:
	// "asound/card5/stream0", "6584/status".
	Path string
	Data []byte
	// Err is why the file could not be read; Data is nil then.
	Err error
}

// collector receives each file a reading reads.
type collector func(path string, data []byte, err error)

// streamName, pcmPlaybackName and subName match the entries of a card's
// directory this reads: streamN files, pcmNp directories (playback; a
// capture device is pcmNc) and their subM directories.
var (
	streamName      = regexp.MustCompile(`^stream(\d+)$`)
	pcmPlaybackName = regexp.MustCompile(`^pcm(\d+)p$`)
	subName         = regexp.MustCompile(`^sub(\d+)$`)
)

// Read takes one reading. The only error is the cards file's: without it
// there is nothing to read (and with a missing one, errors.Is reports
// fs.ErrNotExist, which is a host with no ALSA). Everything below it that
// cannot be read is a problem on its card, and the rest of the reading
// stands.
func (r Reader) Read() (Snapshot, error) {
	return r.read(nil)
}

// Raw reads what Read reads and returns the files themselves, in the
// order read, each once: the cards file; per card its usbid, stream files
// and each playback substream's info, hw_params, sw_params and status;
// and the /proc entries its owner was resolved from. A file that does not
// exist is left out.
func (r Reader) Raw() ([]RawFile, error) {
	var files []RawFile
	seen := map[string]bool{}
	_, err := r.read(func(path string, data []byte, err error) {
		if seen[path] {
			return
		}
		seen[path] = true
		files = append(files, RawFile{Path: path, Data: data, Err: err})
	})
	return files, err
}

// roots resolves the zero values.
func (r Reader) roots() (procRoot, asoundRoot string) {
	procRoot = r.ProcRoot
	if procRoot == "" {
		procRoot = DefaultProcRoot
	}
	asoundRoot = r.AsoundRoot
	if asoundRoot == "" {
		asoundRoot = filepath.Join(procRoot, "asound")
	}
	return procRoot, asoundRoot
}

// read is Read, handing each file it reads to collect when collect is not
// nil, and then also reading the files only a raw dump wants. It reads only
// through the two roots, each opened once as a tree, so every file and
// directory it reaches is inside its root when it is opened.
func (r Reader) read(collect collector) (Snapshot, error) {
	procRoot, asoundRoot := r.roots()
	t := walk{collect: collect, raw: collect != nil, beforeOpen: r.beforeOpen}
	if t.collect == nil {
		t.collect = func(string, []byte, error) {}
	}
	snap := Snapshot{ProcRoot: procRoot, AsoundRoot: asoundRoot, Cards: []CardReading{}}
	path := filepath.Join(asoundRoot, "cards")
	for _, root := range []string{procRoot, asoundRoot} {
		if err := checkHostRead(root, DefaultProcRoot); err != nil {
			return snap, fmt.Errorf("read %s: %w", path, err)
		}
	}
	asound, err := openTree(asoundRoot, maxLinks, maxHops)
	var b []byte
	if err == nil {
		defer asound.close()
		t.asound = asound
		b, err = asound.readFile("cards", maxFileBytes, t.hook("asound/cards"))
	}
	if err != nil {
		// The path once: a *fs.PathError would name it again. What it
		// wraps still answers errors.Is(err, fs.ErrNotExist).
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return snap, fmt.Errorf("read %s: %w", path, err)
	}
	// A /proc tree that cannot be opened leaves every owner unknown, as a
	// task's entry that cannot be read does (procFile).
	if t.proc, t.procErr = openTree(procRoot, maxLinks, maxHops); t.procErr == nil {
		defer t.proc.close()
	}
	t.collect("asound/cards", b, nil)
	cards, probs := ParseCards(b)
	snap.Problems = prefixed("cards", probs)
	for _, c := range cards {
		snap.Cards = append(snap.Cards, t.card(c))
	}
	return snap, nil
}

// walk is one reading in progress.
type walk struct {
	// asound is the ALSA tree, and proc the tree an owner is looked up in;
	// procErr is why proc could not be opened, and proc is nil then.
	asound, proc *tree
	procErr      error
	collect      collector
	raw          bool
	beforeOpen   func(path string)
}

// hook is the Reader's beforeOpen for path, nil when there is none.
func (t walk) hook(path string) func() {
	if t.beforeOpen == nil {
		return nil
	}
	return func() { t.beforeOpen(path) }
}

// card reads one card's directory.
func (t walk) card(c Card) CardReading {
	cr := CardReading{Card: c, Devices: []Device{}, Problems: []string{}}
	dirName := "card" + strconv.Itoa(c.Index)
	// read reads a file under the card. A missing one is absent; any
	// other failure is a problem, and the file is absent too.
	read := func(rel string) ([]byte, bool) {
		b, err := t.asound.readFile(dirName+"/"+rel, maxFileBytes, t.hook("asound/"+dirName+"/"+rel))
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				cr.Problems = append(cr.Problems, dirName+"/"+rel+": "+errText(err))
				t.collect("asound/"+dirName+"/"+rel, nil, err)
			}
			return nil, false
		}
		t.collect("asound/"+dirName+"/"+rel, b, nil)
		return b, true
	}
	// rawOnly reads a file only a raw dump wants.
	rawOnly := func(rel string) {
		if !t.raw {
			return
		}
		b, err := t.asound.readFile(dirName+"/"+rel, maxFileBytes, t.hook("asound/"+dirName+"/"+rel))
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			t.collect("asound/"+dirName+"/"+rel, b, err)
		}
	}
	note := func(rel string, probs []string) {
		cr.Problems = append(cr.Problems, prefixed(dirName+"/"+rel, probs)...)
	}
	// is reports whether what rel names under the card, its links followed
	// inside the tree, is what want asks. DirEntry.IsDir reports a link
	// itself, so a tree that links pcmNp or subM would otherwise have no
	// playback device. A link that leaves the tree is kept, so the read or
	// the listing it was kept for reports the refusal.
	is := func(rel string, want func(fs.FileInfo) bool) bool {
		info, err := t.asound.stat(dirName + "/" + rel)
		if err != nil {
			return errors.Is(err, errOutsideRoot)
		}
		return want(info)
	}
	regular := func(fi fs.FileInfo) bool { return fi.Mode().IsRegular() }
	directory := func(fi fs.FileInfo) bool { return fi.IsDir() }

	if b, ok := read("usbid"); ok && len(bytes.TrimSpace(b)) > 0 {
		if id, err := ParseUSBID(b); err != nil {
			cr.Problems = append(cr.Problems, dirName+"/usbid: "+err.Error())
		} else {
			cr.USBID = &id
		}
	}

	entries, err := t.asound.readDir(dirName, t.hook("asound/"+dirName))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			cr.Problems = append(cr.Problems, dirName+": "+errText(err))
		}
		return cr
	}
	streams, pcms := map[int]bool{}, map[int]bool{}
	for _, e := range entries {
		// streamN is a regular file and pcmNp a directory: a stream whose
		// target is not a regular file is not one.
		if n, ok := numbered(streamName, "stream", e.Name()); ok && is(e.Name(), regular) {
			streams[n] = true
		}
		if n, ok := numbered(pcmPlaybackName, "pcm", e.Name()); ok && is(e.Name(), directory) {
			pcms[n] = true
		}
	}
	for _, n := range sortedKeys(streams, pcms) {
		d := Device{Device: n, Substreams: []Substream{}}
		if streams[n] {
			rel := "stream" + strconv.Itoa(n)
			if b, ok := read(rel); ok {
				st, probs := ParseStream(b)
				d.Stream = &st
				note(rel, probs)
			}
		}
		if !pcms[n] {
			if d.Stream == nil || d.Stream.Playback == nil {
				continue // a capture-only device
			}
			cr.Devices = append(cr.Devices, d)
			continue
		}
		pcmRel := "pcm" + strconv.Itoa(n) + "p"
		rawOnly(pcmRel + "/info")
		subs, err := t.asound.readDir(dirName+"/"+pcmRel, t.hook("asound/"+dirName+"/"+pcmRel))
		if err != nil {
			cr.Problems = append(cr.Problems, dirName+"/"+pcmRel+": "+errText(err))
		}
		subNums := map[int]bool{}
		for _, e := range subs {
			if m, ok := numbered(subName, "sub", e.Name()); ok && is(pcmRel+"/"+e.Name(), directory) {
				subNums[m] = true
			}
		}
		for _, m := range sortedKeys(subNums) {
			base := pcmRel + "/sub" + strconv.Itoa(m) + "/"
			s := Substream{Subdevice: m}
			rawOnly(base + "info")
			if b, ok := read(base + "hw_params"); ok {
				h, probs := ParseHWParams(b)
				s.HWParams = &h
				note(base+"hw_params", probs)
			}
			rawOnly(base + "sw_params")
			if b, ok := read(base + "status"); ok {
				st, probs := ParseStatus(b)
				s.Status = &st
				note(base+"status", probs)
				if st.OwnerPID != nil {
					s.Owner = t.owner(*st.OwnerPID)
				}
			}
			d.Substreams = append(d.Substreams, s)
		}
		cr.Devices = append(cr.Devices, d)
	}
	return cr
}

// owner resolves a substream's owner_pid, a thread id, to the thread's
// name and its process. Name and Tgid come from one read of the thread's
// status, so the two cannot name different tasks. Whatever cannot be read
// stays nil.
func (t walk) owner(tid int) *Owner {
	o := &Owner{TID: tid}
	if tid <= 0 {
		return o
	}
	name, haveName, pid, havePID := t.taskStatus(tid)
	if !haveName {
		if n, ok := t.comm(tid); ok {
			name, haveName = n, true
		}
	}
	if haveName {
		o.Thread = &name
	}
	if !havePID {
		return o
	}
	o.PID = &pid
	if pid == tid {
		if haveName {
			o.Process = &name
		}
		return o
	}
	if n, ok := t.taskName(pid); ok {
		o.Process = &n
	}
	return o
}

// taskStatus reads Name and Tgid from one /proc/<id>/status.
func (t walk) taskStatus(id int) (name string, haveName bool, pid int, havePID bool) {
	b, ok := t.procFile(strconv.Itoa(id) + "/status")
	if !ok {
		return "", false, 0, false
	}
	name, haveName = statusName(b)
	pid, havePID = tgid(b)
	return name, haveName, pid, havePID
}

// taskName is a task's name: the Name line of its status, or comm when
// that file has none.
func (t walk) taskName(id int) (string, bool) {
	name, haveName, _, _ := t.taskStatus(id)
	if haveName {
		return name, true
	}
	return t.comm(id)
}

// statusName returns the Name line of a /proc status file.
func statusName(b []byte) (string, bool) {
	for _, l := range lines(b) {
		if v, ok := strings.CutPrefix(l, "Name:"); ok {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// comm reads /proc/<id>/comm: a task's name, which the kernel ends with a
// newline.
func (t walk) comm(id int) (string, bool) {
	b, ok := t.procFile(strconv.Itoa(id) + "/comm")
	if !ok {
		return "", false
	}
	return strings.TrimSuffix(string(b), "\n"), true
}

// procFile reads a file under the proc root. A failure is never a
// problem: it leaves the owner unknown.
func (t walk) procFile(rel string) ([]byte, bool) {
	var b []byte
	err := t.procErr
	if t.proc != nil {
		b, err = t.proc.readFile(rel, maxProcFileBytes, t.hook(rel))
	}
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			t.collect(rel, nil, err)
		}
		return nil, false
	}
	t.collect(rel, b, nil)
	return b, true
}

// tgid reads the Tgid line of a /proc/<tid>/status: the id of the process
// the thread belongs to, which is the thread's own id for a process's
// main thread.
func tgid(b []byte) (int, bool) {
	for _, l := range lines(b) {
		if v, ok := strings.CutPrefix(l, "Tgid:"); ok {
			n, ok := atoiNonNeg(strings.TrimSpace(v))
			return n, ok && n > 0
		}
	}
	return 0, false
}

// numbered reads the number out of a directory entry's name, accepting
// only the kernel's own spelling of it ("stream0", never "stream00").
func numbered(re *regexp.Regexp, prefix, name string) (int, bool) {
	m := re.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, ok := atoiNonNeg(m[1])
	if !ok || name[len(prefix):len(prefix)+len(m[1])] != strconv.Itoa(n) {
		return 0, false
	}
	return n, true
}

// sortedKeys returns the union of the sets' keys, ascending.
func sortedKeys(sets ...map[int]bool) []int {
	var out []int
	seen := map[int]bool{}
	for _, set := range sets {
		for k := range set {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Ints(out)
	return out
}

// prefixed opens each problem with the file it is about.
func prefixed(file string, probs []string) []string {
	out := make([]string, 0, len(probs))
	for _, p := range probs {
		out = append(out, file+": "+p)
	}
	return out
}

// errText is an error without the path a *fs.PathError repeats, since a
// problem already opens with the file's path.
func errText(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
