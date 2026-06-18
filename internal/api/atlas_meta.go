package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// AtlasMetaStore backs the rich-tier Atlas metadata endpoints
// (GET /v1/atlas-meta/{release,artist}/{mbid} + POST /v1/atlas-ingest).
// Nil-safe — when unwired the routes return 404 (feature-off). *manifest.Store
// satisfies it in production.
type AtlasMetaStore interface {
	GetReleaseAtlasMeta(ctx context.Context, mbid string) (*manifest.ReleaseAtlasMeta, error)
	GetArtistAtlasMeta(ctx context.Context, mbid string) (*manifest.ArtistAtlasMeta, error)
	UpsertReleaseAtlasMeta(ctx context.Context, m manifest.ReleaseAtlasMeta) error
	UpsertArtistAtlasMeta(ctx context.Context, m manifest.ArtistAtlasMeta) error
}

// WithAtlasMeta wires the optional rich-tier Atlas metadata integration.
// `enabled` mirrors cfg.Atlas.Enabled; `ttl` is the freshness window served
// as `ttlSeconds`. When enabled, /v1/health advertises "atlasEnrichment" and
// the meta/ingest routes are live; otherwise they 404. Returns the receiver.
func (s *Server) WithAtlasMeta(enabled bool, ttl time.Duration, store AtlasMetaStore) *Server {
	s.atlasMetaEnabled = enabled
	s.atlasMetaTTL = ttl
	if enabled {
		s.atlasMetaStore = store
	}
	return s
}

// Body + field caps for /v1/atlas-ingest. Bios/descriptions are small HTML
// blobs; the caps reject a malformed/compromised client ballooning bridge.db.
const (
	atlasIngestMaxBodyBytes = 256 << 10 // 256 KiB whole-body
	atlasMaxTextLen         = 16 << 10  // 16 KiB: bio / description / bioSummary
	atlasMaxLabelLen        = 1 << 10   // 1 KiB: record label
	atlasMaxGenres          = 32        // entries
	atlasMaxGenreLen        = 256       // bytes per genre
	atlasMaxSourceLen       = 64        // bytes: attribution source name ("wiki", …)
	atlasMaxSourceURLLen    = 2 << 10   // 2 KiB: attribution URL
)

// --- wire DTOs (see PROTOCOL.md) ---

// atlasReleaseDTO is the release half of an ingest body. found distinguishes a
// real hit from a tombstone (the app checked Atlas and it had nothing).
type atlasReleaseDTO struct {
	MBID                 string   `json:"mbid"`
	Found                bool     `json:"found"`
	Description          string   `json:"description,omitempty"`
	RecordLabel          string   `json:"recordLabel,omitempty"`
	Genres               []string `json:"genres,omitempty"`
	DescriptionSource    string   `json:"descriptionSource,omitempty"`    // attribution provenance
	DescriptionSourceURL string   `json:"descriptionSourceUrl,omitempty"` // attribution link
	AtlasETag            string   `json:"atlasEtag,omitempty"`
}

type atlasArtistDTO struct {
	MBID         string   `json:"mbid"`
	Found        bool     `json:"found"`
	Bio          string   `json:"bio,omitempty"`
	BioSummary   string   `json:"bioSummary,omitempty"`
	Genres       []string `json:"genres,omitempty"`
	BioSource    string   `json:"bioSource,omitempty"`    // attribution provenance
	BioSourceURL string   `json:"bioSourceUrl,omitempty"` // attribution link
	AtlasETag    string   `json:"atlasEtag,omitempty"`
}

type atlasIngestRequest struct {
	Release *atlasReleaseDTO `json:"release,omitempty"`
	Artist  *atlasArtistDTO  `json:"artist,omitempty"`
}

type atlasIngestResponse struct {
	ReleaseIngested bool `json:"releaseIngested"`
	ArtistIngested  bool `json:"artistIngested"`
}

