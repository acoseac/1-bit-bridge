// Log export endpoints for the Diagnostics page.
//
//	GET /api/logs/status  — is there a log file, where, how big
//	GET /api/logs/export  — filtered log as a download
//	GET /api/logs/bundle  — the bug-report bundle
//
// # Why a status endpoint exists at all
//
// The bridge logs to STDERR (logging.Init), and only a SERVICE install
// redirects that to a file — the unit templates set StandardOutput/
// StandardError to packaging.DefaultLogPath(). An operator running
// `bridge serve` in a terminal has no log file whatsoever, and a Download
// button that 404s with no explanation would read as a bug rather than as the
// truth about how this bridge was started. The status call lets the UI say
// which situation it is in before the operator clicks.
package admin

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/version"
)

// maxBundleLogLines caps the log tail carried in a bug-report bundle.
//
// The bundle's job is to be pasteable into an issue, so it takes the most
// RECENT lines rather than the first ones the scan meets — a head-truncated
// bundle would carry the least relevant end of the window.
const maxBundleLogLines = 2000

// bundleLogWindow is how far back the bundle's log tail reaches.
const bundleLogWindow = 24 * time.Hour

// logStatusResponse tells the UI what it can offer.
type logStatusResponse struct {
	Available bool `json:"available"`
	// Reason is filled only when Available is false, so the page can explain
	// rather than showing a dead button.
	Reason string `json:"reason,omitempty"`
	// Path is shown so an operator whose service writes somewhere else can
	// see the mismatch instead of concluding logging is broken.
	Path       string `json:"path,omitempty"`
	SizeBytes  int64  `json:"sizeBytes,omitempty"`
	ModifiedAt string `json:"modifiedAt,omitempty"`
	// Truncates reports that the file is larger than one export can scan, so
	// the UI can warn BEFORE the download rather than only in the footer.
	Truncates bool `json:"truncates,omitempty"`
}

// resolveLogFile returns the log path and its stat, or a reason it cannot.
func (s *Server) resolveLogFile() (string, os.FileInfo, string) {
	path := strings.TrimSpace(s.deps.LogPath)
	if path == "" {
		return "", nil, "this bridge has no log file configured — it logs to the terminal it was started from"
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, nil, "no log file at " + path + " — " + noLogFileHint(runtime.GOOS)
		}
		return path, nil, "cannot read " + path + ": " + err.Error()
	}
	if info.IsDir() {
		return path, nil, path + " is a directory, not a log file"
	}
	return path, info, ""
}

// noLogFileHint explains how an install ends up with no file at the path the
// unit templates use.
//
// Naming only the foreground case is wrong on Linux: a hand-written systemd
// unit that omits `StandardOutput=append:` sends the bridge's output to the
// journal, which is a SERVICE install with no log file — so telling that
// operator "the file is created by a service install" contradicts what they
// can see. bridge.ars.md is exactly that unit (its runbook row reads "systemd
// journal — no separate file"), and the old wording sent a live session
// looking for a foreground process that did not exist.
//
// Takes the GOOS rather than reading it, so BOTH branches are testable from
// whichever platform the suite runs on. The journal wording is the half that
// matters, and it is exactly the half a `runtime.GOOS` check would skip on the
// darwin box this is developed on.
func noLogFileHint(goos string) string {
	if goos == "linux" {
		return "a foreground `bridge serve` logs to its terminal, and a systemd unit without `StandardOutput=append:` logs to the journal instead — read it with `journalctl -u 1-bit-bridge`"
	}
	return "the file is created by a service install; a foreground `bridge serve` logs to its terminal"
}

// apiLogStatus handles GET /api/logs/status.
func (s *Server) apiLogStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	path, info, reason := s.resolveLogFile()
	if reason != "" {
		writeJSON(w, http.StatusOK, logStatusResponse{Available: false, Reason: reason, Path: path})
		return
	}
	writeJSON(w, http.StatusOK, logStatusResponse{
		Available:  true,
		Path:       path,
		SizeBytes:  info.Size(),
		ModifiedAt: info.ModTime().UTC().Format(time.RFC3339),
		Truncates:  info.Size() > maxLogScanBytes,
	})
}

