package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
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

// TestServeAsksDeezerForTheRefusedPortraitsOnceItIsOn boots the real serve
// with enrich.deezerEnabled false over a library of two tracks by an artist
// the mirror knows: their portrait lookups are refused, and the artist's
// image is the 404 no_image while Deezer is off. Switched on from the
// console, the enricher asks Deezer for the portrait by the artist's
// canonical name, with no rescan and no request to the mirror, and the image
// answers 202 pending while the portrait waits (this Deezer offers a picture
// on a host the client refuses, so it waits for good). On main the console's
// switch moved neither: the rows were stamped, and only "Retry missing" or a
// changed file asked again (backlog B761).
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
		status, body := getBytesWithToken(t, b, image, token)
		return status == http.StatusNotFound && strings.Contains(string(body), `"no_image"`)
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
	status, raw := getBytesWithToken(t, b, image, token)
	if status != http.StatusAccepted || !strings.Contains(string(raw), `"pending"`) {
		t.Errorf("the artist's image answered %d %s while its portrait waits with both switches on, want 202 pending", status, raw)
	}
	if got := mirror.requests(); len(got) != asked {
		t.Errorf("the mirror was asked %d time(s) once Deezer was on, want none:\n%s", len(got)-asked,
			strings.Join(got[asked:], "\n"))
	}
}