// atlasMetaResponse is the GET /v1/atlas-meta/{kind}/{mbid} response. found
// false (with ingestedAt set) = tombstone (checked, nothing); a 404 means the
// entity was never checked (the iOS Read-Before-Write gate then fetches
// Atlas). ttlSeconds is the operator's freshness window — iOS treats the row
// as stale once now - ingestedAt > ttlSeconds.
type atlasMetaResponse struct {
	Found       bool     `json:"found"`
	IngestedAt  string   `json:"ingestedAt"` // RFC3339
	TTLSeconds  int64    `json:"ttlSeconds"`
	Description string   `json:"description,omitempty"` // release
	RecordLabel string   `json:"recordLabel,omitempty"` // release
	Bio         string   `json:"bio,omitempty"`         // artist
	BioSummary  string   `json:"bioSummary,omitempty"`  // artist
	Genres      []string `json:"genres,omitempty"`
	// Source + SourceURL attribute the primary text (description for a release,
	// bio for an artist) so iOS renders "Read more on <source>". Empty for a
	// tombstone or when no source contributed.
	Source    string `json:"source,omitempty"`
	SourceURL string `json:"sourceUrl,omitempty"`
}

// atlasIngest handles POST /v1/atlas-ingest. The iOS app (which holds the
// Atlas read:bridge credential) pushes per-entity metadata here; the
// open-source bridge caches + serves it to all the user's devices. UPSERT;
// ingested_at is bridge-stamped. At least one of release / artist must be set.
func (s *Server) atlasIngest(w http.ResponseWriter, r *http.Request) {
	if !s.atlasMetaReady() {
		writeError(w, http.StatusNotFound, "atlas_not_supported", "this bridge does not accept Atlas metadata")
		return
	}
	var req atlasIngestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, atlasIngestMaxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		writeErrorLog(w, r, http.StatusBadRequest, "bad_request", "request body must be {release?,artist?}", err)
		return
	}
	if req.Release == nil && req.Artist == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "at least one of release / artist is required")
		return
	}
	if req.Release != nil {
		if err := validateReleaseDTO(req.Release); err != nil {
			writeError(w, http.StatusBadRequest, "validate", err.Error())
			return
		}
	}
	if req.Artist != nil {
		if err := validateArtistDTO(req.Artist); err != nil {
			writeError(w, http.StatusBadRequest, "validate", err.Error())
			return
		}
	}

	var resp atlasIngestResponse
	if req.Release != nil {
		if err := s.atlasMetaStore.UpsertReleaseAtlasMeta(r.Context(), manifest.ReleaseAtlasMeta{
			ReleaseMBID: req.Release.MBID,
			Found:       req.Release.Found,
			Description: req.Release.Description,
			RecordLabel: req.Release.RecordLabel,
			Genres:      req.Release.Genres,
			Source:      req.Release.DescriptionSource,
			SourceURL:   req.Release.DescriptionSourceURL,
			AtlasETag:   req.Release.AtlasETag,
		}); err != nil {
			writeErrorLog(w, r, http.StatusInternalServerError, "internal", "failed to store release metadata", err)
			return
		}
		resp.ReleaseIngested = true
	}
	if req.Artist != nil {
		if err := s.atlasMetaStore.UpsertArtistAtlasMeta(r.Context(), manifest.ArtistAtlasMeta{
			ArtistMBID: req.Artist.MBID,
			Found:      req.Artist.Found,
			Bio:        req.Artist.Bio,
			BioSummary: req.Artist.BioSummary,
			Genres:     req.Artist.Genres,
			Source:     req.Artist.BioSource,
			SourceURL:  req.Artist.BioSourceURL,
			AtlasETag:  req.Artist.AtlasETag,
		}); err != nil {
			writeErrorLog(w, r, http.StatusInternalServerError, "internal", "failed to store artist metadata", err)
			return
		}
		resp.ArtistIngested = true
	}
	writeJSON(w, http.StatusOK, resp)
}

// atlasMetaRelease handles GET /v1/atlas-meta/release/{mbid}.
func (s *Server) atlasMetaRelease(w http.ResponseWriter, r *http.Request) {
	if !s.atlasMetaReady() {
		writeError(w, http.StatusNotFound, "atlas_not_supported", "this bridge does not serve Atlas metadata")
		return
	}
	mbid := r.PathValue("mbid")
	if !mbidPattern.MatchString(mbid) {
		writeError(w, http.StatusBadRequest, "bad_request", "mbid must be a UUID")
		return
	}
	m, err := s.atlasMetaStore.GetReleaseAtlasMeta(r.Context(), mbid)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal", "failed to read release metadata", err)
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "not_found", "no Atlas metadata cached for this release")
		return
	}
	writeJSON(w, http.StatusOK, atlasMetaResponse{
		Found:       m.Found,
		IngestedAt:  m.IngestedAt.UTC().Format(time.RFC3339),
		TTLSeconds:  int64(s.atlasMetaTTL.Seconds()),
		Description: m.Description,
		RecordLabel: m.RecordLabel,
		Genres:      m.Genres,
		Source:      m.Source,
		SourceURL:   m.SourceURL,
	})
}

