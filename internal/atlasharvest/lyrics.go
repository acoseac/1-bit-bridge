package atlasharvest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
)

// LyricsSink is the store side of the network lyrics tier. Nil disables the
// feature outright, the same way a nil Booklets disables booklets.
type LyricsSink interface {
	AtlasLyricsCandidates(ctx context.Context, now int64, limit int) ([]LyricsCandidate, error)
	UpsertAtlasLyrics(ctx context.Context, path string, doc lyrics.Doc, source lyrics.Source) (bool, error)
	MarkAtlasLyricsAttempt(ctx context.Context, path, albumMBID, trackMBID,
		resolvedMBID, status string, nextAttemptAt int64) error
}

// LyricsCandidate mirrors manifest.AtlasLyricsCandidate. Declared here so this
// package does not import internal/manifest — the adapter in cmd/bridge
// converts, exactly as the booklet and cover sinks do.
type LyricsCandidate struct {
	Path        string
	AlbumMBID   string
	TrackMBID   string
	CachedMBID  string
	Title       string
	TrackNumber int
	DiscNumber  int
	DurationMS  int64
	Attempts    int
}

const (
	// lyricsSweepBudget bounds one sweep's WRITES, and it is not merely a
	// queue guard. Every write strict-advances indexed_at, so an uncapped
	// first sweep over the 9,454 addressable tracks measured here would push
	// 9,454 delta rows to every paired device at once — the same reason
	// auto-optimize has maxPerSweep.
	lyricsSweepBudget = 120

	// lyricsCandidateBatch is how many candidates one sweep considers. Larger
	// than the write budget on purpose: instrumentals and misses consume a
	// candidate without consuming a write.
	lyricsCandidateBatch = 400

	// lyricsPacing is the gap between recording requests. Atlas is self-hosted,
	// but it proxies LRCLIB — a free volunteer service — and a warm request
	// there is a real upstream fetch. 150ms matches the self-hosted MusicBrainz
	// interval this tree already paces at.
	lyricsPacing = 150 * time.Millisecond

	// maxLyricsRecordingBytes caps one recording document. Lyrics normalize to
	// at most lyrics.MaxBodyBytes (512 KiB) and the envelope carries an
	// appears_on list that can run to dozens of releases; 4 MiB is generous
	// for both and still bounds a hostile response.
	maxLyricsRecordingBytes = 4 << 20

	// maxReleaseTracksBytes caps one release listing. `limit` is IGNORED by
	// Atlas (it pins 250 regardless of what is asked), so a page is up to 250
	// entries of six small fields.
	maxReleaseTracksBytes = 4 << 20

	// releasePageSize is what Atlas actually serves per page. Verified live:
	// `?limit=2` returned all 26 tracks of a release and echoed `"limit": 250`,
	// and there is no `next_offset` in the response — so paging advances by
	// this against `total_count` rather than following a cursor.
	releasePageSize = 250

	// maxReleasePages bounds the offset walk so a total_count that never
	// converges cannot spin. 250 * 40 = 10,000 tracks, far past any real
	// release.
	maxReleasePages = 40
)

// Retry policy for a `pending` recording.
//
// Atlas never blocks its read path on the upstream: the first request for an
// unseen recording returns `pending` and warms in the background. Measured over
// 32 unseen recordings on 2026-09-09, every one resolved — min 1.0 s, median
// 6.5 s, p90 15.2 s, max 18.8 s.
//
// That measurement is why there is no sleep here. The sweep warms every track
// in its batch on the first pass and collects on a second, and the polite
// pacing between requests IS the delay: at 150 ms a batch of any real size has
// already spent longer than the median before the second pass begins. A fixed
// short re-probe — two seconds was proposed — would sit below the median and
// miss most of them.
const (
	lyricsPendingBackoff = 2 * time.Minute
	// maxPendingAttempts is when a stubbornly-pending recording stops being
	// re-asked about on this cadence. It is NOT terminal: the row keeps its due
	// time and comes back on the long retry, because pending means Atlas has
	// not answered yet, which is a fact about the upstream and never about the
	// track.
	maxPendingAttempts = 6

	// lyricsMissBackoff matches Atlas's own 30-day negative cache for
	// `unavailable`, so the bridge re-asks at about the point Atlas itself
	// re-probes upstream. Asking sooner spends a request to be told the same
	// thing; never asking again would strand a track whose lyrics were
	// published later.
	lyricsMissBackoff = 30 * 24 * time.Hour

	// lyricsUnresolvedBackoff is for a track we could not turn into a
	// recording MBID at all. The inputs are the operator's tags, which change
	// far more slowly than the upstream, and a retag invalidates the row
	// immediately anyway — so this is long.
	lyricsUnresolvedBackoff = 14 * 24 * time.Hour
)

