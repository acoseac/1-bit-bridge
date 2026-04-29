package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	servertailscale "github.com/acoseac/1-bit-bridge/internal/tailscale"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// tailscaleStatus is the snapshot the admin tile reads. Threaded
// through `*tailscaleAutoPilot` so a single struct owns "everything
// the admin handler needs to render the tile" — keeps the admin
// package free of Tailscale wire-format reasoning.
//
// All fields are copies; the snapshot is immutable once returned.
// Refresh is via `autoPilot.Snapshot()` on every admin GET.
type tailscaleStatus struct {
	CLIAvailable      bool      `json:"cliAvailable"`
	NodeName          string    `json:"nodeName,omitempty"`
	MagicDNSName      string    `json:"magicDNSName,omitempty"`
	HTTPSCertsEnabled bool      `json:"httpsCertsEnabled"`
	CertPresent       bool      `json:"certPresent"`
	CertNotAfter      time.Time `json:"certNotAfter,omitempty"`
	CertPath          string    `json:"certPath,omitempty"`
	LastError         string    `json:"lastError,omitempty"`
	LastChecked       time.Time `json:"lastChecked,omitempty"`
}

// tailscaleAutoPilot owns the auto-mint + auto-renew lifecycle and
// surfaces the status snapshot for the admin tile. One instance per
// `bridge serve` process; threadsafe — admin handlers read via
// Snapshot, the renewer goroutine writes via doDetectAndMint /
// Snapshot.
type tailscaleAutoPilot struct {
	dataDir     string
	certManager *servertls.Manager
	stderr      io.Writer

	// minMintInterval rate-limits operator-triggered "Re-mint now"
	// clicks so a panic-clicker can't hammer Let's Encrypt's quotas.
	// 30s is plenty for an LE cert mint (Tailscale serves a cached
	// response within a couple of seconds when the node is fresh).
	minMintInterval time.Duration

	mu              sync.Mutex
	lastMintAttempt time.Time
	lastSnapshot    atomic.Pointer[tailscaleStatus]
}

// newTailscaleAutoPilot wires the auto-pilot against an existing
// cert manager. Caller is responsible for kicking the goroutines
// (Start). Construction is free — actual work happens on Start.
func newTailscaleAutoPilot(dataDir string, mgr *servertls.Manager, stderr io.Writer) *tailscaleAutoPilot {
	return &tailscaleAutoPilot{
		dataDir:         dataDir,
		certManager:     mgr,
		stderr:          stderr,
		minMintInterval: 30 * time.Second,
	}
}

// Start kicks the initial detect+mint goroutine and the renewer
// ticker. Both honour ctx.Done so SIGINT clears them out cleanly
// alongside the rest of the periodic workers.
func (a *tailscaleAutoPilot) Start(ctx context.Context) {
	go a.runStartup(ctx)
	go a.runRenewer(ctx)
}

// runStartup performs the first detection + mint pass. Runs in a
// goroutine so a slow Tailscale CLI / LE round-trip doesn't block
// `bridge serve`'s listen step.
func (a *tailscaleAutoPilot) runStartup(ctx context.Context) {
	a.detectAndMint(ctx, "startup")
}

// runRenewer wakes every 24h to check whether the on-disk LE cert is
// within the freshness threshold (default 14d) and re-mints if so.
// Cheap: a re-mint when nothing's due is idempotent (Tailscale serves
// a cached LE response).
func (a *tailscaleAutoPilot) runRenewer(ctx context.Context) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.detectAndMint(ctx, "renew")
		}
	}
}

