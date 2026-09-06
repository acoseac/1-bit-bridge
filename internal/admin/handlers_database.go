package admin

// Operator-triggered database compaction.
//
// Deliberately a BUTTON, not a background sweep. `VACUUM` takes an
// exclusive write lock and rewrites the whole file; on a live bridge with
// a phone mid-sync that is a visible stall, and the operator is the only
// one who knows whether now is a good moment. This matches how every
// other expensive maintenance action on this console already works —
// backups, variant GC, clear-all-variants.

import (
	"errors"
	"net/http"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// databaseCompactResponse reports what the compaction actually achieved,
// not what it hoped to. ReclaimedBytes can legitimately be 0 when
// CheckpointBusy is true — see the field's comment.
type databaseCompactResponse struct {
	BeforeBytes    int64 `json:"beforeBytes"`
	AfterBytes     int64 `json:"afterBytes"`
	ReclaimedBytes int64 `json:"reclaimedBytes"`
	// CheckpointBusy means the vacuum succeeded but a reader still held
	// the old WAL snapshot, so the file has not shrunk YET. Reported
	// rather than swallowed: "succeeded and reclaimed nothing" otherwise
	// reads as a broken button.
	CheckpointBusy bool `json:"checkpointBusy"`
}

// apiDatabaseCompact handles POST /api/database/compact.
func (s *Server) apiDatabaseCompact(w http.ResponseWriter, r *http.Request) {
	if s.deps.Manifest == nil {
		writeError(w, http.StatusServiceUnavailable, "no-manifest", "manifest store is not configured")
		return
	}
	// Refuse while ANY scan is running — full or subtree. A scan takes
	// s.mu in bursts for every batch flush, so it would serialise behind
	// the vacuum for the vacuum's whole duration. ScanInFlight, not
	// IsScanning: the latter is full-scan-only by design.
	if s.deps.Scanner != nil && s.deps.Scanner.ScanInFlight() {
		writeError(w, http.StatusConflict, "scan-in-progress",
			"a library scan is running; compaction would block it — try again when the scan finishes")
		return
	}

	res, err := s.deps.Manifest.Compact(r.Context(), s.deps.DBFreeBytes)
	if err != nil {
		if errors.Is(err, manifest.ErrInsufficientDiskSpace) {
			// 507: the operation is understood and valid, the volume
			// cannot hold what it needs. VACUUM writes a complete second
			// copy before it replaces the original.
			writeError(w, http.StatusInsufficientStorage, "insufficient-disk-space", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "compact-failed", err.Error())
		return
	}
	// The one moment the operator is watching this number change; a
	// TTL-stale read here would look like a button that did nothing.
	s.invalidateDatabaseStats()
	writeJSON(w, http.StatusOK, databaseCompactResponse{
		BeforeBytes: res.BeforeBytes,
		AfterBytes:  res.AfterBytes,
		// One definition of the subtraction, and it lives beside the
		// measurement that justifies its clamp — a busy checkpoint leaves
		// peak disk genuinely HIGHER, and "reclaimed -815,760 bytes" is
		// not a truthful rendering of that. CheckpointBusy carries the
		// rest of the story.
		ReclaimedBytes: res.ReclaimedBytes(),
		CheckpointBusy: res.CheckpointBusy,
	})
}
