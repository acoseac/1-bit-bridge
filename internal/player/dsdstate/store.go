package dsdstate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// tmpPrefix is the staged file's name pattern beside the state.
const tmpPrefix = ".player-dsd-*.tmp"

// maxAsideNames bounds the names tried for one bad file's copy, as the
// engine bounds its queue's.
const maxAsideNames = 100

// What Open found at the state's name.
const (
	// Loaded: a state read whole.
	Loaded Outcome = iota + 1
	// Missing: no file, so the defaults, and nothing is written until the
	// first write.
	Missing
	// Damaged: a file read and refused; its bytes are kept beside it
	// (Report.Aside), and a fresh state that refuses DSD took its place
	// (Refusing), or, where that write failed (Report.Kept), holds in memory
	// alone with the bad bytes still at the name.
	Damaged
	// Unread: a file that could not be read, or could not be set aside: it
	// stays where it is, the state is Refusing in memory, and no write
	// replaces the file until it is moved.
	Unread
)

// Outcome is what Open found.
type Outcome int

// Report is what Open found and did, for its caller's log line.
type Report struct {
	Outcome Outcome
	// Why is why the file did not read (Damaged, Unread).
	Why error
	// Aside is where a damaged file's bytes were kept.
	Aside string
	// Kept is why a damaged file could not be set aside (Unread) or
	// replaced (Damaged, its fresh state in memory alone): nil when it was.
	Kept error
}

// ErrNotWritable is a write refused because the state's file is there and
// could not be read or set aside, so a write would replace what the state
// could not keep: the file must be moved first.
var ErrNotWritable = errors.New("dsdstate: the state's file could not be read or set aside, and is not written over until it is moved")

// Store is the state's one writer (the package's doc): it holds the state
// the last write left, and makes each write durable before it answers.
type Store struct {
	dataDir, path string
	write         func(path string, data []byte) error
	now           func() time.Time

	mu      sync.Mutex
	state   State
	blocked error
	report  Report
}

// options are a test's seams, on the store they serve.
type options struct {
	// write writes data to path durably: nil is atomicwrite.WriteBytesDurable
	// under the data directory.
	write func(path string, data []byte) error
	// now is the clock: nil is time.Now.
	now func() time.Time
}

// Open reads the state at <dataDir>/player-dsd.json and returns its writer.
// A missing file is the defaults (Missing), and nothing is written. A file
// that reads and does not decode (Damaged) has its bytes copied, durably, to
// player-dsd.json.bad-<timestamp> beside it, never deleted, and is then
// replaced, in one rename, by a fresh state that refuses DSD (Refusing),
// written durably: so the name holds the bad bytes or the fresh state at
// every moment, and the refusal survives a restart. Where the replacement
// fails, the fresh state holds in memory and the next Open tries again; the
// load never answers the defaults over a file that is there. A file that
// cannot be read (a read error, a named pipe or a directory at the name, one
// past the bound, which cannot be copied whole), or whose bytes cannot be set
// aside, is left where it is (Unread), the state is Refusing in memory, and
// no write replaces the file until it is moved. Its only error is an empty
// data directory: what it found is Report's.
func Open(dataDir string) (*Store, error) { return open(dataDir, options{}) }

func open(dataDir string, o options) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("dsdstate: a data directory is required")
	}
	s := &Store{dataDir: dataDir, path: filepath.Join(dataDir, FileName), write: o.write, now: o.now}
	if s.write == nil {
		s.write = func(path string, data []byte) error {
			return atomicwrite.WriteBytesDurable(s.dataDir, path, data, tmpPrefix)
		}
	}
	if s.now == nil {
		s.now = time.Now
	}
	data, err := readBounded(s.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		s.state, s.report = Defaults(), Report{Outcome: Missing}
	case err != nil:
		s.unread(fmt.Errorf("dsdstate: the state cannot be read: %w", err), nil)
	case len(data) > maxFileBytes:
		s.unread(fmt.Errorf("dsdstate: more than %d bytes", maxFileBytes), errors.New("dsdstate: a file past the bound is not copied aside, since it is not read whole"))
	default:
		st, err := Decode(data)
		if err != nil {
			s.damaged(data, err)
			return s, nil
		}
		s.state, s.report = st, Report{Outcome: Loaded}
	}
	return s, nil
}

// readBounded reads at most maxFileBytes+1 bytes of path, opened as a file
// alone (fsutil.OpenAsFile): a named pipe, a directory or a device at the
// name is refused at once, never waited on.
func readBounded(path string) ([]byte, error) {
	f, _, err := fsutil.OpenAsFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxFileBytes+1))
}