// recordingResponse is the part of `/v1/atlas/recording/{mbid}` this tier uses.
//
// `lyrics_instrumental` is deliberately absent. It comes back as JSON *null*
// rather than false on the live service, so it cannot be read as a bool without
// a pointer, and it says nothing `lyrics_status` does not already say more
// reliably — the status string is the only signal used here.
type recordingResponse struct {
	Status string `json:"lyrics_status"`
	Plain  string `json:"lyrics"`
	Synced string `json:"lyrics_synced"`
	Source string `json:"lyrics_source"`
}

type releaseTracksResponse struct {
	MBID       string         `json:"mbid"`
	Tracks     []ReleaseTrack `json:"tracks"`
	Offset     int            `json:"offset"`
	TotalCount int            `json:"total_count"`
}

// tickLyrics runs one sweep of the network lyrics tier.
//
// Skipped entirely while a library scan is in flight. Both this and the scanner
// write track_lyrics, and during a scan the scanner is about to have an opinion
// about these very tracks: a sweep that writes an Atlas row seconds before the
// scanner extracts a local one bumps indexed_at twice for one track, and every
// paired device syncs it twice. The booklet GC skips on the same hook for a
// related reason.
func (c *Client) tickLyrics(ctx context.Context, st State) error {
	if c.Lyrics == nil || c.scanInProgress() {
		return nil
	}
	cands, err := c.Lyrics.AtlasLyricsCandidates(ctx, c.now().UnixNano(), lyricsCandidateBatch)
	if err != nil {
		return fmt.Errorf("atlas lyrics candidates: %w", err)
	}
	if len(cands) == 0 {
		return nil
	}

	// Pass one: resolve every candidate to a recording MBID, and ask about it.
	// Anything that comes back `pending` is collected rather than waited on —
	// the request itself is what warms it.
	var pending []LyricsCandidate
	pendingMBID := map[string]string{}
	pendingTier := map[string]MatchTier{}
	written := 0
	releases := map[string][]ReleaseTrack{}

	for _, cand := range cands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if written >= lyricsSweepBudget {
			break
		}
		mbid, tier, err := c.resolveRecording(ctx, st, cand, releases)
		if err != nil {
			return err // transport or auth: the whole sweep stops, nothing stamped
		}
		if mbid == "" {
			c.stamp(ctx, cand, "", statusUnresolved, lyricsUnresolvedBackoff)
			c.log().DebugContext(ctx, "atlaslyrics.unresolved", "album", cand.AlbumMBID)
			continue
		}
		took, status, err := c.applyRecording(ctx, st, cand, mbid, tier)
		if err != nil {
			return err
		}
		if status == statusPending {
			pending = append(pending, cand)
			pendingMBID[cand.Path] = mbid
			pendingTier[cand.Path] = tier
			continue
		}
		if took {
			written++
		}
	}

	// Pass two: collect what pass one warmed. No sleep — the pacing above has
	// already spent more wall clock than the measured median warm time.
	for _, cand := range pending {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if written >= lyricsSweepBudget {
			break
		}
		took, status, err := c.applyRecording(ctx, st, cand, pendingMBID[cand.Path], pendingTier[cand.Path])
		if err != nil {
			return err
		}
		if took {
			written++
		}
		if status == statusPending {
			// Still warming. Not a miss and never terminal.
			back := lyricsPendingBackoff
			if cand.Attempts >= maxPendingAttempts {
				back = lyricsMissBackoff
			}
			c.stamp(ctx, cand, pendingMBID[cand.Path], statusPending, back)
		}
	}

	if written > 0 || len(pending) > 0 {
		c.log().InfoContext(ctx, "atlaslyrics.sweep",
			"candidates", len(cands), "written", written, "pending", len(pending))
	}
	return nil
}

// The verdicts, mirrored from manifest so this package stays import-free of it.
const (
	statusAvailable    = "available"
	statusInstrumental = "instrumental"
	statusUnavailable  = "unavailable"
	statusPending      = "pending"
	statusUnresolved   = "unresolved"
)

