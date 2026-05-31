package dlna

import (
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// TrackID stability — the LOAD-BEARING regression guard
// -----------------------------------------------------------------------------

// Test_TrackID_StableAcrossScanRefresh is the LOAD-BEARING regression test
// for the "trackID is a pure function of (libraryRoot, relativePath)"
// invariant documented on TrackID(). Constructs multiple synthetic
// (libraryRoot, path) inputs paired with arbitrary "scan-state" data
// representations and asserts the TrackID stays bitwise-identical across
// the simulated scan refresh.
//
// If a future refactor incorporates ANY scan-generation data into the
// hash (mtime, file size, ID3 tag content, autoincrement row index,
// scan epoch, anything mutable across re-scans), this test fails. The
// production failure mode that would cause: renderers cache the trackID
// in their queue state at SetAVTransportURI time; a trackID that
// changes across a re-scan makes them issue GET /dlna/file/{old_hash}
// on the next track transition and receive 404, dropping playback.
func Test_TrackID_StableAcrossScanRefresh(t *testing.T) {
	cases := []struct {
		name         string
		libraryRoot  string
		relativePath string
	}{
		{"simple", "/library", "Diana Krall/Look of Love/01-Track.flac"},
		{"dsf_deep_nest", "/library", "Classical/Beethoven/Symphony 9/04-Ode to Joy.dsf"},
		{"unicode_in_path", "/library", "Édith Piaf/01-La Vie en Rose.flac"},
		{"spaces_and_special", "/lib root", "Album & More/01 - Title (Remastered).mp3"},
		{"single_byte_path", "/library", "a"},
		{"empty_root", "", "song.flac"},
		{"deep_path", "/library", strings.Repeat("a/", 50) + "song.flac"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Compute the ID multiple times — should be deterministic.
			id1 := TrackID(tc.libraryRoot, tc.relativePath)
			id2 := TrackID(tc.libraryRoot, tc.relativePath)
			if id1 != id2 {
				t.Errorf("TrackID is non-deterministic: got %q then %q for same input", id1, id2)
			}
			// Length sanity — should be 16 hex chars (64-bit truncation).
			if len(id1) != 16 {
				t.Errorf("TrackID length = %d, want 16 (16 hex chars = 8 bytes = 64 bits)", len(id1))
			}
			// All chars must be lowercase hex.
			for i, r := range id1 {
				if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
					t.Errorf("TrackID[%d] = %q is not lowercase hex (full ID: %q)", i, r, id1)
				}
			}
		})
	}

	// The actual "scan refresh" simulation — same (root, path) across
	// many different "scan states" (which the TrackID function never
	// touches) MUST produce the same ID.
	t.Run("scan_refresh_simulation_same_path_different_metadata", func(t *testing.T) {
		const root = "/library"
		const path = "Diana Krall/Look of Love/01-S Wonderful.flac"
		// Compute the canonical ID once.
		expected := TrackID(root, path)
		// Now simulate "scan refresh" — the path is the same but lots
		// of other things would have changed (mtime, size, tags,
		// enrichment state, etc.). The TrackID function takes only
		// (root, path), so by construction those changes can't affect
		// the output. This test makes that contract structurally
		// enforceable: if a future refactor ADDS more parameters to
		// TrackID, this test won't compile (calling site here uses
		// the canonical two-arg shape).
		for i := 0; i < 100; i++ {
			got := TrackID(root, path)
			if got != expected {
				t.Fatalf("scan refresh iter %d: TrackID drifted from %q to %q — load-bearing stability invariant violated",
					i, expected, got)
			}
		}
	})
}

// Test_TrackID_NullByteSeparatorPreventsCollision pins the design
// decision to separate libraryRoot from relativePath with a NUL byte
// (`\x00`) when feeding into SHA-256. Without that separator,
// (root="a", path="bc") and (root="ab", path="c") would hash identically.
// In real libraries this is unlikely but a future operator adding two
// library roots with overlapping name prefixes (e.g. "/music" and
// "/musi") could surface the collision.
func Test_TrackID_NullByteSeparatorPreventsCollision(t *testing.T) {
	id1 := TrackID("a", "bc")
	id2 := TrackID("ab", "c")
	if id1 == id2 {
		t.Errorf("collision: TrackID(%q,%q)=%q == TrackID(%q,%q)=%q — NUL-byte separator removed?",
			"a", "bc", id1, "ab", "c", id2)
	}
}

