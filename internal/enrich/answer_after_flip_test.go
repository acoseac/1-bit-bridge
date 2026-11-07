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
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/enrichgate"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// A completed answer, re-admitted (backlog B617, round 3, point 3): an
// answer whose headers the client's switches admitted, and whose body
// completes after they turn off, is no answer either, so no negative cache
// takes it, whatever context the call carries. Here the answer's body waits
// at a barrier just after the header admission, the switch turns off there,
// and the body follows; every switch is asked once an hour, so no tick can
// refuse it, and only the client's asking once the answer is complete can.

// bodyBarrier is a base transport whose answers call before as their body's
// first Read begins, which the client's answer admission has passed by then.
type bodyBarrier struct {
	base   http.RoundTripper
	before func(r *http.Request)
}

func (b bodyBarrier) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := b.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &barrierBody{ReadCloser: resp.Body, before: func() { b.before(req) }}
	return resp, nil
}

// barrierBody calls before once, as its first Read begins.
type barrierBody struct {
	io.ReadCloser
	once   sync.Once
	before func()
}

func (b *barrierBody) Read(p []byte) (int, error) {
	b.once.Do(b.before)
	return b.ReadCloser.Read(p)
}

// TestAnITunesAnswerCompletedAfterTheFlipIsNoAnswer: iTunes' no-match, its
// body held at the barrier while the switch turns off, comes back as ErrOff,
// never the miss (errNotFound) the artwork chain stamps, on a pass's context
// and on one that carries no switch.
func TestAnITunesAnswerCompletedAfterTheFlipIsNoAnswer(t *testing.T) {
	up := newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"resultCount":0,"results":[]}`)
	})
	for _, gated := range []bool{true, false} {
		kind := "a context that carries no switch"
		if gated {
			kind = "a pass's context"
		}
		t.Run(kind, func(t *testing.T) {
			s := newFlipSwitch(true)
			tr := &http.Transport{}
			t.Cleanup(tr.CloseIdleConnections)
			hc := &http.Client{Timeout: 10 * time.Second, Transport: bodyBarrier{base: tr, before: func(*http.Request) { s.on.Store(false) }}}
			c := NewITunesClient(up.srv.URL, "t", hc, enrichgate.Enrichment(s.get).Every(time.Hour))
			ctx := context.Background()
			if gated {
				var release context.CancelFunc
				ctx, release = enrichgate.CancelWhenOff(ctx, s.get, time.Hour)
				defer release()
			}
			hit, err := c.SearchAlbum(ctx, "Artist", "Album")
			if s.on.Load() {
				t.Fatal("the answer never reached its body: the case tests nothing")
			}
			if hit != nil || !errors.Is(err, enrichgate.ErrOff) || IsNotFound(err) {
				t.Errorf("SearchAlbum = %v, %v; want ErrOff: a no-match completed after the switch-off was taken", hit, err)
			}
		})
	}
}

// TestAnAnswerCompletedAfterTheFlipFillsNoNegativeCache: through each caller
// that keeps a miss, once on a pass's context and once on a context that
// carries no switch, an answer whose body completes after the switch-off
// fills no cache, and the caller is told ErrOff, never the answer's miss or
// a decode error: Deezer's no-match (resolveArtist and
// fetchRecoveredArtistImage, deezerNegCache), MusicBrainz's malformed artist
// answer (resolveArtist, artistCache), its release no-match (enrichOne,
// albumCache) and its release without a release group
// (resolveReleaseGroupMBID, releaseGroupCache).
func TestAnAnswerCompletedAfterTheFlipFillsNoNegativeCache(t *testing.T) {
	const releaseMBID = "88888888-8888-4888-8888-888888888888"
	local := "local-" + strings.Repeat("b", 64)
	type answers struct{ mbArtist, mbRelease, mbLookup, deezerSearch string }
	plain := answers{
		mbArtist:     fmt.Sprintf(`{"artists":[{"id":%q,"score":100,"name":"Artist"}]}`, clientMBID),
		mbRelease:    fmt.Sprintf(`{"releases":[{"id":%q,"score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`, releaseMBID),
		mbLookup:     `{"release-group":{"id":"99999999-9999-4999-8999-999999999999","title":"Album","primary-type":"Album"}}`,
		deezerSearch: `{"data":[]}`,
	}
	for _, c := range []struct {
		name    string
		answers func(a answers) answers
		// held picks the answer whose body waits at the barrier.
		held  func(svc string, r *http.Request) bool
		track manifest.Track
		// write is the caller that keeps a miss; wantErr, that it returns
		// the refusal.
		write   func(e *Enricher, ctx context.Context, tr *manifest.Track) error
		wantErr bool
		cached  func(e *Enricher, tr manifest.Track) bool
	}{
		{
			name:    "resolveArtist, Deezer's no-match",
			answers: func(a answers) answers { return a },
			held: func(svc string, r *http.Request) bool {
				return svc == "deezer" && strings.HasPrefix(r.URL.Path, "/search/artist")
			},
			track:   manifest.Track{Artist: "Artist", Album: "Album", MusicBrainzAlbumID: releaseMBID, ArtworkMBID: local},
			write:   func(e *Enricher, ctx context.Context, tr *manifest.Track) error { return e.resolveArtist(ctx, tr) },
			wantErr: true,
			cached:  func(e *Enricher, _ manifest.Track) bool { return e.deezerNegCache.Has(clientMBID) },
		},
		{
			name:    "fetchRecoveredArtistImage, Deezer's no-match",
			answers: func(a answers) answers { return a },
			held: func(svc string, r *http.Request) bool {
				return svc == "deezer" && strings.HasPrefix(r.URL.Path, "/search/artist")
			},
			track: manifest.Track{Artist: "Artist", Album: "Album", ArtistMBID: clientMBID},
			write: func(e *Enricher, ctx context.Context, tr *manifest.Track) error {
				e.fetchRecoveredArtistImage(ctx, tr, AcousticMatch{ArtistMBID: clientMBID, ArtistName: "Artist"})
				return nil
			},
			cached: func(e *Enricher, _ manifest.Track) bool { return e.deezerNegCache.Has(clientMBID) },
		},
		{
			name: "resolveArtist, MusicBrainz's malformed answer",
			answers: func(a answers) answers {
				a.mbArtist = `{"artists":[{"id":`
				return a
			},
			held: func(svc string, r *http.Request) bool {
				return svc == "mb" && strings.HasPrefix(r.URL.Path, "/artist")
			},
			track:   manifest.Track{Artist: "Artist", Album: "Album", MusicBrainzAlbumID: releaseMBID, ArtworkMBID: local},
			write:   func(e *Enricher, ctx context.Context, tr *manifest.Track) error { return e.resolveArtist(ctx, tr) },
			wantErr: true,
			cached: func(e *Enricher, tr manifest.Track) bool {
				_, ok := e.artistCache.Get(artistCacheKey(tr.Artist, tr.AlbumArtist))
				return ok
			},
		},
		{
			name: "enrichOne, MusicBrainz's release no-match",
			answers: func(a answers) answers {
				a.mbRelease = `{"releases":[]}`
				return a
			},
			held: func(svc string, r *http.Request) bool {
				return svc == "mb" && strings.HasPrefix(r.URL.Path, "/release/") && r.URL.Query().Get("query") != ""
			},
			track: manifest.Track{Artist: "Artist", Album: "Album"},
			write: func(e *Enricher, ctx context.Context, tr *manifest.Track) error {
				e.enrichOne(ctx, tr)
				return nil
			},
			cached: func(e *Enricher, tr manifest.Track) bool {
				_, ok := e.albumCache.Get(releaseCacheKey(tr.Artist, tr.AlbumArtist, tr.Album))
				return ok
			},
		},
		{
			name: "resolveReleaseGroupMBID, a release with no release group",
			answers: func(a answers) answers {
				a.mbLookup = `{}`
				return a
			},
			held: func(svc string, r *http.Request) bool {
				return svc == "mb" && r.URL.Path == "/release/"+releaseMBID
			},
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
				a := c.answers(plain)
				s := newFlipSwitch(true)
				barrier := func(svc string) *http.Client {
					tr := &http.Transport{}
					t.Cleanup(tr.CloseIdleConnections)
					return &http.Client{Timeout: 10 * time.Second, Transport: bodyBarrier{base: tr, before: func(r *http.Request) {
						if c.held(svc, r) {
							s.on.Store(false)
						}
					}}}
				}
				mb := newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasPrefix(r.URL.Path, "/artist"):
						_, _ = io.WriteString(w, a.mbArtist)
					case strings.HasPrefix(r.URL.Path, "/release/") && r.URL.Query().Get("query") != "":
						_, _ = io.WriteString(w, a.mbRelease)
					case r.URL.Path == "/release/"+releaseMBID:
						_, _ = io.WriteString(w, a.mbLookup)
					default:
						http.NotFound(w, r)
					}
				})
				caa := newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
					http.NotFound(w, r)
				})
				deezer := newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/search/artist") {
						_, _ = io.WriteString(w, a.deezerSearch)
						return
					}
					http.NotFound(w, r)
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
				// Every switch is asked once an hour: no tick refuses anything.
				sw := enrichgate.Enrichment(s.get).Every(time.Hour)
				dz := NewDeezerClient(deezer.srv.URL, "t", barrier("deezer"), enrichgate.Deezer(s.get, alwaysOn).Every(time.Hour))
				dz.SetAllowedImageHostsForTest([]string{"127.0.0.1"})
				e := NewEnricher(store, NewMusicBrainzClient(mb.srv.URL, "t", barrier("mb"), sw),
					NewCoverArtClient(caa.srv.URL, "t", nil, sw), dz, artworkDir)
				tuneForTest(e)
				e.switchEvery = time.Hour
				e.WithEnabled(s.get)
				tr := c.track
				tr.Path, tr.Size, tr.ModTime = "Artist/Album/01.flac", 1, time.Now()
				if err := store.UpsertTrack(context.Background(), &tr); err != nil {
					t.Fatal(err)
				}

				ctx := context.Background()
				if gated {
					var release context.CancelFunc
					ctx, release = enrichgate.CancelWhenOff(ctx, e.on, e.switchEvery)
					defer release()
				}
				err = c.write(e, ctx, &tr)
				if s.on.Load() {
					t.Fatal("the answer the case holds never reached its body: the case tests nothing")
				}
				if c.cached(e, tr) {
					t.Error("an answer completed after the switch-off was kept as a miss")
				}
				if c.wantErr && !errors.Is(err, enrichgate.ErrOff) {
					t.Errorf("the caller was told %v, want ErrOff: the answer, or its decode error, was taken", err)
				}
				if !unenrichedPaths(t, store)[tr.Path] {
					t.Error("the track was stamped on an answer completed after the switch-off")
				}
			})
		}
	}
}
