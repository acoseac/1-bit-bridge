package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
	bridgefs "github.com/acoseac/1-bit-bridge/internal/fs"
	"github.com/acoseac/1-bit-bridge/internal/manifest"
)

// libraryAddFailedFormat is the stderr error template for every
// `bridge library add` failure path (live admin POST, headless mutation,
// scan-trigger). One literal so a future copy edit only happens once.
const libraryAddFailedFormat = "library add: %v\n"

// libraryCmd dispatches `bridge library <add|remove> <path>`. CLI
// front-end for the same library-roots mutation the admin API
// (POST /api/roots / DELETE /api/roots) exposes. The wrapper
// auto-detects whether `bridge serve` is running on the configured
// admin address: if so, the live server's mutation handler runs
// (clean teardown, scan reschedule); if not, the CLI mutates
// bridge.yaml + the manifest store directly so headless Ansible /
// shell-script setups work without spinning up a server first.
func libraryCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "library: missing subcommand")
		fmt.Fprintln(stderr, "  bridge library add <path>")
		fmt.Fprintln(stderr, "  bridge library remove <path>")
		return 2
	}
	switch args[0] {
	case "add":
		return libraryAddCmd(ctx, args[1:], stdout, stderr)
	case "remove":
		return libraryRemoveCmd(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "library: unknown subcommand %q\n", args[0])
		return 2
	}
}

func libraryAddCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("library add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "library add: requires exactly one path")
		return 2
	}
	target, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, libraryAddFailedFormat, err)
		return 2
	}
	info, err := os.Stat(target)
	if err != nil {
		fmt.Fprintf(stderr, libraryAddFailedFormat, err)
		return 2
	}
	if !info.IsDir() {
		fmt.Fprintf(stderr, "library add: %s is not a directory\n", target)
		return 2
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "library add: config load: %v\n", err)
		return 2
	}
	if running, code := tryLibraryViaAdmin(ctx, cfg, http.MethodPost, target, stdout, stderr); running {
		return code
	}

	// Offline path. Re-validate, mutate config + manifest directly.
	if slices.Contains(cfg.LibraryRoots, target) {
		fmt.Fprintf(stdout, "library add: %s is already configured (no-op)\n", target)
		return 0
	}
	newList := append(append([]string(nil), cfg.LibraryRoots...), target)
	if err := bridgefs.ValidateRoots(newList); err != nil {
		fmt.Fprintf(stderr, libraryAddFailedFormat, err)
		return 1
	}
	willTransition := len(cfg.LibraryRoots) == 1
	if willTransition {
		// 1 → N: stored path form changes from bare "Artist/…" to
		// "<basename>/Artist/…". Wipe so the next scan repopulates
		// in the new form. Same rationale as the admin API path.
		if err := wipeManifest(ctx, cfg); err != nil {
			fmt.Fprintf(stderr, "library add: wipe manifest: %v\n", err)
			return 1
		}
	}
	cfg.LibraryRoots = newList
	if err := cfg.Save(*configPath); err != nil {
		fmt.Fprintf(stderr, "library add: save config: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "library add: %s added; next start (or `bridge scan`) will index it.\n", target)
	return 0
}

func libraryRemoveCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("library remove", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "bridge.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "library remove: requires exactly one path")
		return 2
	}
	target, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "library remove: %v\n", err)
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "library remove: config load: %v\n", err)
		return 2
	}
	if running, code := tryLibraryViaAdmin(ctx, cfg, http.MethodDelete, target, stdout, stderr); running {
		return code
	}

	// Offline path.
	idx := slices.Index(cfg.LibraryRoots, target)
	if idx < 0 {
		fmt.Fprintf(stdout, "library remove: %s is not configured (no-op)\n", target)
		return 0
	}
	newList := slices.Delete(append([]string(nil), cfg.LibraryRoots...), idx, idx+1)
	if len(newList) == 0 {
		fmt.Fprintln(stderr, "library remove: refusing to remove the last root (use `bridge init` to reconfigure)")
		return 1
	}
	willCollapse := len(cfg.LibraryRoots) > 1 && len(newList) == 1
	store, err := openManifestStore(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "library remove: open store: %v\n", err)
		return 1
	}
	defer store.Close()

	if willCollapse {
		if err := store.WipeAllTracks(ctx); err != nil {
			fmt.Fprintf(stderr, "library remove: wipe manifest: %v\n", err)
			return 1
		}
	} else {
		// Multi-root → multi-root: the manifest stores rows under
		// "<basename>/Artist/Album/Track". `bridgefs.ValidateRoots`
		// (which the admin API runs at add-time) refuses two roots
		// with the same basename, so once the user gets here every
		// surviving root has a unique basename. Defensive guard
		// anyway: if a hand-edited yaml violated the invariant
		// (e.g. operator bypassed the CLI/admin and edited
		// libraryRoots: directly), pruning by basename would wipe
		// the surviving root's tracks too. Refuse rather than
		// silently corrupt (Gemini High on PR #78).
		basename := filepath.Base(target)
		for _, surviving := range newList {
			if filepath.Base(surviving) == basename {
				fmt.Fprintf(stderr, "library remove: refusing — another configured root (%q) shares the basename %q\n", surviving, basename)
				fmt.Fprintln(stderr, "  rename the colliding root first; the manifest path namespace can't disambiguate.")
				return 1
			}
		}
		if _, err := store.DeleteTracksByPrefix(ctx, basename+"/"); err != nil {
			fmt.Fprintf(stderr, "library remove: prune tracks: %v\n", err)
			return 1
		}
	}
	cfg.LibraryRoots = newList
	if err := cfg.Save(*configPath); err != nil {
		fmt.Fprintf(stderr, "library remove: save config: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "library remove: %s removed; manifest cleaned.\n", target)
	return 0
}

// tryLibraryViaAdmin probes the loopback admin API. If a bridge is
// running, hand the mutation off to the live server (which already
// handles teardown, manifest mutation, and scan rescheduling) and
// return (true, exitCode). If not, return (false, _) so the caller
// runs the offline path.
//
// Probe: GET /api/stats with a 200 ms timeout. We deliberately
// don't reuse the longer 5 s timeout from `bridge status`: this is
// a "are you up" probe, not a "tell me about yourself" probe.
func tryLibraryViaAdmin(ctx context.Context, cfg *config.Config, method, path string, stdout, stderr io.Writer) (bool, int) {
	addr := cfg.AdminAddress
	if addr == "" {
		addr = config.DefaultAdminAddress
	}
	probeCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	probeReq, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://"+addr+"/api/stats", nil)
	if err != nil {
		return false, 0
	}
	probeClient := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := probeClient.Do(probeReq)
	if err != nil {
		// Connection refused = service not running → caller falls
		// through to offline path. Distinguish from "admin is up
		// but unhappy" cases below so we don't trample a live
		// bridge's state.
		if isConnRefused(err) {
			return false, 0
		}
		// Other transport errors (timeout, TLS) — admin is
		// possibly listening but we can't tell. Refuse rather
		// than silently fall through to the offline path which
		// would mutate bridge.yaml + the manifest store while a
		// live process holds it (CodeRabbit Major post-merge on
		// PR #82). Operator gets a clear error and can stop the
		// service explicitly before retrying.
		fmt.Fprintf(stderr, "library: admin probe %s: %v\n", addr, err)
		fmt.Fprintln(stderr, "  refusing offline mutation against a possibly-live bridge.")
		fmt.Fprintln(stderr, "  stop the service first, or use the admin console.")
		return true, 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Admin is bound and answering, just unhappy — same race
		// hazard as above. Refuse rather than fall through.
		fmt.Fprintf(stderr, "library: admin probe returned HTTP %d on /api/stats\n", resp.StatusCode)
		fmt.Fprintln(stderr, "  refusing offline mutation; investigate the live bridge first.")
		return true, 1
	}

	// Service is up. Forward the mutation.
	body, _ := json.Marshal(map[string]string{"path": path})
	mutateCtx, mutateCancel := context.WithTimeout(ctx, 30*time.Second)
	defer mutateCancel()
	req, err := http.NewRequestWithContext(mutateCtx, method, "http://"+addr+"/api/roots", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "library: build request: %v\n", err)
		return true, 1
	}
	req.Header.Set("Content-Type", "application/json")
	// Separate client with the longer timeout — pre-fix the same
	// 200 ms probe client serviced the mutation, so an add/remove
	// against a running bridge would fail at 200 ms regardless of
	// the 30 s context (Qodo Bug on PR #78). http.Client.Timeout
	// caps the entire request duration independent of the request
	// context.
	mutateClient := &http.Client{Timeout: 30 * time.Second}
	mutResp, err := mutateClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "library: admin mutation: %v\n", err)
		return true, 1
	}
	defer mutResp.Body.Close()
	if mutResp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(mutResp.Body)
		fmt.Fprintf(stderr, "library: admin returned HTTP %d: %s\n", mutResp.StatusCode, strings.TrimSpace(string(respBody)))
		return true, 1
	}
	verb := "added"
	if method == http.MethodDelete {
		verb = "removed"
	}
	fmt.Fprintf(stdout, "library: %s %s via running bridge; scan rescheduled.\n", verb, path)
	return true, 0
}

// wipeManifest opens the manifest store, wipes it, and closes.
// Used by the offline library-add path on a 1→N transition.
func wipeManifest(ctx context.Context, cfg *config.Config) error {
	store, err := openManifestStore(cfg)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.WipeAllTracks(ctx)
}

// openManifestStore resolves the manifest DB path the same way
// runServe does and opens the store. Centralised here so library
// add/remove and any future offline manifest mutator share the
// resolution rule.
func openManifestStore(cfg *config.Config) (*manifest.Store, error) {
	dbPath := manifest.DefaultDBPath(cfg.DataDir)
	return manifest.OpenStore(dbPath)
}
