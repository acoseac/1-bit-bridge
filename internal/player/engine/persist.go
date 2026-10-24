package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// The queue file's bounds and the writer's timing.
const (
	// queueVersion is the queue file's format.
	queueVersion = 2
	// maxQueueFileBytes caps the queue file read at a start: the real
	// encoding of a full queue (MaxEntries entries carrying
	// MaxTotalMetadataBytes, base64 rounding up on each, references whose
	// bytes JSON escapes) measured 22,595,456 bytes in version 1;
	// version 2 adds 46 bytes for its UUID
	// (TestAMaximumSizeQueueRoundTrips), and this leaves room above it.
	maxQueueFileBytes = 24 << 20
	// writeCoalesce is how long the writer waits after a change for the
	// next before it writes.
	writeCoalesce = 250 * time.Millisecond
	// writeMaxAge is the longest a change waits to be written while changes
	// keep coming.
	writeMaxAge = 2 * time.Second
	// writeRetryMin and writeRetryMax bound the backoff after a failed
	// write.
	writeRetryMin = time.Second
	writeRetryMax = 30 * time.Second
	// maxAsideNames bounds the names moveAside tries for one bad file: the
	// timestamp has nanoseconds, so a second name is already rare.
	maxAsideNames = 100
)

// queueFile is the queue file: the entries, the reservations and the
// values of the IDs and the token, the modes and the selection. It never
// holds the transport state: a start is always stopped.
type queueFile struct {
	Version  int         `json:"version"`
	UUID     string      `json:"uuid,omitempty"`
	Revision uint64      `json:"revision"`
	IDs      fileIDs     `json:"ids"`
	Tokens   fileTokens  `json:"tokens"`
	Repeat   bool        `json:"repeat"`
	Shuffle  bool        `json:"shuffle"`
	Selected uint32      `json:"selected"`
	Entries  []fileEntry `json:"entries"`

	// dangling is a selection the file named that its queue does not hold,
	// which decodeQueue clears.
	dangling uint32
}

// fileIDs is the ID counter: the next ID and the reservation.
type fileIDs struct {
	Next     uint64 `json:"next"`
	Reserved uint64 `json:"reserved"`
}

// fileTokens is the token: the current one and the reservation.
type fileTokens struct {
	Current  uint32 `json:"current"`
	Reserved uint64 `json:"reserved"`
}

// fileEntry is one entry; its metadata is base64, as encoding/json writes
// a []byte.
type fileEntry struct {
	ID   uint32 `json:"id"`
	Ref  string `json:"ref"`
	Meta []byte `json:"meta"`
}

// payload is one revision of what the queue file holds, built by the owner
// and immutable after: the queue it shares with the snapshots is never
// written to.
type payload struct {
	uuid                 string
	rev                  uint64
	q                    *queue
	idsNext, idsReserved uint64
	tokCurrent           uint32
	tokReserved          uint64
	repeat, shuffle      bool
	selected             uint32
}

