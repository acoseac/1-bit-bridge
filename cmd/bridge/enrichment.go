// `bridge enrichment` CLI subcommand: see WHICH tracks came back short
// of a cover / artist MBID / release MBID, and re-queue them.
//
// Two subactions:
//
//	bridge enrichment misses [--facet F] [--limit N] [--path P] [--json]
//	bridge enrichment retry  [--path P]
//
// `retry` is the scripted equivalent of the admin console's "Retry
// missing" button — the same three sanctioned enriched_at writers, the
// same 60s guard — so a backfill after a matching fix doesn't require an
// admin session. There is no new enriched_at writer here.
//
// Both prefer a RUNNING bridge over touching the database directly: the
// live process owns the store, and going through the admin API also
// picks up the harvest re-submit and the enricher's in-memory skip-reason
// tally, neither of which is visible from a cold read.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// adminScheme is the transport used to reach the admin API.
//
// Plain HTTP, which is correct for a LOOPBACK-MODE bridge: the console is
// loopback-only by design there (config's validateLoopbackAddress plus the
// loopbackOnly middleware), so there is no TLS and no remote hop.
//
// It is NOT correct for a public-mode bridge, where admin is served over
// autocert HTTPS behind an adminauth session — a plain-HTTP probe there
// gets `400 Client sent an HTTP request to an HTTPS server`. That is
// deliberate rather than an oversight: the CLI holds no session, so
// speaking HTTPS would only turn a 400 into a 401. probeBridge classifies
// both as bridgeUpAdminUnreachable, `misses` says so instead of claiming
// the bridge is down, and `retry` refuses rather than writing to the store
// behind a live process.
//
// Teaching the CLI to authenticate is the real fix and is its own change.
const adminScheme = "http://"

// enrichmentRetryWaitBudget bounds how long `retry` will sit on the
// server's 60s rate guard before giving up. Slightly over the guard so a
// single back-to-back invocation always succeeds rather than racing it.
const enrichmentRetryWaitBudget = 75 * time.Second

// enrichmentRetryPollInterval is how often we re-attempt while waiting
// out the guard. A var, not a const, purely as a test seam — production
// code MUST NOT reassign it (the renameFunc / commandContext convention).
var enrichmentRetryPollInterval = 5 * time.Second

func enrichmentCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: bridge enrichment <misses|retry> [flags]")
		return 2
	}
	switch args[0] {
	case "misses":
		return enrichmentMissesCmd(ctx, args[1:], stdout, stderr)
	case "retry":
		return enrichmentRetryCmd(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, "usage: bridge enrichment <misses|retry> [flags]")
		fmt.Fprintln(stdout, "  misses  list tracks missing a cover, artist MBID or release MBID")
		fmt.Fprintln(stdout, "  retry   re-queue those tracks for enrichment")
		return 0
	default:
		fmt.Fprintf(stderr, "bridge enrichment: unknown subaction %q (want misses|retry)\n", args[0])
		return 2
	}
}

// --- misses ---

func enrichmentMissesCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("enrichment misses", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	facet := fs.String("facet", "", "narrow to one facet: artwork | artist | release")
	limit := fs.Int("limit", 50, "maximum paths to print per facet (0 = counts only)")
	pathScope := fs.String("path", "", "restrict to a library subtree (default: whole library)")
	asJSON := fs.Bool("json", false, "emit JSON instead of a human summary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *facet != "" && !validMissFacetName(*facet) {
		fmt.Fprintf(stderr, "enrichment misses: --facet must be one of artwork, artist, release (got %q)\n", *facet)
		return 2
	}
	if *limit < 0 {
		fmt.Fprintln(stderr, "enrichment misses: --limit must not be negative")
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "enrichment misses: load config: %v\n", err)
		return 2
	}

	report, err := collectMisses(ctx, cfg, *pathScope)
	if err != nil {
		fmt.Fprintf(stderr, "enrichment misses: %v\n", err)
		return 1
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "enrichment misses: encode: %v\n", err)
			return 1
		}
		return 0
	}
	printMissReport(stdout, report, *facet, *limit)
	return 0
}

