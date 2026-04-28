package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// statusCmd probes the running bridge's loopback admin API for a
// quick terminal-readable summary. Exposed so operators don't need
// to open the admin console for "is it up, what's the track count,
// what endpoints does it advertise".
//
// `--json` dumps the underlying API response so scripts can consume
// it without regexing prose. Default human output prefers a stable
// vertical layout that survives terminal-width changes.
//
// Failure modes: connection refused → "service not running"; any
// other error surfaces as the actual transport failure (so an
// admin-bound-elsewhere or token-mismatch is diagnosable).
func statusCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	jsonOut := fs.Bool("json", false, "print the raw API response as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "status: config load: %v\n", err)
		return 2
	}
	addr := cfg.AdminAddress
	if addr == "" {
		addr = config.DefaultAdminAddress
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var stats map[string]any
	if err := fetchAdminJSON(probeCtx, addr, "/api/stats", &stats); err != nil {
		if isConnRefused(err) {
			fmt.Fprintln(stderr, "bridge: not running on", addr)
			fmt.Fprintln(stderr, "  start it with `bridge start` (if installed) or `bridge serve` to run in the foreground.")
			return 1
		}
		fmt.Fprintf(stderr, "status: probe %s: %v\n", addr, err)
		return 1
	}
	// /api/endpoints returns a JSON ARRAY (`[]adminEndpointEntry`),
	// not an object — decoding into map[string]any leaves the value
	// nil and the human-readable output never paints the endpoints
	// section. Decode into []any to match the wire shape (Qodo Bug
	// on PR #78).
	var endpoints []any
	if err := fetchAdminJSON(probeCtx, addr, "/api/endpoints", &endpoints); err != nil {
		// Non-fatal — stats already succeeded. Surface to stderr but
		// keep going with empty endpoints in human output, omit field
		// in JSON output.
		fmt.Fprintf(stderr, "status: endpoints probe failed: %v\n", err)
		endpoints = nil
	}

	if *jsonOut {
		envelope := map[string]any{
			"stats":     stats,
			"endpoints": endpoints,
		}
		return writeJSONIndent(stdout, envelope)
	}
	return writeStatusHuman(stdout, stats, endpoints)
}

// fetchAdminJSON does a GET against the loopback admin API and
// decodes the response into the caller-supplied destination. Caller
// passes &someMap or &someSlice depending on the endpoint's wire
// shape — /api/stats is an object, /api/endpoints is an array.
func fetchAdminJSON(ctx context.Context, adminAddr, path string, dst any) error {
	url := "http://" + adminAddr + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// writeStatusHuman renders the probed admin payload as a
// scannable two-column block. Keys not present in the response are
// silently skipped so an older bridge serving fewer fields still
// produces useful output. `endpoints` is the JSON-decoded
// `[]adminEndpointEntry` from /api/endpoints — each element is a
// map with `url` + `class` keys.
func writeStatusHuman(w io.Writer, stats map[string]any, endpoints []any) int {
	rows := [][2]string{
		{"Library", asString(stats["libraryName"])},
		{"Server version", fmt.Sprintf("%s (protocol v%v)", asString(stats["serverVersion"]), stats["protocolVersion"])},
		{"Uptime", uptimeFromSec(stats["uptimeSec"])},
		{"Tracks indexed", fmt.Sprintf("%v", asNumber(stats["tracksIndexed"]))},
		{"Scanning", scanStateLabel(stats)},
		{"Listen address", asString(stats["listenAddress"])},
		{"Admin address", asString(stats["adminAddress"])},
		{"Fingerprint", asString(stats["fingerprint"])},
	}
	for _, r := range rows {
		if r[1] == "" {
			continue
		}
		fmt.Fprintf(w, "%-18s %s\n", r[0]+":", r[1])
	}
	if len(endpoints) > 0 {
		fmt.Fprintln(w, "Endpoints:")
		for _, e := range endpoints {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			url := asString(m["url"])
			class := asString(m["class"])
			if url == "" {
				continue
			}
			if class != "" {
				fmt.Fprintf(w, "  - %s (%s)\n", url, class)
			} else {
				fmt.Fprintf(w, "  - %s\n", url)
			}
		}
	}
	return 0
}

func writeJSONIndent(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(w, "status: encode JSON: %v\n", err)
		return 1
	}
	return 0
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func asNumber(v any) any {
	if v == nil {
		return 0
	}
	return v
}

// uptimeFromSec turns the apiStats `uptimeSec` field into a human
// duration. Float because JSON numbers decode as float64; int64
// callers cast cleanly.
func uptimeFromSec(v any) string {
	if v == nil {
		return ""
	}
	var sec int64
	switch n := v.(type) {
	case float64:
		sec = int64(n)
	case int64:
		sec = n
	case int:
		sec = int64(n)
	default:
		return fmt.Sprintf("%v", v)
	}
	d := time.Duration(sec) * time.Second
	return d.Round(time.Second).String()
}

func scanStateLabel(stats map[string]any) string {
	scanning, _ := stats["isScanning"].(bool)
	if !scanning {
		return "idle"
	}
	progress := stats["scanProgress"]
	if progress == nil {
		return "scanning"
	}
	return fmt.Sprintf("scanning (%v tracks indexed so far)", progress)
}

// isConnRefused recognises the "service not running" case across
// platforms. Prefer the typed `syscall.ECONNREFUSED` wrapped by
// the net stack — `errors.Is` on it works cross-platform (the
// Windows ECONNREFUSED is the same constant value as POSIX), so
// the substring fallback is only there to catch wrapping that
// strips the syscall.Errno (rare; some intermediaries wrap with
// fmt.Errorf("%v")). Gemini Medium on PR #78 flagged the prior
// implementation as fragile; this is the more robust shape.
func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	// Substring fallback for stripped-errno cases. Conservative
	// list — only the canonical strings the net stack uses.
	s := err.Error()
	for _, marker := range []string{
		"connection refused",
		"No connection could be made",
		"actively refused",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
