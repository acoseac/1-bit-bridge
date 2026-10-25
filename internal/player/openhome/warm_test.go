package openhome

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/player/library"
)

// slowBuild is how long the first build of a slowIndex's library takes:
// past what an Insert's budget leaves its confinement under warmLimits.
const slowBuild = time.Second

// warmLimits are the listener's limits but its write deadline, which leaves
// an Insert 500 ms of its budget for its confinement before the edit gate
// (its budget, Write less budgetMargin, less editBudget). The default
// limits leave it 3 s: a budget of 9 s less editBudget's 6 s.
var warmLimits = Limits{Write: budgetMargin + editBudget + 500*time.Millisecond}.orDefaults()

// slowIndex is the library index of a large library on a slow box: every
// row is stamped 1, and a read from before that stamp, the one that builds
// the resolver's index of the paths by their ids, takes slowBuild before it
// hands its rows over, as the first build of a hundred thousand tracks read
// from a Pi's SD card can; a read past it is quick, nothing being written
// since. It counts the builds it served.
type slowIndex struct {
	rows   map[string]bool
	mu     sync.Mutex
	builds int
}

// newSlowIndex is a slowIndex of n synthetic tracks and the paths also.
func newSlowIndex(n int, also ...string) *slowIndex {
	s := &slowIndex{rows: map[string]bool{}}
	for i := range n {
		s.rows[fmt.Sprintf("Artist %03d/Album %02d/%02d Track.flac", i/100, i/10%10, i%10)] = true
	}
	for _, p := range also {
		s.rows[p] = true
	}
	return s
}

func (s *slowIndex) TrackPathsIndexedAfter(ctx context.Context, after int64, fn func(string, int64) error) error {
	if after >= 1 {
		return nil
	}
	s.mu.Lock()
	s.builds++
	s.mu.Unlock()
	select {
	case <-time.After(slowBuild):
	case <-ctx.Done():
		return ctx.Err()
	}
	for p := range s.rows {
		if err := fn(p, 1); err != nil {
			return err
		}
	}
	return nil
}

func (s *slowIndex) GetTrack(_ context.Context, p string) (*manifest.Track, error) {
	if !s.rows[p] {
		return nil, nil
	}
	return &manifest.Track{Path: p}, nil
}

func (s *slowIndex) GetUPnPRouting(context.Context, string) (*manifest.UPnPRouting, error) {
	return nil, nil
}

// builtTimes is how many builds the index served.
func (s *slowIndex) builtTimes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.builds
}

// TestTheFirstInsertAfterTheStartFindsTheLibrarysIndexBuilt pins why serve's
// player builds its library's index ahead of the first lookup (B470 PR 5a4,
// CodeRabbit on #1347). An Insert's confinement asks the library (Member)
// before the edit gate, and the library builds its index of the paths by
// their ids at its first lookup, so that build spends the first Insert's
// budget: past what the budget leaves an edit (3 s with the default limits,
// 500 ms under warmLimits), a valid Insert is answered 501, which it was
// before the warm. Here the first build takes a second. Built ahead of the
// Insert (library.Resolver.Warm, which serve's player runs as it starts its
// surface), the first Insert is taken, Read gives its Uri back, and its
// lookup builds nothing: the index's one build is the warm's.
func TestTheFirstInsertAfterTheStartFindsTheLibrarysIndexBuilt(t *testing.T) {
	const rel = "Artist/Album (2020)/01 Track One.flac"
	p := newTestPlayer(t)
	idx := newSlowIndex(5_000, rel)
	res, err := library.New(library.Config{Store: idx, Paths: bridgefs.New([]string{t.TempDir()})})
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Warm(context.Background()); err != nil {
		t.Fatalf("the warm: %v", err)
	}
	h := p.handlerWith("", p.eng, warmLimits)
	h.library = res
	uri := uriOf(library.Reference(rel).String())
	r := invoke(t, h, playlist, "Insert", "AfterId", "0", "Uri", uri, "Metadata", "")
	if r.status != http.StatusOK {
		t.Fatalf("the first Insert after the start: %d, fault %d; want it taken", r.status, r.fault)
	}
	id := ui4Value(t, "NewId", r.values["NewId"])
	if got := read(t, h, playlist, "Read", "Id", u(id))["Uri"]; id == 0 || got != uri {
		t.Errorf("the first Insert: NewId %d, Read gives %q back; want an entry whose Uri is %q", id, got, uri)
	}
	if n := idx.builtTimes(); n != 1 {
		t.Errorf("the index was built %d times, want once, by the warm", n)
	}
}
