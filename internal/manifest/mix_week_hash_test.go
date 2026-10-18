package manifest

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"testing"
)

// mixHashGoldenVectors are FNV-1a 64 over a seed's eight big-endian bytes and
// then a path's bytes, as hash/fnv's New64a computes it. internal/smartplaylist
// holds the same table for hashSeedPath, and a change to either table has to
// change both: the SQL windows order by mix_week_hash and the engine
// shuffles by hashSeedPath, the two packages cannot import each other, and
// these tables are what holds the two functions to one answer. Three seeds
// have the high bit set, which database/sql will not bind as a uint64. The
// paths hash as bytes: the composed and the decomposed "ö" are two answers.
var mixHashGoldenVectors = []struct {
	seed uint64
	path string
	want uint64
}{
	{0x0000000000000000, "", 0xa8c7f832281a39c5},
	{0x0000000000000001, "/lib/00.flac", 0x3db121f1669c9ebd},
	{0x0ebea55475a2790a, "/lib/00.flac", 0xeac0370d761a0fc7}, // the 2026-W42 week seed
	{0x8000000000000000, "/lib/00.flac", 0x3df2c1e059c1434e},
	{0xffffffffffffffff, "Artist/Album/01 Track.dsf", 0x8c2d775f7c1b4fa0},
	{0x8000000000000001, "Björk/Homogenic/01 Hunter.flac", 0x032ce91589e29e8e},
	{0x8000000000000001, "Björk/Homogenic/01 Hunter.flac", 0x81040a42898b490a},
	{0x000000000000002a, "日本/曲.dsf", 0xfe2d003b88c32b98},
}

// The golden vectors hold mixWeekHash to the engine's hashSeedPath, and the
// SQL function the windows order by answers the same hash in 16 hex digits
// with the seed bound as its int64 bit pattern, as the window queries bind it.
func TestMixWeekHash_MatchesTheEngineGoldenVectors(t *testing.T) {
	s := newSPStore(t)
	ctx := context.Background()
	for _, v := range mixHashGoldenVectors {
		ref := fnv.New64a()
		var seed [8]byte
		binary.BigEndian.PutUint64(seed[:], v.seed)
		_, _ = ref.Write(seed[:])
		_, _ = ref.Write([]byte(v.path))
		if ref.Sum64() != v.want {
			t.Errorf("vector (%#016x, %+q) says %#016x; FNV-1a of its bytes is %#016x", v.seed, v.path, v.want, ref.Sum64())
		}
		if got := mixWeekHash(v.seed, v.path); got != v.want {
			t.Errorf("mixWeekHash(%#016x, %+q) = %#016x, want %#016x", v.seed, v.path, got, v.want)
		}
		var hex string
		if err := s.db.QueryRowContext(ctx, `SELECT mix_week_hash(?, ?)`, int64(v.seed), v.path).Scan(&hex); err != nil {
			t.Fatalf("mix_week_hash(%#016x, %+q): %v", v.seed, v.path, err)
		}
		if want := fmt.Sprintf("%016x", v.want); hex != want {
			t.Errorf("SQL mix_week_hash(%#016x, %+q) = %s, want %s", v.seed, v.path, hex, want)
		}
	}
}