// missReport is the CLI's own view. Deliberately a separate shape from
// the admin wire DTO: the CLI reads that DTO when a bridge is running,
// but must also be able to build the same report from a cold store.
type missReport struct {
	Path        string              `json:"path"`
	Scanned     int                 `json:"scanned"`
	Missing     int                 `json:"missing"`
	Facets      map[string]int      `json:"facets"`
	Sample      map[string][]string `json:"sample"`
	Truncated   []string            `json:"truncated,omitempty"`
	SkipReasons map[string]int64    `json:"skipReasons,omitempty"`
	// Source records where the numbers came from, because the two paths
	// differ in what they can see: a running bridge also reports the
	// enricher's in-memory skip reasons.
	Source string `json:"source"`
}

// collectMisses prefers the running bridge, falling back to a direct
// read-only walk of the store when the admin API isn't usable.
//
// The probe happens EXACTLY ONCE here and the verdict is passed down. It
// is a real network round-trip with a 200ms budget, and both the dispatch
// below and the Source line need the answer.
func collectMisses(ctx context.Context, cfg *config.Config, scope string) (*missReport, error) {
	addr := adminAddrOf(cfg)
	liveness := probeBridge(ctx, addr)

	if liveness == bridgeAdminUsable {
		if rep, ok, err := missesViaAdmin(ctx, addr, scope); ok {
			return rep, err
		}
	}
	rep, err := missesViaStore(ctx, cfg, scope)
	if err != nil {
		return nil, err
	}
	// Say WHY we read the store rather than the API. "no bridge running"
	// is a claim, and on a public-mode bridge it is a false one.
	if liveness == bridgeUpAdminUnreachable {
		rep.Source = "manifest store (bridge is running; admin API not reachable from the CLI — " +
			"public mode needs a console session, so skip reasons are unavailable)"
	}
	return rep, nil
}

