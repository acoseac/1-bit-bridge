package atlasharvest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/lyrics"
)

// fakeLyricsSink records what the sweep did, so a test asserts on the WRITES
// rather than on the HTTP traffic.
type fakeLyricsSink struct {
	mu         sync.Mutex
	candidates []LyricsCandidate
	docs       map[string]lyrics.Doc
	sources    map[string]lyrics.Source
	attempts   map[string]fakeAttempt
	order      []string
}

type fakeAttempt struct {
	albumMBID, trackMBID, resolvedMBID, status string
	nextAttemptAt                              int64
}

func newFakeSink(c ...LyricsCandidate) *fakeLyricsSink {
	return &fakeLyricsSink{
		candidates: c,
		docs:       map[string]lyrics.Doc{},
		sources:    map[string]lyrics.Source{},
		attempts:   map[string]fakeAttempt{},
	}
}

func (f *fakeLyricsSink) AtlasLyricsCandidates(context.Context, int64, int) ([]LyricsCandidate, error) {
	return f.candidates, nil
}

func (f *fakeLyricsSink) UpsertAtlasLyrics(_ context.Context, path string, doc lyrics.Doc,
	src lyrics.Source) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[path] = doc
	f.sources[path] = src
	return true, nil
}

func (f *fakeLyricsSink) MarkAtlasLyricsAttempt(_ context.Context, path, albumMBID, trackMBID,
	resolvedMBID, status string, nextAttemptAt int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[path] = fakeAttempt{albumMBID, trackMBID, resolvedMBID, status, nextAttemptAt}
	f.order = append(f.order, path+":"+status)
	return nil
}

// atlasStub serves the two endpoints the tier uses. `recordings` maps a
// recording MBID to the sequence of responses it returns, so the two-phase
// pending→available behaviour can be reproduced exactly.
type atlasStub struct {
	mu         sync.Mutex
	release    []ReleaseTrack
	recordings map[string][]recordingResponse
	hits       map[string]int
	releaseHit int
}

