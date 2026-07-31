// Live safety control for the acceptance policy.
//
// SKIPPED BY DEFAULT. It needs a reachable MusicBrainz-compatible server
// and a JSON dump of real (artist, albumArtist, album, mbid) rows, so it
// can never run in CI. Run it by hand BEFORE changing pickBestRelease:
//
//	BRIDGE_CONTROL_BASE=https://atlas.example/ws/2 \
//	BRIDGE_CONTROL_MATCHED=/path/matched_sample.json \
//	BRIDGE_CONTROL_UNMATCHED=/path/unmatched.json \
//	go test ./internal/enrich/ -run TestAtlasAcceptanceControl -v -count=1
//
// WHY THIS EXISTS AS A TEST RATHER THAN A ONE-OFF SCRIPT: the recall
// numbers behind the folding work were produced by a Python
// reimplementation of the fold, and that reimplementation was NOT
// byte-identical to the shipped Go one (it spaced apostrophes instead of
// deleting them, and left `&` intact). Numbers measured with a lookalike
// are evidence about the lookalike. This runs the REAL exported code
// path, so the claim and the artefact cannot drift.
//
// The gate that matters: ZERO albums that resolve today may move to a
// DIFFERENT RELEASE GROUP. A different pressing of the same group is
// benign (artwork and descriptions resolve through the group).
package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type controlRow struct {
	Artist      string `json:"artist"`
	AlbumArtist string `json:"albumArtist"`
	Album       string `json:"album"`
	MBID        string `json:"mbid"`
	N           int    `json:"n"`
}

