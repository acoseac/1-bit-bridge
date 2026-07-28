package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	_ "image/png" // register the PNG decoder for image.Decode / DecodeConfig
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/image/draw"

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// coverMaxBodyBytes caps the decoded JSON upload body. Covers are small;
// 10 MiB is generous headroom for a high-res source the operator drops in.
const coverMaxBodyBytes = 10 << 20

// coverMaxDim is the longest-side ceiling for the stored (resized) JPEG.
// Matches the album-art ~600 px convention — small enough to serve over
// cellular, large enough for a Retina hero.
const coverMaxDim = 600

// coverSourceMaxDim rejects absurd source dimensions at the header stage
// (before decoding the pixel matrix) so a forged tiny-file/huge-dimensions
// payload can't blow up memory.
const coverSourceMaxDim = 12000

// coverMaxSourcePixels caps the TOTAL decoded pixel count. The per-axis limit
// alone still permits a 12000x12000 source to allocate a ~576 MB RGBA matrix,
// and a highly-compressible PNG (e.g. solid color) encodes those dimensions in
// a body well under coverMaxBodyBytes — an OOM vector on a RAM-constrained host.
// ~16.7 MP (4096x4096, ~67 MB RGBA) is far above any real album cover.
const coverMaxSourcePixels = 4096 * 4096

type coverUploadRequest struct {
	Image string `json:"image"` // base64 (optionally a data: URL); JPEG or PNG
}

// processCoverImage validates + normalizes an uploaded image to a resized
// JPEG and returns the bytes + their SHA-256 hex. Validation is header-first
// (image.DecodeConfig reads only the format signature + dimensions) so a
// malformed/oversized payload is rejected before the full pixel decode.
func processCoverImage(raw []byte) (out []byte, hash string, err error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, "", errors.New("not a decodable image")
	}
	if format != "jpeg" && format != "png" {
		return nil, "", errors.New("unsupported image format (want jpeg or png)")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > coverSourceMaxDim || cfg.Height > coverSourceMaxDim ||
		int64(cfg.Width)*int64(cfg.Height) > coverMaxSourcePixels {
		return nil, "", errors.New("image dimensions out of range")
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, "", errors.New("image decode failed")
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		// Defensive: a malformed image / custom decoder could yield empty
		// bounds even after DecodeConfig passed (Gemini MEDIUM on PR #402).
		return nil, "", errors.New("decoded image has no pixels")
	}
	nw, nh := w, h
	if w > coverMaxDim || h > coverMaxDim {
		if w >= h {
			nw, nh = coverMaxDim, int(float64(h)*float64(coverMaxDim)/float64(w))
		} else {
			nw, nh = int(float64(w)*float64(coverMaxDim)/float64(h)), coverMaxDim
		}
	}
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:]), nil
}

// decodeCoverBody reads the base64-JSON upload body (capped) and decodes the
// image bytes. Tolerates a leading `data:<mime>;base64,` URL prefix.
func decodeCoverBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var req coverUploadRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, coverMaxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return nil, errors.New("request body too large")
		}
		return nil, errors.New("invalid JSON body")
	}
	b64 := req.Image
	if i := strings.IndexByte(b64, ','); i >= 0 && strings.HasPrefix(b64, "data:") {
		b64 = b64[i+1:]
	}
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return nil, errors.New("missing image data")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("invalid base64 image data")
	}
	return raw, nil
}

// uploadCover is the shared upload path for both scopes: validate + resize +
// atomic-write the JPEG, then upsert the (scope,key)→hash mapping.
func (s *Server) uploadCover(w http.ResponseWriter, r *http.Request, scope, key string) {
	// The key feeds BOTH the on-disk filename (sha256 of scope+key,
	// byte-exact) and the playlist_covers row, so the write side has to
	// normalise in lockstep with the read side in
	// internal/api/playlist_covers.go — normalising only one orphans
	// files.
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing key")
		return
	}
	raw, err := decodeCoverBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	out, hash, err := processCoverImage(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_image", err.Error())
		return
	}
	cfg := s.deps.CfgHolder.Load()
	if cfg == nil {
		writeError(w, http.StatusInternalServerError, "internal", "config unavailable")
		return
	}
	dir := manifest.PlaylistCoverDir(cfg.DataDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.Error("uploadCover: mkdir", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not create covers dir")
		return
	}
	path := manifest.PlaylistCoverPath(cfg.DataDir, scope, key, "jpg")
	if err := atomicwrite.WriteBytes(path, out, ".cover-"); err != nil {
		logger.Error("uploadCover: write", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not write cover")
		return
	}
	if err := s.deps.Manifest.SetPlaylistCover(r.Context(), manifest.PlaylistCover{
		Scope: scope, Key: key, ImageHash: hash, Ext: "jpg", UpdatedAt: time.Now().UnixNano(),
	}); err != nil {
		logger.Error("uploadCover: set mapping", "err", err)
		// Don't leave a JPEG on disk with no DB mapping (an unreferenced
		// orphan) — unlink the file we just wrote (Gemini MEDIUM on PR #402).
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			logger.Warn("uploadCover: cleanup orphaned file", "err", rmErr)
		}
		writeError(w, http.StatusInternalServerError, "internal", "could not record cover")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"imageHash": hash})
}

// deleteCover removes a cover mapping + its on-disk JPEG.
func (s *Server) deleteCover(w http.ResponseWriter, r *http.Request, scope, key string) {
	// The key feeds BOTH the on-disk filename (sha256 of scope+key,
	// byte-exact) and the playlist_covers row, so the write side has to
	// normalise in lockstep with the read side in
	// internal/api/playlist_covers.go — normalising only one orphans
	// files.
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "missing key")
		return
	}
	_, ext, ok, err := s.deps.Manifest.DeletePlaylistCover(r.Context(), scope, key)
	if err != nil {
		logger.Error("deleteCover: delete mapping", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not delete cover")
		return
	}
	if ok {
		cfg := s.deps.CfgHolder.Load()
		if cfg != nil {
			if rmErr := os.Remove(manifest.PlaylistCoverPath(cfg.DataDir, scope, key, ext)); rmErr != nil && !os.IsNotExist(rmErr) {
				logger.Warn("deleteCover: unlink file", "err", rmErr)
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiUploadSmartMixCover(w http.ResponseWriter, r *http.Request) {
	s.uploadCover(w, r, manifest.CoverScopeSmartMix, r.PathValue("slug"))
}

func (s *Server) apiDeleteSmartMixCover(w http.ResponseWriter, r *http.Request) {
	s.deleteCover(w, r, manifest.CoverScopeSmartMix, r.PathValue("slug"))
}

func (s *Server) apiUploadPlaylistCover(w http.ResponseWriter, r *http.Request) {
	s.uploadCover(w, r, manifest.CoverScopePlaylist, r.PathValue("id"))
}

func (s *Server) apiDeletePlaylistCover(w http.ResponseWriter, r *http.Request) {
	s.deleteCover(w, r, manifest.CoverScopePlaylist, r.PathValue("id"))
}