func (a *atlasStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/release/"):
			a.releaseHit++
			_ = json.NewEncoder(w).Encode(releaseTracksResponse{
				Tracks: a.release, TotalCount: len(a.release),
			})
		case strings.Contains(r.URL.Path, "/recording/"):
			seg := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			n := a.hits[seg]
			a.hits[seg]++
			seq := a.recordings[seg]
			if len(seq) == 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if n >= len(seq) {
				n = len(seq) - 1
			}
			_ = json.NewEncoder(w).Encode(seq[n])
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func lyricsClient(t *testing.T, stub *atlasStub, sink *fakeLyricsSink) (*Client, State) {
	t.Helper()
	srv := stub.server(t)
	return &Client{
			Lyrics:         sink,
			HTTP:           srv.Client(),
			RequestTimeout: 5 * time.Second,
			LyricsPacing:   time.Nanosecond,
		}, State{
			Token:        "tok",
			AtlasBaseURL: srv.URL,
			ExpiresAt:    time.Now().Add(time.Hour),
		}
}

// `instrumental` is a SUCCESS. It must write no document and must record a
// terminal verdict — storing it as "no lyrics found" is what makes the sweeper
// ask about the same track forever, and an empty document would 404 anyway.
func TestInstrumentalIsASuccessWithNoDocument(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-1"})
	stub := &atlasStub{
		recordings: map[string][]recordingResponse{"rec-1": {{Status: "instrumental"}}},
		hits:       map[string]int{},
	}
	c, st := lyricsClient(t, stub, sink)
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if _, wrote := sink.docs["a/x.flac"]; wrote {
		t.Error("instrumental wrote a lyrics document")
	}
	got := sink.attempts["a/x.flac"]
	if got.status != statusInstrumental {
		t.Errorf("status = %q, want instrumental", got.status)
	}
	if got.nextAttemptAt != 0 {
		t.Errorf("instrumental was given a retry time (%d) — it is terminal", got.nextAttemptAt)
	}
	if got.resolvedMBID != "rec-1" {
		t.Errorf("resolved mbid = %q", got.resolvedMBID)
	}
}

// `pending` is neither an error nor a miss. Atlas warms in the background, so
// the sweep asks again on its second pass — and the FIRST request is what
// started the warm.
func TestPendingIsRetriedWithinTheSweepAndNeverNegativelyCached(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-1"})
	stub := &atlasStub{
		recordings: map[string][]recordingResponse{"rec-1": {
			{Status: "pending"},
			{Status: "available", Plain: "the words", Synced: "[00:01.00] the words"},
		}},
		hits: map[string]int{},
	}
	c, st := lyricsClient(t, stub, sink)
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if stub.hits["rec-1"] < 2 {
		t.Fatalf("the sweep asked %d times; pending must be re-collected in the same sweep", stub.hits["rec-1"])
	}
	doc, ok := sink.docs["a/x.flac"]
	if !ok {
		t.Fatal("the warmed document was never stored")
	}
	if !doc.Synced {
		t.Error("the synced body was available and was not preferred")
	}
	if sink.sources["a/x.flac"] != lyrics.SourceAtlasLRC {
		t.Errorf("stored source = %q, want %q", sink.sources["a/x.flac"], lyrics.SourceAtlasLRC)
	}
	if got := sink.attempts["a/x.flac"].status; got != statusAvailable {
		t.Errorf("status = %q, want available", got)
	}
	// The crucial half: pending must never have been stamped as a miss.
	for _, o := range sink.order {
		if strings.HasSuffix(o, ":"+statusUnavailable) {
			t.Error("a pending recording was negatively cached")
		}
	}
}

// A recording that stays pending is stamped pending with a SHORT backoff — a
// fact about the upstream, never a verdict about the track.
func TestAStubbornlyPendingRecordingIsRearmedNotBuried(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-1"})
	stub := &atlasStub{
		recordings: map[string][]recordingResponse{"rec-1": {{Status: "pending"}}},
		hits:       map[string]int{},
	}
	c, st := lyricsClient(t, stub, sink)
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	got := sink.attempts["a/x.flac"]
	if got.status != statusPending {
		t.Fatalf("status = %q, want pending", got.status)
	}
	if got.nextAttemptAt == 0 {
		t.Fatal("a pending recording was left due immediately, which would spin")
	}
	due := time.Unix(0, got.nextAttemptAt)
	if d := time.Until(due); d > lyricsPendingBackoff+time.Minute {
		t.Errorf("pending backoff is %v, far longer than the %v policy", d, lyricsPendingBackoff)
	}
	if _, wrote := sink.docs["a/x.flac"]; wrote {
		t.Error("a pending recording wrote a document")
	}
}

// `unavailable` is a real miss and gets Atlas's own 30-day negative window,
// which is when Atlas itself re-probes upstream.
func TestUnavailableGetsTheLongBackoff(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-1"})
	stub := &atlasStub{
		recordings: map[string][]recordingResponse{"rec-1": {{Status: "unavailable"}}},
		hits:       map[string]int{},
	}
	c, st := lyricsClient(t, stub, sink)
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	got := sink.attempts["a/x.flac"]
	if got.status != statusUnavailable {
		t.Fatalf("status = %q", got.status)
	}
	if d := time.Until(time.Unix(0, got.nextAttemptAt)); d < 20*24*time.Hour {
		t.Errorf("unavailable backoff is only %v; Atlas caches the miss for 30 days", d)
	}
}

// One release listing serves every candidate on that album. 9,454 candidates
// spanned 1,088 releases when measured, so a per-track fetch would be nine
// times the requests.
func TestOneReleaseFetchServesTheWholeAlbum(t *testing.T) {
	sink := newFakeSink(
		LyricsCandidate{Path: "a/1.flac", AlbumMBID: "alb", Title: "Alpha", DiscNumber: 1, TrackNumber: 1, DurationMS: 100000},
		LyricsCandidate{Path: "a/2.flac", AlbumMBID: "alb", Title: "Beta", DiscNumber: 1, TrackNumber: 2, DurationMS: 200000},
		LyricsCandidate{Path: "a/3.flac", AlbumMBID: "alb", Title: "Gamma", DiscNumber: 1, TrackNumber: 3, DurationMS: 300000},
	)
	stub := &atlasStub{
		release: []ReleaseTrack{
			rt(1, 1, "Alpha", 100000, "rec-1"),
			rt(1, 2, "Beta", 200000, "rec-2"),
			rt(1, 3, "Gamma", 300000, "rec-3"),
		},
		recordings: map[string][]recordingResponse{
			"rec-1": {{Status: "available", Plain: "one"}},
			"rec-2": {{Status: "available", Plain: "two"}},
			"rec-3": {{Status: "available", Plain: "three"}},
		},
		hits: map[string]int{},
	}
	c, st := lyricsClient(t, stub, sink)
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if stub.releaseHit != 1 {
		t.Errorf("fetched the release listing %d times, want 1", stub.releaseHit)
	}
	if len(sink.docs) != 3 {
		t.Errorf("stored %d documents, want 3", len(sink.docs))
	}
}

// A track whose tags already carry a recording MBID never costs a release
// fetch. 838 tracks in this library are in that position.
func TestATaggedRecordingMBIDSkipsTheReleaseFetch(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-9"})
	stub := &atlasStub{
		recordings: map[string][]recordingResponse{"rec-9": {{Status: "available", Plain: "w"}}},
		hits:       map[string]int{},
	}
	c, st := lyricsClient(t, stub, sink)
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if stub.releaseHit != 0 {
		t.Errorf("a tagged recording MBID still cost %d release fetches", stub.releaseHit)
	}
	if _, ok := sink.docs["a/x.flac"]; !ok {
		t.Error("nothing stored")
	}
}

// A cached resolution outranks the tag, because it is the answer a previous
// sweep actually got from Atlas.
func TestACachedResolutionIsPreferred(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-tag", CachedMBID: "rec-cached"})
	stub := &atlasStub{
		recordings: map[string][]recordingResponse{
			"rec-cached": {{Status: "available", Plain: "cached"}},
			"rec-tag":    {{Status: "available", Plain: "tagged"}},
		},
		hits: map[string]int{},
	}
	c, st := lyricsClient(t, stub, sink)
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if stub.hits["rec-cached"] == 0 {
		t.Error("the cached recording MBID was not used")
	}
	if stub.hits["rec-tag"] != 0 {
		t.Error("the tag was consulted even though a resolution was cached")
	}
}

// A transient upstream failure must not write a terminal verdict. A
// thirty-second outage would otherwise sideline every track in flight for
// thirty days — the same classification rule internal/enrich follows.
func TestATransientFailureWritesNoVerdict(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-1"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	c := &Client{Lyrics: sink, HTTP: srv.Client(), RequestTimeout: 5 * time.Second}
	st := State{Token: "t", AtlasBaseURL: srv.URL, ExpiresAt: time.Now().Add(time.Hour)}
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatalf("a 502 must not fail the sweep: %v", err)
	}
	if got, ok := sink.attempts["a/x.flac"]; ok {
		t.Errorf("a transient failure wrote the verdict %q", got.status)
	}
	if len(sink.docs) != 0 {
		t.Error("a 502 stored a document")
	}
}