// pickBestReleaseLegacy is the PRE-FOLD acceptance, kept here and nowhere
// else so the control can diff old against new on identical candidate
// sets. Do not use it in production code.
func pickBestReleaseLegacy(candidates []releaseCandidate, album, artist string) *releaseCandidate {
	albumLower := strings.ToLower(album)
	artistLower := strings.ToLower(artist)
	contains := func(a, bLower string) bool {
		if a == "" || bLower == "" {
			return false
		}
		if strings.EqualFold(a, bLower) {
			return true
		}
		la := strings.ToLower(a)
		return strings.Contains(la, bLower) || strings.Contains(bLower, la)
	}
	var best *releaseCandidate
	bestScore := 0
	for i := range candidates {
		c := &candidates[i]
		if c.Score < 80 || !contains(c.Title, albumLower) {
			continue
		}
		matched := false
		for _, cr := range c.ArtistCredit {
			lc := strings.ToLower(cr.Name)
			if strings.Contains(lc, artistLower) || strings.Contains(artistLower, lc) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		s := c.Score
		if strings.EqualFold(c.Title, album) {
			s += 10
		}
		if len(c.ArtistCredit) > 0 && strings.EqualFold(c.ArtistCredit[0].Name, artist) {
			s += 10
		}
		if best == nil || s > bestScore {
			best, bestScore = c, s
		}
	}
	return best
}

type controlClient struct {
	base  string
	cache map[string][]releaseCandidate
	mb    *MusicBrainzClient
}

func (cc *controlClient) search(ctx context.Context, artist, album string) []releaseCandidate {
	key := artist + "\x00" + album
	if v, ok := cc.cache[key]; ok {
		return v
	}
	q := fmt.Sprintf(`release:"%s" AND artist:"%s"`, escapeLucene(album), escapeLucene(artist))
	u := fmt.Sprintf("%s/release/?query=%s&fmt=json&limit=%d", cc.base, url.QueryEscape(q), releaseSearchLimit)
	var body releaseSearchResponse
	if err := cc.mb.get(ctx, u, &body); err != nil {
		body.Releases = nil
	}
	cc.cache[key] = body.Releases
	time.Sleep(SelfHostedMinInterval)
	return body.Releases
}

// controlLadder is the PRODUCTION ladder — not a mirror of it. Using the
// real builder is the whole point: a control that reimplements the thing
// it is validating measures the reimplementation.
func controlLadder(artist, albumArtist, album string) []releaseAttempt {
	return buildReleaseLadder(artist, albumArtist, album)
}

func loadControlRows(t *testing.T, env string) []controlRow {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		t.Skipf("%s not set", env)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var rows []controlRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return rows
}

func rgOf(c *releaseCandidate) string {
	if c == nil || c.ReleaseGroup == nil {
		return ""
	}
	return c.ReleaseGroup.ID
}

// TestAtlasAcceptanceControl diffs the shipped acceptance against the
// pre-fold one on identical candidate sets.
func TestAtlasAcceptanceControl(t *testing.T) {
	base := os.Getenv("BRIDGE_CONTROL_BASE")
	if base == "" {
		t.Skip("BRIDGE_CONTROL_BASE not set — live control, run by hand")
	}
	rows := loadControlRows(t, "BRIDGE_CONTROL_MATCHED")
	cc := &controlClient{
		base: base, cache: map[string][]releaseCandidate{},
		mb: NewMusicBrainzClient(base, "1-bit-bridge-control/1.0 (arsenie@odysseus.fi)", nil),
	}
	ctx := context.Background()

	var same, siblingRG, diffRG, newMatch, lostMatch, missingGroupArtifact int
	for _, row := range rows {
		if row.Album == "" {
			continue
		}
		var legacy, folded *releaseCandidate
		for _, rung := range controlLadder(row.Artist, row.AlbumArtist, row.Album) {
			cands := cc.search(ctx, rung.artist, rung.album)
			if legacy == nil {
				legacy = pickBestReleaseLegacy(cands, rung.album, rung.artist)
			}
			if folded == nil {
				folded = pickBestRelease(cands, rung.album, rung.artist)
			}
			if legacy != nil && folded != nil {
				break
			}
		}
		switch {
		case legacy == nil && folded == nil:
		case legacy == nil:
			newMatch++
		case folded == nil:
			lostMatch++
			t.Errorf("REGRESSION: %q / %q resolved before and does not now", row.AlbumArtist, row.Album)
		case legacy.ID == folded.ID:
			same++
		case rgOf(legacy) != "" && rgOf(legacy) == rgOf(folded):
			siblingRG++
		case rgOf(legacy) == "" || rgOf(folded) == "":
			// At least one candidate is missing its release-group in the
			// search response — the upstream omitted the field, so
			// same-vs-different group is undecidable. Counted separately
			// instead of falling into diffRG, so the must-be-zero gate can
			// only fail on a PROVEN group move, never on a missing-field
			// artifact. Both rgs are printed so a one-sided omission shows
			// which side lacked it.
			missingGroupArtifact++
			t.Logf("missing-group artifact: %q / %q (was %q (rg %q), now %q (rg %q))",
				row.AlbumArtist, row.Album, legacy.Title, rgOf(legacy), folded.Title, rgOf(folded))
		default:
			diffRG++
			t.Errorf("DIFFERENT RELEASE GROUP: %q / %q\n  was: %q (rg %s)\n  now: %q (rg %s)",
				row.AlbumArtist, row.Album, legacy.Title, rgOf(legacy), folded.Title, rgOf(folded))
		}
	}
	total := same + siblingRG + diffRG + newMatch + lostMatch + missingGroupArtifact
	t.Logf("control over %d albums that resolve today:", total)
	t.Logf("  identical release        %d", same)
	t.Logf("  sibling pressing, same RG %d", siblingRG)
	t.Logf("  DIFFERENT release group   %d   <- must be 0", diffRG)
	t.Logf("  missing-group artifacts   %d   (upstream omitted rg; not a gate)", missingGroupArtifact)
	t.Logf("  newly matched             %d", newMatch)
	t.Logf("  lost                      %d   <- must be 0", lostMatch)
}

// TestAtlasRecallOnUnmatched measures how much of the currently-unmatched
// set the shipped acceptance recovers. Informational — no pass/fail bar,
// because recall depends on the upstream's data, not only on this code.
func TestAtlasRecallOnUnmatched(t *testing.T) {
	base := os.Getenv("BRIDGE_CONTROL_BASE")
	if base == "" {
		t.Skip("BRIDGE_CONTROL_BASE not set — live control, run by hand")
	}
	rows := loadControlRows(t, "BRIDGE_CONTROL_UNMATCHED")
	cc := &controlClient{
		base: base, cache: map[string][]releaseCandidate{},
		mb: NewMusicBrainzClient(base, "1-bit-bridge-control/1.0 (arsenie@odysseus.fi)", nil),
	}
	ctx := context.Background()

	var albums, tracks, totalAlbums, totalTracks int
	for _, row := range rows {
		if row.Album == "" {
			continue
		}
		totalAlbums++
		totalTracks += row.N
		for _, rung := range controlLadder(row.Artist, row.AlbumArtist, row.Album) {
			if got := pickBestRelease(cc.search(ctx, rung.artist, rung.album), rung.album, rung.artist); got != nil {
				albums++
				tracks += row.N
				break
			}
		}
	}
	t.Logf("recall on the currently-unmatched set:")
	t.Logf("  albums %d/%d (%.1f%%)", albums, totalAlbums, 100*float64(albums)/float64(max(totalAlbums, 1)))
	t.Logf("  tracks %d/%d (%.1f%%)", tracks, totalTracks, 100*float64(tracks)/float64(max(totalTracks, 1)))
}
