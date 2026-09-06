package manifest

// Retention for the two tables that have never had any.
//
// Neither `device_registrations` nor `playback_history` has ever had a
// DELETE anywhere in the tree. Both were documented as future work when
// they landed in v1.7 and never got it.
//
// The two are NOT the same kind of problem, and they get different
// answers:
//
//   - A device registration bound to a REVOKED auth token can never be
//     used again. That is garbage, not data, so collecting it is always
//     on and needs no policy.
//
//   - Playback history is the operator's own listening record. It grows
//     ~18k rows a year for a heavy listener (~3 MB), so the problem is
//     not today's size — it is that there is no bound and, until now, no
//     way to see it. That gets a knob, defaulted OFF, with its
//     consequence stated rather than hidden.

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrNoLiveTokens is returned by ReapOrphanDeviceRegistrations when the
// caller supplies an empty live-token set.
//
// FAIL CLOSED. An empty set means "every registration is orphaned", which
// is the one outcome this must never produce — and the realistic way to
// arrive at one is a failure to read the auth store, not a genuinely
// token-less bridge. Same asymmetry as the scanner's errorSubtrees
// sparing: not being able to see something is not evidence that it is
// gone.
//
// The guard is load-bearing, and MEASURED rather than assumed — the two
// empty forms behave completely differently once the SQL runs:
//
//	nil          -> json.Marshal gives `null`; json_each('null') yields one
//	                NULL row, so `token_id NOT IN (NULL)` is NULL (never
//	                true) and ZERO rows are deleted. Accidentally safe.
//	[]string{}   -> json.Marshal gives `[]`; json_each('[]') yields no rows,
//	                so `NOT IN (<empty>)` is TRUE and EVERY row is deleted.
//
// That asymmetry is the trap: a caller doing `ids := make([]string, 0, n)`
// and appending nothing produces the DANGEROUS form, while one returning
// nil produces the safe one — and cmd/bridge's closure builds exactly the
// former. Do not "simplify" this guard away on the reasoning that an
// empty IN-list is harmless; it is harmless in one spelling only.
var ErrNoLiveTokens = errors.New("manifest: refusing to reap device registrations against an empty live-token set")

// ErrCutoffNotInThePast is returned by the two WINDOW reaps when the
// cutoff they are handed is not a moment in the past.
//
// It is deliberately NOT symmetric with the `beforeNS <= 0` no-op beside
// it. Zero means "disabled" -- callers spell "keep everything" that way
// and the sweeper only calls with a window configured -- so a silent
// no-op is the honest answer there. A cutoff at or after NOW says
// "delete every row", which no caller can legitimately mean, and the
// realistic way to produce one is an overflowed
// `time.Now().AddDate(0, 0, -days).UnixNano()` (see
// config.MaxRetentionDays, where the measurement lives).
//
// `config.Validate` already gates every input route the bridge has --
// `Load` runs it after the env overrides, and the settings PATCH runs it
// too -- so this is defence in depth rather than the gate. It is here
// because internal/manifest is a library: a method that empties a table
// when handed a future timestamp is a loaded gun regardless of who
// validates upstream.
var ErrCutoffNotInThePast = errors.New("manifest: retention cutoff is not in the past")

// ReapOrphanDeviceRegistrations deletes registrations whose token_id is
// not in liveTokenIDs — i.e. rows bound to an auth token that has since
// been revoked or rotated away.
//
// The set arrives as ONE bound JSON array consumed by json_each: a single
// static statement with no placeholder construction (no S2077 surface)
// and no bind-ceiling chunking. That shape was questioned in review as a
// scale hazard; it is not one here. The set is one entry per paired
// device (10 on the reference install), so the blob is a few hundred
// bytes, and the alternative — a TEMP table — is per-CONNECTION while
// this Store deliberately runs reads on an unpinned pool, so it would
// need a *sql.Conn or Tx to be correct at all. If this ever becomes a
// large set, THAT is the shape to reach for, not a naive temp table.
//
// Reaping a registration does NOT touch its playback history: those rows
// LEFT JOIN device_registrations for attribution and degrade to
// unattributed, which is a supported state (see ListHistory). There is
// no foreign key on playback_history — nothing anywhere REFERENCES
// device_registrations — so `PRAGMA foreign_keys = 1` has nothing to
// enforce here. Raised as a blocker in review; written down so it is not
// raised twice.
//
// Holds s.mu per the writer contract.
func (s *Store) ReapOrphanDeviceRegistrations(ctx context.Context, liveTokenIDs []string) (int64, error) {
	if len(liveTokenIDs) == 0 {
		return 0, ErrNoLiveTokens
	}
	blob, err := json.Marshal(liveTokenIDs)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM device_registrations
		 WHERE token_id NOT IN (SELECT value FROM json_each(?))
	`, string(blob))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReapStaleDeviceRegistrations deletes registrations untouched since
// beforeNS. Separate from the orphan reap because it is a POLICY (a
// device that has not synced in a year may simply be in a drawer),
// whereas an orphaned row is garbage by construction.
//
// Holds s.mu.
func (s *Store) ReapStaleDeviceRegistrations(ctx context.Context, beforeNS int64) (int64, error) {
	if beforeNS <= 0 {
		return 0, nil
	}
	if beforeNS >= s.now().UnixNano() {
		return 0, ErrCutoffNotInThePast
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM device_registrations WHERE last_seen_at < ?`, beforeNS)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReapPlaybackHistory deletes events that started before beforeNS.
//
// **This degrades Forgotten Favourites, and that is not hidden.**
// PlayStatsForgotten selects tracks whose most recent qualifying play
// predates a cutoff, with NO lower bound at all — a track played twenty
// times two years ago and never since is exactly what it exists to
// surface. Any retention window deletes that evidence. The config field's
// docblock and the settings hint say so; see also the >= 90 day floor
// enforced at config validation, which protects the families that DO have
// bounded windows (HourWindow / SessionWindow / DeepCutsCutoff, all 90d).
//
// Holds s.mu.
func (s *Store) ReapPlaybackHistory(ctx context.Context, beforeNS int64) (int64, error) {
	if beforeNS <= 0 {
		return 0, nil
	}
	if beforeNS >= s.now().UnixNano() {
		return 0, ErrCutoffNotInThePast
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM playback_history WHERE started_at < ?`, beforeNS)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RetentionCounts is the visibility half, and arguably the half that pays
// for itself: an operator cannot sensibly choose a retention policy for a
// table whose size they have never seen. Most, shown the number, will
// correctly choose "off".
type RetentionCounts struct {
	PlaybackHistoryRows     int64
	DeviceRegistrationRows  int64
	OldestPlaybackStartedAt int64 // UnixNano; 0 when the table is empty
}

// RetentionCounts reads both row counts and the oldest retained event.
// Read path — no s.mu.
func (s *Store) RetentionCounts(ctx context.Context) (RetentionCounts, error) {
	var rc RetentionCounts
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MIN(started_at), 0) FROM playback_history`).
		Scan(&rc.PlaybackHistoryRows, &rc.OldestPlaybackStartedAt); err != nil {
		return RetentionCounts{}, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM device_registrations`).
		Scan(&rc.DeviceRegistrationRows); err != nil {
		return RetentionCounts{}, err
	}
	return rc, nil
}