// parseLogQuery reads the shared level/since/redact parameters.
//
// Unknown values are REJECTED rather than defaulted. A typo in `level` that
// silently fell back to "everything" would handed back a far larger and less
// redacted file than the operator asked for — the failure direction that
// matters when the output is about to be shared.
func parseLogQuery(r *http.Request, defaultRedact bool) (*logFilter, string, error) {
	q := r.URL.Query()

	lvl, ok := parseLogLevelName(q.Get("level"))
	if !ok {
		return nil, "", fmt.Errorf("level must be one of error, warn, info, debug")
	}
	since, sinceLabel, ok := parseSincePreset(q.Get("since"), time.Now())
	if !ok {
		return nil, "", fmt.Errorf("since must be one of 15m, 1h, 24h, 7d, all")
	}
	redact := defaultRedact
	if raw := q.Get("redact"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, "", fmt.Errorf("redact must be true or false")
		}
		redact = v
	}
	return &logFilter{minLevel: lvl, since: since, redact: redact}, sinceLabel, nil
}

// apiLogExport handles GET /api/logs/export.
//
// Streams rather than buffering: the log is unbounded (nothing rotates it),
// and the admin server deliberately sets no WriteTimeout, so a large export
// runs to completion instead of being torn mid-flight.
func (s *Server) apiLogExport(w http.ResponseWriter, r *http.Request) {
	path, _, reason := s.resolveLogFile()
	if reason != "" {
		writeError(w, http.StatusNotFound, "no-log-file", reason)
		return
	}
	// Default OFF for the plain export: this is the operator reading their own
	// bridge's log on a loopback console, where the absolute paths are usually
	// the whole point. The bundle flips the default — see apiLogBundle.
	f, sinceLabel, err := parseLogQuery(r, false)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}

	name := fmt.Sprintf("bridge-log-%s-%s.log", levelLabel(f.minLevel),
		time.Now().UTC().Format("20060102-150405"))
	setDownloadHeaders(w, "text/plain; charset=utf-8", name)

	st, err := streamFilteredLog(w, path, f)
	if err != nil {
		// Headers are already sent, so this cannot become a 5xx. Log it and
		// let the footer carry what did happen — a truncated file with an
		// explanatory footer beats one that just stops.
		logger.Warn("log export stream", "err", err)
		fmt.Fprintf(w, "\n# EXPORT FAILED PART-WAY: %v\n", err)
	}
	writeExportFooter(w, st, f, sinceLabel)
}

// apiLogBundle handles GET /api/logs/bundle.
//
// One pasteable text file: what the bridge is, what its counters say, what
// the preflight checks say, and the recent log — the context that makes the
// log interpretable by someone who is not sitting at the machine.
//
// Redaction defaults ON here, unlike the plain export. The asymmetry is the
// point: a bundle exists to be sent somewhere, so the safe default is the one
// that survives being pasted into a public issue. The operator can still turn
// it off for their own use.
func (s *Server) apiLogBundle(w http.ResponseWriter, r *http.Request) {
	f, _, err := parseLogQuery(r, true)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad-request", err.Error())
		return
	}
	// The bundle fixes its own window and level regardless of what the query
	// asked: it is a report shape, not a general export. Only `redact` is
	// honoured, because that is the one choice whose right answer depends on
	// where the file is going.
	f.minLevel = levelDebug
	f.since = time.Now().Add(-bundleLogWindow)

	name := "bridge-bugreport-" + time.Now().UTC().Format("20060102-150405") + ".txt"
	setDownloadHeaders(w, "text/plain; charset=utf-8", name)

	s.writeBundleHeader(w, f.redact)
	s.writeBundleDiagnostics(w)
	s.writeBundleDoctor(w, r, f.redact)
	s.writeBundleLogTail(w, f)
}

