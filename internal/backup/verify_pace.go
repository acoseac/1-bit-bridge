package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"
)

// verifyReadRate is how many bytes a second a verification reads at most,
// its hash of each file and SQLite's reads of the database alike (backlog
// B653: one verifier at a time, at low priority, its reads paced). A test
// seam (SetVerifyReadRateForTest); 0 reads unpaced.
var verifyReadRate int64 = 32 << 20

// pacer holds a verification's reads to its rate: each read is counted, and
// one that would take the reads past the rate waits until it would not,
// unless its context ends first. It is shared by every read of one
// verification.
type pacer struct {
	ctx   context.Context
	rate  int64
	mu    sync.Mutex
	start time.Time
	read  int64
}

// newPacer is a pacer over ctx at the package's rate.
func newPacer(ctx context.Context) *pacer {
	return &pacer{ctx: ctx, rate: verifyReadRate, start: time.Now()}
}

// paceSlack is how far ahead of its rate a pacer lets reads run before it
// waits: a wait shorter than this is not worth a timer.
const paceSlack = 10 * time.Millisecond

// admit counts a read of n bytes, waiting while the reads so far are ahead
// of the rate, and answers the context's end, once it has ended, before any
// read: every read of a verification asks it, so a stop is seen at the next
// read, wherever the check is.
func (p *pacer) admit(n int) error {
	if err := p.ctx.Err(); err != nil {
		return err
	}
	if p.rate <= 0 {
		return nil
	}
	p.mu.Lock()
	p.read += int64(n)
	due := p.start.Add(time.Duration(float64(p.read) / float64(p.rate) * float64(time.Second)))
	p.mu.Unlock()
	wait := time.Until(due)
	if wait < paceSlack {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-p.ctx.Done():
		return p.ctx.Err()
	case <-t.C:
		return nil
	}
}

// hashChunk is how much a verification's hash reads at once.
const hashChunk = 1 << 20

// hashPaced is the size and SHA-256 (hex) of what f holds, read through p,
// from offset 0.
func hashPaced(p *pacer, f io.ReaderAt) (int64, string, error) {
	h := sha256.New()
	buf := make([]byte, hashChunk)
	var off int64
	for {
		if err := p.admit(len(buf)); err != nil {
			return off, "", err
		}
		n, err := f.ReadAt(buf, off)
		h.Write(buf[:n])
		off += int64(n)
		if errors.Is(err, io.EOF) {
			return off, hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil {
			return off, "", err
		}
	}
}

// dbFS is the one-file filesystem a verification hands SQLite through
// modernc's read-only VFS (verify_db.go): it serves the database the
// verification opened, under dbFSName, and nothing else, so SQLite opens no
// other file (no -wal, no -shm, no journal) and creates none, and every read
// of it goes through the verification's pacer.
type dbFS struct {
	f *os.File
	p *pacer
}

// dbFSName is the name the database has in dbFS, the one name it serves.
const dbFSName = "bridge.db"

// Open serves the database under dbFSName and refuses every other name as
// not there.
func (d dbFS) Open(name string) (fs.File, error) {
	if name != dbFSName {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &dbFile{f: d.f, p: d.p}, nil
}

// Stat answers for the one name, so SQLite's question of whether a -wal or a
// journal is there gets no.
func (d dbFS) Stat(name string) (fs.FileInfo, error) {
	if name != dbFSName {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return d.f.Stat()
}

// dbFile is SQLite's view of the database: reads at an offset, each through
// the pacer. Its Close leaves the file open: the verification closes it, once
// SQLite has closed the database.
type dbFile struct {
	f   *os.File
	p   *pacer
	off int64
}

func (d *dbFile) Stat() (fs.FileInfo, error) { return d.f.Stat() }

func (d *dbFile) Read(b []byte) (int, error) {
	if err := d.p.admit(len(b)); err != nil {
		return 0, err
	}
	n, err := d.f.ReadAt(b, d.off)
	d.off += int64(n)
	return n, err
}

func (d *dbFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += d.off
	default:
		return 0, errors.New("backup: dbFile seeks from its start or where it is")
	}
	if offset < 0 {
		return 0, errors.New("backup: dbFile seek before its start")
	}
	d.off = offset
	return offset, nil
}

func (d *dbFile) Close() error { return nil }
