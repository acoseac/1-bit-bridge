package dlna

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureDLNALogs redirects slog.Default for the duration of a test and returns
// the accumulated output.
//
// This works because packageLogger is a logging.Component, whose handler
// resolves slog.Default() at LOG time rather than capturing it at package-init
// time (the dynamicHandler contract). A captured-handler shape would make this
// untestable — and would also break the Windows-service log redirect, which is
// why the indirection exists.
func captureDLNALogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// Test_CDS_Browse_EveryDispatchLogsAResponse pins the request/response pairing
// in the Browse telemetry.
//
// logBrowseRequest fires unconditionally at SOAP dispatch, so any arm that
// returns without a matching response line leaves an ORPHAN request in the log
// — which reads like a hang, and telling a hang apart from a clean "no such
// object" is the entire reason this instrumentation exists. The BrowseMetadata
// success arm and both NoSuchObject fault arms used to return silently; strict
// controllers (mconnect / Kazoo class) lead every drill-down with
// BrowseMetadata, so those are precisely the arms under a microscope.
func Test_CDS_Browse_EveryDispatchLogsAResponse(t *testing.T) {
	lib := &countingLib{tracks: []TrackInfo{
		testTrack("t1", "First"), testTrack("t2", "Second"),
	}}
	h := ContentDirectoryHandler(lib, staticServerURL("http://server"))

	cases := []struct {
		name      string
		objectID  string
		flag      string
		wantLines []string
	}{
		{
			name: "BrowseMetadata success", objectID: "1", flag: "BrowseMetadata",
			wantLines: []string{"Browse request", "Browse response"},
		},
		{
			name: "BrowseMetadata unknown object", objectID: "all_tracks", flag: "BrowseMetadata",
			wantLines: []string{"Browse request", "Browse fault"},
		},
		{
			name: "BrowseDirectChildren success", objectID: "1", flag: "BrowseDirectChildren",
			wantLines: []string{"Browse request", "Browse response"},
		},
		{
			name: "BrowseDirectChildren unknown object", objectID: "all_tracks", flag: "BrowseDirectChildren",
			wantLines: []string{"Browse request", "Browse fault"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureDLNALogs(t)
			req := buildBrowseRequest(t, tc.objectID, tc.flag, 0, 0)
			h(httptest.NewRecorder(), req)

			out := buf.String()
			for _, want := range tc.wantLines {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in Browse telemetry for objectID=%q flag=%q.\nGot:\n%s",
						want, tc.objectID, tc.flag, out)
				}
			}
		})
	}
}