// encode is p as the queue file.
func (p *payload) encode() ([]byte, error) {
	f := queueFile{
		Version:  queueVersion,
		UUID:     p.uuid,
		Revision: p.rev,
		IDs:      fileIDs{Next: p.idsNext, Reserved: p.idsReserved},
		Tokens:   fileTokens{Current: p.tokCurrent, Reserved: p.tokReserved},
		Repeat:   p.repeat,
		Shuffle:  p.shuffle,
		Selected: p.selected,
		Entries:  make([]fileEntry, len(p.q.entries)),
	}
	for i, e := range p.q.entries {
		f.Entries[i] = fileEntry{ID: e.id, Ref: e.ref.s, Meta: e.meta}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// strictJSON decodes data into v: one value, no field v does not name.
func strictJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("does not parse: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("does not parse: data after the value")
	}
	return nil
}

// maxFileCounter bounds the queue file's counters. A run's own grow by a
// block for each start while they are exhausted, far short of it, and a
// start past it would wrap the uint64 (startCounter adds a block, target
// another), so an ID or token handed out before could be handed out again
// (CodeRabbit on #1309).
const maxFileCounter = 1 << 62

// decodeQueue reads a queue file and checks everything the engine would
// not have written: the version, the bounds, every ID nonzero, unique,
// handed out and inside its reservation, every reference, shuffle off. A
// selection the queue does not hold is cleared (queueFile.dangling), never a
// reason to move a good queue aside: nothing selected is a state the engine
// has.
func decodeQueue(data []byte) (*queueFile, []entry, error) {
	if len(data) > maxQueueFileBytes {
		return nil, nil, fmt.Errorf("%d bytes or more, past the cap of %d", len(data), maxQueueFileBytes)
	}
	var f queueFile
	if err := strictJSON(data, &f); err != nil {
		return nil, nil, err
	}
	// The counters' values are a run's: a run whose reservation could not
	// be written records values past it, and hands out nothing beyond it,
	// so they are not checked against the reservation; every entry is.
	switch {
	case f.Version != 1 && f.Version != queueVersion:
		return nil, nil, fmt.Errorf("version %d, not %d", f.Version, queueVersion)
	case f.Version == 2 && !validPlayerUUID(f.UUID) || f.Version == 1 && f.UUID != "":
		return nil, nil, errors.New("queue does not name one lowercase UUID for its version")
	case len(f.Entries) > MaxEntries:
		return nil, nil, fmt.Errorf("%d entries, more than %d", len(f.Entries), MaxEntries)
	case f.Shuffle:
		return nil, nil, errors.New("shuffle is on, which this release does not play")
	case f.IDs.Next > maxFileCounter || f.IDs.Reserved > maxFileCounter || f.Tokens.Reserved > maxFileCounter:
		return nil, nil, fmt.Errorf("a counter past %d (ids next %d, reserved %d; tokens reserved %d), which no run writes and a start would wrap",
			uint64(maxFileCounter), f.IDs.Next, f.IDs.Reserved, f.Tokens.Reserved)
	}
	entries := make([]entry, 0, len(f.Entries))
	seen := make(map[uint32]bool, len(f.Entries))
	total := 0
	for i, fe := range f.Entries {
		switch {
		case fe.ID == 0:
			return nil, nil, fmt.Errorf("entry %d has ID 0", i)
		case seen[fe.ID]:
			return nil, nil, fmt.Errorf("entry %d repeats ID %d", i, fe.ID)
		case uint64(fe.ID) >= f.IDs.Next || uint64(fe.ID) > f.IDs.Reserved:
			return nil, nil, fmt.Errorf("entry %d has ID %d, which was never handed out (next %d, reserved %d)", i, fe.ID, f.IDs.Next, f.IDs.Reserved)
		case len(fe.Meta) > MaxMetadataBytes:
			return nil, nil, fmt.Errorf("entry %d has %d bytes of metadata, more than %d", i, len(fe.Meta), MaxMetadataBytes)
		}
		r, err := NewReference(fe.Ref)
		if err != nil {
			return nil, nil, fmt.Errorf("entry %d: %w", i, err)
		}
		total += len(fe.Meta)
		if total > MaxTotalMetadataBytes {
			return nil, nil, fmt.Errorf("more than %d bytes of metadata", MaxTotalMetadataBytes)
		}
		seen[fe.ID] = true
		entries = append(entries, entry{id: fe.ID, ref: r, meta: fe.Meta})
	}
	if f.Selected != 0 && !seen[f.Selected] {
		f.dangling, f.Selected = f.Selected, 0
	}
	f.Entries = nil
	return &f, entries, nil
}

// readLimited reads at most limit+1 bytes of path, so a file past the cap
// is known as one without reading it whole. It opens the path as the routes
// that serve a file's bytes do (fsutil.OpenAsFile): without waiting, and
// reading only a file, so a named pipe, a directory or a device at the path
// is refused at once, a bad file the engine moves aside, where a plain open
// of a named pipe waited for a writer and held New.
func readLimited(path string, limit int64) ([]byte, error) {
	f, _, err := fsutil.OpenAsFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit+1))
}

