package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/admin"
	servertailscale "github.com/acoseac/1-bit-bridge/internal/tailscale"
	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// tailscaleStderrFormat is the operator-visible stderr line the
// tailscale auto-pilot emits on every error edge during a refresh.
// `(%s)` carries the trigger label, `%s\n` the error message.
const tailscaleStderrFormat = "tailscale (%s): %s\n"

// tailscaleStatus is the snapshot the admin tile reads. Threaded
// through `*tailscaleAutoPilot` so a single struct owns "everything
// the admin handler needs to render the tile" — keeps the admin
// package free of Tailscale wire-format reasoning.
//
// All fields are copies; the snapshot is immutable once returned.
// Refresh is via `autoPilot.Snapshot()` on every admin GET.
//
// Optional time fields are pointers (Qodo on PR #102): a non-pointer
// `time.Time` with `json:",omitempty"` still serialises the zero
// value `"0001-01-01T00:00:00Z"` because `omitempty` doesn't recognise
// time-zero. Pointers honour `omitempty` correctly. Matches the
// `tokenRow.ExpiresAt *time.Time` precedent in [internal/admin].
type tailscaleStatus struct {
	CLIAvailable      bool       `json:"cliAvailable"`
	NodeName          string     `json:"nodeName,omitempty"`
	MagicDNSName      string     `json:"magicDNSName,omitempty"`
	HTTPSCertsEnabled bool       `json:"httpsCertsEnabled"`
	CertPresent       bool       `json:"certPresent"`
	CertNotAfter      *time.Time `json:"certNotAfter,omitempty"`
	CertPath          string     `json:"certPath,omitempty"`
	// MagicDNSURL is the operator-facing bridge URL on the magic-DNS
	// endpoint, including the configured listen port (NOT hard-coded
	// :7788 — operators using `cfg.ListenAddress: :8443` need the
	// right URL for manual recovery flows, CodeRabbit on PR #102).
	// Empty when MagicDNSName is empty or the listen port can't be
	// resolved.
	MagicDNSURL string     `json:"magicDNSURL,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
	LastChecked *time.Time `json:"lastChecked,omitempty"`
}

// tailscaleAutoPilot owns the auto-mint + auto-renew lifecycle and
// surfaces the status snapshot for the admin tile. One instance per
// `bridge serve` process; threadsafe — admin handlers read via
// Snapshot, the renewer goroutine writes via doDetectAndMint /
// Snapshot.
type tailscaleAutoPilot struct {
	dataDir     string
	listenAddr  string
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
//
// `listenAddr` is `cfg.ListenAddress` ("[host]:port" form). Used
// to compose the operator-facing magic-DNS URL with the right port
// for the admin tile's "iOS clients reach the bridge over Tailscale
// at <url>" hint.
func newTailscaleAutoPilot(dataDir, listenAddr string, mgr *servertls.Manager, stderr io.Writer) *tailscaleAutoPilot {
	return &tailscaleAutoPilot{
		dataDir:         dataDir,
		listenAddr:      listenAddr,
		certManager:     mgr,
		stderr:          stderr,
		minMintInterval: 30 * time.Second,
	}
}

// magicDNSURL composes the operator-facing bridge URL on the
// configured listen port for the given magic-DNS hostname. Returns
// "" when the listen address can't be parsed (test paths with
// `:0` mode fall through; the admin tile renders "—" then).
func (a *tailscaleAutoPilot) magicDNSURL(magicDNS string) string {
	if magicDNS == "" {
		return ""
	}
	_, port, err := net.SplitHostPort(a.listenAddr)
	if err != nil || port == "" || port == "0" {
		return ""
	}
	return "https://" + magicDNS + ":" + port
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
	now := time.Now().UTC()
	snap := tailscaleStatus{LastChecked: &now}

	info, err := servertailscale.Detect(ctx)
	if err != nil {
		snap.CLIAvailable = info.CLIAvailable
		snap.LastError = info.LastError
		fmt.Fprintf(a.stderr, "tailscale (%s): detect failed: %v\n", trigger, err)
		// Detect-side failure is "we don't know what state we're in" —
		// safer to clear cached LE state than to keep serving a
		// possibly-stale LE cert under a since-rotated SNI suffix.
		a.certManager.SetMagicDNSSuffix("")
		a.certManager.SetTailscaleCert(nil)
		a.publish(snap)
		return snap
	}
	snap.CLIAvailable = info.CLIAvailable
	snap.NodeName = info.NodeName
	snap.MagicDNSName = info.MagicDNSName
	snap.MagicDNSURL = a.magicDNSURL(info.MagicDNSName)
	if !info.CLIAvailable {
		// Tailscale uninstalled / removed from PATH between bridge
		// startup and this tick. Without clearing, the SNI switcher
		// would keep serving a previously-installed LE cert against
		// a now-orphaned suffix (CodeRabbit on PR #102).
		a.certManager.SetMagicDNSSuffix("")
		a.certManager.SetTailscaleCert(nil)
		snap.LastError = info.LastError
		a.publish(snap)
		return snap
	}
	// Treat empty TailnetSuffix as the canonical "MagicDNS off" state
	// regardless of whether MagicDNSName happens to be set (Qodo on
	// PR #102 — defensive synthesis in Detect could in theory leave
	// a name without a suffix; the SNI switcher needs the suffix to
	// classify connections, so an empty suffix means "fall through to
	// self-signed for everything"). Equivalent to the
	// !info.CLIAvailable branch from a routing-state perspective.
	if info.TailnetSuffix == "" || info.MagicDNSName == "" {
		a.certManager.SetMagicDNSSuffix("")
		a.certManager.SetTailscaleCert(nil)
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
		fmt.Fprintf(a.stderr, tailscaleStderrFormat, trigger, snap.LastError)
		a.publish(snap)
		return snap
	}

	// Install the on-disk cert FIRST regardless of expiry (Qodo on
	// PR #102 reliability concern). Pre-fix: if the cert was within
	// the renewal window AND a subsequent mint failed, the manager
	// was left with no LE cert at all — magic-DNS handshakes fell
	// through to self-signed, breaking ATS for iOS clients even
	// though we had a still-unexpired cert on disk. Now: install
	// what we have so connections keep working, then attempt re-mint
	// to refresh; if mint succeeds we swap to the new cert below,
	// otherwise the existing one keeps serving until next renewer
	// tick. Skip-mint short-circuit only kicks in when the cert is
	// outside the renewal window.
	if existing, err := servertls.LoadTailscaleCertFromDisk(certPath, keyPath); err == nil && existing.Leaf != nil {
		a.certManager.SetTailscaleCert(existing)
		snap.CertPresent = true
		expiry := existing.Leaf.NotAfter
		snap.CertNotAfter = &expiry
		snap.HTTPSCertsEnabled = true
		if time.Until(existing.Leaf.NotAfter) > servertailscale.FreshnessThreshold {
			// Cert is fresh enough to skip the mint, but still
			// warn if we're under the 30-day window (cert is in
			// the 14d-30d zone). Tailscale's auto-renew should
			// refresh well before this point — a persistent
			// warning means the renewer's stuck.
			//
			// Uses `now` (captured at the function head) for a
			// consistent time reference across this detection
			// pass, and includes `trigger` in the prefix so the
			// log line matches the other stderr messages in this
			// function. Gemini medium on PR #250.
			if msg := warnLECertExpiringSoon(info.MagicDNSName, existing.Leaf.NotAfter, now); msg != "" {
				fmt.Fprintf(a.stderr, tailscaleStderrFormat, trigger, msg)
			}
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
		fmt.Fprintf(a.stderr, tailscaleStderrFormat, trigger, snap.LastError)
		a.publish(snap)
		return snap
	}
	a.certManager.SetTailscaleCert(loaded)
	snap.CertPresent = true
	snap.HTTPSCertsEnabled = true
	expiryStr := "unknown"
	if loaded.Leaf != nil {
		expiry := loaded.Leaf.NotAfter
		snap.CertNotAfter = &expiry
		expiryStr = expiry.Format("2006-01-02")
	}
	fmt.Fprintf(os.Stdout, "tailscale (%s): minted LE cert for %s, expires %s\n",
		trigger, info.MagicDNSName, expiryStr)
	// A freshly-minted LE cert should be ~90 days from expiry. If
	// we're already under 30 days remaining, Tailscale's control
	// plane / LE pipeline is having trouble — surface the diagnostic
	// rather than letting it expire silently. Same wording surface
	// as the existing-cert path above; uses `now` + `trigger`
	// prefix for consistency.
	if loaded.Leaf != nil {
		if msg := warnLECertExpiringSoon(info.MagicDNSName, loaded.Leaf.NotAfter, now); msg != "" {
			fmt.Fprintf(a.stderr, tailscaleStderrFormat, trigger, msg)
		}
	}
	a.publish(snap)
	return snap
}

// warnLECertExpiringSoon returns the operator-facing warning string
// when a Tailscale LE cert is within 30 days of expiry — typically
// signalling renewal-loop or tailnet-misconfig trouble since Tailscale
// auto-renews well before that window. Returns "" when the cert is
// comfortably fresh OR the input is zero (no cert known).
//
// Pure helper so `TestWarnLECertExpiringSoon` can pin the thresholding
// without spinning up a real autopilot. The 30-day threshold mirrors
// `bridge cert info`'s manual diagnostic (cmd/bridge/cert.go:105-106)
// so operators see the same signal regardless of which surface they
// look at.
func warnLECertExpiringSoon(magicDNS string, notAfter, now time.Time) string {
	if notAfter.IsZero() {
		return ""
	}
	remaining := notAfter.Sub(now)
	if remaining > 30*24*time.Hour {
		return ""
	}
	if remaining < 0 {
		// Floor at 0 in the message rather than printing a negative
		// "expires in -3 days" which reads worse than "expired 3 days
		// ago" (same convention `bridge cert info` uses).
		daysOver := int(-remaining / (24 * time.Hour))
		return fmt.Sprintf("WARNING: tailscale LE cert for %s has EXPIRED (%d days past). "+
			"Auto-renew appears stuck — run `bridge tailscale status` and check the admin DNS page.",
			magicDNS, daysOver)
	}
	daysLeft := int(remaining / (24 * time.Hour))
	return fmt.Sprintf("WARNING: tailscale LE cert for %s expires in %d days. "+
		"Auto-renew should refresh inside this window; if the warning persists, "+
		"run `bridge tailscale status` and check the admin DNS page.",
		magicDNS, daysLeft)
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
		MagicDNSURL:       s.MagicDNSURL,
		LastError:         s.LastError,
		LastChecked:       s.LastChecked,
	}
}
