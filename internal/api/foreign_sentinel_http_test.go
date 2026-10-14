package api

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// foreignSentinelRef is one foreign playlist or favorite item. The bridge
// stores originFingerprint as given, so each sentinel and a bridge
// fingerprint must survive a PUT and come back on GET unchanged.
type foreignSentinelRef struct {
	fingerprint string
	originPath  string
	title       string
	artist      string
	position    int
	favoritedAt int64
}

func foreignSentinelRefs() []foreignSentinelRef {
	return []foreignSentinelRef{
		{"local", "device/Library/01.flac", "Local title", "Local artist", 0, 1000},
		{"smb", "share/Album/01.flac", "SMB title", "SMB artist", 1, 2000},
		{"upnp", "0/Album/01.flac", "UPnP title", "UPnP artist", 2, 3000},
		{"mpd", "mpd/Artist/Album/01.flac", "MPD title", "MPD artist", 3, 4000},
		{"AB:CD:EF:01", "Other/Album/01.flac", "Bridge title", "Bridge artist", 4, 5000},
	}
}

func readStatusBody(t *testing.T, resp *http.Response) (int, []byte) {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

func matchForeignItem(t *testing.T, ref foreignSentinelRef, fingerprint, originPath, title, artist, path string) {
	t.Helper()
	if fingerprint != ref.fingerprint || originPath != ref.originPath ||
		title != ref.title || artist != ref.artist || path != "" {
		t.Errorf("item %q: fingerprint %q originPath %q title %q artist %q local path %q",
			ref.fingerprint, fingerprint, originPath, title, artist, path)
	}
}

// TestForeignSentinelsRoundTripOpaque pins that a playlist PUT and a
// favorites PUT each accept one foreign item for every sentinel the wire
// names, plus one bridge fingerprint, and that GET returns each item with
// that fingerprint, path, title and artist unchanged.
func TestForeignSentinelsRoundTripOpaque(t *testing.T) {
	refs := foreignSentinelRefs()

	t.Run("playlist", func(t *testing.T) {
		token, dt, srv := newPlaylistTestServer(t)
		id := "6e0b3a5d-9f32-4d4b-8c88-1a2f3e4d5c6b"
		items := make(cappedPlaylistItems, 0, len(refs))
		for _, ref := range refs {
			items = append(items, playlistItemDTO{
				Position:          ref.position,
				OriginFingerprint: ref.fingerprint,
				OriginPath:        ref.originPath,
				Title:             ref.title,
				Artist:            ref.artist,
			})
		}
		raw, err := json.Marshal(playlistDTO{
			ID:             id,
			Name:           "Foreign sentinels",
			LastModifiedAt: 200,
			Items:          items,
		})
		if err != nil {
			t.Fatal(err)
		}
		status, body := readStatusBody(t, doReq(t, srv, http.MethodPut, "/v1/playlists/"+id, token, dt, string(raw)))
		if status != http.StatusOK {
			t.Fatalf("PUT status = %d, body %s", status, body)
		}
		status, body = readStatusBody(t, doReq(t, srv, http.MethodGet, "/v1/playlists/"+id, token, dt, ""))
		if status != http.StatusOK {
			t.Fatalf("GET status = %d, body %s", status, body)
		}
		var got playlistDTO
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Items) != len(refs) {
			t.Fatalf("items = %d, want %d", len(got.Items), len(refs))
		}
		byFP := make(map[string]playlistItemDTO, len(got.Items))
		for _, it := range got.Items {
			byFP[it.OriginFingerprint] = it
		}
		for _, ref := range refs {
			it, ok := byFP[ref.fingerprint]
			if !ok {
				t.Errorf("missing %q", ref.fingerprint)
				continue
			}
			matchForeignItem(t, ref, it.OriginFingerprint, it.OriginPath, it.Title, it.Artist, it.Path)
			if it.Position != ref.position {
				t.Errorf("%q position = %d, want %d", ref.fingerprint, it.Position, ref.position)
			}
		}
	})

	t.Run("favorites", func(t *testing.T) {
		token, dt, srv := newFavoritesTestServer(t)
		tracks := make(cappedFavoriteTracks, 0, len(refs))
		for _, ref := range refs {
			tracks = append(tracks, favoriteTrackDTO{
				OriginFingerprint: ref.fingerprint,
				OriginPath:        ref.originPath,
				Title:             ref.title,
				Artist:            ref.artist,
				FavoritedAt:       ref.favoritedAt,
			})
		}
		raw, err := json.Marshal(favoritesDTO{
			LastModifiedAt:  1000,
			Tracks:          tracks,
			Albums:          cappedFavoriteAlbums{},
			Tombstones:      cappedFavoriteTombstones{},
			AlbumTombstones: cappedFavoriteAlbumTombstones{},
		})
		if err != nil {
			t.Fatal(err)
		}
		status, body := readStatusBody(t, doReq(t, srv, http.MethodPut, "/v1/favorites", token, dt, string(raw)))
		if status != http.StatusOK {
			t.Fatalf("PUT status = %d, body %s", status, body)
		}
		status, body = readStatusBody(t, doReq(t, srv, http.MethodGet, "/v1/favorites", token, dt, ""))
		if status != http.StatusOK {
			t.Fatalf("GET status = %d, body %s", status, body)
		}
		var got favoritesDTO
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Tracks) != len(refs) {
			t.Fatalf("tracks = %d, want %d", len(got.Tracks), len(refs))
		}
		byFP := make(map[string]favoriteTrackDTO, len(got.Tracks))
		for _, it := range got.Tracks {
			byFP[it.OriginFingerprint] = it
		}
		for _, ref := range refs {
			it, ok := byFP[ref.fingerprint]
			if !ok {
				t.Errorf("missing %q", ref.fingerprint)
				continue
			}
			matchForeignItem(t, ref, it.OriginFingerprint, it.OriginPath, it.Title, it.Artist, it.Path)
			if it.FavoritedAt != ref.favoritedAt {
				t.Errorf("%q favoritedAt = %d, want %d", ref.fingerprint, it.FavoritedAt, ref.favoritedAt)
			}
		}
	})
}
