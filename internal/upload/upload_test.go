package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const roomyDisk = int64(1) << 40

func newTestManager(t *testing.T, opts ...Option) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	all := append([]Option{
		WithFreeBytes(func(string) (int64, error) { return roomyDisk, nil }),
	}, opts...)
	return NewManager(Config{}, func() []string { return []string{root} }, all...), root
}

func mustCreate(t *testing.T, m *Manager, decls []FileDecl, opts CreateOptions) *Session {
	t.Helper()
	s, err := m.Create(decls, opts)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s
}

func writeAll(t *testing.T, m *Manager, sid, fid string, data []byte, chunk int) {
	t.Helper()
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		if _, err := m.WriteChunk(sid, fid, int64(off), bytes.NewReader(data[off:end]), nil); err != nil {
			t.Fatalf("WriteChunk at %d: %v", off, err)
		}
	}
}

// ---------------------------------------------------------------------------
// The staging directory must be invisible to the scanner.
// ---------------------------------------------------------------------------

// TestStagingDirIsInvisibleToScanner is the load-bearing property behind
// staging inside the root: the scanner's shouldSkipDir returns SkipDir for any
// "."-prefixed directory BEFORE it upserts a folder row, so nothing under
// staging is ever walked. That is what makes commit a same-filesystem rename
// rather than a cross-device copy.
//
// The negative control is the same file under a NON-dot directory: the folder
// row appears there, proving the dot prefix is what does the work rather than
// some other accident of the fixture.
func TestStagingDirIsInvisibleToScanner(t *testing.T) {
	root := t.TempDir()

	// A file with a real audio extension inside staging. If the walker ever
	// descends, this is what it would find.
	stagingAlbum := filepath.Join(root, StagingDirName, "sid", "Album")
	if err := os.MkdirAll(stagingAlbum, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stagingAlbum, "track.flac"), []byte("not really flac"), 0o600); err != nil {
		t.Fatal(err)
	}

	folders := scanFolderPaths(t, root)
	for _, f := range folders {
		if strings.Contains(f, StagingDirName) {
			t.Fatalf("scanner created a folder row inside the staging dir: %q (all: %v)", f, folders)
		}
	}

	// CONTROL: the identical tree under a name without the leading dot must
	// produce a folder row. Without this the test would also pass if the
	// scanner simply never ran.
	visibleAlbum := filepath.Join(root, "notdot", "sid", "Album")
	if err := os.MkdirAll(visibleAlbum, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(visibleAlbum, "track.flac"), []byte("not really flac"), 0o600); err != nil {
		t.Fatal(err)
	}
	folders = scanFolderPaths(t, root)
	var sawVisible bool
	for _, f := range folders {
		if strings.Contains(f, "notdot") {
			sawVisible = true
		}
		if strings.Contains(f, StagingDirName) {
			t.Fatalf("staging dir became visible on the second scan: %q", f)
		}
	}
	if !sawVisible {
		t.Fatal("CONTROL INVALID: the non-dot directory produced no folder row either, so this test proves nothing about the dot prefix")
	}
}

// ---------------------------------------------------------------------------
// Offsets, torn chunks, resume.
// ---------------------------------------------------------------------------

