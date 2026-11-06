package manifest

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// TestTheManifestOmitsEnrichmentProgressWhileOff: while enrich.enabled is
// off, no leg of /v1/manifest carries the enrichmentProgress block, which a
// phone reads as enrichment under way: not the stream's full leg, not a
// since delta, with rows or with none, and not the paginated first page; and
// each carries it again once the switch is on (backlog B617). The legs are
// the Provider's, the only builders production calls, and its live source
// answers for both through one helper, enrichmentProgressBlock.
func TestTheManifestOmitsEnrichmentProgressWhileOff(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	for _, path := range []string{"Music/a.flac", "Music/b.flac"} {
		tr := &Track{Path: path, Size: 1, ModTime: time.Now()}
		if err := s.UpsertTrack(ctx, tr); err != nil {
			t.Fatal(err)
		}
		if path == "Music/a.flac" {
			if err := s.MarkEnriched(ctx, tr); err != nil {
				t.Fatal(err)
			}
		}
	}
	roots := []string{"/library/Music"}
	p := NewProvider(s, NewScanner(roots, s, ""))
	var on atomic.Bool
	p.SetEnrichmentEnabledSource(on.Load)

	stream := func(since time.Time) func(*testing.T) map[string]json.RawMessage {
		return func(t *testing.T) map[string]json.RawMessage {
			var buf bytes.Buffer
			if err := p.WriteManifest(ctx, &buf, since); err != nil {
				t.Fatal(err)
			}
			return topLevelKeys(t, buf.Bytes())
		}
	}
	built := func(m *Manifest, err error) func(*testing.T) map[string]json.RawMessage {
		return func(t *testing.T) map[string]json.RawMessage {
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			return topLevelKeys(t, raw)
		}
	}
	legs := []struct {
		name  string
		build func(t *testing.T) map[string]json.RawMessage
	}{
		{"the stream's full leg", func(t *testing.T) map[string]json.RawMessage { return stream(time.Time{})(t) }},
		{"a since delta with rows", func(t *testing.T) map[string]json.RawMessage { return stream(time.Now().Add(-time.Hour))(t) }},
		{"a since delta with no row", func(t *testing.T) map[string]json.RawMessage { return stream(time.Now().Add(time.Hour))(t) }},
		{"the paginated first page", func(t *testing.T) map[string]json.RawMessage {
			return built(p.BuildManifestPage(ctx, "", 10))(t)
		}},
	}
	for _, leg := range legs {
		t.Run(leg.name, func(t *testing.T) {
			on.Store(false)
			if raw, has := leg.build(t)["enrichmentProgress"]; has {
				t.Errorf("enrichmentProgress while enrichment is off: %s", raw)
			}
			on.Store(true)
			if _, has := leg.build(t)["enrichmentProgress"]; !has {
				t.Error("no enrichmentProgress once enrichment is on again")
			}
		})
	}
}

// topLevelKeys decodes a manifest's JSON object by its top-level keys.
func topLevelKeys(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("decode manifest: %v\n%s", err, raw)
	}
	return keys
}