// resolveRecording turns a candidate into a recording MBID, cheapest first.
//
// A previously-resolved MBID and the track's own tag both skip the release
// fetch entirely — 838 tracks in this library carry a recording MBID in
// `musicBrainzTrackID`, which the brief for this work believed was zero.
// Everything else costs one release listing per ALBUM, shared through
// `releases` across every candidate on it: 9,454 candidates spanned 1,088
// releases, so the sharing is most of the saving.
func (c *Client) resolveRecording(ctx context.Context, st State, cand LyricsCandidate,
	releases map[string][]ReleaseTrack) (string, MatchTier, error) {
	if cand.CachedMBID != "" {
		return cand.CachedMBID, MatchTagged, nil
	}
	if cand.TrackMBID != "" {
		return cand.TrackMBID, MatchTagged, nil
	}
	if cand.AlbumMBID == "" {
		return "", MatchNone, nil
	}
	entries, ok := releases[cand.AlbumMBID]
	if !ok {
		var err error
		entries, err = c.fetchReleaseTracks(ctx, st, cand.AlbumMBID)
		if err != nil {
			if errors.Is(err, errUnauthorized) || ctx.Err() != nil {
				return "", MatchNone, err
			}
			// A release Atlas does not know is a fact about this album, not a
			// reason to abandon the sweep. Cache the empty answer so its other
			// tracks do not each re-ask.
			c.log().DebugContext(ctx, "atlaslyrics.release_fetch_failed",
				"album", cand.AlbumMBID, "error", err)
			entries = nil
		}
		releases[cand.AlbumMBID] = entries
		c.pace(ctx)
	}
	e, tier := MatchRelease(entries, cand.Title, cand.DiscNumber, cand.TrackNumber, cand.DurationMS)
	if tier == MatchNone {
		return "", MatchNone, nil
	}
	return e.RecordingMBID, tier, nil
}

// applyRecording asks Atlas about one recording and acts on the answer.
func (c *Client) applyRecording(ctx context.Context, st State, cand LyricsCandidate,
	mbid string, tier MatchTier) (bool, string, error) {
	var rec recordingResponse
	err := c.getJSONCapped(ctx, st, "/v1/atlas/recording/"+url.PathEscape(mbid),
		&rec, maxLyricsRecordingBytes, c.requestTimeout())
	c.pace(ctx)
	if err != nil {
		if errors.Is(err, errUnauthorized) || ctx.Err() != nil {
			return false, "", err
		}
		// A transient upstream failure is a fact about the network, never about
		// the track: it must NOT write a terminal verdict, or a thirty-second
		// outage sidelines every track in flight for thirty days. Leaving the
		// attempt row untouched re-offers this candidate on the next sweep.
		c.log().DebugContext(ctx, "atlaslyrics.recording_fetch_failed", "error", err)
		return false, "", nil
	}

	switch rec.Status {
	case statusInstrumental:
		// A SUCCESS, not a miss. There is no document to store, and storing an
		// empty one would 404 at the endpoint anyway; the attempt row is what
		// stops it being asked about forever.
		c.stamp(ctx, cand, mbid, statusInstrumental, 0)
		return false, statusInstrumental, nil
	case statusPending:
		return false, statusPending, nil
	case statusUnavailable:
		c.stamp(ctx, cand, mbid, statusUnavailable, lyricsMissBackoff)
		return false, statusUnavailable, nil
	}

	doc, source, ok := documentFrom(rec)
	if !ok {
		// `available` with nothing usable in it — treat as the miss it is,
		// rather than trusting the label over the payload.
		c.stamp(ctx, cand, mbid, statusUnavailable, lyricsMissBackoff)
		return false, statusUnavailable, nil
	}
	took, err := c.Lyrics.UpsertAtlasLyrics(ctx, cand.Path, doc, source)
	if err != nil {
		return false, "", fmt.Errorf("store atlas lyrics: %w", err)
	}
	c.stamp(ctx, cand, mbid, statusAvailable, 0)
	// Counts, source, tier and lengths — never the text.
	c.log().DebugContext(ctx, "atlaslyrics.stored", "took", took, "synced", doc.Synced,
		"bytes", len(doc.Body), "upstream", rec.Source, "match", tier.String())
	return took, statusAvailable, nil
}

