package enrich

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/enrichgate"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// The send gate's tests (backlog B617, round 2): a switch-off refuses what
// the enricher would send next, and what an upstream answers after it, at
// once, with no tick of the switch's context. Each enricher here asks its
// switch's context once an hour (switchEvery), so only the gate can stop
// anything, and nothing waits on the wall clock but a test's own failsafe.

// gateSwitch is a switch whose every ask is counted.
type gateSwitch struct {
	on    atomic.Bool
	asked atomic.Int64
}

func newGateSwitch() *gateSwitch {
	s := &gateSwitch{}
	s.on.Store(true)
	return s
}

func (s *gateSwitch) get() bool {
	s.asked.Add(1)
	return s.on.Load()
}

// gateEnricher is switchEnricher with MusicBrainz's client built on mbHTTP
// (the default client when nil) and its switches' contexts asking once an
// hour; it stores track and nothing else.
func gateEnricher(t *testing.T, ups *switchUpstreams, mbHTTP *http.Client, track manifest.Track) (*Enricher, *manifest.Store) {
	t.Helper()
	dir := t.TempDir()
	store, err := manifest.OpenStore(filepath.Join(dir, "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artworkDir := filepath.Join(dir, "artwork")
	if err := os.MkdirAll(artworkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	deezer := NewDeezerClient(ups.deezer.srv.URL, "t", nil, onDeezerSwitches)
	deezer.SetAllowedImageHostsForTest([]string{"127.0.0.1"})
	e := NewEnricher(store, NewMusicBrainzClient(ups.mb.srv.URL, "t", mbHTTP, onSwitches),
		NewCoverArtClient(ups.caa.srv.URL, "t", nil, onSwitches), deezer, artworkDir)
	e.WithPremiumCovers(NewAtlasPremiumFetcher(fakeCred{token: "tok", base: ups.atlas.srv.URL, ok: true}, "t", nil, artworkDir, onSwitches))
	tuneForTest(e)
	e.PollInterval = time.Millisecond
	e.switchEvery = time.Hour
	track.ModTime = time.Now()
	if err := store.UpsertTrack(context.Background(), &track); err != nil {
		t.Fatal(err)
	}
	return e, store
}

// parkedAfterTheFlip waits until the enricher has said it was switched off
// and asked its switch three more times, so its batch is over.
func parkedAfterTheFlip(t *testing.T, rec *loggingtest.Recorder, s *gateSwitch) {
	t.Helper()
	waitFor(t, func() bool { return len(rec.Lines(msgEnrichmentSwitchedOff)) > 0 }, "the enricher to say it was switched off")
	from := s.asked.Load()
	waitFor(t, func() bool { return s.asked.Load() >= from+3 }, "three turns of the loop after the switch-off")
}

// requireNothingKept fails where the enricher recorded anything of a pass
// the switch stopped: a stamp, a count, a skip reason, a line.
func requireNothingKept(t *testing.T, e *Enricher, store *manifest.Store, rec *loggingtest.Recorder, path string) {
	t.Helper()
	if !unenrichedPaths(t, store)[path] {
		t.Errorf("%s was stamped although the switch stopped its pass", path)
	}
	if done, skipped := e.Done(), e.skipped.Load(); done != 0 || skipped != 0 {
		t.Errorf("a pass the switch stopped counted %d done and %d skipped", done, skipped)
	}
	if got := e.SkipReasons(); len(got) != 0 {
		t.Errorf("a pass the switch stopped was given skip reasons: %v", got)
	}
	for _, msg := range []string{"enrichment skipped", msgMarkEnriched, msgMarkSkipped, "MB search", "MB artist search", "artist image", "artwork"} {
		if got := rec.Lines(msg); len(got) != 0 {
			t.Errorf("a pass the switch stopped logged %q:\n%s", msg, strings.Join(got, "\n"))
		}
	}
}

// TestAnAnswerAfterTheFlipIsNotTaken: MusicBrainz's answer to the release
// search, held at the fake until the switch is off and then released, is not
// taken: with no tick of the switch's context (an hour away), the client
// refuses it as it arrives, and nothing follows it. No other request is sent,
// no rung after it, nothing is cached for the album, and the row stays at
// enriched_at = 0, counted and logged as nothing. A no-match released after
// the flip is not a miss either: once the switch is on, the same album is
// asked again and found, so the off did not cost it its answer (backlog B617).
func TestAnAnswerAfterTheFlipIsNotTaken(t *testing.T) {
	for _, answer := range []string{"a match", "a no-match"} {
		t.Run(answer, func(t *testing.T) {
			ups := newSwitchUpstreams(t)
			var noMatch atomic.Bool
			noMatch.Store(answer == "a no-match")
			ups.mb = newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/release/") && noMatch.Load():
					_, _ = io.WriteString(w, `{"releases":[]}`)
				case strings.HasPrefix(r.URL.Path, "/release/"):
					fmt.Fprintf(w, `{"releases":[{"id":%q,"score":100,"title":"Other Album","artist-credit":[{"name":"Artist"}]}]}`, switchSearchedMBID)
				case strings.HasPrefix(r.URL.Path, "/artist"):
					fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":"Artist"}]}`, switchArtistMBID)
				default:
					http.NotFound(w, r)
				}
			})
			track := switchTracks[1] // no embedded release: the search is the first request
			e, store := gateEnricher(t, ups, nil, track)
			s := newGateSwitch()
			e.WithEnabled(s.get)

			held, release := make(chan struct{}), make(chan struct{})
			var first sync.Once
			ups.mb.setHold(func(*http.Request) {
				first.Do(func() {
					close(held)
					<-release
				})
			})
			rec := loggingtest.Record(t)
			stop := runEnricher(t, e)
			select {
			case <-held:
			case <-time.After(10 * time.Second):
				t.Fatal("the enricher sent MusicBrainz no search")
			}
			s.on.Store(false)
			close(release)
			parkedAfterTheFlip(t, rec, s)

			if got := ups.all(); len(got) != 1 {
				t.Errorf("the services were asked %d time(s), want the held search alone:\n%s", len(got), strings.Join(got, "\n"))
			}
			if _, cached := e.albumCache.Get(releaseCacheKey(track.Artist, track.AlbumArtist, track.Album)); cached {
				t.Error("the album's answer, released after the flip, was cached")
			}
			requireNothingKept(t, e, store, rec, track.Path)

			// On again: the same album is asked again, and found.
			noMatch.Store(false)
			s.on.Store(true)
			waitFor(t, func() bool { return !unenrichedPaths(t, store)[track.Path] }, "the album to be enriched once the switch is on")
			stop()
			got, err := store.GetTrack(context.Background(), track.Path)
			if err != nil {
				t.Fatal(err)
			}
			if got.MusicBrainzAlbumID != switchSearchedMBID {
				t.Errorf("after on, the album's release is %q, want %q", got.MusicBrainzAlbumID, switchSearchedMBID)
			}
		})
	}
}

