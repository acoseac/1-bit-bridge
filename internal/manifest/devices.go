package manifest

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DeviceRegistration is the row shape of the device_registrations table
// (migration v10). It anchors per-device state — playlist backups and
// playback-history rows are scoped to DeviceToken, NOT to the ephemeral
// auth.Token.ID, so that re-pairing (which mints a fresh auth token)
// reattaches a device to its prior backups automatically.
//
// Wire-type discipline: this struct carries NO `json:` tags. It is a
// SQLite row-scan target only; any admin/API surface that exposes device
// data MUST wrap it in a package-local DTO before encoding.
type DeviceRegistration struct {
	// DeviceToken is the iOS Keychain recovery token (high-entropy hex,
	// device-local / not iCloud-synced). Durable across app reinstalls
	// and re-pairings — the stable key everything else hangs off.
	DeviceToken string
	// TokenID is the auth.Token.ID currently presenting this device
	// token. Updated on every authed request (self-healing rebind).
	TokenID string
	// DeviceName is best-effort display text. Empty on the header-path
	// upsert; populated from the pairing join request's deviceName at
	// approval time.
	DeviceName  string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
}

// UpsertDeviceRegistration binds (or rebinds) a device token to the auth
// token currently presenting it, refreshing last_seen_at. Called from two
// paths:
//
//   - the authed-request header path (X-Device-Token) — name is "" there,
//     so an existing non-empty name is preserved (the COALESCE/CASE guard
//     below keeps the stored name unless a non-empty one is supplied);
//   - pairing approval — name is the join request's deviceName.
//
// The ON CONFLICT clause IS the rebind: a device presenting a new auth
// token after re-pairing updates token_id in place, keeping first_seen_at.
//
// Holds s.mu per the writer contract; timestamps via s.now().
func (s *Store) UpsertDeviceRegistration(ctx context.Context, deviceToken, tokenID, name string) error {
	if deviceToken == "" {
		return errors.New("manifest: UpsertDeviceRegistration requires a non-empty device token")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UnixNano()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO device_registrations
			(device_token, token_id, device_name, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(device_token) DO UPDATE SET
			token_id     = excluded.token_id,
			-- Keep the existing name unless a non-empty one is supplied,
			-- so the header path (name="") never clobbers the name the
			-- pairing path captured.
			device_name  = CASE WHEN excluded.device_name <> ''
			                    THEN excluded.device_name
			                    ELSE device_registrations.device_name END,
			last_seen_at = excluded.last_seen_at
	`, deviceToken, tokenID, name, now, now)
	return err
}

// GetDeviceByToken returns the registration for a device token, or nil if
// none exists. Read path — no s.mu (WAL handles concurrent readers).
func (s *Store) GetDeviceByToken(ctx context.Context, deviceToken string) (*DeviceRegistration, error) {
	var (
		d               DeviceRegistration
		firstNS, lastNS int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT device_token, token_id, device_name, first_seen_at, last_seen_at
		  FROM device_registrations
		 WHERE device_token = ?
	`, deviceToken).Scan(&d.DeviceToken, &d.TokenID, &d.DeviceName, &firstNS, &lastNS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.FirstSeenAt = time.Unix(0, firstNS)
	d.LastSeenAt = time.Unix(0, lastNS)
	return &d, nil
}

// ListDeviceRegistrations returns all device registrations ordered by
// last_seen_at DESC (most-recently-active first). Used by the admin
// Devices surface. Read path — no s.mu.
func (s *Store) ListDeviceRegistrations(ctx context.Context) ([]DeviceRegistration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT device_token, token_id, device_name, first_seen_at, last_seen_at
		  FROM device_registrations
		 ORDER BY last_seen_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceRegistration
	for rows.Next() {
		var (
			d               DeviceRegistration
			firstNS, lastNS int64
		)
		if err := rows.Scan(&d.DeviceToken, &d.TokenID, &d.DeviceName, &firstNS, &lastNS); err != nil {
			return nil, err
		}
		d.FirstSeenAt = time.Unix(0, firstNS)
		d.LastSeenAt = time.Unix(0, lastNS)
		out = append(out, d)
	}
	return out, rows.Err()
}
