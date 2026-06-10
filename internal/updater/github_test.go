package updater

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLatestRelease_404ReturnsSentinel pins the typed-error contract:
// a 404 response from /releases/latest must return ErrNoReleasesPublished
// (so callers can distinguish "definitely no candidate" from a transient
// network error), NOT an empty Release with nil error.
//
// Replaces the legacy short-circuit that returned (&Release{}, nil) and
// caused the admin dashboard's "Updates" card to render "checking…"
// indefinitely.
func TestLatestRelease_404ReturnsSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClient("fake/repo", time.Second).WithBaseURL(srv.URL)
	rel, err := c.LatestRelease(context.Background())

	if !errors.Is(err, ErrNoReleasesPublished) {
		t.Fatalf("err = %v, want ErrNoReleasesPublished", err)
	}
	if rel != nil {
		t.Errorf("Release = %+v, want nil on 404", rel)
	}
}

// TestLatestRelease_RateLimitStillWorks confirms the 404 handler change
// didn't accidentally subsume the 403 rate-limit branch above it.
func TestLatestRelease_RateLimitStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewClient("fake/repo", time.Second).WithBaseURL(srv.URL)
	_, err := c.LatestRelease(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

// TestLatestRelease_200OKStillWorks pins that the success path is
// unchanged after the 404 refactor.
func TestLatestRelease_200OKStillWorks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.3","html_url":"https://x"}`))
	}))
	defer srv.Close()

	c := NewClient("fake/repo", time.Second).WithBaseURL(srv.URL)
	rel, err := c.LatestRelease(context.Background())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rel == nil || rel.TagName != "v1.2.3" {
		t.Errorf("Release = %+v, want TagName=v1.2.3", rel)
	}
}

// TestNewClient_DownloadClientHasNoOverallTimeout structurally pins the
// split-client contract: the API client keeps the short poll Timeout
// (small JSON responses; a hung GitHub API call should fail fast), and
// the download client must carry NO overall Timeout — http.Client.Timeout
// caps the entire exchange including the body read, so a multi-MiB
// release archive on a link slower than ~1.5 MB/s blew past the 10 s poll
// budget mid-stream and permanently broke `bridge update` + the serve
// auto-installer. The download phase is bounded by a context deadline at
// the Install call site (downloadTimeout) instead.
//
// Pinned structurally rather than behaviourally because the behavioural
// form — an httptest server dripping an archive for longer than the poll
// timeout — needs a >10 s wall-clock sleep, too slow for CI.
func TestNewClient_DownloadClientHasNoOverallTimeout(t *testing.T) {
	c := NewClient("fake/repo", 10*time.Second)
	if got := c.http.Timeout; got != 10*time.Second {
		t.Errorf("API client Timeout = %v, want 10s", got)
	}
	if c.download == nil {
		t.Fatal("download client not constructed by NewClient")
	}
	if got := c.download.Timeout; got != 0 {
		t.Errorf("download client Timeout = %v, want 0 (no overall cap)", got)
	}
	if got := c.downloadClient(); got != c.download {
		t.Error("downloadClient() did not return the dedicated download client")
	}
}

// TestDownloadClientFallsBackWhenUnset pins downloadClient's nil-safety
// for bare struct literals (no production path builds one; the fallback
// exists so a future test-only literal can't nil-pointer inside
// http.Client.Do).
func TestDownloadClientFallsBackWhenUnset(t *testing.T) {
	c := &Client{http: &http.Client{Timeout: time.Second}}
	if got := c.downloadClient(); got != c.http {
		t.Error("downloadClient() on a literal without download must fall back to the API client")
	}
}
