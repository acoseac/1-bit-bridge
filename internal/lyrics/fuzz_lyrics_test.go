package lyrics

import (
	"encoding/binary"
	"math/rand"
	"strings"
	"testing"
)

// FuzzParseSYLTToLRC drives the untrusted-input half of this package: a raw
// ID3 SYLT frame body straight from dhowden's m.Raw(), through the parser and
// the LRC renderer.
//
// It carries a PROPERTY, not just "must not panic": every line ToLRC emits has
// to match one of the two line-tag shapes the iOS LRCParser accepts. The
// renderer stamps its output `format: lrc, synced: true` unconditionally, so a
// line neither regex matches is not a rendering blemish — it is a line the
// phone silently drops out of a document the bridge advertised as synced. That
// is exactly how a uint32 millisecond field past 999 minutes shipped
// `[1000:00.000]`, and it is the kind of thing a seed corpus finds and a unit
// test does not.
func FuzzParseSYLTToLRC(f *testing.F) {
	frame := func(enc byte, lang string, entries ...struct {
		ms   uint32
		text string
	}) []byte {
		b := []byte{enc}
		b = append(b, lang...)
		b = append(b, 2, 1, 0) // ms time format, content type 1, empty descriptor
		for _, e := range entries {
			b = append(b, e.text...)
			b = append(b, 0)
			var ts [4]byte
			binary.BigEndian.PutUint32(ts[:], e.ms)
			b = append(b, ts[:]...)
		}
		return b
	}
	type ent = struct {
		ms   uint32
		text string
	}
	f.Add(frame(0, "eng", ent{1000, "First"}, ent{4500, "Second"}))
	f.Add(frame(0, "eng", ent{0, ""}, ent{1000, "After a dummy"}))
	f.Add(frame(0, "eng", ent{500, "Syl"}, ent{700, "la"}, ent{900, "bles"}))
	f.Add(frame(0, "eng", ent{1000, "\nLine start"}, ent{2000, "and end\n"}))
	f.Add(frame(3, "jpn", ent{1000, "日本語"}, ent{2000, "テスト"}))
	f.Add(frame(1, "eng", ent{1000, "\xff\xfeh\x00i\x00"}))
	// The overflow shape: a uint32 past 999 minutes.
	f.Add(frame(0, "eng", ent{60_000_000, "Way past the end"}))
	f.Add(frame(0, "eng", ent{4_294_967_295, "uint32 max"}))
	f.Add([]byte{})
	f.Add([]byte{0, 'e', 'n', 'g', 1, 1, 0}) // MPEG-frame time format: refused

	f.Fuzz(func(t *testing.T, body []byte) {
		s, ok := ParseSYLT(body)
		if !ok {
			return
		}
		lrc, _ := ToLRC(s)
		for _, line := range strings.Split(lrc, "\n") {
			if line == "" {
				continue
			}
			if !lineTag.MatchString(line) && !hoursTag.MatchString(line) {
				t.Fatalf("ToLRC emitted a line neither LRC regex accepts, so the phone "+
					"will drop it out of a document stamped synced:\n%q", line)
			}
		}
		if body, ok := Normalize(lrc); ok && !LooksLikeLRC(body) {
			t.Fatalf("a non-empty rendered SYLT body is not LRC-shaped:\n%q", body)
		}
	})
}

