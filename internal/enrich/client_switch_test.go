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
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// Each client's own switches (backlog B617, round 3): a client is built with
// enrich.enabled (Deezer's with enrich.deezerEnabled beside it) and asks
// them whatever context a call carries, so a caller whose context carries
// no switch sends nothing while they are off, and a request on the wire when
// they turn off is cut off with their refusal within their interval, never
// taken for an upstream's miss.

// clientMBID is the release, and the artist, a client here is asked about.
const clientMBID = "55555555-5555-4555-8555-555555555555"

// clientCall is one of the enricher's clients and the one call a test makes
// of it.
type clientCall struct {
	name string
	// answer is its upstream's answer to the call.
	answer http.HandlerFunc
	// build builds the client against base over on, asked every `every`
	// (0 is enrichgate.Interval), counting each answer it notes in noted,
	// and returns its call: miss is whether the call came back as the
	// upstream's miss, the outcome a caller records.
	build func(t *testing.T, base string, on func() bool, every time.Duration, noted *atomic.Int64) func(context.Context) (miss bool, err error)
}

var clientCalls = []clientCall{
	{
		name: "MusicBrainz",
		answer: func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":"Artist"}]}`, clientMBID)
		},
		build: func(_ *testing.T, base string, on func() bool, every time.Duration, noted *atomic.Int64) func(context.Context) (bool, error) {
			c := NewMusicBrainzClient(base, "t", nil, enrichgate.Enrichment(on).Every(every))
			c.noteAnswered = func() { noted.Add(1) }
			return func(ctx context.Context) (bool, error) {
				res, err := c.SearchArtist(ctx, "Artist")
				return (err == nil && res == nil) || IsNotFound(err), err
			}
		},
	},
	{
		name: "the Cover Art Archive",
		answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(switchJPEG)
		},
		build: func(_ *testing.T, base string, on func() bool, every time.Duration, noted *atomic.Int64) func(context.Context) (bool, error) {
			c := NewCoverArtClient(base, "t", nil, enrichgate.Enrichment(on).Every(every))
			c.noteAnswered = func() { noted.Add(1) }
			return func(ctx context.Context) (bool, error) {
				body, err := c.FetchReleaseFrontStream(ctx, clientMBID, 500)
				if err == nil {
					_, err = io.ReadAll(body)
					_ = body.Close()
				}
				return IsNotFound(err), err
			}
		},
	},
	{
		name: "Deezer",
		answer: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"data":[{"id":1,"name":"Artist","picture_xl":"https://e-cdns-images.dzcdn.net/x.jpg"}]}`)
		},
		build: func(_ *testing.T, base string, on func() bool, every time.Duration, noted *atomic.Int64) func(context.Context) (bool, error) {
			// Deezer's own switch stays on: enrich.enabled is what turns.
			c := NewDeezerClient(base, "t", nil, enrichgate.Deezer(on, alwaysOn).Every(every))
			c.noteAnswered = func() { noted.Add(1) }
			return func(ctx context.Context) (bool, error) {
				u, err := c.SearchArtist(ctx, "Artist")
				return (err == nil && u == "") || IsNotFound(err), err
			}
		},
	},
	{
		name: "iTunes",
		answer: func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"resultCount":1,"results":[{"collectionId":1,"collectionName":"Album",`+
				`"artworkUrl100":"https://is1-ssl.mzstatic.com/x/100x100bb.jpg"}]}`)
		},
		build: func(_ *testing.T, base string, on func() bool, every time.Duration, noted *atomic.Int64) func(context.Context) (bool, error) {
			c := NewITunesClient(base, "t", nil, enrichgate.Enrichment(on).Every(every))
			c.noteAnswered = func() { noted.Add(1) }
			return func(ctx context.Context) (bool, error) {
				hit, err := c.SearchAlbum(ctx, "Artist", "Album")
				return (err == nil && hit == nil) || IsNotFound(err), err
			}
		},
	},
	{
		name: "Atlas premium covers",
		answer: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Atlas-Asset-Source", "qobuz")
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(switchJPEG)
		},
		build: func(t *testing.T, base string, on func() bool, every time.Duration, _ *atomic.Int64) func(context.Context) (bool, error) {
			dir := t.TempDir()
			f := NewAtlasPremiumFetcher(fakeCred{token: "tok", base: base, ok: true}, "t", nil, dir, enrichgate.Enrichment(on).Every(every))
			return func(ctx context.Context) (bool, error) {
				got, err := f.RefetchPremium(ctx, ArtworkCachePath(dir, clientMBID, 500), clientMBID, 500)
				// "Not premium yet" counts an attempt against the cover:
				// this fetcher's miss.
				return err == nil && !got, err
			}
		},
	},
}

