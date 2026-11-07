package enrich

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/enrichgate"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// An error answer, completed after the flip (backlog B617, round 4): a
// client's every return once its answer began asks its switches again, an
// error status's as much as a 200's, since an error body read after a
// switch-off is no more an answer than a decoded one. Here the answer's body
// waits at the barrier (bodyBarrier) just after the header admission, the
// switch turns off there, and the body follows; every switch is asked once
// an hour, so no tick can refuse it.

// barrierClient is a client on a transport of its own whose answers turn s
// off as their body is first read or closed, where held says so.
func barrierClient(t *testing.T, s *flipSwitch, held func(r *http.Request) bool) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Timeout: 10 * time.Second, Transport: bodyBarrier{base: tr, before: func(r *http.Request) {
		if held(r) {
			s.on.Store(false)
		}
	}}}
}

// errorCall is one client's call, answered with an error status.
type errorCall struct {
	name   string
	status int
	// call builds the client against base over hc and on, and makes its
	// call on ctx: miss is whether the call came back as the upstream's
	// error, or its miss, rather than as no answer.
	call func(t *testing.T, base string, hc *http.Client, on func() bool) func(ctx context.Context) (miss bool, err error)
}

var errorCalls = []errorCall{
	{name: "MusicBrainz's artist search", status: http.StatusBadRequest,
		call: func(_ *testing.T, base string, hc *http.Client, on func() bool) func(context.Context) (bool, error) {
			c := NewMusicBrainzClient(base, "t", hc, enrichgate.Enrichment(on).Every(time.Hour))
			return func(ctx context.Context) (bool, error) {
				res, err := c.SearchArtist(ctx, "Artist")
				return res == nil && !errors.Is(err, enrichgate.ErrOff), err
			}
		}},
	{name: "MusicBrainz's release-group lookup", status: http.StatusNotFound,
		call: func(_ *testing.T, base string, hc *http.Client, on func() bool) func(context.Context) (bool, error) {
			c := NewMusicBrainzClient(base, "t", hc, enrichgate.Enrichment(on).Every(time.Hour))
			return func(ctx context.Context) (bool, error) {
				_, err := c.ReleaseGroupMBID(ctx, clientMBID)
				return !errors.Is(err, enrichgate.ErrOff), err
			}
		}},
	{name: "the Cover Art Archive's front cover", status: http.StatusNotFound,
		call: func(_ *testing.T, base string, hc *http.Client, on func() bool) func(context.Context) (bool, error) {
			c := NewCoverArtClient(base, "t", hc, enrichgate.Enrichment(on).Every(time.Hour))
			return func(ctx context.Context) (bool, error) {
				body, err := c.FetchReleaseFrontStream(ctx, clientMBID, 500)
				if body != nil {
					_ = body.Close()
				}
				return !errors.Is(err, enrichgate.ErrOff), err
			}
		}},
	{name: "Deezer's search", status: http.StatusNotFound,
		call: func(_ *testing.T, base string, hc *http.Client, on func() bool) func(context.Context) (bool, error) {
			c := NewDeezerClient(base, "t", hc, enrichgate.Deezer(on, alwaysOn).Every(time.Hour))
			return func(ctx context.Context) (bool, error) {
				_, err := c.SearchArtist(ctx, "Artist")
				return !errors.Is(err, enrichgate.ErrOff), err
			}
		}},
	{name: "Deezer's image", status: http.StatusNotFound,
		call: func(_ *testing.T, base string, hc *http.Client, on func() bool) func(context.Context) (bool, error) {
			c := NewDeezerClient(base, "t", hc, enrichgate.Deezer(on, alwaysOn).Every(time.Hour))
			c.SetAllowedImageHostsForTest([]string{"127.0.0.1"})
			return func(ctx context.Context) (bool, error) {
				_, err := c.FetchImage(ctx, base+"/picture.jpg")
				return !errors.Is(err, enrichgate.ErrOff), err
			}
		}},
	{name: "iTunes' search", status: http.StatusBadRequest,
		call: func(_ *testing.T, base string, hc *http.Client, on func() bool) func(context.Context) (bool, error) {
			c := NewITunesClient(base, "t", hc, enrichgate.Enrichment(on).Every(time.Hour))
			return func(ctx context.Context) (bool, error) {
				_, err := c.SearchAlbum(ctx, "Artist", "Album")
				return !errors.Is(err, enrichgate.ErrOff), err
			}
		}},
	{name: "iTunes' artwork", status: http.StatusNotFound,
		call: func(_ *testing.T, base string, hc *http.Client, on func() bool) func(context.Context) (bool, error) {
			c := NewITunesClient(base, "t", hc, enrichgate.Enrichment(on).Every(time.Hour))
			return func(ctx context.Context) (bool, error) {
				body, err := c.FetchArtwork(ctx, &ITunesAlbum{CollectionID: 1, CollectionName: "Album",
					ArtworkURL100: base + "/x/100x100bb.jpg"})
				if body != nil {
					_ = body.Close()
				}
				return !errors.Is(err, enrichgate.ErrOff), err
			}
		}},
	{name: "Atlas premium covers", status: http.StatusNotFound,
		call: func(t *testing.T, base string, hc *http.Client, on func() bool) func(context.Context) (bool, error) {
			dir := t.TempDir()
			f := NewAtlasPremiumFetcher(fakeCred{token: "tok", base: base, ok: true}, "t", hc, dir,
				enrichgate.Enrichment(on).Every(time.Hour))
			return func(ctx context.Context) (bool, error) {
				got, err := f.RefetchPremium(ctx, ArtworkCachePath(dir, clientMBID, 500), clientMBID, 500)
				// (false, nil) is "no cover at Atlas", which settles the
				// pending cover: the fetcher's miss.
				return !got && !errors.Is(err, enrichgate.ErrOff), err
			}
		}},
}