// TestTornChunkTruncatesToManifestOffset covers a PUT that drops mid-chunk.
//
// The staged file's own size is NOT the offset: bytes past the last
// acknowledged offset were never acknowledged, and appending after them would
// silently embed the garbage in the committed file.
func TestTornChunkTruncatesToManifestOffset(t *testing.T) {
	m, root := newTestManager(t)
	body := []byte("0123456789abcdefghij")
	s := mustCreate(t, m, []FileDecl{{Path: "A/B/x.flac", Size: int64(len(body))}}, CreateOptions{})
	fid := s.Files[0].ID

	if _, err := m.WriteChunk(s.ID, fid, 0, bytes.NewReader(body[:10]), nil); err != nil {
		t.Fatal(err)
	}

	// Simulate the torn write: bytes on disk past the durable offset.
	part := m.partPath(root, s.ID, fid)
	f, err := os.OpenFile(part, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("GARBAGEGARBAGE")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if info, err := os.Stat(part); err != nil || info.Size() != 24 {
		t.Fatalf("fixture wrong: staged size = %v (err %v), want 24", info.Size(), err)
	}

	// The resume read reports the DURABLE offset, not the file size.
	got, err := m.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files[0].Offset != 10 {
		t.Fatalf("resume offset = %d, want 10 — the offset came from the file size, not the manifest", got.Files[0].Offset)
	}

	if _, err := m.WriteChunk(s.ID, fid, 10, bytes.NewReader(body[10:]), nil); err != nil {
		t.Fatal(err)
	}
	staged, err := os.ReadFile(part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(staged, body) {
		t.Fatalf("staged bytes = %q, want %q — the torn tail was not discarded", staged, body)
	}
}

// TestOpenStagedFileIsNotAppendMode distinguishes the two open modes directly.
// Under O_APPEND the seek is ignored and the write lands at EOF; the whole
// point of positioned writes is that it does not.
func TestOpenStagedFileIsNotAppendMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.part")
	if err := os.WriteFile(p, []byte("AAAABBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := openStagedFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(4, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("CCCC")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "AAAACCCC" {
		t.Fatalf("staged file = %q, want %q — O_APPEND ignores the seek and appends", got, "AAAACCCC")
	}
}

func TestOffsetMismatchReportsTheActualOffset(t *testing.T) {
	m, _ := newTestManager(t)
	s := mustCreate(t, m, []FileDecl{{Path: "x.flac", Size: 100}}, CreateOptions{})
	fid := s.Files[0].ID
	if _, err := m.WriteChunk(s.ID, fid, 0, bytes.NewReader(make([]byte, 40)), nil); err != nil {
		t.Fatal(err)
	}
	_, err := m.WriteChunk(s.ID, fid, 99, bytes.NewReader([]byte("x")), nil)
	var mm *OffsetMismatch
	if !errors.As(err, &mm) {
		t.Fatalf("err = %v, want an *OffsetMismatch", err)
	}
	if mm.Actual != 40 {
		t.Errorf("OffsetMismatch.Actual = %d, want 40", mm.Actual)
	}
	if !errors.Is(err, ErrOffsetMismatch) {
		t.Error("OffsetMismatch does not unwrap to ErrOffsetMismatch")
	}
}

// TestSessionResumesAcrossRestart builds a SECOND Manager over the same root,
// as a bridge restart would. The durable state is the manifest plus the
// per-file meta, including the marshalled SHA-256 so the finished hash is
// correct without re-reading what was already staged.
func TestSessionResumesAcrossRestart(t *testing.T) {
	root := t.TempDir()
	roots := func() []string { return []string{root} }
	free := WithFreeBytes(func(string) (int64, error) { return roomyDisk, nil })

	m1 := NewManager(Config{}, roots, free)
	body := bytes.Repeat([]byte("abcdefgh"), 512) // 4096 bytes
	s := mustCreate(t, m1, []FileDecl{{Path: "A/x.flac", Size: int64(len(body))}}, CreateOptions{})
	fid := s.Files[0].ID
	if _, err := m1.WriteChunk(s.ID, fid, 0, bytes.NewReader(body[:1000]), nil); err != nil {
		t.Fatal(err)
	}

	m2 := NewManager(Config{}, roots, free)
	got, err := m2.Get(s.ID)
	if err != nil {
		t.Fatalf("session did not survive the restart: %v", err)
	}
	if got.Files[0].Offset != 1000 {
		t.Fatalf("resumed offset = %d, want 1000", got.Files[0].Offset)
	}
	if _, err := m2.WriteChunk(s.ID, fid, 1000, bytes.NewReader(body[1000:]), nil); err != nil {
		t.Fatal(err)
	}
	got, err = m2.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(body)
	if got.Files[0].SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("resumed hash = %s, want %s — the streaming hash state did not survive",
			got.Files[0].SHA256, hex.EncodeToString(want[:]))
	}
}

func TestDeclaredDigestMismatchRefusesCompletion(t *testing.T) {
	m, _ := newTestManager(t)
	body := []byte("the real bytes")
	wrong := sha256.Sum256([]byte("different bytes"))
	s := mustCreate(t, m, []FileDecl{{
		Path: "x.flac", Size: int64(len(body)), Digest: hex.EncodeToString(wrong[:]),
	}}, CreateOptions{})
	_, err := m.WriteChunk(s.ID, s.Files[0].ID, 0, bytes.NewReader(body), nil)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
}

// TestChunkDigestMismatchDoesNotAdvanceOffset — a rejected chunk must leave the
// client able to simply re-send.
func TestChunkDigestMismatchDoesNotAdvanceOffset(t *testing.T) {
	m, _ := newTestManager(t)
	body := []byte("0123456789")
	s := mustCreate(t, m, []FileDecl{{Path: "x.flac", Size: int64(len(body))}}, CreateOptions{})
	fid := s.Files[0].ID

	bad := sha256.Sum256([]byte("not what we sent"))
	if _, err := m.WriteChunk(s.ID, fid, 0, bytes.NewReader(body[:5]), bad[:]); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("err = %v, want ErrDigestMismatch", err)
	}
	got, err := m.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files[0].Offset != 0 {
		t.Fatalf("offset advanced to %d after a digest mismatch; the client cannot recover by re-sending", got.Files[0].Offset)
	}
	good := sha256.Sum256(body[:5])
	if _, err := m.WriteChunk(s.ID, fid, 0, bytes.NewReader(body[:5]), good[:]); err != nil {
		t.Fatalf("re-send after a rejected chunk failed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Locking.
// ---------------------------------------------------------------------------

// TestConcurrentChunksToOneFileSerialise — two writers race the same offset.
// Exactly one wins and the file is never interleaved.
func TestConcurrentChunksToOneFileSerialise(t *testing.T) {
	m, root := newTestManager(t)
	a := bytes.Repeat([]byte("A"), 4096)
	b := bytes.Repeat([]byte("B"), 4096)
	s := mustCreate(t, m, []FileDecl{{Path: "x.flac", Size: 8192}}, CreateOptions{})
	fid := s.Files[0].ID

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, payload := range [][]byte{a, b} {
		wg.Add(1)
		go func(i int, p []byte) {
			defer wg.Done()
			_, errs[i] = m.WriteChunk(s.ID, fid, 0, bytes.NewReader(p), nil)
		}(i, payload)
	}
	wg.Wait()

	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrOffsetMismatch):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d writers succeeded at the same offset, want exactly 1", won)
	}
	staged, err := os.ReadFile(m.partPath(root, s.ID, fid))
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 4096 {
		t.Fatalf("staged %d bytes, want 4096 — the two writes interleaved", len(staged))
	}
	if !bytes.Equal(staged, a) && !bytes.Equal(staged, b) {
		t.Fatal("staged bytes are a mix of both writers")
	}
}

// TestConcurrentChunksToDifferentFilesDoNotSerialise is the control that keeps
// the lock per-FILE rather than per-session. A session-wide lock would pass
// every other locking test here while quietly serialising a folder upload,
// which is where browser throughput actually comes from.
func TestConcurrentChunksToDifferentFilesDoNotSerialise(t *testing.T) {
	m, _ := newTestManager(t)
	s := mustCreate(t, m, []FileDecl{
		{Path: "a.flac", Size: 8},
		{Path: "b.flac", Size: 8},
	}, CreateOptions{})

	// File A's body blocks until released, holding A's lock.
	release := make(chan struct{})
	blocking := io.MultiReader(bytes.NewReader([]byte("1234")), readerFunc(func(p []byte) (int, error) {
		<-release
		copy(p, "5678")
		return 4, io.EOF
	}))

	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = m.WriteChunk(s.ID, s.Files[0].ID, 0, blocking, nil)
	}()
	<-started

	// B must complete while A is still blocked.
	done := make(chan error, 1)
	go func() {
		_, err := m.WriteChunk(s.ID, s.Files[1].ID, 0, bytes.NewReader([]byte("abcdefgh")), nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write to the second file failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a write to a DIFFERENT file blocked behind an in-flight one — the lock is per-session, not per-file")
	}
	close(release)
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// TestFileLockMapIsReapedOnCompletionAndTeardown — an unpruned map leaks one
// entry per uploaded file for the process lifetime.
func TestFileLockMapIsReapedOnCompletionAndTeardown(t *testing.T) {
	m, _ := newTestManager(t)
	var decls []FileDecl
	for i := 0; i < 25; i++ {
		decls = append(decls, FileDecl{Path: string(rune('a'+i)) + ".flac", Size: 4})
	}
	s := mustCreate(t, m, decls, CreateOptions{})
	for _, f := range s.Files {
		writeAll(t, m, s.ID, f.ID, []byte("data"), 4)
	}
	if n := m.locks.size(); n != 0 {
		t.Fatalf("lock map holds %d entries after every write completed, want 0", n)
	}

	s2 := mustCreate(t, m, decls, CreateOptions{})
	if _, err := m.WriteChunk(s2.ID, s2.Files[0].ID, 0, bytes.NewReader([]byte("da")), nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Abandon(s2.ID); err != nil {
		t.Fatal(err)
	}
	if n := m.locks.size(); n != 0 {
		t.Fatalf("lock map holds %d entries after teardown, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// Commit.
// ---------------------------------------------------------------------------

// TestCommitIsSameFilesystemRename asserts the inode survives. A copy would
// allocate a new one — and on a FUSE-mounted library a copy is the difference
// between instant and rewriting every byte.
func TestCommitIsSameFilesystemRename(t *testing.T) {
	m, root := newTestManager(t)
	body := []byte("audio bytes")
	s := mustCreate(t, m, []FileDecl{{Path: "Artist/Album/01.flac", Size: int64(len(body))}}, CreateOptions{})
	fid := s.Files[0].ID
	writeAll(t, m, s.ID, fid, body, 1024)

	partInfo, err := os.Stat(m.partPath(root, s.ID, fid))
	if err != nil {
		t.Fatal(err)
	}

	res, err := m.Commit(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Committed != 1 || res.Failed != 0 {
		t.Fatalf("commit result = %+v", res)
	}
	dest := filepath.Join(root, "Artist", "Album", "01.flac")
	destInfo, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("committed file missing: %v", err)
	}
	// os.SameFile is the portable identity check — a copy allocates a new
	// file, a rename does not. syscall.Stat_t would not compile on Windows.
	if !os.SameFile(partInfo, destInfo) {
		t.Error("the committed file is not the staged file: the commit copied instead of renaming")
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, body) {
		t.Errorf("committed bytes = %q, want %q", got, body)
	}
	if _, err := os.Stat(filepath.Join(root, StagingDirName, s.ID)); !os.IsNotExist(err) {
		t.Error("staging directory survived a successful commit")
	}
	if len(res.ScanDirs) != 1 || res.ScanDirs[0] != "Artist/Album" {
		t.Errorf("ScanDirs = %v, want [Artist/Album]", res.ScanDirs)
	}
}

func TestCommitSkipsCollisionsUnlessOverwriteRequested(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		name := "skip"
		if overwrite {
			name = "overwrite"
		}
		t.Run(name, func(t *testing.T) {
			m, root := newTestManager(t)
			dest := filepath.Join(root, "Artist", "01.flac")
			if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dest, []byte("ORIGINAL"), 0o644); err != nil {
				t.Fatal(err)
			}

			body := []byte("NEWDATA!")
			s := mustCreate(t, m, []FileDecl{{Path: "Artist/01.flac", Size: int64(len(body))}},
				CreateOptions{Overwrite: overwrite})
			writeAll(t, m, s.ID, s.Files[0].ID, body, 1024)
			res, err := m.Commit(s.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(dest)
			if overwrite {
				if res.Committed != 1 || !bytes.Equal(got, body) {
					t.Fatalf("overwrite requested but file is %q (result %+v)", got, res)
				}
			} else {
				if res.Skipped != 1 || !bytes.Equal(got, []byte("ORIGINAL")) {
					t.Fatalf("default commit clobbered an existing file: %q (result %+v)", got, res)
				}
				if res.Outcomes[0].Status != "skipped" {
					t.Errorf("outcome = %q, want skipped", res.Outcomes[0].Status)
				}
			}
		})
	}
}

// TestCommitRevalidatesPathsFromDisk — the manifest sits on disk between create
// and commit, so it is not trusted to still say what it said. A tampered entry
// must be refused at commit, not acted on.
func TestCommitRevalidatesPathsFromDisk(t *testing.T) {
	m, root := newTestManager(t)
	body := []byte("x")
	s := mustCreate(t, m, []FileDecl{{Path: "ok.flac", Size: 1}}, CreateOptions{})
	writeAll(t, m, s.ID, s.Files[0].ID, body, 1)

	// Tamper: rewrite the manifest with a traversal path.
	mp := m.manifestPath(root, s.ID)
	var doc sessionDoc
	if err := readJSONFile(mp, &doc); err != nil {
		t.Fatal(err)
	}
	doc.Files[0].RelPath = "../escaped.flac"
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(mp, b, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := m.Commit(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || res.Committed != 0 {
		t.Fatalf("a tampered path was not refused: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escaped.flac")); err == nil {
		t.Fatal("a file was written OUTSIDE the library root")
	}
}

func TestCommitRefusesIncompleteFiles(t *testing.T) {
	m, _ := newTestManager(t)
	s := mustCreate(t, m, []FileDecl{{Path: "x.flac", Size: 100}}, CreateOptions{})
	if _, err := m.WriteChunk(s.ID, s.Files[0].ID, 0, bytes.NewReader(make([]byte, 40)), nil); err != nil {
		t.Fatal(err)
	}
	res, err := m.Commit(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed != 1 || res.Outcomes[0].Reason != "incomplete" {
		t.Fatalf("an incomplete file was committed: %+v", res)
	}
}

// ---------------------------------------------------------------------------
// Space and sweeping.
// ---------------------------------------------------------------------------

// TestDiskFloorFailsClosedOnProbeError — a probe error means no way to honour
// the floor. A refused session costs a retry; a wrong guess fills the volume.
func TestDiskFloorFailsClosedOnProbeError(t *testing.T) {
	m, _ := newTestManager(t, WithFreeBytes(func(string) (int64, error) {
		return 0, errors.New("statfs exploded")
	}))
	_, err := m.Create([]FileDecl{{Path: "x.flac", Size: 1}}, CreateOptions{})
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v, want ErrNoSpace — the probe failure was treated as permission to proceed", err)
	}
}

func TestDiskFloorRefusesAndReportsReclaimable(t *testing.T) {
	m, _ := newTestManager(t,
		WithFreeBytes(func(string) (int64, error) { return 1 << 20, nil }),
		WithReclaimable(func(string) int64 { return 7 << 20 }),
	)
	_, err := m.Create([]FileDecl{{Path: "x.flac", Size: 1 << 20}}, CreateOptions{})
	var ns *NoSpace
	if !errors.As(err, &ns) {
		t.Fatalf("err = %v, want a *NoSpace", err)
	}
	if ns.ReclaimableBytes != 7<<20 {
		t.Errorf("ReclaimableBytes = %d, want %d — a 507 cannot offer 'empty trash and resume' without it",
			ns.ReclaimableBytes, 7<<20)
	}
}

func TestSessionMaxBytesIsEnforced(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.Create([]FileDecl{{Path: "a.flac", Size: 600}, {Path: "b.flac", Size: 600}},
		CreateOptions{MaxBytes: 1000})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if _, err := m.Create([]FileDecl{{Path: "a.flac", Size: 400}}, CreateOptions{MaxBytes: 1000}); err != nil {
		t.Fatalf("a session inside the ceiling was refused: %v", err)
	}
}

// TestSweeperRemovesOrphansWithNoReadableManifest — a crash mid-commit, or a
// manifest that will not parse, orphans files that a state-driven sweeper would
// never look at. Those are precisely the ones nothing else cleans up.
func TestSweeperRemovesOrphansWithNoReadableManifest(t *testing.T) {
	now := time.Now()
	m, root := newTestManager(t, WithClock(func() time.Time { return now }))

	orphan := filepath.Join(root, StagingDirName, "orphaned")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "a.part"), []byte("debris"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-48 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}

	// A live session must survive the same pass.
	live := mustCreate(t, m, []FileDecl{{Path: "x.flac", Size: 4}}, CreateOptions{})

	n, err := m.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept %d sessions, want 1", n)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the manifest-less orphan survived the sweep")
	}
	if _, err := m.Get(live.ID); err != nil {
		t.Errorf("the sweep removed a live session: %v", err)
	}
}

func TestSweeperUsesManifestAgeNotFileMtime(t *testing.T) {
	now := time.Now()
	m, root := newTestManager(t, WithClock(func() time.Time { return now }))
	s := mustCreate(t, m, []FileDecl{{Path: "x.flac", Size: 4}}, CreateOptions{})
	writeAll(t, m, s.ID, s.Files[0].ID, []byte("data"), 4)

	// Back-date every FILE in the session. A stat-driven sweeper would reap
	// it; a manifest-driven one must not, because the session is minutes old.
	dir := m.sessionDir(root, s.ID)
	entries, _ := os.ReadDir(dir)
	old := now.Add(-72 * time.Hour)
	for _, e := range entries {
		if e.Name() == "manifest.json" {
			continue
		}
		if err := os.Chtimes(filepath.Join(dir, e.Name()), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(s.ID); err != nil {
		t.Fatalf("a fresh session was swept because its staged FILES were back-dated: %v", err)
	}
}

func TestSweeperRunsOnceAtStartup(t *testing.T) {
	now := time.Now()
	m, root := newTestManager(t, WithClock(func() time.Time { return now }))
	stale := filepath.Join(root, StagingDirName, "stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	// A ticker period far longer than the test: only the STARTUP pass can
	// remove anything here.
	go func() { defer close(done); m.RunSweeper(ctx, time.Hour) }()

	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(stale); os.IsNotExist(err) {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("the stale session survived; the sweeper only runs on its ticker, so a crash leaves orphans for a full period")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestCreateRejectsUnknownRoot(t *testing.T) {
	m, _ := newTestManager(t)
	if _, err := m.Create([]FileDecl{{Path: "x.flac", Size: 1}}, CreateOptions{Root: "/nope"}); !errors.Is(err, ErrUnknownRoot) {
		t.Fatalf("err = %v, want ErrUnknownRoot", err)
	}
}

func TestFindSessionRejectsTraversalIDs(t *testing.T) {
	m, _ := newTestManager(t)
	for _, id := range []string{"..", "../..", "a/b", `a\b`, ".", ""} {
		if _, err := m.Get(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", id, err)
		}
	}
}