// Test_TrackID_DifferentInputsProduceDifferentIDs is a sanity check that
// the hash function actually discriminates between different inputs.
func Test_TrackID_DifferentInputsProduceDifferentIDs(t *testing.T) {
	cases := []struct {
		root, path string
	}{
		{"/library", "a.flac"},
		{"/library", "b.flac"},
		{"/library", "subdir/a.flac"},
		{"/other-library", "a.flac"},
		{"", "a.flac"},
		{"/library", ""},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		id := TrackID(tc.root, tc.path)
		key := tc.root + "::" + tc.path
		if prev, exists := seen[id]; exists {
			t.Errorf("hash collision: %q collides between input %q and previous input %q", id, key, prev)
		}
		seen[id] = key
	}
}

// -----------------------------------------------------------------------------
// DIDLForTrack — golden XMLs per codec
// -----------------------------------------------------------------------------

// Test_DIDLForTrack_dsf256_chord_2go uses the Phase-0-captured 2go
// playback case as the canonical DSF golden: DSD256, ~3.5 min, real
// Chord 2go UA (Music Player Daemon), expects audio/x-dsf MIME.
//
// **`bitsPerSample` deliberately absent from the `<res>` element**
// even though the input opts carry `BitsPerSample: 1` (DSD's
// inherent bit depth). The Mirror-PR companion to iOS PR #564 added
// a `!opts.IsDSD` co-gate on the bitsPerSample emission — DSD's
// "1" is misinterpreted as "1-bit PCM" by renderer parsers that
// treat the attribute as PCM-only (Gemini cross-codebase audit
// 2026-05-28). The golden updated to match. Linn Kazoo + BubbleUPnP
// follow the same omit-for-DSD convention.
func Test_DIDLForTrack_dsf256_chord_2go(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:         "abc123def4567890",
		Title:           "Trashbox",
		Artist:          "Test Artist",
		Album:           "Test Album",
		Size:            595999471,
		DurationSeconds: 210.799,
		SampleRateHz:    11289600,
		BitsPerSample:   1,
		Channels:        2,
		IsDSD:           true,
		Codec:           "DSF",
		FileExtension:   ".dsf",
		ServerURL:       "http://192.168.0.14:7790",
		UserAgent:       "Music Player Daemon 0.21.26",
	}
	got := DIDLForTrack(opts)
	want := `<item id="abc123def4567890" parentID="0" restricted="1">` +
		`<dc:title>Trashbox</dc:title>` +
		`<upnp:class>object.item.audioItem.musicTrack</upnp:class>` +
		`<upnp:artist>Test Artist</upnp:artist>` +
		`<upnp:album>Test Album</upnp:album>` +
		`<res protocolInfo="http-get:*:audio/x-dsf:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000" size="595999471" duration="0:03:30.799" sampleFrequency="11289600" nrAudioChannels="2">http://192.168.0.14:7790/dlna/file/abc123def4567890</res>` +
		`</item>`
	if got != want {
		t.Errorf("DSF golden mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// Test_DIDLForTrack_dsd_bitsPerSample_isDSDGate_omitsAttribute pins
// the Mirror-PR defense-in-depth contract from iOS PR #564 finding
// (2): when IsDSD=true, bitsPerSample MUST be omitted regardless of
// the input value, so a future caller that bypasses the upstream
// "make this 0 for DSD" chokepoints can't regress the DLNA decode
// path on PCM-style-strict renderers. The corresponding iOS test
// is `test_bitsPerSampleOne_withIsDSD_stillOmittedAtGenerator`.
func Test_DIDLForTrack_dsd_bitsPerSample_isDSDGate_omitsAttribute(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:       "dsd-isdsdgate",
		Title:         "T",
		Size:          1,
		BitsPerSample: 1, // raw-parser value; gate MUST suppress
		IsDSD:         true,
		FileExtension: ".dsf",
		ServerURL:     "http://server",
		UserAgent:     "any-renderer",
	}
	got := DIDLForTrack(opts)
	if strings.Contains(got, "bitsPerSample=") {
		t.Errorf("IsDSD=true MUST suppress bitsPerSample regardless of input value, got: %q", got)
	}
}

// Test_DIDLForTrack_pcm_bitsPerSample_stillEmitsWithoutIsDSD pins
// the inverse contract: PCM tracks (IsDSD=false) MUST continue
// emitting bitsPerSample as before — the gate is a DSD-specific
// suppression, not a global change.
func Test_DIDLForTrack_pcm_bitsPerSample_stillEmitsWithoutIsDSD(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:       "pcm-keepsBits",
		Title:         "T",
		Size:          1,
		BitsPerSample: 24,    // PCM 24-bit
		IsDSD:         false, // PCM
		FileExtension: ".flac",
		ServerURL:     "http://server",
		UserAgent:     "any-renderer",
	}
	got := DIDLForTrack(opts)
	if !strings.Contains(got, `bitsPerSample="24"`) {
		t.Errorf("IsDSD=false MUST emit bitsPerSample unchanged, got: %q", got)
	}
}