// TestEveryClientsErrorAnswerCompletedAfterTheFlipIsNoAnswer: each client's
// call, answered with an error status whose body the client reads or closes
// after the switch turned off, comes back as ErrOff, never the status's error
// or the upstream's miss, on a context that carries no switch.
func TestEveryClientsErrorAnswerCompletedAfterTheFlipIsNoAnswer(t *testing.T) {
	for _, c := range errorCalls {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, "an error answer")
			}))
			t.Cleanup(srv.Close)
			s := newFlipSwitch(true)
			hc := barrierClient(t, s, func(*http.Request) bool { return true })
			miss, err := c.call(t, srv.URL, hc, s.get)(context.Background())
			if s.on.Load() {
				t.Fatal("the answer's body was never read or closed: the case tests nothing")
			}
			if !errors.Is(err, enrichgate.ErrOff) || miss {
				t.Errorf("the call = %v (taken as the upstream's: %v); want ErrOff", err, miss)
			}
		})
	}
}

// TestAPremiumCoverCompletedAfterTheFlipCountsNoCover: the premium fetch's
// TryCache, which answers only whether a cover was cached, so an error
// status reads false whatever its switch says, asks it again at its return
// after a 200 too: a cover whose body completed after the switch-off counts
// as none, on a context that carries no switch. What it wrote stays.
func TestAPremiumCoverCompletedAfterTheFlipCountsNoCover(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(premiumJPEG)
	}))
	t.Cleanup(srv.Close)
	s := newFlipSwitch(true)
	hc := barrierClient(t, s, func(*http.Request) bool { return true })
	dir := t.TempDir()
	f := NewAtlasPremiumFetcher(fakeCred{token: "tok", base: srv.URL, ok: true}, "t", hc, dir,
		enrichgate.Enrichment(s.get).Every(time.Hour))
	path := ArtworkCachePath(dir, clientMBID, 500)
	cached := f.TryCache(context.Background(), path, clientMBID, 500)
	if s.on.Load() {
		t.Fatal("the answer's body was never read: the case tests nothing")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the cover was not written, so the return asked nothing a written cover tests: %v", err)
	}
	if cached {
		t.Error("a cover whose answer completed after the switch-off counted as cached")
	}
}

