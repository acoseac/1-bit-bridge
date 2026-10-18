package smartplaylist

import (
	"encoding/binary"
	"hash/fnv"
	"testing"
)

// hashSeedPathGoldenVectors are FNV-1a 64 over a seed's eight big-endian
// bytes and then a path's bytes, as hash/fnv's New64a computes it.
// internal/manifest holds the same table for mixWeekHash, and a change to
// either table has to change both: the engine shuffles by hashSeedPath and
// the SQL windows order by mix_week_hash, the two packages cannot import
// each other, and these tables are what holds the two functions to one
// answer. Three seeds have the high bit set. The paths hash as bytes: the
// composed and the decomposed "ö" are two answers.
var hashSeedPathGoldenVectors = []struct {
	seed uint64
	path string
	want uint64
}{
	{0x0000000000000000, "", 0xa8c7f832281a39c5},
	{0x0000000000000001, "/lib/00.flac", 0x3db121f1669c9ebd},
	{0x0ebea55475a2790a, "/lib/00.flac", 0xeac0370d761a0fc7}, // SeedFromISOWeek(2026, 42)
	{0x8000000000000000, "/lib/00.flac", 0x3df2c1e059c1434e},
	{0xffffffffffffffff, "Artist/Album/01 Track.dsf", 0x8c2d775f7c1b4fa0},
	{0x8000000000000001, "Björk/Homogenic/01 Hunter.flac", 0x032ce91589e29e8e},
	{0x8000000000000001, "Björk/Homogenic/01 Hunter.flac", 0x81040a42898b490a},
	{0x000000000000002a, "日本/曲.dsf", 0xfe2d003b88c32b98},
}

// The golden vectors hold hashSeedPath to the manifest's mixWeekHash, which
// orders the analyzed-pool and deep-cut windows.
func TestHashSeedPath_MatchesTheManifestGoldenVectors(t *testing.T) {
	if got := SeedFromISOWeek(2026, 42); got != 0x0ebea55475a2790a {
		t.Errorf("SeedFromISOWeek(2026, 42) = %#016x; the table names it as 0x0ebea55475a2790a", got)
	}
	for _, v := range hashSeedPathGoldenVectors {
		ref := fnv.New64a()
		var seed [8]byte
		binary.BigEndian.PutUint64(seed[:], v.seed)
		_, _ = ref.Write(seed[:])
		_, _ = ref.Write([]byte(v.path))
		if ref.Sum64() != v.want {
			t.Errorf("vector (%#016x, %+q) says %#016x; FNV-1a of its bytes is %#016x", v.seed, v.path, v.want, ref.Sum64())
		}
		if got := hashSeedPath(v.seed, v.path); got != v.want {
			t.Errorf("hashSeedPath(%#016x, %+q) = %#016x, want %#016x", v.seed, v.path, got, v.want)
		}
	}
}