// A rejected token stops the sweep and reaches handleErr, so the credential is
// wiped rather than hammered — the defect the booklet fetch leg once had.
func TestARejectedTokenStopsTheSweep(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-1"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := &Client{Lyrics: sink, HTTP: srv.Client(), RequestTimeout: 5 * time.Second}
	st := State{Token: "t", AtlasBaseURL: srv.URL, ExpiresAt: time.Now().Add(time.Hour)}
	err := c.tickLyrics(context.Background(), st)
	if err == nil {
		t.Fatal("a 401 was swallowed; the credential would never be wiped")
	}
	if len(sink.attempts) != 0 {
		t.Error("a rejected token wrote a verdict about the track")
	}
}

// The sweep does not run while a scan is in flight. Both write track_lyrics,
// and during a scan the scanner is about to have an opinion about these very
// tracks — writing an Atlas row seconds before it extracts a local one bumps
// indexed_at twice and every paired device syncs the track twice.
func TestTheSweepStandsDownDuringAScan(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-1"})
	stub := &atlasStub{
		recordings: map[string][]recordingResponse{"rec-1": {{Status: "available", Plain: "w"}}},
		hits:       map[string]int{},
	}
	c, st := lyricsClient(t, stub, sink)
	c.ScanInProgress = func() bool { return true }
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if len(sink.docs) != 0 || len(sink.attempts) != 0 {
		t.Error("the sweep wrote while a scan was in flight")
	}
	// ...and runs once the scan is done, so the guard is a pause, not an off
	// switch.
	c.ScanInProgress = func() bool { return false }
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if len(sink.docs) != 1 {
		t.Error("the sweep never resumed after the scan")
	}
}