// TestAnErrorAnswerCompletedAfterTheFlipFillsNoNegativeCache: through the
// callers that keep a MusicBrainz failure, an error status whose body
// completes after the switch-off fills no cache, on a pass's context and on
// one that carries no switch; and once the switch is on again, the album
// whose search was refused is asked again and found, rather than stamped
// against a cached miss.
func TestAnErrorAnswerCompletedAfterTheFlipFillsNoNegativeCache(t *testing.T) {
	const releaseMBID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	local := "local-" + strings.Repeat("c", 64)
	search := func(r *http.Request) bool {
		return strings.HasPrefix(r.URL.Path, "/release/") && r.URL.Query().Get("query") != ""
	}
	artist := func(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/artist") }
	lookup := func(r *http.Request) bool { return r.URL.Path == "/release/"+releaseMBID }
	for _, c := range []struct {
		name   string
		status int
		held   func(r *http.Request) bool
		track  manifest.Track
		write  func(e *Enricher, ctx context.Context, tr *manifest.Track) error
		// wantErr says the caller returns the refusal; thenOn, that the
		// track is enriched again once the switch is on.
		wantErr, thenOn bool
		cached          func(e *Enricher, tr manifest.Track) bool
	}{
		{
			name: "enrichOne, a 400 to the release search", status: http.StatusBadRequest, held: search,
			track: manifest.Track{Artist: "Artist", Album: "Album"},
			write: func(e *Enricher, ctx context.Context, tr *manifest.Track) error {
				e.enrichOne(ctx, tr)
				return nil
			},
			thenOn: true,
			cached: func(e *Enricher, tr manifest.Track) bool {
				_, ok := e.albumCache.Get(releaseCacheKey(tr.Artist, tr.AlbumArtist, tr.Album))
				return ok
			},
		},
		{
			name: "resolveArtist, a 400 to the artist search", status: http.StatusBadRequest, held: artist,
			track:   manifest.Track{Artist: "Artist", Album: "Album", MusicBrainzAlbumID: releaseMBID, ArtworkMBID: local},
			write:   func(e *Enricher, ctx context.Context, tr *manifest.Track) error { return e.resolveArtist(ctx, tr) },
			wantErr: true,
			cached: func(e *Enricher, tr manifest.Track) bool {
				_, ok := e.artistCache.Get(artistCacheKey(tr.Artist, tr.AlbumArtist))
				return ok
			},
		},
		{
			name: "resolveReleaseGroupMBID, a 404 for the release", status: http.StatusNotFound, held: lookup,
			track: manifest.Track{Artist: "Artist", Album: "Album", MusicBrainzAlbumID: releaseMBID},
			write: func(e *Enricher, ctx context.Context, _ *manifest.Track) error {
				_, err := e.resolveReleaseGroupMBID(ctx, releaseMBID, "")
				return err
			},
			wantErr: true,
			cached: func(e *Enricher, _ manifest.Track) bool {
				_, ok := e.releaseGroupCache.Get(releaseMBID)
				return ok
			},
		},
	} {
		for _, gated := range []bool{true, false} {
			kind := "a context that carries no switch"
			if gated {
				kind = "a pass's context"
			}
			t.Run(c.name+", "+kind, func(t *testing.T) {
				s := newFlipSwitch(true)
				var failing atomic.Bool
				failing.Store(true)
				mb := newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
					switch {
					case failing.Load() && c.held(r):
						w.WriteHeader(c.status)
						_, _ = io.WriteString(w, "an error answer")
					case artist(r):
						fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":"Artist"}]}`, clientMBID)
					case search(r):
						fmt.Fprintf(w, `{"releases":[{"id":%q,"score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`, releaseMBID)
					default:
						http.NotFound(w, r)
					}
				})
				caa := newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
					http.NotFound(w, r)
				})
				deezer := newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, `{"data":[]}`)
				})

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
				sw := enrichgate.Enrichment(s.get).Every(time.Hour)
				hc := barrierClient(t, s, func(r *http.Request) bool { return failing.Load() && c.held(r) })
				dz := NewDeezerClient(deezer.srv.URL, "t", nil, enrichgate.Deezer(s.get, alwaysOn).Every(time.Hour))
				e := NewEnricher(store, NewMusicBrainzClient(mb.srv.URL, "t", hc, sw),
					NewCoverArtClient(caa.srv.URL, "t", nil, sw), dz, artworkDir)
				tuneForTest(e)
				e.switchEvery = time.Hour
				e.WithEnabled(s.get)
				tr := c.track
				tr.Path, tr.Size, tr.ModTime = "Artist/Album/01.flac", 1, time.Now()
				if err := store.UpsertTrack(context.Background(), &tr); err != nil {
					t.Fatal(err)
				}

				pass := func() (context.Context, func()) {
					if !gated {
						return context.Background(), func() {}
					}
					ctx, release := enrichgate.CancelWhenOff(context.Background(), e.on, e.switchEvery)
					return ctx, release
				}
				ctx, release := pass()
				err = c.write(e, ctx, &tr)
				release()
				if s.on.Load() {
					t.Fatal("the error answer's body was never read: the case tests nothing")
				}
				if c.cached(e, tr) {
					t.Error("an error answer completed after the switch-off was kept as a miss")
				}
				if c.wantErr && !errors.Is(err, enrichgate.ErrOff) {
					t.Errorf("the caller was told %v, want ErrOff: the error answer was taken", err)
				}
				if !unenrichedPaths(t, store)[tr.Path] {
					t.Error("the track was stamped on an answer completed after the switch-off")
				}
				if !c.thenOn {
					return
				}

				// On again: the album is asked again and found.
				failing.Store(false)
				s.on.Store(true)
				row, err := store.GetTrack(context.Background(), tr.Path)
				if err != nil {
					t.Fatal(err)
				}
				ctx, release = pass()
				e.enrichOne(ctx, row)
				release()
				got, err := store.GetTrack(context.Background(), tr.Path)
				if err != nil {
					t.Fatal(err)
				}
				if unenrichedPaths(t, store)[tr.Path] || got.MusicBrainzAlbumID != releaseMBID {
					t.Errorf("once on, the track's release is %q (still waiting: %v), want %q: it was stamped against the cached miss",
						got.MusicBrainzAlbumID, unenrichedPaths(t, store)[tr.Path], releaseMBID)
				}
			})
		}
	}
}
