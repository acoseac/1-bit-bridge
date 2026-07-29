package enrich

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

func TestStripAlbumEditionSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		// Real titles from the production library that failed to resolve.
		{"Goats Head Soup (2020 Deluxe)", "Goats Head Soup"},
		{"The Absence (Japanese edition)", "The Absence"},
		{"1000 Forms Of Fear (Deluxe Version)", "1000 Forms Of Fear"},
		{"Getz/Gilberto (Expanded Edition)", "Getz/Gilberto"},
		{"Sticky Fingers (Remastered)", "Sticky Fingers"},
		{"Hello, I Must Be Going! (Remastered Hi-Res Version)", "Hello, I Must Be Going!"},
		{"Tarzan (Original Motion Picture Soundtrack)", "Tarzan"},
		{"Some Album [Bonus Disc]", "Some Album"},

		// Nothing to strip.
		{"Kind of Blue", ""},
		{"", ""},

		// Anchored to the END: a title that OPENS with a parenthetical keeps it.
		{"(I Can't Get No) Satisfaction", ""},

		// Stripping must not empty the title.
		{"(Remastered)", ""},
		{"   (Live)   ", ""},

		// One qualifier only — the regex is not greedy across both.
		{"Album (Deluxe) (Remastered)", "Album (Deluxe)"},
	}
	for _, tc := range cases {
		if got := stripAlbumEditionSuffix(tc.in); got != tc.want {
			t.Errorf("stripAlbumEditionSuffix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// mbFallbackServer serves a release search that only matches one specific
// (artist, album) pair, and records every query it was asked.
type mbFallbackServer struct {
	wantArtist, wantAlbum string

	mu      sync.Mutex
	queries []string
}

func (m *mbFallbackServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/release/") {
			io.WriteString(w, `{"artists":[]}`)
			return
		}
		// Query().Get already percent-decodes; unescaping again would
		// mangle a literal % in an album title.
		q := r.URL.Query().Get("query")
		m.mu.Lock()
		m.queries = append(m.queries, q)
		m.mu.Unlock()

		if q == `release:"`+m.wantAlbum+`" AND artist:"`+m.wantArtist+`"` {
			io.WriteString(w, `{"releases":[{"id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","score":100,`+
				`"title":"`+m.wantAlbum+`","artist-credit":[{"name":"`+m.wantArtist+`"}]}]}`)
			return
		}
		io.WriteString(w, `{"releases":[]}`)
	}
}

func (m *mbFallbackServer) asked() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.queries...)
}

// TestSearchReleaseWithFallbacks covers the two retries and, as importantly,
// that they do NOT run when they shouldn't.
//
// Every case asserts the exact QUERY COUNT as well as the outcome, so a change
// that fires redundant upstream calls fails here too — not just one that breaks
// resolution.
func TestSearchReleaseWithFallbacks(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name         string
		track        manifest.Track
		mbArtist     string // the one (artist, album) pair the stub will match
		mbAlbum      string
		wantResolved bool
		wantQueries  int
	}{
		{
			name:     "primary hit asks exactly once",
			track:    manifest.Track{Path: "a.flac", Artist: "Metallica", Album: "Load", AlbumArtist: "Metallica"},
			mbArtist: "Metallica", mbAlbum: "Load",
			wantResolved: true, wantQueries: 1,
		},
		{
			// The junk per-track artist is real: this is how a production file
			// was tagged, and it is why the album never resolved.
			name: "falls back to albumArtist",
			track: manifest.Track{Path: "a.flac", Artist: "[ME] Load [145412591] [1996]",
				Album: "Load", AlbumArtist: "Metallica"},
			mbArtist: "Metallica", mbAlbum: "Load",
			wantResolved: true, wantQueries: 2,
		},
		{
			name: "falls back to a stripped edition suffix",
			track: manifest.Track{Path: "a.flac", Artist: "The Rolling Stones",
				Album: "Goats Head Soup (2020 Deluxe)", AlbumArtist: "The Rolling Stones"},
			mbArtist: "The Rolling Stones", mbAlbum: "Goats Head Soup",
			wantResolved: true, wantQueries: 2,
		},
		{
			// Compilation shape: credited to Various Artists AND carrying an
			// edition suffix. 7 of 180 sampled albums needed exactly this.
			name: "combines both when neither alone is enough",
			track: manifest.Track{Path: "a.flac", Artist: "Bon Jovi",
				Album: "Acoustic Music (Deluxe Edition)", AlbumArtist: "Various Artists"},
			mbArtist: "Various Artists", mbAlbum: "Acoustic Music",
			wantResolved: true, wantQueries: 4,
		},
		{
			// albumArtist differs only in case and there is no suffix to strip,
			// so there is nothing new to ask.
			name: "no redundant query when albumArtist equals artist",
			track: manifest.Track{Path: "a.flac", Artist: "Miles Davis",
				Album: "Kind of Blue", AlbumArtist: "miles davis"},
			mbArtist: "nobody", mbAlbum: "nothing",
			wantResolved: false, wantQueries: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &mbFallbackServer{wantArtist: tc.mbArtist, wantAlbum: tc.mbAlbum}
			s := httptest.NewServer(srv.handler())
			defer s.Close()
			e := NewEnricher(nil, NewMusicBrainzClient(s.URL, "t", nil), nil, nil, t.TempDir())
			e.MBMinInterval = 0 // no need to pace a local test server

			trk := tc.track
			res, err := e.searchReleaseWithFallbacks(ctx, &trk)
			if err != nil {
				t.Fatalf("searchReleaseWithFallbacks: %v", err)
			}
			asked := srv.asked()
			if (res != nil) != tc.wantResolved {
				t.Errorf("resolved = %v, want %v; queries were %v", res != nil, tc.wantResolved, asked)
			}
			if len(asked) != tc.wantQueries {
				t.Errorf("issued %d queries, want %d: %v", len(asked), tc.wantQueries, asked)
			}
		})
	}

	t.Run("an error stops the chain instead of being masked", func(t *testing.T) {
		// Error semantics are the contract: a fallback runs only after a clean
		// (nil, nil). If a transient error were swallowed and retried into a
		// miss, the caller would markSkipped and stamp enriched_at, making a
		// blip permanent — the exact failure PR #74 fixed.
		var calls int
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/release/") {
				calls++
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			io.WriteString(w, `{"artists":[]}`)
		}))
		defer s.Close()
		e := NewEnricher(nil, NewMusicBrainzClient(s.URL, "t", nil), nil, nil, t.TempDir())
		e.MBMinInterval = 0
		res, err := e.searchReleaseWithFallbacks(ctx, &manifest.Track{
			Path: "a.flac", Artist: "A", Album: "B (Deluxe)", AlbumArtist: "C",
		})
		if err == nil {
			t.Fatal("want the upstream error propagated, got nil")
		}
		if res != nil {
			t.Errorf("want nil result alongside the error, got %+v", res)
		}
		if calls != 1 {
			t.Errorf("issued %d release queries, want 1 — the chain must stop on error", calls)
		}
	})
}
