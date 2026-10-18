package manifest

import (
	"context"
	"database/sql"
)

// playlistFeedVersionAt answers, on this commit only, what the version is
// here: the cost is not part of it. The next commit puts the cost in the
// version and deletes this file.
func playlistFeedVersionAt(ctx context.Context, tx *sql.Tx, _ int) (string, bool, error) {
	return playlistFeedVersion(ctx, tx)
}