// TestAWriteAfterTheFlipIsNotMade: what the enricher writes of a pass asks
// enrich.enabled again at the write, since an answer taken while the switch
// was on can reach its write after the flip with nothing sent in between. Off
// there, stampEnriched, markSkipped and the fingerprint tag veto write
// nothing, count nothing and log nothing, and the refusal ends the pass's
// context; on, each writes (backlog B617).
func TestAWriteAfterTheFlipIsNotMade(t *testing.T) {
	type writer struct {
		write   func(e *Enricher, ctx context.Context, tr *manifest.Track)
		written func(t *testing.T, e *Enricher, store *manifest.Store, rec *loggingtest.Recorder, path string) bool
	}
	stamped := func(t *testing.T, _ *Enricher, store *manifest.Store, _ *loggingtest.Recorder, path string) bool {
		return !unenrichedPaths(t, store)[path]
	}
	writers := map[string]writer{
		"stampEnriched": {
			write: func(e *Enricher, ctx context.Context, tr *manifest.Track) { e.stampEnriched(ctx, tr) },
			written: func(t *testing.T, e *Enricher, store *manifest.Store, rec *loggingtest.Recorder, path string) bool {
				return stamped(t, e, store, rec, path) || e.Done() != 0
			},
		},
		"markSkipped": {
			write: func(e *Enricher, ctx context.Context, tr *manifest.Track) {
				e.markSkipped(ctx, tr, skipReasonNoMBMatch, "no acceptable release candidate")
			},
			written: func(t *testing.T, e *Enricher, store *manifest.Store, rec *loggingtest.Recorder, path string) bool {
				return stamped(t, e, store, rec, path) || e.skipped.Load() != 0 || len(e.SkipReasons()) != 0 ||
					len(rec.Lines("enrichment skipped")) != 0
			},
		},
		"recordAcousticTagVeto": {
			write: func(e *Enricher, ctx context.Context, tr *manifest.Track) { e.recordAcousticTagVeto(ctx, tr) },
			written: func(t *testing.T, _ *Enricher, store *manifest.Store, _ *loggingtest.Recorder, path string) bool {
				vetoes, err := store.FreshAcoustIDTagVetoes(context.Background(), 0)
				if err != nil {
					t.Fatal(err)
				}
				_, ok := vetoes[path]
				return ok
			},
		},
	}
	for name, w := range writers {
		for _, on := range []bool{false, true} {
			state := map[bool]string{false: "off", true: "on"}[on]
			t.Run(name+" with the switch "+state, func(t *testing.T) {
				track := switchTracks[1]
				e, store := gateEnricher(t, newSwitchUpstreams(t), nil, track)
				rows, err := store.UnenrichedTracks(context.Background(), 10)
				if err != nil || len(rows) != 1 {
					t.Fatalf("the row waiting for the enricher: %d, %v", len(rows), err)
				}
				s := newGateSwitch()
				ctx, release := enrichgate.CancelWhenOff(context.Background(), s.get, time.Hour)
				defer release()
				s.on.Store(on)
				rec := loggingtest.Record(t)
				w.write(e, ctx, &rows[0])
				if got := w.written(t, e, store, rec, track.Path); got != on {
					t.Errorf("%s with the switch %s wrote %v, want %v", name, state, got, on)
				}
				if refused := enrichgate.Refusal(ctx) == enrichgate.ErrOff; refused == on {
					t.Errorf("%s with the switch %s: the pass's context ended by the switch is %v, want %v", name, state, refused, !on)
				}
			})
		}
	}
}