// TestEveryClientRefusesABareContextWhileItsSwitchIsOff: with its switch off
// a client sends nothing on a context that carries no switch, and answers
// ErrOff (never Deezer's refusal, nor a miss), noting no answer; switched
// on, the same call is answered.
func TestEveryClientRefusesABareContextWhileItsSwitchIsOff(t *testing.T) {
	for _, c := range clientCalls {
		t.Run(c.name, func(t *testing.T) {
			var asked atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				asked.Add(1)
				c.answer(w, r)
			}))
			t.Cleanup(srv.Close)
			var noted atomic.Int64
			miss, err := c.build(t, srv.URL, func() bool { return false }, 0, &noted)(context.Background())
			if !errors.Is(err, enrichgate.ErrOff) || errors.Is(err, enrichgate.ErrDeezerOff) {
				t.Errorf("err = %v, want ErrOff", err)
			}
			if miss {
				t.Error("the refusal came back as the upstream's miss")
			}
			if n := asked.Load(); n != 0 {
				t.Errorf("the upstream was asked %d time(s) with the switch off", n)
			}
			if n := noted.Load(); n != 0 {
				t.Errorf("the client noted %d answer(s) it was not given", n)
			}
			// Switched on, the same call is answered: what the refusal
			// stood in for was an answer.
			if miss, err := c.build(t, srv.URL, alwaysOn, 0, &noted)(context.Background()); err != nil || miss {
				t.Errorf("switched on: miss %v, err %v; want the upstream's answer", miss, err)
			}
		})
	}
}

// TestEveryClientCutsOffABareContextsRequestWhenItsSwitchTurnsOff: a request
// on a context that carries no switch, waiting on an upstream that holds it,
// is cut off once the client's switch turns off, within the interval its
// switches are asked at in production (enrichgate.Interval, a second; the
// bound allows a second more for a loaded host), with ErrOff: no answer, so
// no miss, and none noted.
func TestEveryClientCutsOffABareContextsRequestWhenItsSwitchTurnsOff(t *testing.T) {
	for _, c := range clientCalls {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newFlipSwitch(true)
			holding := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				select {
				case holding <- struct{}{}:
				default:
				}
				// Held until the client gives up, then nothing is answered
				// (build-ci-tests.md, B526).
				<-r.Context().Done()
				panic(http.ErrAbortHandler)
			}))
			t.Cleanup(srv.Close)
			var noted atomic.Int64
			call := c.build(t, srv.URL, s.get, 0, &noted)

			flipped := make(chan time.Time, 1)
			done := make(chan struct{})
			defer close(done)
			go func() {
				select {
				case <-holding:
				case <-done:
					return
				}
				s.on.Store(false)
				flipped <- time.Now()
			}()
			miss, err := call(context.Background())
			var after time.Duration
			select {
			case at := <-flipped:
				after = time.Since(at)
			default:
				t.Fatalf("the call returned (miss %v, err %v) before the upstream held it", miss, err)
			}
			if !errors.Is(err, enrichgate.ErrOff) {
				t.Errorf("err = %v, want ErrOff", err)
			}
			if after > 2*enrichgate.Interval {
				t.Errorf("cut off %v after the switch-off, past its interval (%v)", after, enrichgate.Interval)
			}
			if miss {
				t.Error("the cut came back as the upstream's miss")
			}
			if n := noted.Load(); n != 0 {
				t.Errorf("the client noted %d answer(s) it was not given", n)
			}
		})
	}
}

// flipSwitch is a switch a test turns.
type flipSwitch struct{ on atomic.Bool }

func newFlipSwitch(on bool) *flipSwitch {
	s := &flipSwitch{}
	s.on.Store(on)
	return s
}

func (s *flipSwitch) get() bool { return s.on.Load() }

