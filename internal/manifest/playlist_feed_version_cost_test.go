package manifest

import (
	"context"
	"database/sql"
	"testing"
)

// TestTheFeedVersionNamesTheEncodersCostOfAnInvalidByte (CodeRabbit on
// #1277): the bytes a byte of invalid UTF-8 costs decide where the pages
// split, and that cost is the encoder's (jsonInvalidByteBytes), so it is in
// the feed's version. Two builds of one version with different encoders
// then answer different versions rather than one version with different
// pages.
func TestTheFeedVersionNamesTheEncodersCostOfAnInvalidByte(t *testing.T) {
	s := newFeedStore(t, "A/01.flac")
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	versions := map[int]string{}
	for _, cost := range []int{3, 6} {
		v, _, err := playlistFeedVersionAt(ctx, tx, cost)
		if err != nil {
			t.Fatal(err)
		}
		versions[cost] = v
	}
	if versions[3] == versions[6] {
		t.Errorf("the version is %s whatever an invalid byte costs", versions[3])
	}
	current, _, err := playlistFeedVersion(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if want, ok := versions[jsonInvalidByteBytes]; !ok || current != want {
		t.Errorf("the feed's version %s is not the one at this encoder's cost %d (%v)", current, jsonInvalidByteBytes, versions)
	}
}
