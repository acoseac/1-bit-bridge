package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// logTestServer wires a test admin server whose LogPath points at a file
// holding `body`. Passing "" wires no log path at all — the foreground
// `bridge serve` shape, which has no log file to export.
func logTestServer(t *testing.T, body string) *Server {
	t.Helper()
	s, _, _ := newTestServer(t)
	if body != "" {
		s.deps.LogPath = writeTempLog(t, body)
	}
	// A doctor whose report carries absolute paths.
	//
	// Wiring one is load-bearing, not scenery: with DoctorRun nil the bundle
	// takes its "doctor is not wired" branch, so the preflight section
	// contained no paths and the redaction assertions passed while the real
	// endpoint was printing the config dir, library roots and cert path in
	// full under a `redacted: true` header. The leak was found by running the
	// endpoint; this fixture is what makes it a test failure.
	s.deps.DoctorRun = func(context.Context) *DoctorReport {
		return &DoctorReport{
			OK: 1,
			Checks: []DoctorCheck{{
				Name:    "config-dir",
				Status:  "ok",
				Summary: "/Users/arsenie/Library/Application Support/1-bit-bridge is writable",
				Hint:    "see /Users/arsenie/Library/Logs/1-bit-bridge.log",
			}},
		}
	}
	return s
}

// doGet drives the request through the REAL handler chain rather than
// calling the handler method directly, so these cases also cover routing and
// the loopback guard. httptest.NewRequest defaults RemoteAddr to a
// non-loopback address, which loopbackOnly correctly 403s — hence the
// explicit override, matching the sibling admin tests.
func doGet(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// TestLogStatusUnavailableExplainsWhy pins the honest-degradation contract.
// Only a SERVICE install redirects stderr to a file, so "no log file" is a
// routine state and the console has to say which state it is in — a bare
// failure would read as a broken bridge.
func TestLogStatusUnavailableExplainsWhy(t *testing.T) {
	rec := doGet(t, logTestServer(t, ""), "/api/logs/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — 'no log file' is a normal answer, not an error", rec.Code)
	}
	var got logStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Available {
		t.Error("reported a log file with no LogPath wired")
	}
	if got.Reason == "" {
		t.Error("no reason given; the UI would show a dead button with no explanation")
	}
}

