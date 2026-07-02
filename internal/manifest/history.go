package manifest

import (
	"context"
	"errors"
)

// PlaybackHistoryRow is one playback-telemetry event. No json tags
// (wire-type discipline): the API + admin wrap it in DTOs. The handler
// validates/normalizes events BEFORE calling InsertHistoryBatch, so the
// store assumes well-formed rows (non-empty path, finite DurationUsed).
type PlaybackHistoryRow struct {
	DeviceToken  string
	Path         string
	StartedAt    int64   // UnixNano UTC
	DurationUsed float64 // seconds listened (REAL — fractional skip seconds preserved)
	Codec        string
	VariantID    string
	IfaceType    string // CarPlay / USB-DAC / Bluetooth / BuiltInSpeakers / Unknown
	DeviceName   string
	OutputRate   int // Hz delivered to hardware (0 = unknown)
	IsDoP        bool
}

// InsertHistoryBatch inserts a pre-validated slice of events in one
// transaction. The handler is responsible for dropping malformed events
// first (so one bad event never rolls back the rest of the device's
// stats). Holds s.mu; received_at via s.now().
func (s *Store) InsertHistoryBatch(ctx context.Context, events []PlaybackHistoryRow) error {
	if len(events) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit; unwind guard otherwise

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO playback_history
			(device_token, path, started_at, duration_used, codec, variant_id,
			 iface_type, device_name, output_rate, is_dop, received_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := s.now().UnixNano()
	for _, e := range events {
		if e.DeviceToken == "" || e.Path == "" {
			return errors.New("manifest: InsertHistoryBatch received an unvalidated event (empty token/path)")
		}
		dop := 0
		if e.IsDoP {
			dop = 1
		}
		if _, err := stmt.ExecContext(ctx,
			e.DeviceToken, e.Path, e.StartedAt, e.DurationUsed,
			nullable(e.Codec), nullable(e.VariantID), nullable(e.IfaceType),
			nullable(e.DeviceName), e.OutputRate, dop, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// HistoryEventOut is a read-path event row (carries the AUTOINCREMENT id
// for cursor paging). No json tags — admin and the /v1/history handler
// wrap it in their own DTOs.
//
// SourceDeviceToken / SourceDeviceName attribute the event to the PLAYING
// device (the latter resolved from device_registrations at read time) —
// distinct from DeviceName, which is the OUTPUT hardware (e.g. the USB
// DAC). SourceDeviceToken is a recovery secret: wire DTOs MUST NOT expose
// it verbatim (the /v1/history handler derives a SHA-256-based display id;
// the admin surface keeps its 8-char-prefix convention).
type HistoryEventOut struct {
	ID                int64
	Path              string
	StartedAt         int64
	DurationUsed      float64
	Codec             string
	VariantID         string
	IfaceType         string
	DeviceName        string
	OutputRate        int
	IsDoP             bool
	SourceDeviceToken string
	SourceDeviceName  string
}

// ListHistory returns events newest-started first, paged by an opaque
// cursor (the last id of the prior page; "" / 0 for the first page).
// An empty deviceToken returns a GLOBAL all-devices feed (mirroring the
// histogram/TopTracks empty-token convention) — used by the loopback
// admin console's owner-visible history log AND the authenticated
// GET /v1/history all-devices feed; a non-empty token scopes to one
// device. Each row carries source-device attribution via a LEFT JOIN on
// device_registrations (events from a device whose registration was
// never formed resolve to an empty name). Read path — no s.mu.
func (s *Store) ListHistory(ctx context.Context, deviceToken string, limit int, afterID int64) ([]HistoryEventOut, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	// afterID paging walks DESC by started_at; for a stable cursor we page
	// by id (monotonic with insert order) when afterID > 0. The `WHERE 1=1`
	// base lets the device-token and cursor clauses append uniformly.
	q := `
		SELECT h.id, h.path, h.started_at, h.duration_used,
		       COALESCE(h.codec,''), COALESCE(h.variant_id,''), COALESCE(h.iface_type,''),
		       COALESCE(h.device_name,''), COALESCE(h.output_rate,0), h.is_dop,
		       h.device_token, COALESCE(d.device_name,'')
		  FROM playback_history h
		  LEFT JOIN device_registrations d ON d.device_token = h.device_token
		 WHERE 1=1`
	var args []any
	if deviceToken != "" {
		q += ` AND h.device_token = ?`
		args = append(args, deviceToken)
	}
	if afterID > 0 {
		q += ` AND h.id < ?`
		args = append(args, afterID)
	}
	q += ` ORDER BY h.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]HistoryEventOut, 0, limit) // limit clamped to [1,1000] above
	for rows.Next() {
		var e HistoryEventOut
		var dop int
		if err := rows.Scan(&e.ID, &e.Path, &e.StartedAt, &e.DurationUsed,
			&e.Codec, &e.VariantID, &e.IfaceType, &e.DeviceName, &e.OutputRate, &dop,
			&e.SourceDeviceToken, &e.SourceDeviceName); err != nil {
			return nil, err
		}
		e.IsDoP = dop != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// HistoryBucket is one (label, count) aggregate row.
type HistoryBucket struct {
	Label string
	Count int64
}

// histogram runs a COUNT(*) GROUP BY over a single non-null column,
// optionally scoped to a device token (empty = all devices, admin-wide).
func (s *Store) histogram(ctx context.Context, column, deviceToken string) ([]HistoryBucket, error) {
	// `column` is NEVER caller-supplied — only the fixed names below reach
	// here, so the interpolation is safe (no user input in the SQL text).
	q := `SELECT COALESCE(` + column + `, '(unknown)') AS label, COUNT(*) AS n
	        FROM playback_history`
	var args []any
	if deviceToken != "" {
		q += ` WHERE device_token = ?`
		args = append(args, deviceToken)
	}
	q += ` GROUP BY label ORDER BY n DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryBucket
	for rows.Next() {
		var b HistoryBucket
		if err := rows.Scan(&b.Label, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// CodecHistogram / RouteHistogram aggregate play counts by codec /
// hardware interface. deviceToken "" aggregates across all devices
// (admin-wide). Read path — no s.mu.
func (s *Store) CodecHistogram(ctx context.Context, deviceToken string) ([]HistoryBucket, error) {
	return s.histogram(ctx, "codec", deviceToken)
}

func (s *Store) RouteHistogram(ctx context.Context, deviceToken string) ([]HistoryBucket, error) {
	return s.histogram(ctx, "iface_type", deviceToken)
}

// TopTracks returns the most-played paths (by event count) across all
// devices, capped at limit. Read path — no s.mu.
func (s *Store) TopTracks(ctx context.Context, limit int) ([]HistoryBucket, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT path AS label, COUNT(*) AS n
		  FROM playback_history
		 GROUP BY path ORDER BY n DESC LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]HistoryBucket, 0, limit) // limit clamped to [1,100] above
	for rows.Next() {
		var b HistoryBucket
		if err := rows.Scan(&b.Label, &b.Count); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// HistoryEventCount returns the total number of stored events (admin
// dashboard). Read path — no s.mu.
func (s *Store) HistoryEventCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM playback_history`).Scan(&n)
	return n, err
}
