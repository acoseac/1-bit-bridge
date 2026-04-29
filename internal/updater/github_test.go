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