// atlasMetaArtist handles GET /v1/atlas-meta/artist/{mbid}.
func (s *Server) atlasMetaArtist(w http.ResponseWriter, r *http.Request) {
	if !s.atlasMetaReady() {
		writeError(w, http.StatusNotFound, "atlas_not_supported", "this bridge does not serve Atlas metadata")
		return
	}
	mbid := r.PathValue("mbid")
	if !mbidPattern.MatchString(mbid) {
		writeError(w, http.StatusBadRequest, "bad_request", "mbid must be a UUID")
		return
	}
	m, err := s.atlasMetaStore.GetArtistAtlasMeta(r.Context(), mbid)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal", "failed to read artist metadata", err)
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "not_found", "no Atlas metadata cached for this artist")
		return
	}
	writeJSON(w, http.StatusOK, atlasMetaResponse{
		Found:      m.Found,
		IngestedAt: m.IngestedAt.UTC().Format(time.RFC3339),
		TTLSeconds: int64(s.atlasMetaTTL.Seconds()),
		Bio:        m.Bio,
		BioSummary: m.BioSummary,
		Genres:     m.Genres,
		Source:     m.Source,
		SourceURL:  m.SourceURL,
	})
}

func (s *Server) atlasMetaReady() bool {
	return s.atlasMetaEnabled && s.atlasMetaStore != nil
}

func validateReleaseDTO(d *atlasReleaseDTO) error {
	if !mbidPattern.MatchString(d.MBID) {
		return fmt.Errorf("release.mbid must be a UUID")
	}
	if len(d.Description) > atlasMaxTextLen {
		return fmt.Errorf("release.description exceeds %d bytes", atlasMaxTextLen)
	}
	if len(d.RecordLabel) > atlasMaxLabelLen {
		return fmt.Errorf("release.recordLabel exceeds %d bytes", atlasMaxLabelLen)
	}
	if err := validateSource("release.descriptionSource", d.DescriptionSource, d.DescriptionSourceURL); err != nil {
		return err
	}
	return validateGenres("release.genres", d.Genres)
}

func validateArtistDTO(d *atlasArtistDTO) error {
	if !mbidPattern.MatchString(d.MBID) {
		return fmt.Errorf("artist.mbid must be a UUID")
	}
	if len(d.Bio) > atlasMaxTextLen {
		return fmt.Errorf("artist.bio exceeds %d bytes", atlasMaxTextLen)
	}
	if len(d.BioSummary) > atlasMaxTextLen {
		return fmt.Errorf("artist.bioSummary exceeds %d bytes", atlasMaxTextLen)
	}
	if err := validateSource("artist.bioSource", d.BioSource, d.BioSourceURL); err != nil {
		return err
	}
	return validateGenres("artist.genres", d.Genres)
}

// validateSource caps the attribution source name + URL. Both are optional; an
// empty pair is fine. Bounds a malformed/compromised client ballooning bridge.db.
func validateSource(field, source, sourceURL string) error {
	if len(source) > atlasMaxSourceLen {
		return fmt.Errorf("%s exceeds %d bytes", field, atlasMaxSourceLen)
	}
	if len(sourceURL) > atlasMaxSourceURLLen {
		return fmt.Errorf("%sUrl exceeds %d bytes", field, atlasMaxSourceURLLen)
	}
	return nil
}

func validateGenres(field string, g []string) error {
	if len(g) > atlasMaxGenres {
		return fmt.Errorf("%s exceeds %d entries", field, atlasMaxGenres)
	}
	for _, x := range g {
		if len(x) > atlasMaxGenreLen {
			return fmt.Errorf("%s entry exceeds %d bytes", field, atlasMaxGenreLen)
		}
	}
	return nil
}
