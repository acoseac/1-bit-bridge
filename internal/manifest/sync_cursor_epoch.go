package manifest

import "context"

// ClientEpoch is the history a delta request's cursor was issued in, as the
// request names it (`epoch=`, PROTOCOL.md): the `epoch` of the manifest
// response the client took its cursor from. Sent is false where the request
// named none, as every app from before the epoch was on the wire does, and
// Value is then empty. One that was sent, whatever it holds, is compared with
// the epoch the response carries (deltaDeletionFields).
type ClientEpoch struct {
	Value string
	Sent  bool
}

// responseEpoch is the epoch a manifest response carries (Store.BackupEpoch),
// read once for the response: empty where it cannot be read, which is logged,
// and the response then carries none.
func responseEpoch(ctx context.Context, store *Store) string {
	epoch, err := store.BackupEpoch(ctx)
	if err != nil {
		logger.Warn("manifest epoch read failed; the response carries none", "err", err)
		return ""
	}
	return epoch
}
