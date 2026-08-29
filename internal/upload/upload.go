package upload

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

var logger = logging.Component("upload")

// Sentinels. Callers map these onto status codes; see the admin handlers.
var (
	ErrNotFound       = errors.New("upload: session not found")
	ErrOffsetMismatch = errors.New("upload: offset mismatch")
	ErrDigestMismatch = errors.New("upload: digest mismatch")
	ErrTooLarge       = errors.New("upload: exceeds limit")
	ErrNoSpace        = errors.New("upload: insufficient free space")
	ErrIncomplete     = errors.New("upload: file incomplete")
	ErrUnknownRoot    = errors.New("upload: not a configured library root")
)

// OffsetMismatch carries the offset the server actually holds, so a client
// seeks instead of guessing. It wraps ErrOffsetMismatch.
type OffsetMismatch struct{ Actual int64 }

func (e *OffsetMismatch) Error() string {
	return fmt.Sprintf("%v: server holds %d", ErrOffsetMismatch, e.Actual)
}
func (e *OffsetMismatch) Unwrap() error { return ErrOffsetMismatch }

// NoSpace carries what a purge could reclaim, so a 507 can be turned into an
// "empty trash and resume" action rather than a dead end.
type NoSpace struct {
	FreeBytes        int64
	NeedBytes        int64
	ReclaimableBytes int64
}

func (e *NoSpace) Error() string {
	return fmt.Sprintf("%v: %d free, %d needed", ErrNoSpace, e.FreeBytes, e.NeedBytes)
}
func (e *NoSpace) Unwrap() error { return ErrNoSpace }

// Defaults. Every one resolves from zero AND from a negative value: a -1 typed
// hoping to disable a cap must not yield an unbounded one.
const (
	DefaultMaxFileBytes    = 8 << 30 // clears a SACD ISO (~4.7 GB)
	DefaultMaxSessionFiles = 2000    // the browser's ceiling, not the server's
	DefaultMinFreeBytes    = 5 << 30
	DefaultSessionTTL      = 24 * time.Hour
	DefaultChunkBytes      = 4 << 20 // a HINT; the server accepts any size
)

// Config is resolved once at construction.
type Config struct {
	MaxFileBytes    int64
	MaxSessionFiles int
	MinFreeBytes    int64
	SessionTTL      time.Duration
	ChunkBytes      int64
}

func (c Config) resolved() Config {
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = DefaultMaxFileBytes
	}
	if c.MaxSessionFiles <= 0 {
		c.MaxSessionFiles = DefaultMaxSessionFiles
	}
	if c.MinFreeBytes <= 0 {
		c.MinFreeBytes = DefaultMinFreeBytes
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = DefaultSessionTTL
	}
	if c.ChunkBytes <= 0 {
		c.ChunkBytes = DefaultChunkBytes
	}
	return c
}

// Manager owns staging for every configured root.
type Manager struct {
	cfg   Config
	locks *keyedLocks

	// roots returns the LIVE library roots. A snapshot would keep routing
	// against a set the admin has since changed (the same reason the api
	// server borrows a hot-reloading Resolver rather than a slice).
	roots func() []string

	// freeBytes probes a volume. Injected so tests can drive the floor and
	// the fail-closed path without filling a disk.
	freeBytes func(dir string) (int64, error)

	// reclaimable reports what a trash purge could return, for the NoSpace
	// error. Nil until PR 5 wires it.
	reclaimable func(root string) int64

	now func() time.Time
}

// Option configures a Manager.
type Option func(*Manager)

// WithFreeBytes overrides the disk probe.
func WithFreeBytes(fn func(dir string) (int64, error)) Option {
	return func(m *Manager) { m.freeBytes = fn }
}

// WithClock overrides time.Now.
func WithClock(fn func() time.Time) Option { return func(m *Manager) { m.now = fn } }

// WithReclaimable supplies the trash-purge estimate used by NoSpace.
func WithReclaimable(fn func(root string) int64) Option {
	return func(m *Manager) { m.reclaimable = fn }
}