// bundleText passes free text through the redactor when the bundle is
// redacted.
//
// Load-bearing for every non-log section. Redaction used to be applied only
// inside streamFilteredLog, so the header promised "absolute paths replaced"
// and the preflight section then printed the config dir, the library roots and
// the cert path in full. A bundle that CLAIMS to be redacted and is not is
// worse than one that makes no claim — the operator trusts the label and posts
// it. Any future section that emits a string rather than a number goes through
// here. (Found by running the real endpoint, not by the handler tests, whose
// server has no doctor wired.)
func bundleText(s string, redact bool) string {
	if !redact {
		return s
	}
	return redactLogLine(s)
}

func (s *Server) writeBundleHeader(w http.ResponseWriter, redacted bool) {
	fmt.Fprintf(w, "1-bit-bridge bug report bundle\n")
	fmt.Fprintf(w, "==============================\n\n")
	fmt.Fprintf(w, "generated:        %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "server version:   %s\n", version.ServerVersion)
	fmt.Fprintf(w, "protocol version: %d\n", version.ProtocolVersion)
	fmt.Fprintf(w, "min client:       %s\n", version.MinClientVersion)
	fmt.Fprintf(w, "platform:         %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(w, "go runtime:       %s\n", runtime.Version())
	if !s.deps.StartedAt.IsZero() {
		fmt.Fprintf(w, "uptime:           %s\n", time.Since(s.deps.StartedAt).Round(time.Second))
	}
	fmt.Fprintf(w, "redacted:         %t\n", redacted)
	if redacted {
		fmt.Fprintf(w, "                  absolute paths and non-loopback IPs replaced.\n")
		fmt.Fprintf(w, "                  This is a courtesy pass, not an anonymiser — read\n")
		fmt.Fprintf(w, "                  the file before posting it somewhere public.\n")
	}
	fmt.Fprintln(w)
}

func (s *Server) writeBundleDiagnostics(w http.ResponseWriter) {
	d := s.diagnosticsSnapshot()
	fmt.Fprintf(w, "-- diagnostics ----------------------------------------------\n")
	fmt.Fprintf(w, "sqlite lock wait p50/p99:  %.3f / %.3f s\n", d.SQLiteLockWaitP50, d.SQLiteLockWaitP99)
	if d.MBCacheLookups > 0 {
		fmt.Fprintf(w, "enrichment cache:          %.1f%% hit over %d lookups\n",
			d.MBCacheHitRatio*100, d.MBCacheLookups)
	} else {
		fmt.Fprintf(w, "enrichment cache:          no lookups yet\n")
	}
	fmt.Fprintf(w, "conversion pool:           %d in flight, %d completed, p50/p99 %.3f / %.3f s\n",
		d.UpscaleJobsInFlight, d.UpscaleJobsCompletedTotal, d.UpscaleDurationP50, d.UpscaleDurationP99)
	fmt.Fprintf(w, "tailscale:                 %s, %d peers online\n",
		d.TailscaleNodeState, d.TailscalePeersOnline)

	// Sorted so two bundles from the same bridge diff cleanly — Go map order
	// is randomised, and a report whose lines shuffle between runs is
	// needlessly hard to compare.
	levels := make([]string, 0, len(d.LogEventCounts))
	for k := range d.LogEventCounts {
		levels = append(levels, k)
	}
	sort.Strings(levels)
	fmt.Fprintf(w, "log events since start:    ")
	if len(levels) == 0 {
		fmt.Fprintf(w, "none recorded")
	}
	for i, k := range levels {
		if i > 0 {
			fmt.Fprintf(w, ", ")
		}
		fmt.Fprintf(w, "%s=%d", strings.ToLower(k), d.LogEventCounts[k])
	}
	fmt.Fprintf(w, "\n\n")
}

func (s *Server) writeBundleDoctor(w http.ResponseWriter, r *http.Request, redact bool) {
	fmt.Fprintf(w, "-- preflight ------------------------------------------------\n")
	if s.deps.DoctorRun == nil {
		fmt.Fprintf(w, "doctor is not wired on this bridge\n\n")
		return
	}
	rep := s.deps.DoctorRun(r.Context())
	if rep == nil {
		fmt.Fprintf(w, "doctor produced no report\n\n")
		return
	}
	// Summary and Hint are the fields that carry paths — the config dir, the
	// library roots, the cert location — so both go through bundleText. Name
	// and Status are fixed vocabulary.
	for _, c := range rep.Checks {
		fmt.Fprintf(w, "[%-4s] %-22s %s\n", c.Status, c.Name, bundleText(c.Summary, redact))
		if c.Hint != "" {
			fmt.Fprintf(w, "         ↳ %s\n", bundleText(c.Hint, redact))
		}
	}
	fmt.Fprintf(w, "\n%d ok, %d warn, %d fail\n\n", rep.OK, rep.Warn, rep.Fail)
}

func (s *Server) writeBundleLogTail(w http.ResponseWriter, f *logFilter) {
	fmt.Fprintf(w, "-- log (last %s, most recent %d lines) ----------------------\n",
		bundleLogWindow, maxBundleLogLines)
	path, _, reason := s.resolveLogFile()
	if reason != "" {
		// The reason embeds the resolved log path, so it is redactable text
		// like any other — not a status string.
		fmt.Fprintf(w, "%s\n", bundleText(reason, f.redact))
		return
	}
	// Collected through a ring rather than streamed, because the bundle wants
	// the TAIL of the window and the scan runs forward.
	ring := &tailRing{max: maxBundleLogLines}
	st, err := streamFilteredLog(ring, path, f)
	if err != nil {
		fmt.Fprintf(w, "reading log failed: %v\n", err)
	}
	for _, line := range ring.lines() {
		fmt.Fprintln(w, line)
	}
	if st.Emitted > maxBundleLogLines {
		fmt.Fprintf(w, "\n# (%d earlier lines in this window omitted — download the full log for those)\n",
			st.Emitted-maxBundleLogLines)
	}
	writeExportFooter(w, st, f, "last "+bundleLogWindow.String())
}

// tailRing is an io.Writer that keeps only the last `max` lines written to it.
//
// streamFilteredLog writes whole lines followed by '\n', so splitting on the
// newline is exact rather than heuristic — this is not a general-purpose
// line-buffering writer and is not exported.
type tailRing struct {
	max  int
	buf  []string
	next int
	full bool
	// partial accumulates a write that did not end on a newline boundary.
	// bufio flushes on its own buffer size, not on line boundaries, so a line
	// can and does arrive split across two Write calls.
	partial strings.Builder
}

// Write splits p on newlines, retaining only the last `max` lines.
//
// Scans p in place rather than building one string per call. streamFilteredLog
// writes through a 64 KiB bufio.Writer, so a call arrives per FLUSH, not per
// line — concatenating `partial + string(p)` allocated (and immediately
// discarded) a 64 KiB string on every one.
//
// Each retained line is also copied explicitly. The obvious alternative —
// slicing lines out of one accumulated buffer — makes every retained line
// ALIAS that buffer, so keeping a single line pins the whole 64 KiB behind it.
// Copying costs exactly the bytes actually kept and lets the rest be collected.
func (t *tailRing) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			// No terminator in this chunk: hold it for the next call.
			t.partial.Write(p)
			break
		}
		if t.partial.Len() > 0 {
			// A line split across calls — finish it from the carry.
			t.partial.Write(p[:i])
			t.push(t.partial.String())
			t.partial.Reset()
		} else {
			t.push(string(p[:i]))
		}
		p = p[i+1:]
	}
	return n, nil
}

func (t *tailRing) push(line string) {
	if t.max <= 0 {
		return
	}
	if len(t.buf) < t.max {
		t.buf = append(t.buf, line)
		return
	}
	t.buf[t.next] = line
	t.next = (t.next + 1) % t.max
	t.full = true
}

// lines returns the retained lines in write order.
func (t *tailRing) lines() []string {
	if rem := t.partial.String(); rem != "" {
		t.push(rem)
		t.partial.Reset()
	}
	if !t.full {
		return t.buf
	}
	out := make([]string, 0, len(t.buf))
	out = append(out, t.buf[t.next:]...)
	out = append(out, t.buf[:t.next]...)
	return out
}
