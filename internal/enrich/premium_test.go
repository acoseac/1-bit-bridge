package enrich

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCred is a static AtlasCredentialSource for the premium-fetcher tests.
type fakeCred struct {
	token, base string
	ok          bool
}

func (f fakeCred) AtlasCredential() (string, string, bool) { return f.token, f.base, f.ok }

// jpeg is a tiny valid-enough JPEG-ish blob (SOI marker + payload). The
// premium fetcher streams bytes verbatim; content is opaque to it.
var premiumJPEG = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("ATLAS-PREMIUM")...)

func TestAtlasPremiumFetcher_TryCache(t *testing.T) {
	const mbid = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

	t.Run("200 caches premium bytes and sends bearer", func(t *testing.T) {
		var gotAuth, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(premiumJPEG)
		}))
		defer srv.Close()

		f := NewAtlasPremiumFetcher(fakeCred{token: "tok123", base: srv.URL, ok: true}, "test-ua", srv.Client())
		path := filepath.Join(t.TempDir(), mbid+"-500.jpg")
		if !f.TryCache(context.Background(), path, mbid, 500) {
			t.Fatal("TryCache returned false on a 200 response")
		}
		if gotAuth != "Bearer tok123" {
			t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer tok123")
		}
		if want := "/release/" + mbid + "/front-500"; gotPath != want {
			t.Errorf("request path = %q, want %q", gotPath, want)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("cache file not written: %v", err)
		}
		if !bytes.Equal(got, premiumJPEG) {
			t.Errorf("cached bytes != premium bytes")
		}
	})

	t.Run("404 returns false and writes no file", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		defer srv.Close()
		f := NewAtlasPremiumFetcher(fakeCred{token: "tok", base: srv.URL, ok: true}, "ua", srv.Client())
		path := filepath.Join(t.TempDir(), mbid+"-500.jpg")
		if f.TryCache(context.Background(), path, mbid, 500) {
			t.Error("TryCache returned true on a 404 (should fall through to CAA)")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("a file was written on a 404: %v", err)
		}
	})

	t.Run("401 returns false (token rejected → CAA fallback)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		f := NewAtlasPremiumFetcher(fakeCred{token: "stale", base: srv.URL, ok: true}, "ua", srv.Client())
		path := filepath.Join(t.TempDir(), mbid+"-500.jpg")
		if f.TryCache(context.Background(), path, mbid, 500) {
			t.Error("TryCache returned true on a 401")
		}
	})

	t.Run("no credential makes no request and returns false", func(t *testing.T) {
		var hits int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			_, _ = w.Write(premiumJPEG)
		}))
		defer srv.Close()
		// ok=false → fetcher must short-circuit before any HTTP call.
		f := NewAtlasPremiumFetcher(fakeCred{ok: false}, "ua", srv.Client())
		path := filepath.Join(t.TempDir(), mbid+"-500.jpg")
		if f.TryCache(context.Background(), path, mbid, 500) {
			t.Error("TryCache returned true with no credential")
		}
		if hits != 0 {
			t.Errorf("made %d HTTP request(s) with no credential; want 0", hits)
		}
	})

	t.Run("trailing slash on base URL is handled", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			_, _ = w.Write(premiumJPEG)
		}))
		defer srv.Close()
		f := NewAtlasPremiumFetcher(fakeCred{token: "t", base: srv.URL + "/", ok: true}, "ua", srv.Client())
		path := filepath.Join(t.TempDir(), mbid+"-1200.jpg")
		if !f.TryCache(context.Background(), path, mbid, 1200) {
			t.Fatal("TryCache returned false")
		}
		if strings.Contains(gotPath, "//") {
			t.Errorf("double slash in request path: %q", gotPath)
		}
		if want := "/release/" + mbid + "/front-1200"; gotPath != want {
			t.Errorf("request path = %q, want %q", gotPath, want)
		}
	})
}
