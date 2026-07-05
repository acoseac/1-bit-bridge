package manifest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestManifestEmptyTracksMarshalsAsArray guards the `"tracks":[]` wire shape.
// Manifest.Tracks has NO `omitempty` (the field must always be present), so a
// nil slice would marshal as `"tracks":null` and break strict downstream
// decoders (Swift's non-optional [Track]). Every producer today assigns a
// non-nil slice (ListTracks / ListTracksPage return `[]Track{}` / a preallocated
// slice); this locks that against a future refactor that builds a Manifest with
// a nil Tracks field.
func TestManifestEmptyTracksMarshalsAsArray(t *testing.T) {
	s := openTempStore(t)
	t.Cleanup(func() { _ = s.Close() })

	cases := []struct {
		name string
		fn   func() (*Manifest, error)
	}{
		{"BuildManifest", func() (*Manifest, error) {
			return BuildManifest(context.Background(), s, nil, time.Time{})
		}},
		{"BuildManifestPage", func() (*Manifest, error) {
			return BuildManifestPage(context.Background(), s, nil, "", 100)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := tc.fn()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if m.Tracks == nil {
				t.Fatalf("%s: Tracks is nil (would marshal as \"tracks\":null)", tc.name)
			}
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(b), `"tracks":null`) {
				t.Errorf("%s: got \"tracks\":null, want \"tracks\":[]; body=%s", tc.name, b)
			}
			if !strings.Contains(string(b), `"tracks":[]`) {
				t.Errorf("%s: missing \"tracks\":[]; body=%s", tc.name, b)
			}
		})
	}
}
