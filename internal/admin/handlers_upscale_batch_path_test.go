package admin

import (
	"net/http"
	"testing"
)

// The inspector's batch-submit handler must normalise `path` before it
// reaches the coordinator. It used to forward `req.Path` VERBATIM —
// the only path-bearing admin endpoint that didn't — while every
// read-side endpoint routed through normaliseBrowsePath.
//
// That mattered because the store's prefix helpers treat a prefix that
// trims to empty as whole-library: `{"path": "//"}` enqueued the ENTIRE
// library, while the rollup card rendered from the same submit showed
// 0 tracks.
func TestApiUpscaleBatchSubmit_NormalisesPath(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantPath   string
		wantStatus int
	}{
		{"plain", "MusicA", "MusicA", http.StatusAccepted},
		{"leading slash", "/MusicA", "MusicA", http.StatusAccepted},
		{"many leading slashes", "///MusicA", "MusicA", http.StatusAccepted},
		{"trailing slash", "MusicA/", "MusicA", http.StatusAccepted},
		{"dot segments", "MusicA/Album/../Album", "MusicA/Album", http.StatusAccepted},
		// Slash-only inputs are the whole-library scope, which is a
		// legal submit — but they must arrive as "" rather than as a
		// string the store has to rescue.
		{"empty is whole library", "", "", http.StatusAccepted},
		{"slash is whole library", "/", "", http.StatusAccepted},
		{"double slash is whole library", "//", "", http.StatusAccepted},
		// Traversal and backslashes are refused outright, matching the
		// read-side endpoints rather than being passed to the store.
		{"traversal refused", "../etc", "", http.StatusBadRequest},
		{"slash-hidden traversal refused", "//../etc", "", http.StatusBadRequest},
		{"backslash refused", `Music\Album`, "", http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			stub := &fakeBatchCoordinator{}
			srv.deps.BatchCoordinator = stub

			var res AdminBatchSubmitResult
			code := doJSON(t, srv.Handler(), "POST", "/api/upscale/batch",
				map[string]any{"path": tc.in, "targetRate": 192000, "targetBits": 24}, &res)
			if code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", code, tc.wantStatus)
			}
			if tc.wantStatus != http.StatusAccepted {
				if len(stub.submitCalls) != 0 {
					t.Fatalf("refused input still reached the coordinator as %q",
						stub.submitCalls[0].path)
				}
				return
			}
			if len(stub.submitCalls) != 1 {
				t.Fatalf("Submit calls = %d, want 1", len(stub.submitCalls))
			}
			if got := stub.submitCalls[0].path; got != tc.wantPath {
				t.Fatalf("coordinator saw path %q, want %q", got, tc.wantPath)
			}
		})
	}
}
