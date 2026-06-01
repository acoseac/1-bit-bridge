package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// HistoryStore is the optional backing store for POST /v1/history/batch.
// Nil-safe — when unwired the route returns 404 (feature-off).
// *manifest.Store satisfies it in production.
type HistoryStore interface {
	InsertHistoryBatch(ctx context.Context, events []manifest.PlaybackHistoryRow) error
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

type historyBatchRequest struct {
	Events []historyEventDTO `json:"events"`
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

	clean := make([]manifest.PlaybackHistoryRow, 0, len(body.Events))
	dropped := 0
	for i, e := range body.Events {
		if i >= historyMaxBatchEvents {
			dropped += len(body.Events) - i
			break
		}
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