// detectAndMint is the unified "look at the world, mint if needed,
// publish snapshot" routine called from both startup + renewer +
// admin "Re-mint now" paths. The trigger string lands in the
// startup log so operators can tell which path produced a given
// stderr message.
//
// Returns the resulting snapshot so the admin Re-mint handler can
// reply with the post-action state synchronously.
func (a *tailscaleAutoPilot) detectAndMint(ctx context.Context, trigger string) tailscaleStatus {
	snap := tailscaleStatus{LastChecked: time.Now().UTC()}

	info, err := servertailscale.Detect(ctx)
	if err != nil {
		snap.CLIAvailable = info.CLIAvailable
		snap.LastError = info.LastError
		fmt.Fprintf(a.stderr, "tailscale (%s): detect failed: %v\n", trigger, err)
		a.publish(snap)
		return snap
	}
	snap.CLIAvailable = info.CLIAvailable
	snap.NodeName = info.NodeName
	snap.MagicDNSName = info.MagicDNSName
	if !info.CLIAvailable {
		snap.LastError = info.LastError
		a.publish(snap)
		return snap
	}
	if info.MagicDNSName == "" {
		snap.LastError = info.LastError
		if snap.LastError == "" {
			snap.LastError = "no MagicDNS name detected"
		}
		a.publish(snap)
		return snap
	}

	// Hand the suffix to the SNI switcher even before we know the LE
	// cert's status — that way Get() can already classify SNI without
	// waiting on the mint. If MintCert ultimately fails, Get falls
	// through to self-signed for that SNI (= pre-PR behaviour).
	a.certManager.SetMagicDNSSuffix(info.TailnetSuffix)

	certPath, keyPath := servertailscale.LECertPaths(a.dataDir)
	snap.CertPath = certPath
	if err := servertailscale.EnsureCertDir(a.dataDir); err != nil {
		snap.LastError = fmt.Sprintf("create %s/tls: %v", a.dataDir, err)
		fmt.Fprintf(a.stderr, "tailscale (%s): %s\n", trigger, snap.LastError)
		a.publish(snap)
		return snap
	}

	// Decide whether the on-disk cert is fresh enough to skip minting.
	if existing, err := servertls.LoadTailscaleCertFromDisk(certPath, keyPath); err == nil {
		if existing.Leaf != nil && time.Until(existing.Leaf.NotAfter) > servertailscale.FreshnessThreshold {
			a.certManager.SetTailscaleCert(existing)
			snap.CertPresent = true
			snap.CertNotAfter = existing.Leaf.NotAfter
			snap.HTTPSCertsEnabled = true
			a.publish(snap)
			return snap
		}
	}

	// Cert missing or within the renewal window — mint a fresh one.
	a.mu.Lock()
	if time.Since(a.lastMintAttempt) < a.minMintInterval && trigger == "admin" {
		// Operator-triggered re-clicks within the rate-limit window —
		// silently no-op and re-publish the existing snapshot. The
		// admin handler will see the unchanged state and render the
		// "minting…" affordance accordingly.
		a.mu.Unlock()
		if last := a.lastSnapshot.Load(); last != nil {
			return *last
		}
		a.publish(snap)
		return snap
	}
	a.lastMintAttempt = time.Now()
	a.mu.Unlock()

	if err := servertailscale.MintCert(ctx, info.BinaryPath, info.MagicDNSName, certPath, keyPath); err != nil {
		switch {
		case errors.Is(err, servertailscale.ErrHTTPSCertsDisabled):
			snap.LastError = "HTTPS Certificates not enabled in tailnet — visit https://login.tailscale.com/admin/dns and toggle on"
		case errors.Is(err, servertailscale.ErrPermission):
			snap.LastError = err.Error()
		default:
			snap.LastError = err.Error()
		}
		fmt.Fprintf(a.stderr, "tailscale (%s): mint failed: %s\n", trigger, snap.LastError)
		a.publish(snap)
		return snap
	}

	loaded, err := servertls.LoadTailscaleCertFromDisk(certPath, keyPath)
	if err != nil {
		snap.LastError = fmt.Sprintf("load freshly-minted cert: %v", err)
		fmt.Fprintf(a.stderr, "tailscale (%s): %s\n", trigger, snap.LastError)
		a.publish(snap)
		return snap
	}
	a.certManager.SetTailscaleCert(loaded)
	snap.CertPresent = true
	snap.HTTPSCertsEnabled = true
	if loaded.Leaf != nil {
		snap.CertNotAfter = loaded.Leaf.NotAfter
	}
	fmt.Fprintf(os.Stdout, "tailscale (%s): minted LE cert for %s, expires %s\n",
		trigger, info.MagicDNSName, snap.CertNotAfter.Format("2006-01-02"))
	a.publish(snap)
	return snap
}

// Snapshot returns the latest tile state. Cheap (atomic load) so the
// admin handler can call it on every GET without coordinating with
// the renewer.
func (a *tailscaleAutoPilot) Snapshot() tailscaleStatus {
	if s := a.lastSnapshot.Load(); s != nil {
		return *s
	}
	return tailscaleStatus{}
}

// RefreshNow is the entry point for the admin "Re-mint now" button.
// Forces a detect+mint pass and returns the resulting snapshot.
// Rate-limited via minMintInterval inside detectAndMint to absorb
// double-clicks before they reach `tailscale cert` and the LE quota.
func (a *tailscaleAutoPilot) RefreshNow(ctx context.Context) tailscaleStatus {
	return a.detectAndMint(ctx, "admin")
}

func (a *tailscaleAutoPilot) publish(snap tailscaleStatus) {
	a.lastSnapshot.Store(&snap)
}

// tailscaleAdminAdapter is the bridge between the cmd/bridge auto-pilot
// (which knows about exec.Cmd, file paths, and timer goroutines) and the
// internal/admin TailscaleProvider interface (which is purely wire-shape +
// Status()/RefreshNow() reads). Keeping the adapter in cmd/bridge means
// internal/admin doesn't import internal/tailscale or this file's types,
// so admin tests don't need to spin up the auto-pilot.
//
// Translates between the local `tailscaleStatus` and admin's
// `TailscaleStatus` field-by-field. They're field-identical today; the
// adapter exists so a future divergence (admin adds/removes a field) is
// localised here instead of needing to touch the auto-pilot itself.
type tailscaleAdminAdapter struct {
	auto *tailscaleAutoPilot
}

func (a tailscaleAdminAdapter) Status() admin.TailscaleStatus {
	return toAdminStatus(a.auto.Snapshot())
}

func (a tailscaleAdminAdapter) RefreshNow(ctx context.Context) admin.TailscaleStatus {
	return toAdminStatus(a.auto.RefreshNow(ctx))
}

func toAdminStatus(s tailscaleStatus) admin.TailscaleStatus {
	return admin.TailscaleStatus{
		CLIAvailable:      s.CLIAvailable,
		NodeName:          s.NodeName,
		MagicDNSName:      s.MagicDNSName,
		HTTPSCertsEnabled: s.HTTPSCertsEnabled,
		CertPresent:       s.CertPresent,
		CertNotAfter:      s.CertNotAfter,
		CertPath:          s.CertPath,
		LastError:         s.LastError,
		LastChecked:       s.LastChecked,
	}
}
