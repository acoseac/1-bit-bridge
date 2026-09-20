package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// A client replaying a queued delete is indistinguishable from a person
// tidying up, one request at a time — which is how 17 playlists went on
// 2026-09-20 with nothing in the journal but seventeen 200s. The warning
// is what puts the run in the log; the console's restore panel is what
// undoes it.

// seedAndDelete PUTs `n` playlists and DELETEs them all through the real
// handler chain, returning the ids in the order they were deleted.
func seedAndDelete(t *testing.T, srv *Server, token, deviceToken string, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("5d9a2f4c-8e21-4c3a-9b77-0f1e2d3c4b%02d", i)
		body := `{"id":"` + id + `","name":"P` + fmt.Sprint(i) +
			`","lastModifiedAt":200,"items":[{"position":0,"path":"A/B/c.flac"}]}`
		if resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, deviceToken, body); resp.StatusCode != http.StatusOK {
			t.Fatalf("PUT %s = %d, want 200", id, resp.StatusCode)
		}
		ids = append(ids, id)
	}
	for _, id := range ids {
		if resp := doReq(t, srv, http.MethodDelete, "/v1/playlists/"+id, token, deviceToken, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("DELETE %s = %d, want 200", id, resp.StatusCode)
		}
	}
	return ids
}

func countWarnLines(out, msg string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, msg) {
			n++
		}
	}
	return n
}

// Not parallel: withTestSlog swaps a process global.
func TestAMassPlaylistDeleteWarnsOncePastTheThreshold(t *testing.T) {
	buf := withTestSlog(t)
	token, dt, srv := newPlaylistTestServer(t)

	const n = manifest.PlaylistDeleteBurstThreshold + 2
	seedAndDelete(t, srv, token, dt, n)

	out := buf.String()
	got := countWarnLines(out, "playlist mass delete")
	// One per tombstone from the threshold on — the count is the payload,
	// and the last line is where the run stopped.
	if want := n - manifest.PlaylistDeleteBurstThreshold + 1; got != want {
		t.Errorf("mass-delete warnings = %d, want %d\n%s", got, want, out)
	}
	if !strings.Contains(out, fmt.Sprintf("deleted=%d", n)) {
		t.Errorf("no warning reported the final count %d\n%s", n, out)
	}
	// The operator has to be able to match the line to a row on the
	// Devices page, and to know there is a way back.
	if !strings.Contains(out, "tokenId=") {
		t.Errorf("warning carries no token id\n%s", out)
	}
	if !strings.Contains(out, "Recently deleted") {
		t.Errorf("warning does not name the recovery route\n%s", out)
	}
}

func TestADeleteUnderTheThresholdIsSilent(t *testing.T) {
	buf := withTestSlog(t)
	token, dt, srv := newPlaylistTestServer(t)

	seedAndDelete(t, srv, token, dt, manifest.PlaylistDeleteBurstThreshold-1)

	if got := countWarnLines(buf.String(), "playlist mass delete"); got != 0 {
		t.Errorf("warnings below the threshold = %d, want 0\n%s", got, buf.String())
	}
}

// Two devices each deleting a few playlists is not one burst: pooling
// them would warn about a library nobody mass-deleted, and the log line
// names a device that did a fraction of the work.
func TestDeletesFromDifferentDevicesDoNotPool(t *testing.T) {
	buf := withTestSlog(t)
	token, _, srv := newPlaylistTestServer(t)

	const each = manifest.PlaylistDeleteBurstThreshold - 1
	// Device tokens are lowercase hex by contract (validDeviceToken).
	for i, dev := range []string{"aaaa1111", "bbbb2222"} {
		for j := 0; j < each; j++ {
			id := fmt.Sprintf("5d9a2f4c-8e21-4c3a-9b77-0f1e2d3c%d%03d", i, j)
			body := `{"id":"` + id + `","name":"P","lastModifiedAt":200,"items":[]}`
			if resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dev, body); resp.StatusCode != http.StatusOK {
				t.Fatalf("PUT %s = %d", id, resp.StatusCode)
			}
			if resp := doReq(t, srv, http.MethodDelete, "/v1/playlists/"+id, token, dev, ""); resp.StatusCode != http.StatusOK {
				t.Fatalf("DELETE %s = %d", id, resp.StatusCode)
			}
		}
	}
	// 2 * (threshold-1) tombstones in the window, no device past it.
	if got := countWarnLines(buf.String(), "playlist mass delete"); got != 0 {
		t.Errorf("warnings for two sub-threshold devices = %d, want 0\n%s", got, buf.String())
	}
}

// The DELETE records who asked, not who wrote — and every /v1 leg keeps
// behaving as before, because the tombstone is unchanged.
func TestDeleteRecordsTheDeletingDevice(t *testing.T) {
	token, _, srv := newPlaylistTestServer(t)
	id := "5d9a2f4c-8e21-4c3a-9b77-0f1e2d3c4b5a"
	body := `{"id":"` + id + `","name":"Favs","lastModifiedAt":200,"items":[{"position":0,"path":"A/B/c.flac"}]}`

	if resp := doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, "aaaa1111", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	if resp := doReq(t, srv, http.MethodDelete, "/v1/playlists/"+id, token, "bbbb2222", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE = %d", resp.StatusCode)
	}

	store, ok := srv.playlistStore.(*manifest.Store)
	if !ok {
		t.Fatalf("playlist store is %T, want *manifest.Store", srv.playlistStore)
	}
	rows, err := store.ListDeletedPlaylistsForAdmin(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("deleted rows = %+v (err=%v)", rows, err)
	}
	if rows[0].DeletedByToken != "bbbb2222" {
		t.Errorf("deletedBy = %q, want bbbb2222", rows[0].DeletedByToken)
	}
	if rows[0].WroteLastToken != "aaaa1111" {
		t.Errorf("wroteLast = %q, want aaaa1111", rows[0].WroteLastToken)
	}
}