// NewManager builds a Manager over the given live-root accessor.
func NewManager(cfg Config, roots func() []string, opts ...Option) *Manager {
	m := &Manager{
		cfg:       cfg.resolved(),
		locks:     newKeyedLocks(),
		roots:     roots,
		freeBytes: defaultFreeBytes,
		now:       time.Now,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// ChunkBytes is the size hint handed to clients at session create.
func (m *Manager) ChunkBytes() int64 { return m.cfg.ChunkBytes }

// resolveRoot matches a caller-supplied root against the live set. An empty
// string selects the first configured root, which is the unambiguous cloud
// case (exactly one bridge-managed root).
func (m *Manager) resolveRoot(want string) (string, error) {
	roots := m.roots()
	if len(roots) == 0 {
		return "", ErrUnknownRoot
	}
	if want == "" {
		return roots[0], nil
	}
	for _, r := range roots {
		if fsutil.EvalSymlinksOrClean(r) == fsutil.EvalSymlinksOrClean(want) {
			return r, nil
		}
		if filepath.Base(r) == want {
			return r, nil
		}
	}
	return "", ErrUnknownRoot
}

// FileDecl is one client-declared file at session create.
type FileDecl struct {
	Path   string
	Size   int64
	Digest string // optional hex SHA-256
}

// CreateOptions are the per-session knobs a caller (ultimately the control
// plane) supplies.
type CreateOptions struct {
	Root      string
	Overwrite bool
	// MaxBytes is a request-scoped ceiling. It is NOT per-root accounting —
	// the bridge never tries to learn what a tenant is allowed to hold.
	MaxBytes int64
}

// FileStatus is the per-file view returned to clients.
type FileStatus struct {
	ID          string
	Path        string
	Size        int64
	Offset      int64
	SHA256      string
	Complete    bool
	DuplicateOf string // an existing library path this looks like
}

// Session is the client-facing session view.
type Session struct {
	ID         string
	Root       string
	RootName   string
	CreatedAt  time.Time
	Overwrite  bool
	ChunkBytes int64
	Files      []FileStatus
}

// Create validates the declaration, reserves staging, and writes the manifest.
//
// Validation is deliberately front-loaded: a rejection here costs nothing,
// whereas the same rejection at commit costs the whole transfer.
func (m *Manager) Create(decls []FileDecl, opts CreateOptions) (*Session, error) {
	root, err := m.resolveRoot(opts.Root)
	if err != nil {
		return nil, err
	}
	if len(decls) == 0 {
		return nil, fmt.Errorf("%w: no files declared", ErrInvalidPath)
	}
	if len(decls) > m.cfg.MaxSessionFiles {
		return nil, fmt.Errorf("%w: %d files declared, limit is %d", ErrTooLarge, len(decls), m.cfg.MaxSessionFiles)
	}

	doc := sessionDoc{
		ID:        newID(),
		Root:      root,
		CreatedAt: m.now().UTC(),
		Overwrite: opts.Overwrite,
		MaxBytes:  opts.MaxBytes,
	}

	var total int64
	seen := make(map[string]bool, len(decls))
	for _, d := range decls {
		clean, err := ValidateRelPath(d.Path)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", d.Path, err)
		}
		if seen[clean] {
			return nil, fmt.Errorf("%w: %q declared twice", ErrInvalidPath, clean)
		}
		seen[clean] = true
		if d.Size < 0 {
			return nil, fmt.Errorf("%w: %q has a negative size", ErrInvalidPath, clean)
		}
		if d.Size > m.cfg.MaxFileBytes {
			return nil, fmt.Errorf("%w: %q is %d bytes, limit is %d", ErrTooLarge, clean, d.Size, m.cfg.MaxFileBytes)
		}
		if d.Digest != "" {
			if _, err := hex.DecodeString(d.Digest); err != nil || len(d.Digest) != 64 {
				return nil, fmt.Errorf("%w: %q has a malformed digest", ErrInvalidPath, clean)
			}
		}
		total += d.Size
		doc.Files = append(doc.Files, fileDoc{
			ID: newID(), RelPath: clean, Size: d.Size, Digest: strings.ToLower(d.Digest),
		})
	}

	// Request-scoped ceiling, supplied by the caller.
	if opts.MaxBytes > 0 && total > opts.MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes declared, session limit is %d", ErrTooLarge, total, opts.MaxBytes)
	}
	if err := m.checkSpace(root, total); err != nil {
		return nil, err
	}

	dir := m.sessionDir(root, doc.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create staging dir: %w", err)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if err := atomicwrite.WriteBytes(m.manifestPath(root, doc.ID), b, ".manifest-"); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("write session manifest: %w", err)
	}
	return m.view(doc), nil
}

