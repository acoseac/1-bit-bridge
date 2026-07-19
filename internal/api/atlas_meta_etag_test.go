package api

import (
	"strings"
	"testing"
)

// TestValidateAtlasDTO_CapsETag is the Q21 guard: AtlasETag was the one text
// field on the Atlas-push DTOs with no length cap, so a compromised client
// could balloon bridge.db via oversized ETags across many MBIDs.
func TestValidateAtlasDTO_CapsETag(t *testing.T) {
	const uuid = "00000000-0000-0000-0000-000000000000"

	relOver := &atlasReleaseDTO{MBID: uuid, AtlasETag: strings.Repeat("x", atlasMaxETagLen+1)}
	if err := validateReleaseDTO(relOver); err == nil {
		t.Error("oversized release.atlasEtag accepted, want rejected")
	}
	relAt := &atlasReleaseDTO{MBID: uuid, AtlasETag: strings.Repeat("x", atlasMaxETagLen)}
	if err := validateReleaseDTO(relAt); err != nil {
		t.Errorf("at-cap release.atlasEtag rejected: %v", err)
	}

	artOver := &atlasArtistDTO{MBID: uuid, AtlasETag: strings.Repeat("x", atlasMaxETagLen+1)}
	if err := validateArtistDTO(artOver); err == nil {
		t.Error("oversized artist.atlasEtag accepted, want rejected")
	}
}
