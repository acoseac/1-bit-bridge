package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
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
		if _, err := m.WriteChunk(sid, fid, int64(off), bytes.NewReader(data[off:end]), nil, 0); err != nil {
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

	if _, err := m.WriteChunk(s.ID, fid, 0, bytes.NewReader(body[:10]), nil, 0); err != nil {
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

	if _, err := m.WriteChunk(s.ID, fid, 10, bytes.NewReader(body[10:]), nil, 0); err != nil {
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
	if _, err := m.WriteChunk(s.ID, fid, 0, bytes.NewReader(make([]byte, 40)), nil, 0); err != nil {
		t.Fatal(err)
	}
	_, err := m.WriteChunk(s.ID, fid, 99, bytes.NewReader([]byte("x")), nil, 0)
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
	if _, err := m1.WriteChunk(s.ID, fid, 0, bytes.NewReader(body[:1000]), nil, 0); err != nil {
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
	if _, err := m2.WriteChunk(s.ID, fid, 1000, bytes.NewReader(body[1000:]), nil, 0); err != nil {
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
	_, err := m.WriteChunk(s.ID, s.Files[0].ID, 0, bytes.NewReader(body), nil, 0)
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
	if _, err := m.WriteChunk(s.ID, fid, 0, bytes.NewReader(body[:5]), bad[:], 0); !errors.Is(err, ErrDigestMismatch) {
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
	if _, err := m.WriteChunk(s.ID, fid, 0, bytes.NewReader(body[:5]), good[:], 0); err != nil {
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
			_, errs[i] = m.WriteChunk(s.ID, fid, 0, bytes.NewReader(p), nil, 0)
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
	blockedDone := make(chan struct{})
	go func() {
		defer close(blockedDone)
		close(started)
		_, _ = m.WriteChunk(s.ID, s.Files[0].ID, 0, blocking, nil, 0)
	}()
	<-started
	// The blocked writer must be JOINED before the test returns, or it keeps
	// writing into the staging directory while t.TempDir's cleanup removes it
	// — a flake that surfaces as "directory not empty", nowhere near the
	// assertion. Registered before the release below so it runs after it.
	t.Cleanup(func() {
		select {
		case <-blockedDone:
		case <-time.After(5 * time.Second):
			t.Error("the blocked writer never finished")
		}
	})

	// B must complete while A is still blocked.
	done := make(chan error, 1)
	go func() {
		_, err := m.WriteChunk(s.ID, s.Files[1].ID, 0, bytes.NewReader([]byte("abcdefgh")), nil, 0)
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
	if _, err := m.WriteChunk(s2.ID, s2.Files[0].ID, 0, bytes.NewReader([]byte("da")), nil, 0); err != nil {
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
	// Prime the file identity WHILE THE PATH STILL EXISTS.
	//
	// On Unix the inode is already in the FileInfo, but on Windows
	// os.SameFile loads the volume + file index lazily, by re-opening
	// fileStat.path — and after the commit that path has been renamed away, so
	// the load fails and SameFile returns false no matter what happened.
	// Comparing the value with itself forces that load now and caches it
	// (os.sameFile calls loadFileId on both arguments, which is idempotent),
	// so the comparison below measures the rename rather than the lookup.
	_ = os.SameFile(partInfo, partInfo)

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
	if _, err := m.WriteChunk(s.ID, s.Files[0].ID, 0, bytes.NewReader(make([]byte, 40)), nil, 0); err != nil {
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
	// Cleanup rather than a bare `defer cancel()`: cancelling without JOINING
	// leaves the sweeper running against a temp dir t.TempDir is removing, and
	// t.Cleanup covers the t.Fatal paths a defer at the bottom would not.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the sweeper did not stop on context cancellation")
		}
	})

	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(stale); os.IsNotExist(err) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the stale session survived; the sweeper only runs on its ticker, so a crash leaves orphans for a full period")
		case <-time.After(10 * time.Millisecond):
		}
	}
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

// TestCommittedFileIsReadableLikeNormalLibraryContent — the staged file's mode
// survives the rename, so it IS the committed file's mode. At 0o600 an uploaded
// track is readable only by the bridge's own user, unlike every other file in
// the library, and a Samba share or a backup job running as someone else
// silently cannot read it.
func TestCommittedFileIsReadableLikeNormalLibraryContent(t *testing.T) {
	m, root := newTestManager(t)
	body := []byte("audio")
	s := mustCreate(t, m, []FileDecl{{Path: "A/x.flac", Size: int64(len(body))}}, CreateOptions{})
	writeAll(t, m, s.ID, s.Files[0].ID, body, 16)
	if _, err := m.Commit(s.ID); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "A", "x.flac"))
	if err != nil {
		t.Fatal(err)
	}
	// Group/other read bits, modulo whatever the process umask trims. A
	// typical umask of 022 leaves 0644; the assertion is only that the file
	// is not owner-only.
	if info.Mode().Perm()&0o044 == 0 {
		t.Errorf("committed file mode is %v — readable only by the bridge's own user", info.Mode().Perm())
	}
}

// TestConcurrentCommitsToOneDestinationDoNotClobber — the per-file lock is
// keyed on (session, file), so two SESSIONS targeting the same destination
// would both stat it, both find nothing, and both rename. os.Rename REPLACES,
// so the second silently destroys the first — exactly what skip-by-default
// exists to prevent.
func TestConcurrentCommitsToOneDestinationDoNotClobber(t *testing.T) {
	m, root := newTestManager(t)
	const dest = "Artist/Album/01.flac"

	mk := func(body []byte) string {
		s := mustCreate(t, m, []FileDecl{{Path: dest, Size: int64(len(body))}}, CreateOptions{})
		writeAll(t, m, s.ID, s.Files[0].ID, body, 1024)
		return s.ID
	}
	a := mk(bytes.Repeat([]byte("A"), 64))
	b := mk(bytes.Repeat([]byte("B"), 64))

	var wg sync.WaitGroup
	results := make([]*CommitResult, 2)
	for i, sid := range []string{a, b} {
		wg.Add(1)
		go func(i int, sid string) {
			defer wg.Done()
			res, err := m.Commit(sid)
			if err != nil {
				t.Errorf("commit: %v", err)
				return
			}
			results[i] = res
		}(i, sid)
	}
	wg.Wait()

	committed, skipped := 0, 0
	for _, r := range results {
		if r == nil {
			continue
		}
		committed += r.Committed
		skipped += r.Skipped
	}
	if committed != 1 || skipped != 1 {
		t.Fatalf("committed=%d skipped=%d, want exactly one of each — a concurrent commit clobbered the other",
			committed, skipped)
	}
	got, err := os.ReadFile(filepath.Join(root, "Artist", "Album", "01.flac"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 64 || (got[0] != 'A' && got[0] != 'B') {
		t.Fatalf("destination holds %d bytes starting %q — the two commits interleaved", len(got), got[:1])
	}
}

// TestOversizeChunkIsRefusedRatherThanTruncated — io.LimitReader already bounds
// what is WRITTEN, so this is not an overflow. It is about not hiding a client
// bug behind a 200.
func TestOversizeChunkIsRefusedRatherThanTruncated(t *testing.T) {
	m, _ := newTestManager(t)
	s := mustCreate(t, m, []FileDecl{{Path: "x.flac", Size: 10}}, CreateOptions{})
	body := bytes.Repeat([]byte("x"), 500)
	_, err := m.WriteChunk(s.ID, s.Files[0].ID, 0, bytes.NewReader(body), nil, int64(len(body)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	got, err := m.Get(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files[0].Offset != 0 {
		t.Errorf("offset advanced to %d on a refused chunk", got.Files[0].Offset)
	}
	// An undeclared length still gets the bounded-truncation behaviour.
	if _, err := m.WriteChunk(s.ID, s.Files[0].ID, 0, bytes.NewReader(body), nil, -1); err != nil {
		t.Fatalf("chunked (undeclared length) upload failed: %v", err)
	}
	got, _ = m.Get(s.ID)
	if got.Files[0].Offset != 10 {
		t.Errorf("offset = %d, want 10 — LimitReader should bound an undeclared oversize", got.Files[0].Offset)
	}
}

// TestReadOnlyLibraryIsNamedRatherThanA500 — staging lives inside the library
// root, so a read-only mount fails every session at the first mkdir. The
// generic answer ("create staging dir: ...") reaches the client as a 500
// "upload failed" with the real cause visible only in the server log, which is
// the least useful place for it.
//
// This is not hypothetical: bridge.ars.md mounted its B2-backed library with
// rclone's --read-only until this feature needed writes.
func TestReadOnlyLibraryIsNamedRatherThanA500(t *testing.T) {
	if runtime.GOOS == "windows" {
		// os.Chmod on Windows sets only the read-only ATTRIBUTE, and that does
		// not stop files being created inside a directory — so a 0o500 fixture
		// is simply writable there and MkdirAll succeeds. Reproducing this
		// would need an ACL edit via icacls, which this repo deliberately
		// avoids shelling out to. The CLASSIFICATION itself is covered on every
		// platform by TestClassifyStagingError*.
		t.Skip("Chmod cannot make a directory unwritable on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the mode bits this fixture relies on")
	}
	root := t.TempDir()
	// A directory the bridge cannot create anything inside stands in for a
	// read-only mount: both surface as a permission-class failure at the same
	// call, and EROFS cannot be staged without an actual mount.
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	m := NewManager(Config{}, func() []string { return []string{root} },
		WithFreeBytes(func(string) (int64, error) { return roomyDisk, nil }))
	_, err := m.Create([]FileDecl{{Path: "A/x.flac", Size: 1}}, CreateOptions{})
	if !errors.Is(err, ErrLibraryNotWritable) {
		t.Fatalf("err = %v, want ErrLibraryNotWritable", err)
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("the message does not name the root: %v", err)
	}
}

func TestClassifyStagingErrorPassesUnrelatedFailuresThrough(t *testing.T) {
	got := classifyStagingError("/lib", errors.New("disk on fire"))
	if errors.Is(got, ErrLibraryNotWritable) {
		t.Error("an unrelated failure was misreported as an unwritable library")
	}
	if !strings.Contains(got.Error(), "disk on fire") {
		t.Errorf("the original cause was dropped: %v", got)
	}
	// Both mappings, on every platform: these need no filesystem, so they are
	// the coverage that survives where the end-to-end fixture cannot run.
	for name, in := range map[string]error{
		"EROFS":      syscall.EROFS,
		"permission": fs.ErrPermission,
		"EACCES":     syscall.EACCES,
	} {
		got := classifyStagingError("/lib", in)
		if !errors.Is(got, ErrLibraryNotWritable) {
			t.Errorf("%s was not classified as an unwritable library: %v", name, got)
		}
		if !strings.Contains(got.Error(), "/lib") {
			t.Errorf("%s message does not name the root: %v", name, got)
		}
	}
}

// TestJunkFilesDoNotBlockTheWholeSession is the case a real folder upload hits
// on the first try: a Mac folder contains .DS_Store, and refusing the session
// for it means the operator cannot upload an album because of a file the
// operating system put there without asking.
func TestJunkFilesDoNotBlockTheWholeSession(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create([]FileDecl{
		{Path: "Album/.DS_Store", Size: 6148},
		{Path: "Album/01 Track.dsf", Size: 1000},
		{Path: "Album/cover.jpg", Size: 500},
		{Path: "Album/notes.txt", Size: 10},
		{Path: "Album/._01 Track.dsf", Size: 4096},
	}, CreateOptions{})
	if err != nil {
		t.Fatalf("a session with junk in it was refused outright: %v", err)
	}
	if len(s.Files) != 2 {
		t.Fatalf("accepted %d files, want 2 (the .dsf and the .jpg): %+v", len(s.Files), s.Files)
	}
	if len(s.Rejected) != 3 {
		t.Fatalf("reported %d rejections, want 3: %+v", len(s.Rejected), s.Rejected)
	}
	// The report has to name the file AND say why, or it is not actionable.
	for _, r := range s.Rejected {
		if r.Path == "" || r.Reason == "" {
			t.Errorf("rejection is not actionable: %+v", r)
		}
	}
}

// TestSessionWithNothingAcceptableIsAnError — skip-and-report must not become
// "silently create an empty session the operator watches do nothing".
func TestSessionWithNothingAcceptableIsAnError(t *testing.T) {
	m, _ := newTestManager(t)
	_, err := m.Create([]FileDecl{
		{Path: "Album/.DS_Store", Size: 6148},
		{Path: "Album/Thumbs.db", Size: 100},
	}, CreateOptions{})
	if !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("err = %v, want ErrInvalidPath", err)
	}
	if !strings.Contains(err.Error(), "nothing to upload") {
		t.Errorf("the message does not say what happened: %v", err)
	}
}

// TestHostilePathsAreRejectedNotAccepted — skip-and-report must not turn into
// accepting a traversal alongside the good files.
func TestHostilePathsAreRejectedNotAccepted(t *testing.T) {
	m, _ := newTestManager(t)
	s, err := m.Create([]FileDecl{
		{Path: "../escape.flac", Size: 1},
		{Path: "/etc/passwd.flac", Size: 1},
		{Path: "Album/ok.flac", Size: 1},
	}, CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Files) != 1 || s.Files[0].Path != "Album/ok.flac" {
		t.Fatalf("accepted files = %+v, want only Album/ok.flac", s.Files)
	}
	if len(s.Rejected) != 2 {
		t.Errorf("hostile paths were not reported: %+v", s.Rejected)
	}
}