// The write budget bounds one sweep. It is not a queue guard: every write
// strict-advances indexed_at, so an uncapped first sweep over the 9,454
// addressable tracks measured here pushes 9,454 delta rows to every paired
// device at once.
func TestTheSweepRespectsItsWriteBudget(t *testing.T) {
	var cands []LyricsCandidate
	recs := map[string][]recordingResponse{}
	for i := 0; i < lyricsSweepBudget+25; i++ {
		mb := "rec-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		cands = append(cands, LyricsCandidate{Path: "a/" + mb + ".flac", AlbumMBID: "alb", TrackMBID: mb})
		recs[mb] = []recordingResponse{{Status: "available", Plain: "w"}}
	}
	sink := newFakeSink(cands...)
	stub := &atlasStub{recordings: recs, hits: map[string]int{}}
	c, st := lyricsClient(t, stub, sink)
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if len(sink.docs) > lyricsSweepBudget {
		t.Errorf("wrote %d documents, past the %d budget", len(sink.docs), lyricsSweepBudget)
	}
	if len(sink.docs) == 0 {
		t.Error("the budget stopped everything")
	}
}

func TestDocumentFrom(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rec        recordingResponse
		wantOK     bool
		wantSynced bool
		wantSource lyrics.Source
	}{
		{"synced wins when it parses as LRC",
			recordingResponse{Plain: "plain", Synced: "[00:01.00] timed"}, true, true, lyrics.SourceAtlasLRC},
		{"plain when there is no synced body",
			recordingResponse{Plain: "plain"}, true, false, lyrics.SourceAtlas},
		// A body labelled synced that carries no timing would render as
		// nothing on the client, which re-parses it with its own LRC parser.
		{"a synced body with no time tags falls back to plain",
			recordingResponse{Plain: "plain", Synced: "no timing here"}, true, false, lyrics.SourceAtlas},
		// The content is still lyrics; only the timing is unusable. Keeping it
		// as plain text is what the extractor does with an embedded tag that
		// fails the same gate, and LooksLikeLRC mirrors the client's own
		// parser — so a body with no timing here has none there either.
		{"a synced body with no time tags and no plain body is kept as plain",
			recordingResponse{Synced: "no timing here"}, true, false, lyrics.SourceAtlas},
		{"nothing at all", recordingResponse{}, false, false, ""},
		{"whitespace is not a document", recordingResponse{Plain: "   \n\n "}, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, src, ok := documentFrom(tc.rec)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if doc.Synced != tc.wantSynced || src != tc.wantSource {
				t.Errorf("synced=%v source=%q, want %v/%q", doc.Synced, src, tc.wantSynced, tc.wantSource)
			}
			if doc.Synced && doc.Format != lyrics.FormatLRC {
				t.Errorf("a synced document must be format %q, got %q", lyrics.FormatLRC, doc.Format)
			}
		})
	}
}

// `available` whose payload carries nothing usable is the miss it actually is.
// Trusting the label over the body would store an empty document, which 404s at
// the endpoint while the attempt says the track is done.
func TestAvailableWithNoBodyIsTreatedAsAMiss(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-1"})
	stub := &atlasStub{
		recordings: map[string][]recordingResponse{"rec-1": {{Status: "available"}}},
		hits:       map[string]int{},
	}
	c, st := lyricsClient(t, stub, sink)
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if len(sink.docs) != 0 {
		t.Error("an empty payload was stored as a document")
	}
	if got := sink.attempts["a/x.flac"].status; got != statusUnavailable {
		t.Errorf("status = %q, want unavailable", got)
	}
}

// The release walk pages on OFFSET against total_count, because the live
// service ignores `limit` and returns no next_offset. A page that returns
// nothing ends the walk instead of repeating it forever.
func TestFetchReleaseTracksPagesOnOffset(t *testing.T) {
	total := releasePageSize + 7
	all := make([]ReleaseTrack, total)
	for i := range all {
		all[i] = rt(1, i+1, "T", 1000, "rec")
	}
	var offsets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		off := r.URL.Query().Get("offset")
		offsets = append(offsets, off)
		start := 0
		if off != "" {
			_, _ = fmtSscan(off, &start)
		}
		end := start + releasePageSize
		if end > len(all) {
			end = len(all)
		}
		page := []ReleaseTrack{}
		if start < len(all) {
			page = all[start:end]
		}
		_ = json.NewEncoder(w).Encode(releaseTracksResponse{Tracks: page, TotalCount: total, Offset: start})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), RequestTimeout: 5 * time.Second}
	st := State{Token: "t", AtlasBaseURL: srv.URL, ExpiresAt: time.Now().Add(time.Hour)}
	got, err := c.fetchReleaseTracks(context.Background(), st, "alb")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total {
		t.Errorf("collected %d entries, want %d", len(got), total)
	}
	if len(offsets) != 2 {
		t.Errorf("made %d requests (%v), want 2", len(offsets), offsets)
	}
}