// TestARefusalWithABareContextRecordsNoMiss: the enricher's callers read a
// client's refusal from its error (enrichgate.Refused), not only from their
// context, so a track enriched on a context that carries no switch, whose
// request the clients' own switches cut off, records nothing: no cache
// entry, no stamp, no count, no line, at each request an enricher makes.
// The enricher's own switch stays on here, so the clients' alone are tested.
func TestARefusalWithABareContextRecordsNoMiss(t *testing.T) {
	const (
		releaseMBID = "66666666-6666-4666-8666-666666666666"
		groupMBID   = "77777777-7777-4777-8777-777777777777"
	)
	local := "local-" + strings.Repeat("a", 64)
	for _, c := range []struct {
		name string
		// track is the row enriched; held picks the request the fakes hold.
		track manifest.Track
		held  func(service string, r *http.Request) bool
		// cached reports whether the enricher cached a miss for it.
		cached func(e *Enricher, tr manifest.Track) bool
		// caaMisses has the Cover Art Archive answer the release with 404.
		caaMisses bool
	}{
		{
			name:  "the artist search",
			track: manifest.Track{Artist: "Artist", Album: "Album", MusicBrainzAlbumID: releaseMBID, ArtworkMBID: local},
			held:  func(svc string, r *http.Request) bool { return svc == "mb" && strings.HasPrefix(r.URL.Path, "/artist") },
			cached: func(e *Enricher, tr manifest.Track) bool {
				_, ok := e.artistCache.Get(artistCacheKey(tr.Artist, tr.AlbumArtist))
				return ok
			},
		},
		{
			name:  "the release search",
			track: manifest.Track{Artist: "Artist", Album: "Album"},
			held: func(svc string, r *http.Request) bool {
				return svc == "mb" && strings.HasPrefix(r.URL.Path, "/release/") && r.URL.Query().Get("query") != ""
			},
			cached: func(e *Enricher, tr manifest.Track) bool {
				_, ok := e.albumCache.Get(releaseCacheKey(tr.Artist, tr.AlbumArtist, tr.Album))
				return ok
			},
		},
		{
			name:      "the release-group lookup",
			track:     manifest.Track{Artist: "Artist", Album: "Album", MusicBrainzAlbumID: releaseMBID},
			caaMisses: true,
			held: func(svc string, r *http.Request) bool {
				return svc == "mb" && r.URL.Path == "/release/"+releaseMBID && r.URL.Query().Get("query") == ""
			},
			cached: func(e *Enricher, _ manifest.Track) bool {
				_, ok := e.releaseGroupCache.Get(releaseMBID)
				return ok
			},
		},
		{
			name:   "the cover",
			track:  manifest.Track{Artist: "Artist", Album: "Album", MusicBrainzAlbumID: releaseMBID},
			held:   func(svc string, r *http.Request) bool { return svc == "caa" },
			cached: func(*Enricher, manifest.Track) bool { return false },
		},
		{
			name:  "the portrait",
			track: manifest.Track{Artist: "Artist", Album: "Album", MusicBrainzAlbumID: releaseMBID, ArtworkMBID: local},
			held: func(svc string, r *http.Request) bool {
				return svc == "deezer" && strings.HasPrefix(r.URL.Path, "/search/artist")
			},
			cached: func(e *Enricher, _ manifest.Track) bool { return e.deezerNegCache.Len() != 0 },
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newFlipSwitch(true)
			holding := make(chan struct{}, 1)
			hold := func(svc string) func(r *http.Request) {
				return func(r *http.Request) {
					if !c.held(svc, r) {
						return
					}
					select {
					case holding <- struct{}{}:
					default:
					}
					<-r.Context().Done()
				}
			}
			mb := newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasPrefix(r.URL.Path, "/artist"):
					fmt.Fprintf(w, `{"artists":[{"id":%q,"score":100,"name":"Artist"}]}`, clientMBID)
				case strings.HasPrefix(r.URL.Path, "/release/") && r.URL.Query().Get("query") != "":
					fmt.Fprintf(w, `{"releases":[{"id":%q,"score":100,"title":"Album","artist-credit":[{"name":"Artist"}]}]}`, releaseMBID)
				case r.URL.Path == "/release/"+releaseMBID:
					fmt.Fprintf(w, `{"release-group":{"id":%q,"title":"Album","primary-type":"Album"}}`, groupMBID)
				default:
					http.NotFound(w, r)
				}
			})
			caa := newCountingUpstream(t, func(_ *countingUpstream, w http.ResponseWriter, r *http.Request) {
				if c.caaMisses && strings.HasPrefix(r.URL.Path, "/release/") {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "image/jpeg")
				_, _ = w.Write(switchJPEG)
			})
			deezer := newCountingUpstream(t, func(u *countingUpstream, w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/search/artist") {
					fmt.Fprintf(w, `{"data":[{"id":1,"name":"Artist","picture_xl":%q}]}`, u.srv.URL+"/picture.jpg")
					return
				}
				w.Header().Set("Content-Type", "image/jpeg")
				_, _ = w.Write(switchJPEG)
			})
			mb.setHold(hold("mb"))
			caa.setHold(hold("caa"))
			deezer.setHold(hold("deezer"))

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
			const every = 5 * time.Millisecond
			sw := enrichgate.Enrichment(s.get).Every(every)
			dz := NewDeezerClient(deezer.srv.URL, "t", nil, enrichgate.Deezer(s.get, alwaysOn).Every(every))
			dz.SetAllowedImageHostsForTest([]string{"127.0.0.1"})
			e := NewEnricher(store, NewMusicBrainzClient(mb.srv.URL, "t", nil, sw),
				NewCoverArtClient(caa.srv.URL, "t", nil, sw), dz, artworkDir)
			tuneForTest(e)
			tr := c.track
			tr.Path, tr.Size, tr.ModTime = "Artist/Album/01.flac", 1, time.Now()
			if err := store.UpsertTrack(context.Background(), &tr); err != nil {
				t.Fatal(err)
			}

			done := make(chan struct{})
			defer close(done)
			go func() {
				select {
				case <-holding:
					s.on.Store(false)
				case <-done:
				}
			}()
			rec := loggingtest.Record(t)
			e.enrichOne(context.Background(), &tr)
			if s.on.Load() {
				t.Fatal("the request the case holds was never sent: the case tests nothing")
			}
			requireNothingKept(t, e, store, rec, tr.Path)
			if c.cached(e, tr) {
				t.Errorf("the refusal at %s was cached as a miss", c.name)
			}
			if got := rec.Failures(); len(got) != 0 {
				t.Errorf("a refusal was logged as a failure:\n%s", strings.Join(got, "\n"))
			}
		})
	}
}