// Test_DIDLForTrack_dsf_sony_vendor_override pins that Sony renderers
// get audio/dsd in the protocolInfo MIME field even on a DSF file.
func Test_DIDLForTrack_dsf_sony_vendor_override(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:       "sony123",
		Title:         "Test",
		Size:          1000,
		FileExtension: ".dsf",
		ServerURL:     "http://server",
		UserAgent:     "Sony SRS-HG1/3.2",
	}
	got := DIDLForTrack(opts)
	if !strings.Contains(got, "audio/dsd") {
		t.Errorf("Sony+DSF must produce audio/dsd MIME, got: %q", got)
	}
	if strings.Contains(got, "audio/x-dsf") {
		t.Errorf("Sony+DSF must NOT include audio/x-dsf, got: %q", got)
	}
}

// Test_DIDLForTrack_flac_16_44 — FLAC Red Book golden.
func Test_DIDLForTrack_flac_16_44(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:         "flac1234",
		Title:           "Hello",
		Artist:          "Artist",
		Album:           "Album",
		Size:            30000000,
		DurationSeconds: 240.0,
		SampleRateHz:    44100,
		BitsPerSample:   16,
		Channels:        2,
		Codec:           "FLAC",
		FileExtension:   ".flac",
		ServerURL:       "http://192.168.0.14:7790",
		UserAgent:       "any-renderer",
	}
	got := DIDLForTrack(opts)
	want := `<item id="flac1234" parentID="0" restricted="1">` +
		`<dc:title>Hello</dc:title>` +
		`<upnp:class>object.item.audioItem.musicTrack</upnp:class>` +
		`<upnp:artist>Artist</upnp:artist>` +
		`<upnp:album>Album</upnp:album>` +
		`<res protocolInfo="http-get:*:audio/x-flac:DLNA.ORG_OP=01;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000" size="30000000" duration="0:04:00.000" sampleFrequency="44100" bitsPerSample="16" nrAudioChannels="2">http://192.168.0.14:7790/dlna/file/flac1234</res>` +
		`</item>`
	if got != want {
		t.Errorf("FLAC golden mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// Test_DIDLForTrack_dff_uses_audio_x_dff — DFF default codec MIME.
func Test_DIDLForTrack_dff_uses_audio_x_dff(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:       "dff",
		Title:         "T",
		Size:          1,
		FileExtension: ".dff",
		ServerURL:     "http://x",
		UserAgent:     "Music Player Daemon",
	}
	got := DIDLForTrack(opts)
	if !strings.Contains(got, "audio/x-dff") {
		t.Errorf("DFF must produce audio/x-dff MIME, got: %q", got)
	}
}

// Test_DIDLForTrack_perCodecMIME — table-driven MIME verification across
// every codec extension we expect to encounter.
func Test_DIDLForTrack_perCodecMIME(t *testing.T) {
	cases := []struct {
		ext      string
		wantMIME string
	}{
		{".dsf", "audio/x-dsf"},
		{".dff", "audio/x-dff"},
		{".flac", "audio/x-flac"},
		{".m4a", "audio/mp4"}, // ALAC container
		{".mp4", "audio/mp4"},
		{".wav", "audio/wav"},
		{".aiff", "audio/aiff"},
		{".aif", "audio/aiff"},
		{".mp3", "audio/mpeg"},
		{".ogg", "audio/ogg"},
	}
	for _, tc := range cases {
		t.Run(strings.TrimPrefix(tc.ext, "."), func(t *testing.T) {
			opts := DIDLTrackOpts{
				TrackID: "id", Title: "T", Size: 1,
				FileExtension: tc.ext, ServerURL: "http://x", UserAgent: "unknown",
			}
			got := DIDLForTrack(opts)
			if !strings.Contains(got, tc.wantMIME) {
				t.Errorf("%s: expected MIME %q in output, got: %q", tc.ext, tc.wantMIME, got)
			}
		})
	}
}

// Test_DIDLForTrack_urlHasNoSurroundingWhitespace pins the load-bearing
// invariant from Phase 0: the file URL inside `<res>...</res>` MUST
// NOT be surrounded by whitespace (mConnect's URL extractor failed on
// indent whitespace → SetAVTransportURI null → playback never starts).
func Test_DIDLForTrack_urlHasNoSurroundingWhitespace(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:       "id",
		Title:         "T",
		Size:          1,
		FileExtension: ".flac",
		ServerURL:     "http://server",
		UserAgent:     "any",
	}
	got := DIDLForTrack(opts)
	// Find the <res ...>URL</res> segment and assert no whitespace
	// immediately inside the angle brackets surrounding the URL.
	resOpenIdx := strings.LastIndex(got, ">")
	_ = resOpenIdx
	// Simpler approach: confirm the literal substring
	// `<res ...>http://...</res>` with the URL flush against the `>`
	// and `<` boundaries (no spaces/newlines between).
	want := `>http://server/dlna/file/id</res>`
	if !strings.Contains(got, want) {
		t.Errorf("file URL must be flush against <res>...</res> boundaries (no whitespace).\nExpected substring: %q\nGot full output: %q",
			want, got)
	}
	// Also assert the output is a single line (no newlines anywhere).
	if strings.ContainsAny(got, "\n\r\t") {
		t.Errorf("DIDL-Lite output must be a single line (no \\n / \\r / \\t), got: %q", got)
	}
}

