package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/atlasharvest"
	"github.com/acoseac/1-bit-bridge/internal/enrich"
)

// recordingPremiumFetcher records whether the sweep reached the network /
// filesystem layer at all. The security property is that a malformed MBID
// never gets that far, so "was it called" is the assertion — not "what did
// it write", which would pass against a fetcher that wrote nothing for an
// unrelated reason.
type recordingPremiumFetcher struct {
	called    int
	lastPath  string
	returnGot bool
}

func (f *recordingPremiumFetcher) TryCache(context.Context, string, string, int) bool { return false }

func (f *recordingPremiumFetcher) RefetchPremium(_ context.Context, path, _ string, _ int) (bool, error) {
	f.called++
	f.lastPath = path
	return f.returnGot, nil
}

// TestRefetchPremiumRefusesAMalformedReleaseMBID pins the sink half of the
// path-traversal fix, on the adapter cmd/bridge wires between the harvest
// client and the enricher.
//
// This is the site the whole chain ends at: releaseMBID is chosen by the
// Atlas upstream and becomes the LEADING component of
// ArtworkCachePath's filepath.Join, whose writer does
// os.MkdirAll(filepath.Dir(path)) — so a traversing value created its own
// parent directories outside artworkDir and wrote up to MaxCoverArtBytes
// of upstream-chosen bytes there, then unlinked two siblings beside it.
func TestRefetchPremiumRefusesAMalformedReleaseMBID(t *testing.T) {
	base := t.TempDir()
	artworkDir := filepath.Join(base, "artwork")
	if err := os.MkdirAll(artworkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A sibling of artworkDir that a traversal would reach.
	victim := filepath.Join(base, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}

	fetcher := &recordingPremiumFetcher{returnGot: true}
	a := atlasCoverRefetcher{premium: fetcher, artworkDir: artworkDir, coverSize: 500}

	got, err := a.RefetchPremium(context.Background(), "../victim/owned")
	if err != nil {
		t.Fatalf("a malformed MBID must not abort the sweep for the releases behind it: %v", err)
	}
	if got {
		t.Error("refused refetch reported a cover was upgraded")
	}
	if fetcher.called != 0 {
		t.Fatalf("the fetcher was reached %d time(s) with a traversing MBID (path %q)",
			fetcher.called, fetcher.lastPath)
	}
	// Nothing on the way to the target may have been created either.
	ents, rerr := os.ReadDir(victim)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(ents) != 0 {
		t.Fatalf("a refused refetch touched a sibling directory: %v", ents)
	}
}

// TestRefetchPremiumAcceptsAWellFormedReleaseMBID is the positive control.
// Without it the test above passes against an adapter that refuses every
// MBID, which would silently disable premium cover upgrades altogether.
func TestRefetchPremiumAcceptsAWellFormedReleaseMBID(t *testing.T) {
	artworkDir := t.TempDir()
	fetcher := &recordingPremiumFetcher{}
	a := atlasCoverRefetcher{premium: fetcher, artworkDir: artworkDir, coverSize: 500}

	if _, err := a.RefetchPremium(context.Background(), "9f9e9d9c-9b9a-4998-9796-959493929190"); err != nil {
		t.Fatalf("RefetchPremium: %v", err)
	}
	if fetcher.called != 1 {
		t.Fatalf("a well-formed MBID did not reach the fetcher (called %d times)", fetcher.called)
	}
}

// TestHarvestMBIDPatternMatchesEnrich pins the two anchored UUID patterns
// against each other by BEHAVIOUR.
//
// There are three copies in the tree — api.mbidPattern,
// enrich.mbidValidPattern and atlasharvest's — each separate because the
// dependency direction forbids sharing, and each saying so. Three copies
// of a security predicate is two chances to drift, and cmd/bridge is the
// one package that imports both of the two that guard this chain, so the
// guard lives here.
//
// Compares answers rather than regexp source: the source text agreeing is
// neither necessary nor sufficient for the two gates to admit the same
// set.
func TestHarvestMBIDPatternMatchesEnrich(t *testing.T) {
	for _, in := range []string{
		"9f9e9d9c-9b9a-4998-9796-959493929190",
		"9F9E9D9C-9B9A-4998-9796-959493929190",
		"",
		"../../../../tmp/pwned",
		"/etc/cron.d/x",
		`..\..\x`,
		"9f9e9d9c-9b9a-4998-9796-959493929190/",
		"9f9e9d9c-9b9a-4998-9796-959493929190\n",
		"9f9e9d9c9b9a49989796959493929190",
		"zzzzzzzz-9b9a-4998-9796-959493929190",
		"9f9e9d9c-9b9a-4998-9796-9594939291900",
	} {
		if h, e := atlasharvest.IsValidMBID(in), enrich.IsValidMBID(in); h != e {
			t.Errorf("disagreement on %q: atlasharvest=%v enrich=%v", in, h, e)
		}
	}
}
