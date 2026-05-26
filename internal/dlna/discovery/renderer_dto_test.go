package discovery

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Golden JSON shape — pins the wire contract iOS PR 6's
// `BridgeRendererDiscovery` will decode. Any change to field
// names / casing requires a paired iOS-side change.
func TestRenderersResponse_GoldenJSON(t *testing.T) {
	resp := RenderersResponse{
		Renderers: []RendererInfo{
			{
				UDN:              "uuid:abcd1234-5678-90ab-cdef-1234567890ab",
				FriendlyName:     "Chord 2go",
				Manufacturer:     "Chord Electronics",
				ModelDescription: "2go Network Music Streamer",
				ModelName:        "2go",
				ControlURL:       "http://192.168.1.42:8080/avtransport/control",
				EventURL:         "http://192.168.1.42:8080/avtransport/event",
				SinkProtocolInfos: []string{
					"http-get:*:audio/x-dsf:*",
					"http-get:*:audio/x-flac:*",
				},
				LastSeenAt: time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC),
			},
		},
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	// Pin field-name shape (camelCase, NOT snake_case).
	wantFields := []string{
		`"renderers"`,
		`"udn":"uuid:abcd1234-5678-90ab-cdef-1234567890ab"`,
		`"friendlyName":"Chord 2go"`,
		`"manufacturer":"Chord Electronics"`,
		`"modelDescription":"2go Network Music Streamer"`,
		`"modelName":"2go"`,
		`"controlURL":"http://192.168.1.42:8080/avtransport/control"`,
		`"eventURL":"http://192.168.1.42:8080/avtransport/event"`,
		`"sinkProtocolInfos":["http-get:*:audio/x-dsf:*","http-get:*:audio/x-flac:*"]`,
		`"lastSeenAt":"2026-05-26T12:00:00Z"`,
	}
	for _, w := range wantFields {
		if !strings.Contains(s, w) {
			t.Errorf("JSON missing %q\ngot: %s", w, s)
		}
	}
}

// Empty-renderers case ships a non-null empty array — iOS-side
// JSONDecoder fails on `null` for non-optional `[]` fields, and
// using `omitempty` would make the response shape inconsistent
// (sometimes `"renderers": []`, sometimes the key omitted entirely).
func TestRenderersResponse_EmptyListShipsAsArray(t *testing.T) {
	resp := RenderersResponse{Renderers: []RendererInfo{}}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"renderers":[]`) {
		t.Errorf("empty list should marshal as [], got: %s", s)
	}
}

// `omitempty` on optional fields keeps the wire compact when the
// bridge couldn't fetch GetProtocolInfo (renderer offline during
// the probe — sinkProtocolInfos is nil). Verify nil slice gets
// omitted (vs explicitly empty array which serializes to []).
func TestRenderersResponse_OmitemptyOnOptionalFields(t *testing.T) {
	resp := RenderersResponse{
		Renderers: []RendererInfo{
			{
				UDN:          "uuid:minimal",
				FriendlyName: "Minimal",
				ControlURL:   "http://192.168.1.42/control",
				LastSeenAt:   time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC),
				// All other fields zero — should omit from wire.
			},
		},
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	// Required fields present
	for _, w := range []string{`"udn":"uuid:minimal"`, `"friendlyName":"Minimal"`, `"controlURL"`, `"lastSeenAt"`} {
		if !strings.Contains(s, w) {
			t.Errorf("JSON missing required field %q\ngot: %s", w, s)
		}
	}
	// Optional fields omitted
	for _, w := range []string{`"manufacturer"`, `"modelDescription"`, `"modelName"`, `"eventURL"`, `"sinkProtocolInfos"`} {
		if strings.Contains(s, w) {
			t.Errorf("optional empty field %q should be omitted, got: %s", w, s)
		}
	}
}