// TestLogStatusMissingFileDoesNotBlameTheForeground pins the wording for the
// state bridge.ars.md is actually in: a systemd service install whose unit
// omits `StandardOutput=append:`, so output goes to the journal and the
// configured path holds no file. The old message said the file "is created by
// a service install", which is the one explanation that install can rule out
// by inspection — on Linux the journal has to be named.
func TestLogStatusMissingFileDoesNotBlameTheForeground(t *testing.T) {
	s := logTestServer(t, "")
	s.deps.LogPath = filepath.Join(t.TempDir(), "bridge.log") // wired, never written

	var got logStatusResponse
	if err := json.Unmarshal(doGet(t, s, "/api/logs/status").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Available {
		t.Fatal("reported a log file that was never written")
	}
	if !strings.Contains(got.Reason, s.deps.LogPath) {
		t.Errorf("reason omits the path it looked at: %q", got.Reason)
	}
	if !strings.Contains(got.Reason, noLogFileHint(runtime.GOOS, runningInContainer())) {
		t.Errorf("reason %q does not carry this environment's hint", got.Reason)
	}

	// Every branch asserted BY NAME rather than under a GOOS guard, so all three
	// run on whichever platform the suite runs on. On the Linux CI runner a
	// runtime.GOOS-driven assertion would exercise only the systemd branch and
	// leave the other two — the ones macOS/Windows and container operators
	// actually read — unpinned exactly where the tests execute.
	linux := noLogFileHint("linux", false)
	if !strings.Contains(linux, "journalctl") {
		t.Errorf("linux hint never names the journal, so a systemd operator is sent hunting for a foreground process: %q", linux)
	}
	if strings.Contains(linux, "created by a service install") {
		t.Errorf("linux hint still blames the absence on not being a service install: %q", linux)
	}

	other := noLogFileHint("darwin", false)
	if !strings.Contains(other, "created by a service install") {
		t.Errorf("non-linux hint lost the service-install explanation: %q", other)
	}
	if strings.Contains(other, "journalctl") {
		t.Errorf("non-linux hint offers journalctl, which exists on neither macOS nor Windows: %q", other)
	}

	// A container is Linux but has no systemd and no journalctl binary — the
	// official image runs `bridge serve` in the foreground, so its output is the
	// container's stdout. Verified against the real image on the Docker test
	// host, where HOME is set and this exact branch is what renders.
	container := noLogFileHint("linux", true)
	if strings.Contains(container, "journalctl") {
		t.Errorf("container hint offers journalctl, which is not installed in the image: %q", container)
	}
	if !strings.Contains(container, "docker logs") {
		t.Errorf("container hint does not name where the logs actually are: %q", container)
	}
}

func TestLogStatusReportsSizeAndPath(t *testing.T) {
	s := logTestServer(t, realLogSample+"\n")
	var got logStatusResponse
	if err := json.Unmarshal(doGet(t, s, "/api/logs/status").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Available || got.Path == "" || got.SizeBytes == 0 {
		t.Errorf("status = %+v, want available with a path and non-zero size", got)
	}
	if got.Truncates {
		t.Error("a small log reported as exceeding the scan budget")
	}
}

func TestLogExportFiltersAndCarriesFooter(t *testing.T) {
	s := logTestServer(t, realLogSample+"\n")
	rec := doGet(t, s, "/api/logs/export?level=warn&since=all")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "level=ERROR") || !strings.Contains(body, "level=WARN") {
		t.Error("warn-and-above export is missing ERROR or WARN records")
	}
	if strings.Contains(body, "level=INFO") {
		t.Error("warn-and-above export carried an INFO record")
	}
	// The footer is what tells a later reader whether they hold the whole
	// log and whether it was redacted.
	for _, want := range []string{"1-bit-bridge log export", "# level:", "# redacted:", "# lines:"} {
		if !strings.Contains(body, want) {
			t.Errorf("footer missing %q", want)
		}
	}
}

// TestLogExportRedactDefaultsOffAndHonoursTheFlag pins the asymmetry with the
// bundle: the plain export is the operator reading their own bridge, where the
// absolute paths are usually the point.
func TestLogExportRedactDefaultsOffAndHonoursTheFlag(t *testing.T) {
	s := logTestServer(t, realLogSample+"\n")

	plain := doGet(t, s, "/api/logs/export?level=all&since=all").Body.String()
	if !strings.Contains(plain, "/Users/arsenie/medialibtest") {
		t.Error("default export redacted an absolute path; the plain export defaults to raw")
	}
	if !strings.Contains(plain, "# redacted:   false") {
		t.Error("footer does not record that this export was unredacted")
	}

	red := doGet(t, s, "/api/logs/export?level=all&since=all&redact=true").Body.String()
	if strings.Contains(red, "/Users/arsenie") {
		t.Error("redact=true left an absolute path in the output")
	}
	if strings.Contains(red, "207.241.224.2") {
		t.Error("redact=true left a public IP in the output")
	}
}

// TestLogExportRejectsUnknownParameters: a typo must not silently widen the
// export. Defaulting `level=warnn` to "everything" would hand back a much
// larger, less filtered file than was asked for.
func TestLogExportRejectsUnknownParameters(t *testing.T) {
	s := logTestServer(t, realLogSample+"\n")
	for _, q := range []string{
		"/api/logs/export?level=warnn",
		"/api/logs/export?since=3h",
		"/api/logs/export?redact=maybe",
	} {
		if code := doGet(t, s, q).Code; code != http.StatusBadRequest {
			t.Errorf("%s → %d, want 400", q, code)
		}
	}
}

func TestLogExportWithoutLogFileIs404(t *testing.T) {
	if code := doGet(t, logTestServer(t, ""), "/api/logs/export").Code; code != http.StatusNotFound {
		t.Errorf("export with no log file = %d, want 404", code)
	}
}

// TestLogBundleCarriesContextAndRedactsByDefault pins the bundle's shape and
// its opposite default: it exists to be sent somewhere, so the safe default is
// the one that survives being pasted into a public issue.
func TestLogBundleCarriesContextAndRedactsByDefault(t *testing.T) {
	s := logTestServer(t, realLogSample+"\n")
	rec := doGet(t, s, "/api/logs/bundle")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"bug report bundle",
		"server version:",
		"protocol version:",
		"platform:",
		"-- diagnostics ---",
		"-- preflight ---",
		"-- log (last ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bundle missing section/field %q", want)
		}
	}
	if !strings.Contains(body, "redacted:         true") {
		t.Error("bundle header does not record that it was redacted")
	}
	// Every section, not just the log tail. The header PROMISES absolute
	// paths are replaced; a section that ignores that promise is worse than
	// no redaction, because the operator trusts the label and posts the file.
	if strings.Contains(body, "/Users/arsenie") {
		t.Errorf("bundle leaked an absolute path under `redacted: true`:\n%s",
			leakContext(body, "/Users/arsenie"))
	}
	// And the preflight section must actually be populated — an empty one
	// would satisfy the assertion above for the wrong reason.
	if !strings.Contains(body, "config-dir") {
		t.Error("preflight section is empty, so the redaction assertion proved nothing")
	}
}

// leakContext returns the offending line, so a failure names WHERE the leak is
// rather than dumping the whole bundle.
func leakContext(body, needle string) string {
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	return "(not found)"
}

func TestLogBundleRedactionCanBeDisabled(t *testing.T) {
	s := logTestServer(t, realLogSample+"\n")
	body := doGet(t, s, "/api/logs/bundle?redact=false").Body.String()
	if !strings.Contains(body, "/Users/arsenie/medialibtest") {
		t.Error("redact=false still redacted the bundle")
	}
}

// TestLogBundleWorksWithoutALogFile: the bundle's value is the CONTEXT
// (version, counters, preflight), so a bridge with no log file must still get
// a usable report rather than a 404.
func TestLogBundleWorksWithoutALogFile(t *testing.T) {
	rec := doGet(t, logTestServer(t, ""), "/api/logs/bundle")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the bundle is still useful without a log", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "server version:") {
		t.Error("bundle without a log file lost its context sections")
	}
	if !strings.Contains(body, "no log file") && !strings.Contains(body, "logs to the terminal") {
		t.Errorf("bundle does not explain the missing log:\n%s", body)
	}
}