// documentFrom picks the better of the two bodies Atlas may return.
//
// The synced one wins whenever it is genuinely LRC-shaped — that is the whole
// point of the tier, and 18 of 32 measured recordings carried one. It must
// actually PARSE as LRC before being labelled `synced: true`, because the
// client re-parses the body with its own LRC parser and a document that claims
// timing it does not have renders as nothing. `LooksLikeLRC` is the same gate
// the extractor applies to an embedded text tag.
//
// Both bodies go through `Normalize`, which is idempotent and enforces
// `MaxBodyBytes`; a body that normalizes to nothing is not a document.
func documentFrom(rec recordingResponse) (lyrics.Doc, lyrics.Source, bool) {
	if body, ok := lyrics.Normalize(rec.Synced); ok && lyrics.LooksLikeLRC(body) {
		return lyrics.Doc{Format: lyrics.FormatLRC, Synced: true, Body: body},
			lyrics.SourceAtlasLRC, true
	}
	if body, ok := lyrics.Normalize(rec.Plain); ok {
		return lyrics.Doc{Format: lyrics.FormatText, Synced: false, Body: body},
			lyrics.SourceAtlas, true
	}
	// A synced field that does not parse as LRC, with no plain body beside it.
	// The content is still lyrics — only the timing is unusable — so it is
	// kept as PLAIN text rather than discarded, which is exactly what
	// TextCandidate does with an embedded tag that fails the same gate.
	// Demoting it is safe in both directions: LooksLikeLRC mirrors the iOS
	// LRCParser's own line-tag shapes, so a body the bridge cannot see timing
	// in is one the client could not have parsed either.
	if body, ok := lyrics.Normalize(rec.Synced); ok {
		return lyrics.Doc{Format: lyrics.FormatText, Synced: false, Body: body},
			lyrics.SourceAtlas, true
	}
	return lyrics.Doc{}, "", false
}

// stamp records the attempt. A stamping failure is logged and swallowed: the
// document is already stored, and refusing the sweep over a bookkeeping write
// would lose that.
func (c *Client) stamp(ctx context.Context, cand LyricsCandidate, mbid, status string, backoff time.Duration) {
	var next int64
	if backoff > 0 {
		next = c.now().Add(backoff).UnixNano()
	}
	if err := c.Lyrics.MarkAtlasLyricsAttempt(ctx, cand.Path, cand.AlbumMBID, cand.TrackMBID,
		mbid, status, next); err != nil {
		c.log().WarnContext(ctx, "atlaslyrics.stamp_failed", "status", status, "error", err)
	}
}

// fetchReleaseTracks walks a release's entries.
//
// Paged on OFFSET against `total_count`, not on a cursor: the live service
// ignores `limit` (it pins 250 whatever is asked) and returns no `next_offset`,
// so there is nothing to follow. Bounded by maxReleasePages so a total_count
// that never converges cannot spin, and by a no-progress check so a page that
// returns nothing ends the walk rather than repeating it.
func (c *Client) fetchReleaseTracks(ctx context.Context, st State, albumMBID string) ([]ReleaseTrack, error) {
	var out []ReleaseTrack
	offset := 0
	for page := 0; page < maxReleasePages; page++ {
		var resp releaseTracksResponse
		path := fmt.Sprintf("/v1/atlas/release/%s/tracks?offset=%d",
			url.PathEscape(albumMBID), offset)
		if err := c.getJSONCapped(ctx, st, path, &resp, maxReleaseTracksBytes, c.requestTimeout()); err != nil {
			return nil, err
		}
		if len(resp.Tracks) == 0 {
			break
		}
		out = append(out, resp.Tracks...)
		if resp.TotalCount > 0 && len(out) >= resp.TotalCount {
			break
		}
		offset += len(resp.Tracks)
		if len(resp.Tracks) < releasePageSize {
			break
		}
	}
	return out, nil
}

// pace sleeps between upstream requests, and is context-aware so a shutdown is
// not held up by the politeness interval.
//
// The interval is a field rather than the constant so the suite does not spend
// real seconds asleep — a budget-sized sweep at the production interval is 20+
// seconds of pure sleep, and CI already pays enough for SQLite under the race
// detector. Zero means the default, so production never has to set it.
func (c *Client) pace(ctx context.Context) {
	d := c.LyricsPacing
	if d <= 0 {
		d = lyricsPacing
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
