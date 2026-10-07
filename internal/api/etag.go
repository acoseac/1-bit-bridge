package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// setETag writes a strong validator. Callers pass the unquoted form.
func setETag(w http.ResponseWriter, etag string) {
	w.Header().Set("ETag", `"`+etag+`"`)
}

// writeNotModified answers 304 with the same strong ETag and an empty body.
func writeNotModified(w http.ResponseWriter, etag string) {
	setETag(w, etag)
	w.WriteHeader(http.StatusNotModified)
}

// favoritesETag is <epoch>.<revision>, unquoted.
func favoritesETag(epoch string, revision int64) string {
	return epoch + "." + strconv.FormatInt(revision, 10)
}

// playlistCanonItem is the list row the playlist ETag hashes. Field order
// is alphabetical, and imageHash is always present (the wire summary
// omits an empty one).
type playlistCanonItem struct {
	ID             string `json:"id"`
	ImageHash      string `json:"imageHash"`
	LastModifiedAt int64  `json:"lastModifiedAt"`
	Name           string `json:"name"`
	TrackCount     int    `json:"trackCount"`
}

// playlistListCanon is the canonical playlist list. Epoch is the ETag
// prefix and is not inside the hash. Empty slices, never null.
type playlistListCanon struct {
	DeletedIDs []string            `json:"deletedIds"`
	Playlists  []playlistCanonItem `json:"playlists"`
}

// playlistListETag is <epoch>.<sha256 hex of the canonical list>.
func playlistListETag(epoch string, canon playlistListCanon) (string, error) {
	if canon.DeletedIDs == nil {
		canon.DeletedIDs = []string{}
	}
	if canon.Playlists == nil {
		canon.Playlists = []playlistCanonItem{}
	}
	raw, err := json.Marshal(canon)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return epoch + "." + hex.EncodeToString(sum[:]), nil
}

// noneMatch reports whether If-None-Match matches etag. Comparison is
// weak: a W/ prefix is stripped. "*" matches only when starMatches is
// set (a favorites document that has been stored, or any playlist list).
func noneMatch(header, etag string, starMatches bool) bool {
	if header == "" {
		return false
	}
	for _, part := range strings.Split(header, ",") {
		tag := strings.TrimSpace(part)
		if tag == "*" {
			if starMatches {
				return true
			}
			continue
		}
		tag = strings.TrimPrefix(tag, "W/")
		tag = strings.TrimPrefix(tag, "w/")
		tag = strings.TrimSpace(tag)
		tag = strings.Trim(tag, `"`)
		if tag == etag {
			return true
		}
	}
	return false
}