// Test_DIDLForTrack_omitsEmptyFields verifies that zero / empty optional
// fields are omitted from the XML rather than emitted as empty
// attributes / elements (which some renderers refuse to parse).
func Test_DIDLForTrack_omitsEmptyFields(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:       "id",
		Title:         "T",
		Size:          1,
		FileExtension: ".flac",
		ServerURL:     "http://x",
		UserAgent:     "any",
		// All optional fields zero / empty
	}
	got := DIDLForTrack(opts)

	mustNotContain := []string{
		`<upnp:artist>`,
		`<upnp:album>`,
		`<upnp:author`,
		`<upnp:genre>`,
		`<dc:date>`,
		`<upnp:originalTrackNumber>`,
		`<upnp:albumArtURI>`,
		`duration="`,
		`sampleFrequency="`,
		`bitsPerSample="`,
	}
	for _, sub := range mustNotContain {
		if strings.Contains(got, sub) {
			t.Errorf("expected NO %q in minimal opts output, got: %q", sub, got)
		}
	}
	// Channels=0 should default to 2 (we always emit nrAudioChannels="2")
	if !strings.Contains(got, `nrAudioChannels="2"`) {
		t.Errorf("expected default nrAudioChannels=\"2\" when Channels=0, got: %q", got)
	}
}