// flipAtEOF is a transport under the gate that turns its switch off when the
// body of the first answer whose path starts with prefix has been read to its
// end: after the gate took that answer, before the enricher's next send.
type flipAtEOF struct {
	base   http.RoundTripper
	prefix string
	off    func()
	once   sync.Once
}

func (f *flipAtEOF) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := f.base.RoundTrip(req)
	if err != nil || !strings.HasPrefix(req.URL.Path, f.prefix) {
		return resp, err
	}
	resp.Body = &eofHook{ReadCloser: resp.Body, at: func() { f.once.Do(f.off) }}
	return resp, nil
}

type eofHook struct {
	io.ReadCloser
	at func()
}

func (h *eofHook) Read(p []byte) (int, error) {
	n, err := h.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		h.at()
	}
	return n, err
}

// TestARequestAfterTheFlipIsRefusedAtItsSend: the switch goes off just after
// the first rung of the artist ladder was answered (no match) and taken, and
// the second rung, which would follow it, is refused at its send by the gate:
// the switch's context asks once an hour, so no tick refuses it. MusicBrainz
// sees the first rung alone, no rung runs past the refusal, Deezer is asked
// nothing, the artist is cached as nothing, and the row stays at
// enriched_at = 0 (backlog B617).
func TestARequestAfterTheFlipIsRefusedAtItsSend(t *testing.T) {
	ups := newSwitchUpstreams(t)
	ups.mb = newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/artist") && strings.Contains(r.URL.Query().Get("query"), "Beta") {
			_, _ = io.WriteString(w, `{"artists":[]}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/artist") {
			fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":"Alpha"}]}`, switchArtistMBID)
			return
		}
		http.NotFound(w, r)
	})
	s := newGateSwitch()
	flip := &flipAtEOF{base: http.DefaultTransport, prefix: "/artist", off: func() { s.on.Store(false) }}
	// The release is embedded: the cover comes first, then the artist
	// ladder, whose rungs are "Alpha; Beta" and "Alpha".
	track := manifest.Track{Path: "Alpha/Album/01.flac", Size: 1, Artist: "Alpha; Beta", Album: "Album", MusicBrainzAlbumID: switchReleaseMBID}
	if rungs := buildArtistLadder(track.Artist, ""); len(rungs) != 2 {
		t.Fatalf("the fixture's artist builds rungs %q, want two", rungs)
	}
	e, store := gateEnricher(t, ups, &http.Client{Timeout: 10 * time.Second, Transport: flip}, track)
	e.WithEnabled(s.get)
	rec := loggingtest.Record(t)
	stop := runEnricher(t, e)
	parkedAfterTheFlip(t, rec, s)
	stop()

	var searches []string
	for _, r := range ups.mb.requests() {
		if strings.HasPrefix(r, "GET /artist") {
			searches = append(searches, r)
		}
	}
	if len(searches) != 1 || !strings.Contains(searches[0], "Beta") {
		t.Errorf("MusicBrainz was asked for %d artist(s), want the first rung alone:\n%s", len(searches), strings.Join(searches, "\n"))
	}
	if got := ups.deezer.requests(); len(got) != 0 {
		t.Errorf("Deezer was asked after the flip:\n%s", strings.Join(got, "\n"))
	}
	if _, cached := e.artistCache.Get(artistCacheKey(track.Artist, track.AlbumArtist)); cached {
		t.Error("the artist was cached by a pass the switch stopped")
	}
	requireNothingKept(t, e, store, rec, track.Path)
}

