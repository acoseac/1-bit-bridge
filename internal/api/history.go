package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// HistoryStore is the optional backing store for POST /v1/history/batch
// and the GET /v1/history all-devices read feed. Nil-safe — when unwired
// the routes return 404 (feature-off). *manifest.Store satisfies it in
// production.
type HistoryStore interface {
	InsertHistoryBatch(ctx context.Context, events []manifest.PlaybackHistoryRow) error
	// ListHistory pages events newest-first; deviceToken "" is the global
	// all-devices feed (the only form the public read endpoint uses —
	// listening history is user-wide across every paired device).
	ListHistory(ctx context.Context, deviceToken string, limit int, afterID int64) ([]manifest.HistoryEventOut, error)
}

// WithHistoryStore wires the playback-telemetry feature. Advertises the
// "playbackHistory" health-feature flag when set. Returns the receiver.
func (s *Server) WithHistoryStore(hs HistoryStore) *Server {
	s.historyStore = hs
	return s
}

// historyMaxBodyBytes caps POST /v1/history/batch. A drained offline queue
// is bounded by the client's batch size; 4 MiB covers a large catch-up
// flush (thousands of compact events) with headroom.
const historyMaxBodyBytes = 4 << 20

// historyMaxBatchEvents bounds events per request defensively (the iOS
// coordinator batches in the low hundreds). Excess is dropped + counted.
const historyMaxBatchEvents = 10000

// --- wire DTOs (see PROTOCOL.md Appendix A.4) ---

type hardwareTargetDTO struct {
	InterfaceType string `json:"interfaceType,omitempty"`
	DeviceName    string `json:"deviceName,omitempty"`
	OutputRate    int    `json:"outputRate,omitempty"`
	IsDoP         bool   `json:"isDoP,omitempty"`
}

type historyEventDTO struct {
	Path         string             `json:"path"`
	StartedAt    int64              `json:"startedAt"`    // UnixNano UTC
	DurationUsed float64            `json:"durationUsed"` // seconds listened
	Codec        string             `json:"codec,omitempty"`
	VariantID    string             `json:"variantId,omitempty"`
	OutputTarget *hardwareTargetDTO `json:"outputTarget,omitempty"`
}

// errHistoryEventsNotArray is returned when "events" is present but is
// neither a JSON array nor null. Surfaces as the same generic 400 a stdlib
// type-mismatch would produce, keeping the wire contract intact.
var errHistoryEventsNotArray = errors.New("history events must be a JSON array")

// cappedHistoryEvents decodes the "events" array with a decode-time count
// cap. The 4 MiB MaxBytesReader bounds the raw bytes, but `{}` is ~3 bytes
// with its separator, so a crafted body still decodes to ~1.4M structs before
// any post-decode guard fires — and the handler then sized its output slice
// on that same uncapped count, doubling the amplification from a single
// authed POST. No body-size cap fixes this (a parseable event is ~3 bytes
// while a real one is hundreds), so the fix streams the array and stops
// materialising structs at historyMaxBatchEvents.
//
// Mirrors cappedPlaylistItems in playlists.go, with one deliberate
// difference: playlists REJECT an oversized array, whereas history DROPS the
// excess and reports the count in the 202 (a device draining a long offline
// queue must not have the whole batch refused). Elements past the cap are
// parsed into an empty struct — parsed so the count stays exact and the array
// is still validated, into `struct{}` so no per-element fields are allocated.
type cappedHistoryEvents struct {
	items    []historyEventDTO
	overflow int
}

func (c *cappedHistoryEvents) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	// Tolerate JSON null (→ empty), matching stdlib slice-decode.
	if tok == nil {
		c.items, c.overflow = nil, 0
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return errHistoryEventsNotArray
	}
	items := make([]historyEventDTO, 0)
	overflow := 0
	for dec.More() {
		if len(items) >= historyMaxBatchEvents {
			var skip struct{}
			if err := dec.Decode(&skip); err != nil {
				return err
			}
			overflow++
			continue
		}
		var e historyEventDTO
		if err := dec.Decode(&e); err != nil {
			return err
		}
		items = append(items, e)
	}
	// Consume AND verify the closing ']' — dec.More() returning false only
	// says the next byte is a closing delimiter or EOF, so a malformed value
	// whose "events" isn't a cleanly-terminated array must not pass silently.
	closeTok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := closeTok.(json.Delim); !ok || d != ']' {
		return errHistoryEventsNotArray
	}
	c.items, c.overflow = items, overflow
	return nil
}

type historyBatchRequest struct {
	Events cappedHistoryEvents `json:"events"`
}

type historyBatchResponse struct {
	Accepted int `json:"accepted"`
	Dropped  int `json:"dropped"`
}

