package trash

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
)

// originRecord is the library root a trashed file came from, and the path
// of that file relative to the root. The manifest path's meaning changes
// when a root is added or removed, so the restore reads this instead of
// interpreting the stored path under whatever layout is current.
type originRecord struct {
	Root string `json:"root"`
	Rel  string `json:"rel"`
}

const originWriteFailed = "could not record which library root this file came from"

// originSidecar sits beside the trashed audio file. Its name begins with a
// dot, which validRel refuses, so List can never hand it out as an entry.
func originSidecar(audio string) string {
	return filepath.Join(filepath.Dir(audio), "."+filepath.Base(audio)+".bridge-root")
}

func hiddenTrashPath(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

func writeOriginRecord(audio, root, rel string) error {
	rec := originRecord{Root: root, Rel: rel}
	if err := checkOriginRecord(rec); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	// The temp name starts with a dot so a crash mid-write leaves something
	// List and the sweep count both skip.
	return atomicwrite.WriteBytes(originSidecar(audio), data, ".bridge-root-*")
}

func readOriginRecord(path string) (originRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return originRecord{}, err
	}
	var rec originRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return originRecord{}, err
	}
	if err := checkOriginRecord(rec); err != nil {
		return originRecord{}, err
	}
	return rec, nil
}

func checkOriginRecord(rec originRecord) error {
	if rec.Root == "" || strings.ContainsRune(rec.Root, 0) || !filepath.IsAbs(rec.Root) {
		return errors.New("trash: root record")
	}
	if rec.Rel == "" || strings.ContainsRune(rec.Rel, 0) || strings.Contains(rec.Rel, `\`) {
		return errors.New("trash: root record")
	}
	for _, seg := range strings.Split(rec.Rel, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.HasPrefix(seg, ".") {
			return errors.New("trash: root record")
		}
	}
	return nil
}

func removeOriginRecord(audio string) {
	_ = os.Remove(originSidecar(audio))
}