// TestADeezerFlipCancelsDeezersRequestAndNotMusicBrainzs: on one batch's
// context, a MusicBrainz search and a Deezer search are both held at their
// fakes when Deezer alone is switched off. The Deezer search's own context
// (deezerCall) cuts it off, as errDeezerOff, which is not cached as a miss,
// and the image is never fetched; the MusicBrainz search runs on and answers
// once released, since enrich.enabled is on (backlog B617).
func TestADeezerFlipCancelsDeezersRequestAndNotMusicBrainzs(t *testing.T) {
	ups := newSwitchUpstreams(t)
	e, _, artworkDir := switchEnricher(t, ups)
	e.switchEvery = time.Millisecond
	enrichment, deezer := newGateSwitch(), newGateSwitch()
	e.WithEnabled(enrichment.get).WithDeezerEnabled(deezer.get)
	batch, releaseBatch := enrichgate.CancelWhenOff(context.Background(), e.enabled, e.switchEvery)
	defer releaseBatch()

	mbHeld, mbRelease := make(chan struct{}), make(chan struct{})
	mbCancelled := make(chan struct{})
	ups.mb.setHold(func(r *http.Request) {
		close(mbHeld)
		select {
		case <-mbRelease:
		case <-r.Context().Done():
			close(mbCancelled)
		}
	})
	deezerHeld, deezerCancelled := make(chan struct{}), make(chan struct{})
	ups.deezer.setHold(func(r *http.Request) {
		close(deezerHeld)
		select {
		case <-r.Context().Done():
			close(deezerCancelled)
		case <-time.After(10 * time.Second):
		}
	})

	type answer struct {
		res *SearchResult
		err error
	}
	mbAnswer := make(chan answer, 1)
	go func() {
		res, err := e.mb.SearchArtist(batch, "Artist")
		mbAnswer <- answer{res, err}
	}()
	deezerAnswer := make(chan error, 1)
	go func() {
		_, err := e.ensureArtistImageCached(batch, switchArtistMBID, "Artist")
		deezerAnswer <- err
	}()
	for _, held := range []chan struct{}{mbHeld, deezerHeld} {
		select {
		case <-held:
		case <-time.After(10 * time.Second):
			t.Fatal("a search was never sent")
		}
	}

	deezer.on.Store(false)
	select {
	case <-deezerCancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("the Deezer search held at the switch-off was not cut off")
	}
	if err := <-deezerAnswer; !errors.Is(err, errDeezerOff) {
		t.Errorf("the Deezer fetch answered %v, want errDeezerOff", err)
	}
	// The batch's switch has been asked meanwhile, once a millisecond, and
	// the MusicBrainz search is still held: no Deezer flip reaches it.
	from := enrichment.asked.Load()
	waitFor(t, func() bool { return enrichment.asked.Load() >= from+5 }, "five asks of the batch's switch")
	select {
	case <-mbCancelled:
		t.Fatal("the Deezer flip cut off the MusicBrainz search")
	default:
	}
	close(mbRelease)
	if got := <-mbAnswer; got.err != nil || got.res == nil || got.res.MBID != switchArtistMBID {
		t.Errorf("the MusicBrainz search answered %+v, %v; want the artist", got.res, got.err)
	}
	if batch.Err() != nil {
		t.Error("the Deezer flip ended the batch's context")
	}
	if reqs := ups.deezer.requests(); len(reqs) != 1 {
		t.Errorf("Deezer was asked %v, want the held search alone", reqs)
	}
	if e.deezerNegCache.Has(switchArtistMBID) {
		t.Error("the cut-off search was negative-cached")
	}
	if fileExists(ArtistImagePath(artworkDir, switchArtistMBID)) {
		t.Error("a portrait was cached after Deezer was switched off")
	}
}
