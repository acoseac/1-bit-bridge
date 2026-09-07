package admin

import "testing"

func TestParseVariantDeleteRequest_kindPCMAccepted(t *testing.T) {
	req, code, _ := parseVariantDeleteRequest(map[string][]string{
		"prefix": {"DSD"},
		"kind":   {"pcm"},
	})
	if code != "" {
		t.Fatalf("kind=pcm: got code=%q, want empty", code)
	}
	if req.Kind != "pcm" {
		t.Errorf("kind: got %q, want pcm", req.Kind)
	}
}
