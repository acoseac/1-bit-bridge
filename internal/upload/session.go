package upload

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"time"
)

// sessionDoc is the on-disk session manifest. It lives beside the parts as
// manifest.json, which is what lets a resume survive a bridge restart.
//
// The declaration is immutable once written; per-file progress lives in a
// separate small file so a chunk write does not rewrite a manifest that may
// list two thousand entries.
type sessionDoc struct {
	ID        string    `json:"id"`
	Root      string    `json:"root"`
	CreatedAt time.Time `json:"createdAt"`
	Overwrite bool      `json:"overwrite"`
	MaxBytes  int64     `json:"maxBytes,omitempty"`
	Files     []fileDoc `json:"files"`
}

type fileDoc struct {
	ID      string `json:"id"`
	RelPath string `json:"relPath"`
	Size    int64  `json:"size"`
	// Digest is an optional client-declared whole-file SHA-256, hex. When
	// present it is verified as the final byte lands.
	Digest string `json:"digest,omitempty"`
}

// fileState is the DURABLE offset, and it is the only offset that counts.
//
// The staged file's own size is not authoritative: a PUT that drops mid-chunk
// leaves bytes past the last acknowledged offset, and appending after them
// would silently embed the garbage. Every open truncates back to Offset first.
//
// HashState is the marshalled running SHA-256 (108 bytes), so a resumed upload
// produces the correct whole-file hash without re-reading what it already
// staged.
type fileState struct {
	Offset    int64  `json:"offset"`
	HashState []byte `json:"hashState,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not a recoverable condition; a
		// time-derived id would silently collide.
		panic("upload: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func (m *Manager) sessionDir(root, sid string) string {
	return filepath.Join(root, StagingDirName, sid)
}

func (m *Manager) manifestPath(root, sid string) string {
	return filepath.Join(m.sessionDir(root, sid), "manifest.json")
}

func (m *Manager) partPath(root, sid, fid string) string {
	return filepath.Join(m.sessionDir(root, sid), fid+PartSuffix)
}

func (m *Manager) statePath(root, sid, fid string) string {
	return filepath.Join(m.sessionDir(root, sid), fid+".meta")
}

func readJSONFile(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func (m *Manager) readState(root, sid, fid string) (fileState, error) {
	var st fileState
	err := readJSONFile(m.statePath(root, sid, fid), &st)
	if os.IsNotExist(err) {
		return fileState{}, nil // never written to == offset 0
	}
	if err != nil {
		return fileState{}, err
	}
	return st, nil
}

// resumeHasher rebuilds the running SHA-256 from persisted state.
func resumeHasher(st fileState) (hash.Hash, error) {
	h := sha256.New()
	if len(st.HashState) == 0 {
		return h, nil
	}
	u, ok := h.(encoding.BinaryUnmarshaler)
	if !ok {
		return nil, fmt.Errorf("sha256 hash is not resumable on this Go build")
	}
	if err := u.UnmarshalBinary(st.HashState); err != nil {
		return nil, fmt.Errorf("resume hash state: %w", err)
	}
	return h, nil
}

func marshalHasher(h hash.Hash) ([]byte, error) {
	mm, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return nil, fmt.Errorf("sha256 hash is not marshalable on this Go build")
	}
	return mm.MarshalBinary()
}
