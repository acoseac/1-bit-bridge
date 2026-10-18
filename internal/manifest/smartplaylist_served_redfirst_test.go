package manifest

import (
	"context"
	"time"
)

// This file lives only on the red-first commit of the same-day exclusion fix
// (backlog B451), and the fix deletes it. The day-boundary tests call the two
// methods the fix adds to the store. Here they answer as the store and the
// regenerator do today: the exclusion reads every row stamped at or after
// now minus seven 24-hour days, and a write appends its rows and prunes the
// rows stamped before that same instant. That is the call shape
// assembleInputs and recordDiscoveryServed use on this commit, so the tests
// fail by name against today's semantics instead of failing to compile.

// SmartPlaylistServedOnEarlierDays answers as assembleInputs reads the
// table on this commit: SmartPlaylistServedSince(now - days * 24h).
func (s *Store) SmartPlaylistServedOnEarlierDays(ctx context.Context, nowNS int64, days int) (map[string]struct{}, error) {
	return s.SmartPlaylistServedSince(ctx, nowNS-int64(days)*int64(24*time.Hour))
}

// ReplaceSmartPlaylistServedToday answers as recordDiscoveryServed writes on
// this commit: every path appended at nowNS, then the rows before
// now - days * 24h deleted.
func (s *Store) ReplaceSmartPlaylistServedToday(ctx context.Context, nowNS int64, days int, served map[string][]string) error {
	var rows []SmartPlaylistServedRow
	for slug, paths := range served {
		for _, p := range paths {
			rows = append(rows, SmartPlaylistServedRow{ServedAt: nowNS, Slug: slug, Path: p})
		}
	}
	return s.RecordSmartPlaylistServed(ctx, rows, nowNS-int64(days)*int64(24*time.Hour))
}
