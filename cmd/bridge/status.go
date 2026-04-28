package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
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

	stats, statsRaw, err := fetchAdminJSON(probeCtx, addr, "/api/stats")
	if err != nil {
		if isConnRefused(err) {
			fmt.Fprintln(stderr, "bridge: not running on", addr)
			fmt.Fprintln(stderr, "  start it with `bridge start` (if installed) or `bridge serve` to run in the foreground.")
			return 1
		}
		fmt.Fprintf(stderr, "status: probe %s: %v\n", addr, err)
		return 1
	}
	endpoints, endpointsRaw, err := fetchAdminJSON(probeCtx, addr, "/api/endpoints")
	if err != nil {
		// Non-fatal — stats already succeeded. Surface to stderr but
		// keep going with empty endpoints in human output, omit field
		// in JSON output.
		fmt.Fprintf(stderr, "status: endpoints probe failed: %v\n", err)
		endpoints = nil
		endpointsRaw = nil
	}

	if *jsonOut {
		envelope := map[string]any{
			"stats":     stats,
			"endpoints": endpoints,
		}
		_ = statsRaw     // kept for future re-use; envelope is the operator-facing surface
		_ = endpointsRaw // ditto
		return writeJSONIndent(stdout, envelope)
	}
	return writeStatusHuman(stdout, stats, endpoints)
}

// fetchAdminJSON does a GET against the loopback admin API and
// decodes the response into a generic map. Returns both the parsed
// map and the raw bytes so a future caller (e.g. `--json` envelope)
// could ship the wire shape verbatim without re-marshalling.
func fetchAdminJSON(ctx context.Context, adminAddr, path string) (map[string]any, []byte, error) {
	url := "http://" + adminAddr + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return out, body, nil
}

// writeStatusHuman renders the probed admin payload as a
// scannable two-column block. Keys not present in the response are
// silently skipped so an older bridge serving fewer fields still
// produces useful output.
func writeStatusHuman(w io.Writer, stats, endpoints map[string]any) int {
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
	if endpoints != nil {
		if list, ok := endpoints["endpoints"].([]any); ok && len(list) > 0 {
			fmt.Fprintln(w, "Endpoints:")
			for _, e := range list {
				m, ok := e.(map[string]any)
				if !ok {
					continue
				}
				url := asString(m["url"])
				kind := asString(m["kind"])
				if url == "" {
					continue
				}
				if kind != "" {
					fmt.Fprintf(w, "  - %s (%s)\n", url, kind)
				} else {
					fmt.Fprintf(w, "  - %s\n", url)
				}
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
// platforms. We can't import syscall.ECONNREFUSED on Windows in a
// portable way, so the check is a substring match on the error
// message — same approach the rest of the cmd/bridge code uses
// for cross-platform error classification.
func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*net.OpError); ok {
		s := err.Error()
		if containsAny(s, "connection refused", "No connection could be made", "actively refused") {
			return true
		}
	}
	s := err.Error()
	return containsAny(s, "connection refused", "No connection could be made", "actively refused")
}

func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && len(haystack) >= len(n) {
			for i := 0; i+len(n) <= len(haystack); i++ {
				if haystack[i:i+len(n)] == n {
					return true
				}
			}
		}
	}
	return false
}