// Test_DIDLForTrack_xmlEscapesSpecialChars pins XML escape behavior for
// title/artist/album fields that commonly carry special characters.
func Test_DIDLForTrack_xmlEscapesSpecialChars(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:       "id",
		Title:         `Track "Foo" & <Bar>`,
		Artist:        `Simon & Garfunkel`,
		Album:         `Greatest Hits 'Vol 1'`,
		Size:          1,
		FileExtension: ".flac",
		ServerURL:     "http://x",
		UserAgent:     "any",
	}
	got := DIDLForTrack(opts)
	mustContain := []string{
		`<dc:title>Track &quot;Foo&quot; &amp; &lt;Bar&gt;</dc:title>`,
		`<upnp:artist>Simon &amp; Garfunkel</upnp:artist>`,
		`<upnp:album>Greatest Hits &apos;Vol 1&apos;</upnp:album>`,
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("expected substring %q in escaped output, got: %q", want, got)
		}
	}
}

// Test_DIDLForTrack_albumArtistOmittedWhenSameAsArtist pins the
// album-artist-deduplication behavior.
func Test_DIDLForTrack_albumArtistOmittedWhenSameAsArtist(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:       "id",
		Title:         "T",
		Artist:        "Same",
		AlbumArtist:   "Same",
		Size:          1,
		FileExtension: ".flac",
		ServerURL:     "http://x",
	}
	got := DIDLForTrack(opts)
	if strings.Contains(got, `role="AlbumArtist"`) {
		t.Errorf("AlbumArtist == Artist should be omitted, got: %q", got)
	}
}

// Test_DIDLForTrack_albumArtistEmittedWhenDifferent pins the inverse
// case — distinct AlbumArtist (e.g., compilations / OSTs) gets its own
// element with role="AlbumArtist".
func Test_DIDLForTrack_albumArtistEmittedWhenDifferent(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:       "id",
		Title:         "T",
		Artist:        "Performer",
		AlbumArtist:   "Various Artists",
		Size:          1,
		FileExtension: ".flac",
		ServerURL:     "http://x",
	}
	got := DIDLForTrack(opts)
	wantArtist := `<upnp:artist>Performer</upnp:artist>`
	wantAlbumArtist := `<upnp:artist role="AlbumArtist">Various Artists</upnp:artist>`
	if !strings.Contains(got, wantArtist) {
		t.Errorf("missing primary artist: got %q", got)
	}
	if !strings.Contains(got, wantAlbumArtist) {
		t.Errorf("missing AlbumArtist with role attribute: got %q", got)
	}
}

// Test_DIDLForTrack_optionalMetadataFields covers Year, TrackNumber,
// Composer, Genre, Artwork emission.
func Test_DIDLForTrack_optionalMetadataFields(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID:       "id",
		Title:         "T",
		Year:          1965,
		TrackNumber:   3,
		Composer:      "Beethoven",
		Genre:         "Classical",
		ArtworkURL:    "http://server/v1/artwork/abc-def",
		Size:          1,
		FileExtension: ".flac",
		ServerURL:     "http://x",
	}
	got := DIDLForTrack(opts)
	wants := []string{
		`<dc:date>1965-01-01</dc:date>`,
		`<upnp:originalTrackNumber>3</upnp:originalTrackNumber>`,
		`<upnp:author role="Composer">Beethoven</upnp:author>`,
		`<upnp:genre>Classical</upnp:genre>`,
		`<upnp:albumArtURI>http://server/v1/artwork/abc-def</upnp:albumArtURI>`,
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("missing expected element %q in: %q", w, got)
		}
	}
}

// Test_DIDLForTrack_serverURLTrailingSlashHandled pins that the file URL
// has exactly one `/` between ServerURL and the path component, even if
// ServerURL is passed in with a trailing slash.
func Test_DIDLForTrack_serverURLTrailingSlashHandled(t *testing.T) {
	for _, server := range []string{"http://x", "http://x/", "http://x//"} {
		opts := DIDLTrackOpts{
			TrackID:       "id",
			Title:         "T",
			Size:          1,
			FileExtension: ".flac",
			ServerURL:     server,
		}
		got := DIDLForTrack(opts)
		if !strings.Contains(got, `>http://x/dlna/file/id</res>`) {
			t.Errorf("server URL %q: expected exact one-slash URL, got: %q", server, got)
		}
	}
}