// readBounded reads a file the engine owns: nil, nil when there is none;
// an error that makes it bad otherwise.
func (e *Engine) readBounded(path string, limit int64) ([]byte, error) {
	data, err := e.sm.readFile(path, limit)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("cannot be read: %w", err)
	case int64(len(data)) > limit:
		return nil, fmt.Errorf("more than %d bytes", limit)
	}
	return data, nil
}

// moveAside renames a bad file to its name with ".bad-" and a timestamp
// after it, never deleting it, and logs why. A name is taken only when
// Lstat says nothing is there, and the next is tried only when it says
// something is: any other answer (a parent without search permission,
// ENOTDIR, an I/O error), or maxAsideNames names all taken, is a move that
// cannot be made. It reports false when the move was not made: the file
// then stays where it is.
func (e *Engine) moveAside(what, path string, why error) bool {
	stamp := e.clk.Now().UTC().Format("20060102T150405.000000000Z")
	to := path + ".bad-" + stamp
	for i := 1; ; i++ {
		_, err := e.sm.lstat(to)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err == nil && i > maxAsideNames {
			err = fmt.Errorf("%d names beside it are taken", maxAsideNames)
		}
		if err != nil {
			e.log.Error("player: the "+what+" is bad and could not be moved aside: no name beside it is free; it is not overwritten until it is moved, and inserts are refused until then",
				"path", path, "reason", why, "err", err)
			return false
		}
		to = fmt.Sprintf("%s.bad-%s-%d", path, stamp, i)
	}
	if err := e.sm.rename(path, to); err != nil {
		e.log.Error("player: the "+what+" is bad and could not be moved aside; it is not overwritten until it is moved, and inserts are refused until then",
			"path", path, "reason", why, "err", err)
		return false
	}
	e.log.Warn("player: the "+what+" was bad and is moved aside; starting from what remains", "path", path, "moved_to", to, "reason", why)
	return true
}

// writer is the one goroutine that writes the queue file: always the newest
// revision the owner accepted, each write after the last has returned, so
// an older revision never lands after a newer one.
type writer struct {
	e *Engine

	mu       sync.Mutex
	latest   *payload
	at       time.Time // when latest was accepted
	since    time.Time // when the oldest revision not yet written was accepted
	written  uint64
	off      bool // the queue file is a bad file that stays: nothing is written over it
	failures int
	backoff  time.Duration
	retryAt  time.Time

	kick chan struct{}
	stop chan struct{}
	// stopOnce closes stop: at the shutdown's flush, or as the owner leaves
	// by a panic (Engine.abandon).
	stopOnce sync.Once
}

func newWriter(e *Engine) *writer {
	return &writer{e: e, kick: make(chan struct{}, 1), stop: make(chan struct{})}
}

// stopWriting asks the writer to write the newest revision once more, if it
// is not written, and exit.
func (w *writer) stopWriting() { w.stopOnce.Do(func() { close(w.stop) }) }

// update hands the writer a newer revision.
func (w *writer) update(p *payload) {
	w.mu.Lock()
	now := w.e.clk.Now()
	if w.latest == nil || w.latest.rev <= w.written {
		w.since = now
	}
	w.latest, w.at = p, now
	w.mu.Unlock()
	poke(w.kick)
}

// setOff stops or resumes writing.
func (w *writer) setOff(off bool) {
	w.mu.Lock()
	w.off = off
	w.mu.Unlock()
	poke(w.kick)
}

// due is the revision to write and when: writeCoalesce after the last
// change, at most writeMaxAge after the oldest unwritten one, and not
// before a failed write's backoff has passed.
func (w *writer) due() (*payload, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.off || w.latest == nil || w.latest.rev <= w.written {
		return nil, time.Time{}
	}
	due := w.at.Add(writeCoalesce)
	if limit := w.since.Add(writeMaxAge); limit.Before(due) {
		due = limit
	}
	if w.failures > 0 && due.Before(w.retryAt) {
		due = w.retryAt
	}
	return w.latest, due
}