// historyBatch handles POST /v1/history/batch. Validate-then-transact: bad
// events are dropped (not faulted) and counted in `dropped`, so one corrupt
// event never rolls back the device's other stats.
func (s *Server) historyBatch(w http.ResponseWriter, r *http.Request) {
	if s.historyStore == nil {
		writeError(w, http.StatusNotFound, "playback_history_not_supported",
			"this bridge does not record playback history")
		return
	}
	dt := deviceTokenFromContext(r.Context())
	if dt == "" {
		writeError(w, http.StatusBadRequest, "device_token_required",
			"playback history requires the X-Device-Token header")
		return
	}

	var body historyBatchRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, historyMaxBodyBytes))
	if err := dec.Decode(&body); err != nil {
		writeErrorLog(w, r, http.StatusBadRequest, "bad_request",
			"request body must be {events:[...]}", err)
		return
	}

	// len(items) is bounded by historyMaxBatchEvents at decode time (see
	// cappedHistoryEvents), so this pre-size can no longer be driven by the
	// request. `overflow` carries the count the decoder parsed-and-discarded
	// past the cap, preserving the exact `dropped` figure the 202 reports.
	events := body.Events.items
	clean := make([]manifest.PlaybackHistoryRow, 0, len(events))
	dropped := body.Events.overflow
	for _, e := range events {
		path := strings.ReplaceAll(strings.TrimSpace(e.Path), `\`, "/")
		// Strip a leading slash: iOS normalizes bridge-source paths with a
		// leading "/", but the scanner stores track paths without one, so
		// the verbatim form wouldn't join against the tracks table for the
		// admin top-tracks aggregation (Gemini HIGH on PR #336).
		path = strings.TrimPrefix(path, "/")
		// Drop (don't fault) malformed events.
		if path == "" || e.StartedAt <= 0 ||
			math.IsNaN(e.DurationUsed) || math.IsInf(e.DurationUsed, 0) || e.DurationUsed < 0 {
			dropped++
			continue
		}
		row := manifest.PlaybackHistoryRow{
			DeviceToken:  dt,
			Path:         path,
			StartedAt:    e.StartedAt,
			DurationUsed: e.DurationUsed,
			Codec:        e.Codec,
			VariantID:    e.VariantID,
		}
		if t := e.OutputTarget; t != nil {
			row.IfaceType = t.InterfaceType
			row.DeviceName = t.DeviceName
			row.OutputRate = t.OutputRate
			row.IsDoP = t.IsDoP
		}
		clean = append(clean, row)
	}

	if err := s.historyStore.InsertHistoryBatch(r.Context(), clean); err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to record playback history", err)
		return
	}
	writeJSON(w, http.StatusAccepted, historyBatchResponse{Accepted: len(clean), Dropped: dropped})
}

// --- GET /v1/history — all-devices read feed (additive, see PROTOCOL.md) ---

// historyReadEventDTO is one event on the read feed. DeviceID/DeviceName
// attribute the event to the PLAYING device; outputTarget.deviceName is
// the OUTPUT hardware (the DAC), as on the upload shape.
type historyReadEventDTO struct {
	Path         string             `json:"path"`
	StartedAt    int64              `json:"startedAt"`    // UnixNano UTC
	DurationUsed float64            `json:"durationUsed"` // seconds listened
	Codec        string             `json:"codec,omitempty"`
	VariantID    string             `json:"variantId,omitempty"`
	OutputTarget *hardwareTargetDTO `json:"outputTarget,omitempty"`
	DeviceID     string             `json:"deviceId,omitempty"`
	DeviceName   string             `json:"deviceName,omitempty"`
}

type historyListResponse struct {
	Events     []historyReadEventDTO `json:"events"`
	NextCursor int64                 `json:"nextCursor"` // 0 = no further pages
}

// historyDeviceIDLen is the length of the wire `deviceId` — the first 16
// hex chars of SHA-256(deviceToken). The raw recovery token is a secret
// and MUST NOT appear on the wire; the hash prefix is a stable, non-
// reversible display id a client can also derive for its own token to
// mark "this device".
const historyDeviceIDLen = 16

func historyDeviceID(deviceToken string) string {
	if deviceToken == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(deviceToken))
	return hex.EncodeToString(sum[:])[:historyDeviceIDLen]
}

// historyList handles GET /v1/history?limit=&after= — the authenticated,
// cursor-paged listening-history feed across ALL of the user's devices
// (single-user model: every paired device belongs to the bridge operator,
// so the feed is user-wide by design; a future multi-user mode would
// re-scope it). Advertised via the `playbackHistoryRead` feature flag.
func (s *Server) historyList(w http.ResponseWriter, r *http.Request) {
	if s.historyStore == nil {
		writeError(w, http.StatusNotFound, "playback_history_not_supported",
			"this bridge does not record playback history")
		return
	}
	q := safeQuery(r)
	// Default/cap mirror the store's own clamp; resolving them here too
	// lets the short-page check below emit a definitive nextCursor=0 on
	// the last non-empty page (saves the client one empty-page fetch).
	limit := 200
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be a positive integer")
			return
		}
		if n > 1000 {
			n = 1000
		}
		limit = n
	}
	var after int64
	if v := q.Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "after must be a non-negative integer cursor")
			return
		}
		after = n
	}

	events, err := s.historyStore.ListHistory(r.Context(), "", limit, after)
	if err != nil {
		writeErrorLog(w, r, http.StatusInternalServerError, "internal",
			"failed to list playback history", err)
		return
	}
	resp := historyListResponse{Events: make([]historyReadEventDTO, 0, len(events))}
	for _, e := range events {
		dto := historyReadEventDTO{
			Path:         e.Path,
			StartedAt:    e.StartedAt,
			DurationUsed: e.DurationUsed,
			Codec:        e.Codec,
			VariantID:    e.VariantID,
			DeviceID:     historyDeviceID(e.SourceDeviceToken),
			DeviceName:   e.SourceDeviceName,
		}
		if e.IfaceType != "" || e.DeviceName != "" || e.OutputRate != 0 || e.IsDoP {
			dto.OutputTarget = &hardwareTargetDTO{
				InterfaceType: e.IfaceType,
				DeviceName:    e.DeviceName,
				OutputRate:    e.OutputRate,
				IsDoP:         e.IsDoP,
			}
		}
		resp.Events = append(resp.Events, dto)
	}
	// A full page may have more behind it; a short (or empty) page is
	// definitively the last — signal that with nextCursor=0.
	if len(events) == limit {
		resp.NextCursor = events[len(events)-1].ID // ListHistory is DESC by id
	}
	writeJSON(w, http.StatusOK, resp)
}
