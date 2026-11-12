package main

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// untilServed waits for cond, giving up only at the test binary's deadline
// (serveGiveUpTime), and then names what serve was doing.
func untilServed(t *testing.T, cond func() bool, what string) {
	t.Helper()
	giveUp := serveGiveUp(t)
	for !cond() {
		select {
		case <-giveUp:
			t.Fatalf("gave up waiting for %s\n%s", what, serveStacks())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// imageAnswer is one answer of an image route: its status, body and
// Retry-After.
type imageAnswer struct {
	status     int
	body       string
	retryAfter string
}

// getImageWithToken asks the API for path with token and keeps the answer's
// Retry-After beside its status and body.
func getImageWithToken(t *testing.T, b *consoleBridge, path, token string) imageAnswer {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, b.apiBase+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := b.phone.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s: read body: %v", path, err)
	}
	return imageAnswer{status: resp.StatusCode, body: string(body), retryAfter: resp.Header.Get("Retry-After")}
}

// TestServeAsksDeezerForTheRefusedPortraitsOnceItIsOn boots the real serve
// with enrich.deezerEnabled false over a library of two tracks by an artist
// the mirror knows: their portrait lookups are refused, and the artist's
// image is the 404 no_image while Deezer is off. Switched on from the
// console, the enricher asks Deezer for the portrait by the artist's
// canonical name, with no rescan and no request to the mirror; while that
// search is held the image answers 202 pending with its Retry-After, and once
// Deezer answers with a picture on a host the client refuses, which no wait
// will change for that name, it answers the 404 no_image with none. Until
// backlog B761's second round it answered 202 pending for good there; on
// main the console's switch moved neither: the rows were stamped, and only
// "Retry missing" or a changed file asked again (backlog B761).
func TestServeAsksDeezerForTheRefusedPortraitsOnceItIsOn(t *testing.T) {
	persistentMedium(t)
	mirror := serveSwitchMirror(t)
	deezer := newSwitchCounter(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/search/artist") {
			fmt.Fprintf(w, `{"data":[{"id":1,"name":"Artist","picture_xl":"http://%s/picture.jpg","picture_big":""}]}`, r.Host)
			return
		}
		http.NotFound(w, r)
	})
	// The search is held until the test lets it go; the cleanup that lets it
	// go is registered after the counter's Close, so it runs first.
	release := make(chan struct{})
	var once sync.Once
	letGo := func() { once.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	deezer.setHold(func(r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/search/artist") {
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	fill := func(lib string) {
		for _, name := range []string{"01 One.flac", "02 Two.flac"} {
			writeHiResFLACFixture(t, filepath.Join(lib, "Artist", "Album", name))
		}
	}
	yaml := "enrich:\n  musicbrainzBaseURL: " + mirror.srv.URL + "/ws/2\n  coverArtBaseURL: " + mirror.srv.URL +
		"/caa\n  deezerEnabled: false\n"
	b := startConsoleBridge(t, yaml, fill, func(o *serveOpts) {
		o.deezerBase = deezer.srv.URL
		o.enricherPoll = 20 * time.Millisecond
	})
	waitForTracksIndexed(t, b.console, b.adminBase, 2, b.stderr)
	mint := pairViaAdmin(t, t.Context(), b.console, b.adminBase+"/api/tokens",
		`{"name":"refused portraits"}`, http.StatusCreated, b.stderr)
	token := linkQueryItems(t, mint.PairURL)["token"]
	if token == "" {
		t.Fatalf("the console's pairing link carries no token: %s", mint.PairURL)
	}
	image := "/v1/artist-image/" + switchArtistMBID
	untilServed(t, func() bool {
		got := getImageWithToken(t, b, image, token)
		return got.status == http.StatusNotFound && strings.Contains(got.body, `"no_image"`)
	}, "the artist's tracks to be stamped with Deezer off (its image 404 no_image)")
	if got := deezer.requests(); len(got) != 0 {
		t.Fatalf("Deezer was asked while it was off:\n%s", strings.Join(got, "\n"))
	}
	asked := mirror.count()

	status, body := patchSettings(t, b, `{"enrichDeezerEnabled":true}`)
	if status != http.StatusOK || !strings.Contains(body, "enrichDeezerEnabled") {
		t.Fatalf("PATCH enrichDeezerEnabled on = %d %s", status, body)
	}
	untilServed(t, func() bool { return deezer.asked("GET /search/artist") > 0 },
		"Deezer to be asked for the refused portrait once it is on")
	if got := deezer.requests(); got[0] != "GET /search/artist?q=Artist&limit=5" {
		t.Errorf("Deezer was asked %q, want the search by the artist's canonical name", got[0])
	}
	if got := getImageWithToken(t, b, image, token); got.status != http.StatusAccepted || !strings.Contains(got.body, `"pending"`) ||
		got.retryAfter == "" {
		t.Errorf("the artist's image answered %d %s (Retry-After %q) while its portrait's search is held, want 202 pending with a Retry-After",
			got.status, got.body, got.retryAfter)
	}

	letGo()
	var settled imageAnswer
	untilServed(t, func() bool {
		settled = getImageWithToken(t, b, image, token)
		return settled.status == http.StatusNotFound && strings.Contains(settled.body, `"no_image"`)
	}, "the artist's image to answer 404 no_image once Deezer's answer ended the name")
	if settled.retryAfter != "" {
		t.Errorf("the 404 no_image carries Retry-After %q, want none", settled.retryAfter)
	}
	if got := mirror.requests(); len(got) != asked {
		t.Errorf("the mirror was asked %d time(s) once Deezer was on, want none:\n%s", len(got)-asked,
			strings.Join(got[asked:], "\n"))
	}
}