// run is the writer goroutine. Asked to stop, it writes the newest
// revision once more if it is not written, and exits. A panic in a write is
// that write's failure (attempt); one anywhere else on the goroutine ends
// the writer (ended).
func (w *writer) run() {
	var t timer
	defer w.ended()
	defer func() {
		if t != nil {
			t.Stop()
		}
	}()
	for {
		select {
		case <-w.stop:
			if p, _ := w.due(); p != nil {
				w.write(p)
			}
			w.e.post(msgWriterExited{})
			return
		default:
		}
		p, due := w.due()
		if p != nil {
			if !due.After(w.e.clk.Now()) {
				w.write(p)
				continue
			}
			if t != nil {
				t.Stop()
			}
			t = w.e.clk.At(due, func() { poke(w.kick) })
		}
		if w.e.sm.writerWaits != nil {
			seen := w.writtenRev()
			if p != nil {
				seen = p.rev
			}
			w.e.sm.writerWaits(seen)
		}
		select {
		case <-w.kick:
		case <-w.stop:
		}
	}
}

// writtenRev is the newest revision written.
func (w *writer) writtenRev() uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// ended is the writer goroutine's last word when it leaves by a panic, which
// it recovers and logs: nothing writes the queue file from then on, so the
// state stays dirty, and the owner is told so, as of a write that failed,
// and that the writer has exited, so its shutdown waits for no flush. A
// writer that returned has told the owner itself.
func (w *writer) ended() {
	p := recover()
	if p == nil {
		return
	}
	w.e.logPanic("the queue file's writer", p)
	w.mu.Lock()
	w.failures++
	failures, written := w.failures, w.written
	w.mu.Unlock()
	w.e.post(msgPersisted{written: written, err: errors.New("the queue file's writer panicked and has stopped: nothing writes the queue file until the engine starts again; the bridge's log has why"), failures: failures})
	w.e.post(msgWriterExited{})
}

// attempt encodes p and writes it. A panic in either is recovered and
// logged, and is the write's failure: the state stays dirty, and the write
// is retried after a backoff, as after any failed write.
func (w *writer) attempt(p *payload) (err error) {
	defer func() {
		if r := recover(); r != nil {
			w.e.logPanic("the queue file's writer", r)
			err = errors.New("the queue file write panicked; the bridge's log has why")
		}
	}()
	data, err := p.encode()
	if err != nil {
		return err
	}
	return w.e.sm.writeQueue(w.e.cfg.QueuePath, data)
}

// write writes p and tells the owner how it went: a failure keeps the
// state dirty and is retried after a backoff, logged once until a write
// lands.
func (w *writer) write(p *payload) {
	began := w.e.clk.Now()
	err := w.attempt(p)
	w.mu.Lock()
	recovered := false
	if err == nil {
		recovered = w.failures > 0
		w.written = max(w.written, p.rev)
		w.failures, w.backoff = 0, 0
		if w.latest.rev > w.written {
			w.since = began
		}
	} else {
		w.failures++
		w.backoff = min(max(2*w.backoff, writeRetryMin), writeRetryMax)
		w.retryAt = w.e.clk.Now().Add(w.backoff)
	}
	failures, written := w.failures, w.written
	w.mu.Unlock()
	switch {
	case err != nil && failures == 1:
		w.e.log.Warn("player: queue file write failed; the state stays dirty and the write is retried with backoff", "path", w.e.cfg.QueuePath, "err", err)
	case recovered:
		w.e.log.Info("player: queue file written again", "path", w.e.cfg.QueuePath)
	}
	w.e.post(msgPersisted{written: written, err: err, failures: failures})
}

// poke wakes whoever waits on ch, once, without waiting.
func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