func fmtSscan(s string, out *int) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, nil
		}
		n = n*10 + int(r-'0')
	}
	*out = n
	return 1, nil
}

// FuzzMatchRelease drives the matcher on arbitrary listings. Atlas relays
// LRCLIB and MusicBrainz, so these titles and positions are untrusted input;
// the property is that a match, when one is claimed, is an entry that was
// actually IN the listing — never a fabricated or zero-valued one.
func FuzzMatchRelease(f *testing.F) {
	f.Add("Alpha", 1, 1, int64(100000), "Alpha", 1, 1, int64(100000))
	f.Add("", 0, 0, int64(0), "", 0, 0, int64(0))
	f.Add("Hasta mañana", 2, 5, int64(143000), "Hasta Manana", 1, 5, int64(143500))
	f.Fuzz(func(t *testing.T, localTitle string, localDisc, localNum int, localDur int64,
		e1Title string, e1Medium, e1Pos int, e1Len int64) {
		entries := []ReleaseTrack{
			{MediumPosition: e1Medium, Position: e1Pos, Title: e1Title, LengthMS: e1Len, RecordingMBID: "rec-1"},
			{MediumPosition: e1Medium, Position: e1Pos + 1, Title: e1Title + " (alt)", LengthMS: e1Len, RecordingMBID: "rec-2"},
		}
		got, tier := MatchRelease(entries, localTitle, localDisc, localNum, localDur)
		if tier == MatchNone {
			if got.RecordingMBID != "" {
				t.Fatalf("MatchNone returned a track: %+v", got)
			}
			return
		}
		if got.RecordingMBID != "rec-1" && got.RecordingMBID != "rec-2" {
			t.Fatalf("matched an entry that was not in the listing: %+v", got)
		}
		// A corroborated match must genuinely agree on the title, which is the
		// property the tier's name claims and the reason it skips the veto.
		if tier == MatchCorroborated && foldTitle(got.Title) != foldTitle(localTitle) {
			t.Fatalf("corroborated on titles that differ: %q vs %q", got.Title, localTitle)
		}
	})
}