// unread is a file left where it is: the state refuses in memory, and no
// write replaces the file.
func (s *Store) unread(why, kept error) {
	s.state = Refusing(s.now())
	s.blocked = errors.Join(why, kept)
	s.report = Report{Outcome: Unread, Why: why, Kept: kept}
}

// damaged keeps data, a file that read and did not decode, beside it, then
// replaces it with the fresh state that refuses (Open's doc).
func (s *Store) damaged(data []byte, why error) {
	aside, err := s.setAside(data)
	if err != nil {
		s.unread(why, fmt.Errorf("dsdstate: the state's bytes could not be kept aside: %w", err))
		return
	}
	fresh := Refusing(s.now())
	s.state, s.report = fresh, Report{Outcome: Damaged, Why: why, Aside: aside}
	out, err := fresh.encode()
	if err == nil {
		err = s.write(s.path, out)
	}
	if err != nil {
		s.report.Kept = fmt.Errorf("dsdstate: the fresh state is in memory alone: %w", err)
	}
}

// setAside writes data, durably, to a free name beside the state:
// player-dsd.json.bad-<timestamp>, and "-<n>" after it while one is taken. A
// name is taken only when Lstat says nothing is there; any other answer, or
// maxAsideNames names taken, is a copy that cannot be made.
func (s *Store) setAside(data []byte) (string, error) {
	when := s.now().UTC().Format("20060102T150405.000000000Z")
	to := s.path + ".bad-" + when
	for i := 1; ; i++ {
		_, err := os.Lstat(to)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err == nil && i > maxAsideNames {
			err = fmt.Errorf("%d names beside it are taken", maxAsideNames)
		}
		if err != nil {
			return "", err
		}
		to = fmt.Sprintf("%s.bad-%s-%d", s.path, when, i)
	}
	if err := s.write(to, data); err != nil {
		return "", err
	}
	return to, nil
}

// Report is what Open found and did.
func (s *Store) Report() Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.report
}

// State is a copy of the state the last write left.
func (s *Store) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Clone()
}

// Update makes fn's change to a copy of the state, under the store's lock,
// and writes it: it returns once the file and every directory entry up to
// the data directory are durable (atomicwrite.WriteBytesDurable), and only
// then holds the state as changed, so a write refused (fn's error, a state
// that does not check, a write that failed, ErrNotWritable) leaves the state
// as it was, and is the caller's to answer as nothing changed. A write that
// fails may still leave its bytes in the file, which a restart reads, as the
// writer says: it is not counted made, never counted undone. A change that
// leaves the state as it was writes nothing, so a fresh install with nothing
// to write has no file. Writes are serialized on the lock, each on the state
// the last one left: a compare-and-set (B9's revision) compares inside fn.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(fn)
}

func (s *Store) updateLocked(fn func(*State) error) error {
	if s.blocked != nil {
		return fmt.Errorf("%w: %w", ErrNotWritable, s.blocked)
	}
	next := s.state.Clone()
	if err := fn(&next); err != nil {
		return err
	}
	next = next.normalized()
	data, err := next.encode()
	if err != nil {
		return err
	}
	if was, err := s.state.encode(); err == nil && bytes.Equal(was, data) {
		return nil
	}
	if err := s.write(s.path, data); err != nil {
		return fmt.Errorf("dsdstate: the state is not written: %w", err)
	}
	s.state = next
	return nil
}

// Migrate runs the migration's step on the state and writes what it
// changed, as Update writes (the design's section 3.6; Migrate, the pure
// table, is the step): the part of the bridge's at once, when no migration
// ran and the legacy keys set any key, and the part of a DAC's once bound,
// the DAC the binding resolves to now (nil while none does), is the DAC it
// waits for. When the step asks the owner's record, it reads it through
// ReadRecord, under stateDir, the bridge's state directory, which
// dsdQualification names a record within (serve's data directory), by the
// name the migration took. It reads nothing and writes nothing over a state
// that did not read, which a marker keeps from any migration.
func (s *Store) Migrate(stateDir string, legacy Legacy, bound *Bound) (Step, error) {
	var step Step
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.updateLocked(func(st *State) error {
		in := Input{Legacy: legacy, Bound: bound, Now: s.now()}
		if bound != nil {
			if name, ask := RecordToRead(*st, legacy); ask {
				in.Record = ReadRecord(stateDir, name, *bound)
			}
		}
		next, done, err := Migrate(*st, in)
		if err != nil {
			return err
		}
		*st, step = next, done
		return nil
	})
	return step, err
}
