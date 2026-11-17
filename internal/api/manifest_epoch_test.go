package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/manifest"
	"github.com/acoseac/1-bit-bridge/internal/urlquery"
)

// TestTheManifestCarriesItsEpochAndADeltaFromAnotherIsIncomplete: through
// the real router and the real Provider, every manifest response carries the
// store's backup epoch (the full one, every page of a walk and a delta), a
// delta that sends no epoch or the bridge's own is answered as before, and
// one that sends another, or an empty one, answers deltaIncomplete with no
// `deleted` and its tracks as they are (backlog B653); an epoch on a request
// that is no delta is not read; and /v1/health advertises manifestEpoch.
func TestTheManifestCarriesItsEpochAndADeltaFromAnotherIsIncomplete(t *testing.T) {
	f := newFormatsFixture(t)
	ctx := context.Background()
	epoch, err := f.store.BackupEpoch(ctx)
	if err != nil || epoch == "" {
		t.Fatalf("BackupEpoch = %q, %v", epoch, err)
	}
	const other = "0123456789abcdef0123456789abcdef"

	// keys decodes a manifest answer's top level, so a key's absence is read
	// as absence.
	keys := func(t *testing.T, body []byte) map[string]json.RawMessage {
		t.Helper()
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			t.Fatalf("decode: %v\n%s", err, body)
		}
		return top
	}

	resp, body := f.get(t, "/v1/manifest")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("full: status %d: %s", resp.StatusCode, body)
	}
	if m := manifestOf(t, body); m.Epoch != epoch {
		t.Errorf("full: epoch %q, want the store's %q", m.Epoch, epoch)
	}
	// An epoch on a full request is not read: the full manifest is the
	// client's whole state.
	resp, body = f.get(t, "/v1/manifest?epoch="+other)
	if m := manifestOf(t, body); resp.StatusCode != http.StatusOK || m.Epoch != epoch || len(m.Tracks) == 0 || m.DeltaIncomplete {
		t.Errorf("full with another epoch: status %d, epoch %q, %d tracks, incomplete %v; want 200, the store's epoch, its tracks",
			resp.StatusCode, m.Epoch, len(m.Tracks), m.DeltaIncomplete)
	}

	cursor := ""
	for page := 0; ; page++ {
		target := "/v1/manifest?limit=1"
		if cursor != "" {
			target += "&cursor=" + urlquery.Escape(cursor)
		}
		resp, body := f.get(t, target)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page %d: status %d: %s", page, resp.StatusCode, body)
		}
		m := manifestOf(t, body)
		if m.Epoch != epoch {
			t.Errorf("page %d: epoch %q, want the store's %q", page, m.Epoch, epoch)
		}
		if m.NextCursor == nil {
			if page == 0 {
				t.Fatal("the walk ended on its first page: no later page was asked")
			}
			break
		}
		cursor = *m.NextCursor
	}

	// Rows enough that one deletion is a deletion list, never the journal's
	// mass delete, which resets its coverage.
	var filler []*manifest.Track
	for i := 0; i < 20; i++ {
		filler = append(filler, &manifest.Track{Path: "Filler/" + strconv.Itoa(i) + ".flac", Size: 1, ModTime: time.Unix(1, 0).UTC()})
	}
	if err := f.store.UpsertTrackBatch(ctx, filler); err != nil {
		t.Fatal(err)
	}
	since := time.Now().UTC()
	time.Sleep(2 * time.Millisecond)
	const gone, changed = "Artist/Album/01 Gate One.flac", "Other/05 Gate Five.mp3"
	if err := f.store.DeleteTrack(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpsertTrack(ctx, &manifest.Track{Path: changed, Size: 2, ModTime: since, Title: "Retagged"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	for _, c := range []struct {
		name       string
		query      string
		incomplete bool
	}{
		{"no epoch sent", "", false},
		{"the bridge's epoch", "&epoch=" + epoch, false},
		{"another epoch", "&epoch=" + other, true},
		{"an empty epoch", "&epoch=", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			resp, body := f.get(t, "/v1/manifest?since="+urlquery.Escape(since.Format(time.RFC3339Nano))+c.query)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", resp.StatusCode, body)
			}
			m := manifestOf(t, body)
			if m.Epoch != epoch {
				t.Errorf("epoch %q, want the store's %q", m.Epoch, epoch)
			}
			if got := trackPaths(m.Tracks); !slices.Equal(got, []string{changed}) {
				t.Errorf("tracks %q, want %q whatever the epoch", got, changed)
			}
			top := keys(t, body)
			_, hasDeleted := top["deleted"]
			if c.incomplete {
				if !m.DeltaIncomplete || hasDeleted {
					t.Errorf("deltaIncomplete %v, deleted key %v; want incomplete and no deleted", m.DeltaIncomplete, hasDeleted)
				}
				return
			}
			if m.DeltaIncomplete || !slices.Equal(m.Deleted, []string{gone}) {
				t.Errorf("deleted %q (incomplete %v), want %q", m.Deleted, m.DeltaIncomplete, gone)
			}
		})
	}

	hresp, hbody := f.get(t, "/v1/health")
	var h HealthResponse
	if err := json.Unmarshal(hbody, &h); err != nil || hresp.StatusCode != http.StatusOK {
		t.Fatalf("health: status %d, %v", hresp.StatusCode, err)
	}
	if !slices.Contains(h.Features, "manifestEpoch") {
		t.Errorf("features %q lack manifestEpoch", h.Features)
	}

	// The handler hands the provider what the request sent: an epoch on a
	// delta (read through safeQuery), none where the request sent none, and
	// none on a request that is no delta.
	since2 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Format(time.RFC3339Nano)
	for _, c := range []struct {
		query string
		want  manifest.ClientEpoch
	}{
		{"since=" + since2 + "&epoch=" + other, manifest.ClientEpoch{Value: other, Sent: true}},
		{"since=" + since2 + "&epoch=", manifest.ClientEpoch{Sent: true}},
		{"since=" + since2, manifest.ClientEpoch{}},
		{"epoch=" + other, manifest.ClientEpoch{}},
	} {
		mp := &fakeManifestProvider{body: map[string]any{"echo": "ok"}}
		hs, tok := withManifest(t, mp)
		req, _ := http.NewRequest(http.MethodGet, hs.URL+"/v1/manifest?"+c.query, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || mp.lastEpoch != c.want {
			t.Errorf("?%s: status %d, the provider was handed %+v; want 200 and %+v", c.query, resp.StatusCode, mp.lastEpoch, c.want)
		}
	}
}