// checkSpace fails CLOSED. A probe error means no way to honour the floor, and
// a refused session costs a retry while a wrong guess fills the volume.
func (m *Manager) checkSpace(root string, need int64) error {
	free, err := m.freeBytes(root)
	if err != nil {
		logger.Error("free-space probe failed; refusing upload", "root", root, "err", err)
		return fmt.Errorf("%w: free-space probe failed: %v", ErrNoSpace, err)
	}
	if free-need < m.cfg.MinFreeBytes {
		e := &NoSpace{FreeBytes: free, NeedBytes: need + m.cfg.MinFreeBytes}
		if m.reclaimable != nil {
			e.ReclaimableBytes = m.reclaimable(root)
		}
		return e
	}
	return nil
}

func (m *Manager) view(doc sessionDoc) *Session {
	s := &Session{
		ID:         doc.ID,
		Root:       doc.Root,
		RootName:   filepath.Base(doc.Root),
		CreatedAt:  doc.CreatedAt,
		Overwrite:  doc.Overwrite,
		ChunkBytes: m.cfg.ChunkBytes,
	}
	for _, f := range doc.Files {
		st, err := m.readState(doc.Root, doc.ID, f.ID)
		if err != nil {
			logger.Warn("read upload file state", "session", doc.ID, "file", f.ID, "err", err)
		}
		s.Files = append(s.Files, FileStatus{
			ID:       f.ID,
			Path:     f.RelPath,
			Size:     f.Size,
			Offset:   st.Offset,
			SHA256:   st.SHA256,
			Complete: st.Offset == f.Size,
		})
	}
	return s
}

// findSession locates a session across every configured root.
func (m *Manager) findSession(sid string) (sessionDoc, error) {
	if sid == "" || strings.ContainsAny(sid, `/\.`) {
		return sessionDoc{}, ErrNotFound
	}
	for _, root := range m.roots() {
		var doc sessionDoc
		err := readJSONFile(m.manifestPath(root, sid), &doc)
		if err == nil {
			// The manifest records its own root; trust the location we
			// found it in, so a hand-edited Root cannot redirect a commit.
			doc.Root = root
			return doc, nil
		}
		if !os.IsNotExist(err) {
			return sessionDoc{}, err
		}
	}
	return sessionDoc{}, ErrNotFound
}

// Get returns the current view of a session, including per-file offsets. This
// is the resume read.
func (m *Manager) Get(sid string) (*Session, error) {
	doc, err := m.findSession(sid)
	if err != nil {
		return nil, err
	}
	return m.view(doc), nil
}