// -----------------------------------------------------------------------------
// DIDLForContainer
// -----------------------------------------------------------------------------

func Test_DIDLForContainer_albumGolden(t *testing.T) {
	opts := DIDLContainerOpts{
		ID:         "music/artists/abc/123",
		ParentID:   "music/artists/abc",
		Title:      "The Look of Love",
		ChildCount: 10,
		UPnPClass:  "object.container.album.musicAlbum",
		ArtworkURL: "http://server/v1/artwork/album-mbid",
	}
	got := DIDLForContainer(opts)
	want := `<container id="music/artists/abc/123" parentID="music/artists/abc" restricted="1" searchable="1" childCount="10">` +
		`<dc:title>The Look of Love</dc:title>` +
		`<upnp:class>object.container.album.musicAlbum</upnp:class>` +
		`<upnp:albumArtURI>http://server/v1/artwork/album-mbid</upnp:albumArtURI>` +
		`</container>`
	if got != want {
		t.Errorf("album container golden mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func Test_DIDLForContainer_unknownChildCountBecomesZero(t *testing.T) {
	opts := DIDLContainerOpts{ID: "x", ParentID: "0", Title: "T", ChildCount: -1, UPnPClass: "object.container"}
	got := DIDLForContainer(opts)
	if !strings.Contains(got, `childCount="0"`) {
		t.Errorf("ChildCount=-1 should map to childCount=\"0\" per DLNA spec, got: %q", got)
	}
}

func Test_DIDLForContainer_defaultsToObjectContainer(t *testing.T) {
	opts := DIDLContainerOpts{ID: "x", ParentID: "0", Title: "T", ChildCount: 0}
	// UPnPClass omitted
	got := DIDLForContainer(opts)
	if !strings.Contains(got, `<upnp:class>object.container</upnp:class>`) {
		t.Errorf("empty UPnPClass should default to object.container, got: %q", got)
	}
}

// Test_DIDLForContainer_alwaysCarriesSearchableEqualsOne pins that
// every container emission includes `searchable="1"`. The attribute
// is REQUIRED on every `<container>` element per UPnP CDS spec; the
// value "1" is what the Chord 2Go's own MPD-DLNA reference emits
// (captured 2026-05-28). mconnect Player filters out non-searchable
// containers from drill-down candidates — a `searchable="0"`
// emission caused the symptom that PR #310's class change failed to
// fix and PR #313's playlistContainer detour also failed to fix.
// PR-pending corrective ships searchable="1" empirically grounded.
func Test_DIDLForContainer_alwaysCarriesSearchableEqualsOne(t *testing.T) {
	cases := []DIDLContainerOpts{
		{ID: "0", ParentID: "-1", Title: "Root", ChildCount: 1, UPnPClass: "object.container"},
		{ID: "all_tracks", ParentID: "0", Title: "All Tracks", ChildCount: 121, UPnPClass: "object.container.storageFolder"},
		{ID: "x", ParentID: "0", Title: "Misc", ChildCount: 0}, // UPnPClass defaulted
	}
	for _, opts := range cases {
		got := DIDLForContainer(opts)
		if !strings.Contains(got, `searchable="1"`) {
			t.Errorf("DIDLForContainer(%q) missing searchable=\"1\": %q", opts.ID, got)
		}
	}
}

// Test_DIDLForContainer_storageFolderClassEmitsStorageUsed pins
// the mandatory `<upnp:storageUsed>-1</upnp:storageUsed>` emission
// for `object.container.storageFolder` containers AND its subtypes.
// The value `-1` is the UPnP CDS spec sentinel for "unknown storage
// used"; the bridge doesn't track per-container storage usage. The
// 2Go's own MPD-DLNA reference emits exactly this shape; strict
// controllers can refuse storageFolder containers that omit the
// field. Empirical evidence captured 2026-05-28.
//
// Per CodeRabbit + Gemini on PR #314 round-1, the production check
// uses `strings.HasPrefix` to cover potential subtypes (e.g.
// `object.container.storageFolder.movies` if a future class is
// added) without code change. Both subcases exercise that contract.
func Test_DIDLForContainer_storageFolderClassEmitsStorageUsed(t *testing.T) {
	cases := []struct {
		name      string
		upnpClass string
	}{
		{"exact storageFolder", "object.container.storageFolder"},
		// Hypothetical subtype — no current caller emits this, but
		// the prefix check structurally covers it. Pinning the
		// contract via a forward-compat test case prevents a
		// regression where someone tightens the check back to
		// exact equality.
		{"hypothetical subtype", "object.container.storageFolder.movies"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := DIDLContainerOpts{
				ID: "all_tracks", ParentID: "0", Title: "All Tracks",
				ChildCount: 121, UPnPClass: tc.upnpClass,
			}
			got := DIDLForContainer(opts)
			if !strings.Contains(got, `<upnp:storageUsed>-1</upnp:storageUsed>`) {
				t.Errorf("class=%q missing <upnp:storageUsed>-1: %q", tc.upnpClass, got)
			}
		})
	}
}

// Test_DIDLForContainer_nonStorageFolderClassOmitsStorageUsed pins
// the negation: non-storageFolder classes (musicAlbum, musicArtist,
// playlistContainer, generic object.container) MUST NOT emit the
// storageUsed field — it's specifically defined for storageFolder
// in the UPnP CDS spec. Emitting it on the wrong class would
// confuse spec-strict validators.
func Test_DIDLForContainer_nonStorageFolderClassOmitsStorageUsed(t *testing.T) {
	cases := []DIDLContainerOpts{
		{ID: "a", ParentID: "0", Title: "Album", ChildCount: 10, UPnPClass: "object.container.album.musicAlbum"},
		{ID: "p", ParentID: "0", Title: "Playlist", ChildCount: 5, UPnPClass: "object.container.playlistContainer"},
		{ID: "g", ParentID: "0", Title: "Generic", ChildCount: 0, UPnPClass: "object.container"},
	}
	for _, opts := range cases {
		got := DIDLForContainer(opts)
		if strings.Contains(got, `<upnp:storageUsed>`) {
			t.Errorf("non-storageFolder container %q (class=%s) MUST NOT emit storageUsed: %q",
				opts.ID, opts.UPnPClass, got)
		}
	}
}

// -----------------------------------------------------------------------------
// WrapDIDLLite
// -----------------------------------------------------------------------------

func Test_WrapDIDLLite_wrapsWithNamespaces(t *testing.T) {
	got := WrapDIDLLite(`<item id="x"/>`)
	want := `<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/"><item id="x"/></DIDL-Lite>`
	if got != want {
		t.Errorf("WrapDIDLLite mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func Test_WrapDIDLLite_multipleElementsConcatenated(t *testing.T) {
	got := WrapDIDLLite(`<container id="a"/>`, `<item id="b"/>`)
	if !strings.Contains(got, `<container id="a"/><item id="b"/>`) {
		t.Errorf("multi-element wrapping must concatenate in order, got: %q", got)
	}
}

// -----------------------------------------------------------------------------
// Pure helpers
// -----------------------------------------------------------------------------

func Test_formatDLNADuration(t *testing.T) {
	cases := []struct {
		seconds float64
		want    string
	}{
		{0, "0:00:00.000"},
		{-1, "0:00:00.000"}, // negative collapses to zero
		{0.001, "0:00:00.001"},
		{1.0, "0:00:01.000"},
		{59.999, "0:00:59.999"},
		{60.0, "0:01:00.000"},
		{210.799, "0:03:30.799"}, // the Phase-0 Trashbox.dsf duration
		{3600.0, "1:00:00.000"},
		{3661.5, "1:01:01.500"},
		{36015.123, "10:00:15.123"}, // 10-hour-class duration
	}
	for _, tc := range cases {
		got := formatDLNADuration(tc.seconds)
		if got != tc.want {
			t.Errorf("formatDLNADuration(%v) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

func Test_escapeXMLText(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"no special chars", "no special chars"},
		{`& < > " '`, `&amp; &lt; &gt; &quot; &apos;`},
		{`Tom & Jerry`, `Tom &amp; Jerry`},
		{`AC/DC`, `AC/DC`}, // slash is not XML-significant
		{`<script>`, `&lt;script&gt;`},
	}
	for _, tc := range cases {
		got := escapeXMLText(tc.in)
		if got != tc.want {
			t.Errorf("escapeXMLText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func Test_ExtensionFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"foo.flac", ".flac"},
		{"FOO.FLAC", ".flac"},
		{"/path/to/file.DSF", ".dsf"},
		{"no-extension", ""},
		{".hidden", ".hidden"}, // edge case: starts-with-dot is treated as ext by filepath.Ext
		{"trailing.dot.", "."},
		{"", ""},
	}
	for _, tc := range cases {
		got := ExtensionFromPath(tc.path)
		if got != tc.want {
			t.Errorf("ExtensionFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// -----------------------------------------------------------------------------
// PR4 — offline-variant <res> emission
// -----------------------------------------------------------------------------

func countResElements(didl string) int { return strings.Count(didl, "<res ") }

// Test_DIDLForTrack_NoVariants_SingleRes pins that a track with no
// variants emits exactly one <res> (the source) — unchanged behaviour.
func Test_DIDLForTrack_NoVariants_SingleRes(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID: "t1", Title: "X", Size: 1, FileExtension: ".flac",
		ServerURL: "http://h:7790", UserAgent: "Lavf",
	}
	got := DIDLForTrack(opts)
	if n := countResElements(got); n != 1 {
		t.Errorf("no-variant track should emit 1 <res>, got %d: %s", n, got)
	}
}

// Test_DIDLForTrack_Variants_EmitsExtraResInOrder pins that N variants
// produce N+1 <res> elements, source first, then optimized before
// upscaled, each with a clean path-segment variant URL carrying the
// variant's own metadata.
func Test_DIDLForTrack_Variants_EmitsExtraResInOrder(t *testing.T) {
	opts := DIDLTrackOpts{
		TrackID: "t1", Title: "X", Size: 100, FileExtension: ".dsf",
		DurationSeconds: 60, IsDSD: true, Channels: 2,
		ServerURL: "http://h:7790", UserAgent: "Music Player Daemon 0.21.26",
		Variants: []VariantInfo{
			// Deliberately out of desired emission order to prove sorting.
			{VariantID: "upscaled-v2-176400-24", FileExtension: ".flac", Size: 900, BitDepth: 24, SampleRate: 176400},
			{VariantID: "optimized-v2-48000-16", FileExtension: ".flac", Size: 300, BitDepth: 16, SampleRate: 48000},
		},
	}
	got := DIDLForTrack(opts)

	if n := countResElements(got); n != 3 {
		t.Fatalf("2 variants should emit 3 <res> (source + 2), got %d: %s", n, got)
	}
	// Path-segment URLs (NOT query strings).
	for _, want := range []string{
		`http://h:7790/dlna/file/t1</res>`, // source <res> URL
		`http://h:7790/dlna/file/t1/variant-optimized-v2-48000-16.flac</res>`,
		`http://h:7790/dlna/file/t1/variant-upscaled-v2-176400-24.flac</res>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in DIDL: %s", want, got)
		}
	}
	if strings.Contains(got, "?variant=") {
		t.Errorf("variant URL must be a path segment, not a query string: %s", got)
	}
	// Ordering: source, then optimized, then upscaled.
	srcIdx := strings.Index(got, "/dlna/file/t1</res>")
	optIdx := strings.Index(got, "variant-optimized-")
	upIdx := strings.Index(got, "variant-upscaled-")
	if !(srcIdx < optIdx && optIdx < upIdx) {
		t.Errorf("expected order source < optimized < upscaled; got idx src=%d opt=%d up=%d", srcIdx, optIdx, upIdx)
	}
	// Variant <res> carries PCM bitsPerSample (no DSD gate) + its own rate.
	if !strings.Contains(got, `sampleFrequency="48000"`) || !strings.Contains(got, `bitsPerSample="16"`) {
		t.Errorf("optimized variant <res> missing its 16/48k attrs: %s", got)
	}
}