// The sweep must be dispatched by the REAL tick, not only callable on its own.
//
// Every other test here calls tickLyrics directly, and a feature whose only
// caller is its own test is the shape this tree has shipped dead three times —
// a helper nothing calls, a type nothing constructs, a handler nothing
// dispatches to. This drives `tick`, which is what Run drives in production.
func TestTheRealTickDispatchesTheLyricsLeg(t *testing.T) {
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-1"})
	stub := &atlasStub{
		recordings: map[string][]recordingResponse{"rec-1": {{Status: "available", Plain: "words"}}},
		hits:       map[string]int{},
	}
	c, st := lyricsClient(t, stub, sink)
	// tick() runs the submit and poll legs before this one, so the client has
	// to be complete: nil MBIDs is not an "off" state for those, it is a nil
	// deref. Production always supplies them (manifestStore).
	c.MBIDs = emptyMBIDs{}
	c.Sink = discardMeta{}
	// tick() reads the credential from the state store, so give it a real one.
	dir := t.TempDir()
	store, err := OpenStateStore(dir + "/atlas-harvest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCredential(st.Token, st.AtlasBaseURL, st.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	c.State = store

	c.tick(context.Background())

	if _, ok := sink.docs["a/x.flac"]; !ok {
		t.Fatal("tick() did not reach the lyrics leg — the sweep is unwired")
	}
}

// ...and a nil sink leaves it dormant, which is what makes the feature opt-in.
func TestANilLyricsSinkDisablesTheTier(t *testing.T) {
	stub := &atlasStub{recordings: map[string][]recordingResponse{}, hits: map[string]int{}}
	sink := newFakeSink(LyricsCandidate{Path: "a/x.flac", AlbumMBID: "alb", TrackMBID: "rec-1"})
	c, st := lyricsClient(t, stub, sink)
	c.Lyrics = nil
	if err := c.tickLyrics(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if len(sink.docs) != 0 || len(sink.attempts) != 0 {
		t.Error("a nil sink still wrote")
	}
}

// emptyMBIDs is a library with nothing to submit, so tick()'s submit leg is a
// no-op rather than a nil deref.
type emptyMBIDs struct{}

func (emptyMBIDs) DistinctArtistMBIDs(context.Context) ([]string, error)      { return nil, nil }
func (emptyMBIDs) DistinctReleaseMBIDs(context.Context) ([]string, error)     { return nil, nil }
func (emptyMBIDs) DistinctReleaseTextMBIDs(context.Context) ([]string, error) { return nil, nil }

type discardMeta struct{}

func (discardMeta) UpsertArtistMeta(context.Context, ArtistMeta) error   { return nil }
func (discardMeta) UpsertReleaseMeta(context.Context, ReleaseMeta) error { return nil }

// A transient failure on the RELEASE fetch must write no verdict either.
//
// The sibling test above uses a candidate carrying a tagged recording MBID,
// which skips the release listing entirely — so it proved the recording leg and
// said nothing about this one. A 502 here used to reach MatchNone and stamp
// `unresolved` with a fourteen-day backoff, parking a real album because Atlas
// was restarting. (Gemini, PR #888.)
func TestATransientReleaseFetchFailureWritesNoVerdict(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantStamp bool
		wantErr   bool
	}{
		// The upstream failed to answer: nothing is known, nothing is written.
		{"502 bad gateway", http.StatusBadGateway, false, true},
		{"503 unavailable", http.StatusServiceUnavailable, false, true},
		// 4xx by number, transient by meaning — a rate limit says nothing
		// about whether the album exists.
		{"429 too many requests", http.StatusTooManyRequests, false, true},
		{"408 request timeout", http.StatusRequestTimeout, false, true},
		// The upstream ANSWERED: Atlas does not have this release. Durable, so
		// the candidate is stamped and the sweep carries on.
		{"404 not found", http.StatusNotFound, true, false},
		{"400 bad request", http.StatusBadRequest, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := newFakeSink(LyricsCandidate{
				Path: "a/x.flac", AlbumMBID: "alb", Title: "Song", DiscNumber: 1, TrackNumber: 1,
			})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			c := &Client{Lyrics: sink, HTTP: srv.Client(), RequestTimeout: 5 * time.Second,
				LyricsPacing: time.Nanosecond}
			st := State{Token: "t", AtlasBaseURL: srv.URL, ExpiresAt: time.Now().Add(time.Hour)}

			err := c.tickLyrics(context.Background(), st)
			if tc.wantErr && err == nil {
				t.Error("a transient failure did not stop the sweep")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("an answered 4xx stopped the sweep: %v", err)
			}
			got, stamped := sink.attempts["a/x.flac"]
			if stamped != tc.wantStamp {
				t.Errorf("stamped = %v (%q), want %v", stamped, got.status, tc.wantStamp)
			}
			if tc.wantStamp && got.status != statusUnresolved {
				t.Errorf("status = %q, want unresolved", got.status)
			}
		})
	}
}

// The classifier itself, because the whole transient/durable split rests on it
// and it must read the CODE rather than the message.
func TestIsUpstreamAnswered(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{
		{http.StatusNotFound, true}, {http.StatusBadRequest, true}, {http.StatusGone, true},
		{http.StatusTooManyRequests, false}, {http.StatusRequestTimeout, false},
		{http.StatusBadGateway, false}, {http.StatusInternalServerError, false},
	} {
		if got := isUpstreamAnswered(&httpStatusError{Code: tc.code}); got != tc.want {
			t.Errorf("code %d = %v, want %v", tc.code, got, tc.want)
		}
	}
	if isUpstreamAnswered(errors.New("dial tcp: connection refused")) {
		t.Error("a transport error was read as an upstream answer")
	}
	if isUpstreamAnswered(nil) {
		t.Error("nil was read as an upstream answer")
	}
	// A BODY quoting a 404 must not be mistaken for one — the substring trap.
	if isUpstreamAnswered(&httpStatusError{Code: 503, Body: "upstream said: http 404: nope"}) {
		t.Error("the body's text outvoted the status code")
	}
}