// FuzzNormalize pins the guarantees every caller downstream relies on: the
// output is idempotent (Tag hashes it, so a second pass changing anything
// would re-key an unchanged document), bounded, and free of the carriage
// returns and trailing whitespace the client's parsers are not asked to handle.
func FuzzNormalize(f *testing.F) {
	f.Add("\ufeffHello\r\nWorld  \r\n\n\n")
	f.Add("   \t  \n \t ")
	f.Add("[00:01.000]tagged\r[00:02.000]lines")
	f.Add("é vs é") // NFC folds these together
	f.Add("")
	f.Fuzz(func(t *testing.T, in string) {
		out, ok := Normalize(in)
		if !ok {
			return
		}
		if again, ok2 := Normalize(out); !ok2 || again != out {
			t.Fatalf("Normalize is not idempotent: %q -> %q -> %q (ok=%v)", in, out, again, ok2)
		}
		if len(out) > MaxBodyBytes {
			t.Fatalf("Normalize returned %d bytes, past the %d cap", len(out), MaxBodyBytes)
		}
		if strings.Contains(out, "\r") {
			t.Fatalf("carriage return survived: %q", out)
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimRight(line, " \t") != line {
				t.Fatalf("trailing whitespace survived on %q", line)
			}
		}
		if strings.HasPrefix(out, "\n") || strings.HasSuffix(out, "\n") {
			t.Fatalf("leading/trailing newline survived: %q", out)
		}
	})
}

// FuzzPickIsShuffleInvariant is the determinism property.
//
// The candidate slice is built partly by ranging over dhowden's m.Raw(), a Go
// map whose iteration order is randomised per run. So Pick's comparator has to
// be a strict TOTAL order: any pair it calls equal is decided by chance, the
// winner flips between scans, lyricsTag re-keys, indexed_at bumps, and the
// track re-enters every paired device's delta on every scan. Shuffling the
// input must never change the answer.
func FuzzPickIsShuffleInvariant(f *testing.F) {
	f.Add("sylt|lrc|1|0|abc\ntext|text|0|0|abc")
	f.Add("text|text|0|0|xy\ntext|text|0|2|xy") // same body, fabricated vs junk priority
	f.Add("sylt|lrc|1|0|aaa\nvorbis-synced|lrc|1|0|bbb")
	f.Add("sidecar-lrc|lrc|1|0|q\nsidecar-ttml|ttml|1|0|q")
	f.Add("text|text|0|0|same\ntext-lrc|lrc|1|0|same") // equal length, different source
	// THE tie: same source rank, same priority, same body LENGTH, different
	// bodies — the one shape the comparator used to leave undecided. Without
	// this seed the corpus never reaches the tie-break tail at all, and a
	// control that deletes the tail passes.
	f.Add("text|text|0|0|abc\ntext|text|0|0|xyz")
	f.Add("text|text|0|0|aaa\ntext|text|0|0|aaa\ntext|text|0|0|bbb")
	f.Add("sylt|lrc|1|0|dup\nsylt|lrc|0|0|dup")
	f.Fuzz(func(t *testing.T, spec string) {
		var cands []Candidate
		for _, line := range strings.Split(spec, "\n") {
			parts := strings.Split(line, "|")
			// parts[3] carries the priority digit; an empty field would
			// panic on the index below. The corpus found that immediately —
			// a harness bug, not a Pick bug, and the seed stays to pin it.
			if len(parts) != 5 || parts[4] == "" || parts[3] == "" {
				continue
			}
			cands = append(cands, Candidate{
				Source:   Source(parts[0]),
				Doc:      Doc{Format: parts[1], Synced: parts[2] == "1", Body: parts[4]},
				Priority: int(parts[3][0]) % 3,
			})
		}
		if len(cands) < 2 {
			return
		}
		want, ok := Pick(append([]Candidate(nil), cands...))
		if !ok {
			t.Fatal("Pick refused a non-empty candidate set")
		}
		rng := rand.New(rand.NewSource(int64(len(spec))))
		for i := 0; i < 12; i++ {
			shuffled := append([]Candidate(nil), cands...)
			rng.Shuffle(len(shuffled), func(a, b int) {
				shuffled[a], shuffled[b] = shuffled[b], shuffled[a]
			})
			got, ok := Pick(shuffled)
			if !ok {
				t.Fatal("Pick refused a shuffled copy of the same set")
			}
			if got.Doc != want.Doc || got.Source != want.Source || got.Priority != want.Priority {
				t.Fatalf("Pick is order-dependent — the winner flips with map iteration order.\n"+
					"first: %+v\nshuffled: %+v", want, got)
			}
		}
	})
}