// missesViaAdmin fetches the report from a bridge whose admin API the
// caller has ALREADY established is usable. It does not probe.
func missesViaAdmin(ctx context.Context, addr, scope string) (*missReport, bool, error) {
	// No `limit` — omitting it makes the server apply its own cap, which
	// is what we want and which cannot drift the way a hardcoded copy of
	// that cap would. The CLI does its own --limit trimming at print time.
	endpoint := adminScheme + addr + "/api/enrichment/misses"
	if scope != "" {
		endpoint += "?path=" + url.QueryEscape(scope)
	}
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, true, err
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("admin request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, true, fmt.Errorf("admin returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var wire struct {
		Path    string         `json:"path"`
		Scanned int            `json:"scanned"`
		Missing int            `json:"missing"`
		Facets  map[string]int `json:"facets"`
		Sample  map[string][]struct {
			Path   string   `json:"path"`
			Facets []string `json:"facets"`
		} `json:"sample"`
		Truncated   []string         `json:"truncated"`
		SkipReasons map[string]int64 `json:"skipReasons"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, true, fmt.Errorf("decode admin response: %w", err)
	}
	rep := &missReport{
		Path: wire.Path, Scanned: wire.Scanned, Missing: wire.Missing,
		Facets: wire.Facets, Sample: map[string][]string{},
		Truncated: wire.Truncated, SkipReasons: wire.SkipReasons,
		Source: "running bridge",
	}
	for f, rows := range wire.Sample {
		for _, row := range rows {
			rep.Sample[f] = append(rep.Sample[f], row.Path)
		}
	}
	return rep, true, nil
}

func missesViaStore(ctx context.Context, cfg *config.Config, scope string) (*missReport, error) {
	store, err := openManifestStore(cfg)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer store.Close()

	rep := &missReport{
		Path: scope, Facets: map[string]int{}, Sample: map[string][]string{},
		Source: "manifest store (no bridge running)",
	}
	// Cold read: no sample cap. The CLI is the surface for the FULL
	// enumeration — the admin endpoint is the capped one.
	err = store.StreamTrackMetaRefsUnderPrefix(ctx, scope, func(ref manifest.TrackMetaRef) error {
		rep.Scanned++
		facets := ref.MissFacets()
		if len(facets) == 0 {
			return nil
		}
		rep.Missing++
		for _, f := range facets {
			rep.Facets[f]++
			rep.Sample[f] = append(rep.Sample[f], ref.Path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// missFacetOrder is the display order. Stable so two runs of the CLI are
// diffable.
var missFacetOrder = []string{
	manifest.MissFacetArtwork, manifest.MissFacetArtist, manifest.MissFacetRelease,
}

// selectedFacets narrows the display order to the requested facet, or
// returns all of them when none was requested.
func selectedFacets(facet string) []string {
	if facet == "" {
		return missFacetOrder
	}
	for _, f := range missFacetOrder {
		if f == facet {
			return []string{f}
		}
	}
	return nil
}

func printMissReport(w io.Writer, rep *missReport, facet string, limit int) {
	scope := rep.Path
	if scope == "" {
		scope = "(whole library)"
	}
	facets := selectedFacets(facet)

	fmt.Fprintf(w, "Enrichment misses — %s\n", scope)
	fmt.Fprintf(w, "  source   %s\n", rep.Source)
	fmt.Fprintf(w, "  scanned  %d tracks\n", rep.Scanned)
	fmt.Fprintf(w, "  missing  %d tracks short of at least one field\n\n", rep.Missing)
	for _, f := range facets {
		fmt.Fprintf(w, "  %-8s %d\n", f, rep.Facets[f])
	}

	printSkipReasons(w, rep.SkipReasons)
	if limit > 0 {
		printMissSamples(w, rep, facets, limit)
	}
	// Say plainly when the SERVER capped the sample — a truncated list
	// that looks complete is how a partial view gets mistaken for the
	// whole picture.
	if len(rep.Truncated) > 0 {
		fmt.Fprintf(w, "\n  note: the running bridge capped the sample for %s.\n",
			strings.Join(rep.Truncated, ", "))
		fmt.Fprintln(w, "        stop the bridge and re-run for the full list.")
	}
}

func printSkipReasons(w io.Writer, reasons map[string]int64) {
	if len(reasons) == 0 {
		return
	}
	fmt.Fprintln(w, "\n  why the enricher stopped short (since process start):")
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(w, "    %-16s %d\n", k, reasons[k])
	}
}

func printMissSamples(w io.Writer, rep *missReport, facets []string, limit int) {
	for _, f := range facets {
		paths := rep.Sample[f]
		if len(paths) == 0 {
			continue
		}
		shown := paths
		if len(shown) > limit {
			shown = shown[:limit]
		}
		fmt.Fprintf(w, "\n  %s (%d shown of %d):\n", f, len(shown), rep.Facets[f])
		for _, p := range shown {
			fmt.Fprintf(w, "    %s\n", p)
		}
	}
}

// --- retry ---

func enrichmentRetryCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("enrichment retry", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	pathScope := fs.String("path", "", "restrict to a library subtree (default: whole library)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "enrichment retry: load config: %v\n", err)
		return 2
	}

	addr := adminAddrOf(cfg)
	switch probeBridge(ctx, addr) {
	case bridgeAdminUsable:
		return retryViaAdmin(ctx, addr, *pathScope, stdout, stderr)
	case bridgeUpAdminUnreachable:
		// REFUSE rather than write to the store behind a live bridge's
		// back. Every other writer holds Store.mu, which serialises within
		// ONE process — a second process bypasses it entirely. This is the
		// same call tryLibraryViaAdmin makes for the same reason.
		fmt.Fprintf(stderr, "enrichment retry: a bridge is running on %s but its admin API\n", addr)
		fmt.Fprintln(stderr, "  is not reachable from the CLI (public mode serves admin over HTTPS behind")
		fmt.Fprintln(stderr, "  an adminauth session).")
		fmt.Fprintln(stderr, "  Refusing to reset enriched_at directly — that would write to the store")
		fmt.Fprintln(stderr, "  from a second process, bypassing the single-writer contract.")
		fmt.Fprintln(stderr, "  Either press \"Retry missing\" in the admin console, or:")
		fmt.Fprintln(stderr, "    sudo systemctl stop 1-bit-bridge && bridge enrichment retry && sudo systemctl start 1-bit-bridge")
		return 1
	}

	// Offline: reset directly. Same sanctioned writer the admin handler
	// calls; the harvest re-submit has no offline equivalent (there is no
	// harvest client running to nudge), so say so rather than implying a
	// full retry happened.
	store, err := openManifestStore(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "enrichment retry: open store: %v\n", err)
		return 1
	}
	defer store.Close()
	var n int64
	if *pathScope == "" {
		n, err = store.ResetEnrichedMisses(ctx)
	} else {
		n, err = store.ResetEnrichedMissesUnderPrefix(ctx, *pathScope)
	}
	if err != nil {
		fmt.Fprintf(stderr, "enrichment retry: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "enrichment retry: re-queued %d tracks (no bridge running).\n", n)
	fmt.Fprintln(stdout, "  they enrich on the next `bridge serve`.")
	fmt.Fprintln(stdout, "  note: artist bios and album descriptions are harvest-owned and were NOT")
	fmt.Fprintln(stdout, "        re-submitted — start the bridge and retry there for those.")
	return 0
}

func retryViaAdmin(ctx context.Context, addr, scope string, stdout, stderr io.Writer) int {
	endpoint := adminScheme + addr + "/api/enrichment/retry"
	body := "{}"
	if scope != "" {
		endpoint = adminScheme + addr + "/api/library/enrichment/retry"
		raw, err := json.Marshal(map[string]string{"path": scope})
		if err != nil {
			fmt.Fprintf(stderr, "enrichment retry: encode: %v\n", err)
			return 1
		}
		body = string(raw)
	}

	deadline := time.Now().Add(enrichmentRetryWaitBudget)
	announced := false
	for {
		status, payload, err := postAdminJSON(ctx, endpoint, body)
		// 429 is evaluated BEFORE err on purpose. postAdminJSON reports a
		// body-read failure as an error while still returning the status,
		// and retrying a rate limit needs the status only — the body is
		// noise. Checking err first would make a truncated 429 response
		// skip the wait and report failure, which is the behaviour the
		// round-1 fix to postAdminJSON accidentally introduced.
		if status != http.StatusTooManyRequests && err != nil {
			fmt.Fprintf(stderr, "enrichment retry: %v\n", err)
			return 1
		}
		if status == http.StatusTooManyRequests {
			remain := time.Until(deadline)
			if remain <= 0 {
				fmt.Fprintln(stderr, "enrichment retry: still rate-limited after waiting; giving up.")
				fmt.Fprintln(stderr, "  a retry was triggered recently — the tracks are already re-queued.")
				return 1
			}
			// Say what we are doing. A silent wait on a 60s guard reads
			// as a hang, and the operator kills it before it lands.
			if !announced {
				fmt.Fprintf(stdout, "enrichment retry: waiting on the server's rate limit (up to %ds)…\n",
					int(remain.Seconds()))
				announced = true
			}
			select {
			case <-ctx.Done():
				return 130
			case <-time.After(enrichmentRetryPollInterval):
			}
			continue
		}
		if status >= 400 {
			fmt.Fprintf(stderr, "enrichment retry: admin returned HTTP %d: %s\n", status, strings.TrimSpace(payload))
			return 1
		}
		var ack struct {
			ResetTracks        int64 `json:"resetTracks"`
			HarvestResubmitted bool  `json:"harvestResubmitted"`
		}
		if err := json.Unmarshal([]byte(payload), &ack); err != nil {
			// The server ACCEPTED the retry — only its ack is unreadable. So
			// warn and still exit 0: reporting failure here would make a
			// script re-run a retry that already happened, and the re-run
			// would then hit the 60s guard. Suppress the count rather than
			// print a bogus "0 tracks".
			fmt.Fprintf(stderr, "enrichment retry: accepted (HTTP %d) but the ack was unreadable: %v\n",
				status, err)
			return 0
		}
		fmt.Fprintf(stdout, "enrichment retry: re-queued %d tracks via the running bridge.\n", ack.ResetTracks)
		if ack.HarvestResubmitted {
			fmt.Fprintln(stdout, "  artist bios / album descriptions re-submitted to Atlas.")
		}
		return 0
	}
}

// postAdminJSON posts a JSON body and returns (status, body, transportErr).
// A non-2xx status is NOT an error — the caller distinguishes 429 from a
// real failure.
func postAdminJSON(ctx context.Context, url, body string) (int, string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	// csrfGuard requires this on every body-bearing mutation.
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("admin request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		// Return the status and whatever was read alongside the error, so
		// the caller can still distinguish a 429 it should wait out from a
		// genuine failure even when the body was truncated mid-read.
		return resp.StatusCode, string(raw), fmt.Errorf("read admin response: %w", err)
	}
	return resp.StatusCode, string(raw), nil
}

// --- shared helpers ---

func validMissFacetName(f string) bool {
	switch f {
	case manifest.MissFacetArtwork, manifest.MissFacetArtist, manifest.MissFacetRelease:
		return true
	}
	return false
}

// adminAddrOf resolves the loopback address to probe from an ALREADY
// LOADED config. adminAddrFromCfg (menu.go) takes a config PATH and
// re-loads; both callers here have the parsed config in hand.
//
// probeLoopbackAddr maps a wildcard bind (":7789") to a dialable
// loopback host — without it, a Windows host that can't dial its own
// wildcard address would look like "no bridge running" and silently take
// the offline path against a live process.
func adminAddrOf(cfg *config.Config) string {
	addr := cfg.AdminAddress
	if addr == "" {
		addr = config.DefaultAdminAddress
	}
	return probeLoopbackAddr(addr)
}

// bridgeLiveness is the tri-state result of probing the admin port.
//
// Two states is not enough, and assuming it was shipped a lie. On a
// PUBLIC-MODE bridge the admin listener is HTTPS (autocert) and gated by
// an adminauth session, so a plain-HTTP loopback probe gets `400 Client
// sent an HTTP request to an HTTPS server` and an HTTPS one gets 401. A
// boolean collapses both onto "not running" — and the offline path then
// reports "no bridge running" while the bridge is serving traffic.
type bridgeLiveness int

const (
	// bridgeDown — nothing is listening on the admin port.
	bridgeDown bridgeLiveness = iota
	// bridgeAdminUsable — the admin API answered 200; use it.
	bridgeAdminUsable
	// bridgeUpAdminUnreachable — something IS listening but the API is
	// not usable from here: public-mode TLS, an adminauth session we do
	// not have, or an unhealthy admin. The bridge is RUNNING.
	bridgeUpAdminUnreachable
)

// probeBridge classifies the admin port with the same 200ms "are you up"
// budget tryLibraryViaAdmin uses.
//
// Connection-refused is the only signal that means "not running". Any HTTP
// response at all — including 400 and 401 — means a process is bound to
// that port, which for the purposes of "may I write to the store?" is the
// question that matters.
func probeBridge(ctx context.Context, addr string) bridgeLiveness {
	probeCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, adminScheme+addr+"/api/stats", nil)
	if err != nil {
		return bridgeDown
	}
	resp, err := (&http.Client{Timeout: 200 * time.Millisecond}).Do(req)
	if err != nil {
		if isConnRefused(err) {
			return bridgeDown
		}
		// A timeout or TLS error means something is there and we cannot
		// talk to it. Treat as running — the conservative reading, and the
		// same call tryLibraryViaAdmin makes.
		return bridgeUpAdminUnreachable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return bridgeAdminUsable
	}
	return bridgeUpAdminUnreachable
}