// List returns every live session across every root.
func (m *Manager) List() ([]*Session, error) {
	var out []*Session
	for _, root := range m.roots() {
		entries, err := os.ReadDir(filepath.Join(root, StagingDirName))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			var doc sessionDoc
			if err := readJSONFile(m.manifestPath(root, e.Name()), &doc); err != nil {
				continue // orphan; the sweeper reaps it
			}
			doc.Root = root
			out = append(out, m.view(doc))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// WriteChunk appends one chunk at offset.
//
// chunkDigest, when non-nil, is the expected raw SHA-256 of THIS chunk's bytes
// (RFC 9530 Content-Digest). A mismatch leaves the offset untouched so the
// client simply re-sends.
func (m *Manager) WriteChunk(sid, fid string, offset int64, r io.Reader, chunkDigest []byte) (int64, error) {
	doc, err := m.findSession(sid)
	if err != nil {
		return 0, err
	}
	var fd *fileDoc
	for i := range doc.Files {
		if doc.Files[i].ID == fid {
			fd = &doc.Files[i]
			break
		}
	}
	if fd == nil {
		return 0, ErrNotFound
	}

	unlock := m.locks.lock(sid + "/" + fid)
	defer unlock()

	st, err := m.readState(doc.Root, sid, fid)
	if err != nil {
		return 0, err
	}
	if offset != st.Offset {
		return st.Offset, &OffsetMismatch{Actual: st.Offset}
	}
	if st.Offset >= fd.Size {
		return st.Offset, nil // already complete; idempotent re-send
	}

	part := m.partPath(doc.Root, sid, fid)
	f, err := openStagedFile(part)
	if err != nil {
		return st.Offset, err
	}
	defer func() { _ = f.Close() }()

	// Discard anything past the durable offset: a dropped PUT leaves a tail
	// that was never acknowledged.
	if err := f.Truncate(st.Offset); err != nil {
		return st.Offset, fmt.Errorf("truncate staged file to durable offset: %w", err)
	}
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		return st.Offset, err
	}

	h, err := resumeHasher(st)
	if err != nil {
		return st.Offset, err
	}
	chunkHash := sha256.New()
	remaining := fd.Size - st.Offset
	limited := io.LimitReader(r, remaining)
	written, copyErr := io.Copy(io.MultiWriter(f, h, chunkHash), limited)

	if written == 0 && copyErr != nil {
		return st.Offset, copyErr
	}
	if chunkDigest != nil && !bytes.Equal(chunkHash.Sum(nil), chunkDigest) {
		// Offset unchanged: the next attempt truncates this away.
		return st.Offset, ErrDigestMismatch
	}
	// Bytes durable BEFORE the offset that advertises them. The reverse order
	// lets a crash advertise an offset the file cannot honour, and then the
	// truncate-on-open would be discarding real data instead of garbage.
	if err := f.Sync(); err != nil {
		return st.Offset, fmt.Errorf("sync staged file: %w", err)
	}

	next := st.Offset + written
	newState := fileState{Offset: next}
	if next >= fd.Size {
		sum := hex.EncodeToString(h.Sum(nil))
		if fd.Digest != "" && sum != fd.Digest {
			// Do not advertise a completed file whose content disagrees
			// with what the client said it was sending.
			return st.Offset, fmt.Errorf("%w: declared %s, received %s", ErrDigestMismatch, fd.Digest, sum)
		}
		newState.SHA256 = sum
	} else {
		if newState.HashState, err = marshalHasher(h); err != nil {
			return st.Offset, err
		}
	}
	b, err := json.Marshal(newState)
	if err != nil {
		return st.Offset, err
	}
	if err := atomicwrite.WriteBytes(m.statePath(doc.Root, sid, fid), b, ".meta-"); err != nil {
		return st.Offset, fmt.Errorf("persist upload offset: %w", err)
	}
	if copyErr != nil && !errors.Is(copyErr, io.EOF) {
		// Partial chunk: the bytes we did take are durable and recorded, so
		// the client resumes from `next` rather than re-sending them.
		return next, copyErr
	}
	return next, nil
}

// Abandon removes a session's staging directory.
func (m *Manager) Abandon(sid string) error {
	doc, err := m.findSession(sid)
	if err != nil {
		return err
	}
	return m.removeSession(doc.Root, sid)
}

func (m *Manager) removeSession(root, sid string) error {
	if err := os.RemoveAll(m.sessionDir(root, sid)); err != nil {
		return err
	}
	m.locks.forgetPrefix(sid + "/")
	return nil
}

// CommitOutcome is one file's result.
type CommitOutcome struct {
	Path   string
	Status string // "committed" | "skipped" | "failed"
	Reason string
	Bytes  int64
}

// CommitResult is what the caller reports and scans.
type CommitResult struct {
	Root      string
	Outcomes  []CommitOutcome
	ScanDirs  []string // library-relative directories that gained content
	Committed int
	Skipped   int
	Failed    int
}

// Commit renames every complete staged file into the library.
//
// Per-file best-effort with an honest report: files that landed are real files,
// so a partial album is incomplete rather than corrupt, and the caller is told
// exactly which failed and why.
func (m *Manager) Commit(sid string) (*CommitResult, error) {
	doc, err := m.findSession(sid)
	if err != nil {
		return nil, err
	}
	res := &CommitResult{Root: doc.Root}
	dirs := make(map[string]struct{})

	for _, fd := range doc.Files {
		out := CommitOutcome{Path: fd.RelPath, Bytes: fd.Size}
		st, err := m.readState(doc.Root, sid, fd.ID)
		if err != nil || st.Offset != fd.Size {
			out.Status, out.Reason = "failed", "incomplete"
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		// Re-validate. The manifest has been on disk since Create and is not
		// trusted to still say what it said.
		clean, err := ValidateRelPath(fd.RelPath)
		if err != nil {
			out.Status, out.Reason = "failed", err.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		dest := filepath.Join(doc.Root, filepath.FromSlash(clean))
		if err := AssertRootContains(doc.Root, dest); err != nil {
			out.Status, out.Reason = "failed", err.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		if !doc.Overwrite {
			if _, err := os.Stat(dest); err == nil {
				out.Status, out.Reason = "skipped", "a file already exists at this path"
				res.Skipped++
				res.Outcomes = append(res.Outcomes, out)
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			out.Status, out.Reason = "failed", err.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		// RenameWithRetry absorbs the Windows scan-on-close window.
		if err := atomicwrite.RenameWithRetry(m.partPath(doc.Root, sid, fd.ID), dest); err != nil {
			out.Status, out.Reason = "failed", err.Error()
			res.Failed++
			res.Outcomes = append(res.Outcomes, out)
			continue
		}
		if err := fsutil.SyncParentDir(dest); err != nil {
			logger.Warn("sync parent dir after commit", "path", dest, "err", err)
		}
		out.Status = "committed"
		res.Committed++
		res.Outcomes = append(res.Outcomes, out)
		if d := path.Dir(clean); d != "." {
			dirs[d] = struct{}{}
		} else {
			dirs["."] = struct{}{}
		}
	}

	for d := range dirs {
		res.ScanDirs = append(res.ScanDirs, d)
	}
	sort.Strings(res.ScanDirs)

	if err := m.removeSession(doc.Root, sid); err != nil {
		logger.Warn("remove staging dir after commit", "session", sid, "err", err)
	}
	return res, nil
}

// openStagedFile opens a .part for positioned writing.
//
// The flags are the point: NOT O_APPEND. POSIX sets the file offset to
// end-of-file before EVERY O_APPEND write, so an explicit Seek is silently
// ignored. Here that would produce the right bytes by accident — the caller
// truncates to the durable offset first, making EOF equal that offset — and an
// accident that happens to be correct is one edit away from not being. With
// explicit positioning the offset is an assertion: a future change that drops
// the truncate fails loudly instead of appending to garbage.
func openStagedFile(path string) (*os.File, error) {
	// 0o644, not 0o600: this mode SURVIVES the commit rename, so it is the
	// mode the file has once it is part of the library. 0o600 leaves an
	// uploaded track readable only by the bridge's own user — unlike every
	// other file in the library — so a Samba share, a backup tool, or any
	// other service running as a different user silently cannot read it.
	// The process umask still applies, so a hardened host gets its own
	// stricter answer rather than this one.
	//
	// Staging is not exposed in the meantime: its directory is 0o700.
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
}
