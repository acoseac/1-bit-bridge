// Package config loads and validates bridge.yaml (library roots, listen
// address, TLS paths, scan interval).
//
// Relative paths in the config file resolve against the config file's own
// directory, matching how most Unix tools handle config-relative paths.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/acoseac/1-bit-bridge/internal/logging"
)

// numCPUMinusOne is the worker-pool size hint used by EffectiveWorkers.
// Extracted as a function so tests can override behaviour without
// touching the runtime package directly.
func numCPUMinusOne() int { return runtime.NumCPU() - 1 }

// validateLogger surfaces non-fatal config-validation warnings to the
// operator. Used today by `Validate()` to emit one entry per dropped
// CustomEndpoints item so silent prune-and-warn is observable.
var validateLogger = logging.Component("config")

// Config mirrors the on-disk bridge.yaml shape. See config/bridge.yaml.example.
type Config struct {
	LibraryRoots    []string `yaml:"libraryRoots"`
	ListenAddress   string   `yaml:"listenAddress"`
	AdminAddress    string   `yaml:"adminAddress,omitempty"`
	DataDir         string   `yaml:"dataDir"`
	TLSCertPath     string   `yaml:"tlsCertPath,omitempty"`
	TLSKeyPath      string   `yaml:"tlsKeyPath,omitempty"`
	ScanIntervalSec int      `yaml:"scanIntervalSec"`
	LibraryName     string   `yaml:"libraryName"`
	// CustomEndpoints lets operators advertise URLs that aren't
	// discoverable from the host's interface table — e.g. a reverse
	// proxy, a port-forwarded WAN URL, or a non-default Tailscale
	// MagicDNS that the auto-detector didn't pick up. Each entry is
	// a complete URL ("https://my-bridge.example.com:7788") and is
	// surfaced to iOS via /v1/health and the admin endpoints panel.
	//
	// Validation: each entry must parse as an absolute URL with
	// scheme="https" — entries failing validation are dropped at
	// load with a `.warn` log and don't fail the whole config.
	//
	// Operators add entries via the admin console's Settings page;
	// hand-edits to bridge.yaml work too. Used as input to the cert
	// SAN gather (PR feat/tls-broader-sans) so that adding a new
	// custom DNS hostname here, then rotating the cert, makes that
	// URL TLS-handshake-compatible from iOS.
	CustomEndpoints []string           `yaml:"customEndpoints,omitempty"`
	Update          UpdateConfig       `yaml:"update,omitempty"`
	Backup          BackupConfig       `yaml:"backup,omitempty"`
	LibraryWatch    LibraryWatchConfig `yaml:"libraryWatch,omitempty"`
	Upscale         UpscaleConfig      `yaml:"upscale,omitempty"`
	Tailscale       TailscaleConfig    `yaml:"tailscale,omitempty"`
}

// TailscaleConfig selects how the bridge integrates with Tailscale.
// `cli` (default) preserves the historical CLI-shell-out flow:
// `tailscale status` for endpoint detection + `tailscale cert` for
// LE-on-magic-DNS cert minting. The CLI path requires the operator's
// host to have `tailscaled` running and `tailscale` in $PATH, and
// ships LE cert/key files to disk for the SNI cert switcher to read.
//
// `tsnet` makes the bridge its own tailnet node via the embedded
// `tailscale.com/tsnet` library — no external daemon needed, no
// on-disk LE cert dance (tsnet's ListenTLS terminates LE in-process).
// State (machine identity + control-plane keys) persists under
// `<dataDir>/tailscale/`. First-time auth uses an interactive
// browser flow; subsequent boots load persisted state. Headless
// deploys can pre-seed the state by setting AuthKey or the
// TS_AUTHKEY environment variable.
//
// `disabled` skips both — useful for LAN-only deployments where the
// operator doesn't want tailscale code paths exercised.
//
// Default is `cli` to preserve back-compat. Operators opt into
// `tsnet` per-device; the default may be flipped in a future release
// after the tsnet path soaks.
type TailscaleConfig struct {
	// Mode selects the integration: "cli" (default), "tsnet", or
	// "disabled". Empty value falls back to "cli" at load time.
	Mode string `yaml:"mode,omitempty"`

	// AuthKey is the Tailscale auth key used by tsnet on first run.
	//
	// Precedence (matches Tailscale's standard idiom):
	//   1. TS_AUTHKEY environment variable (preferred — keeps
	//      secrets out of yaml on disk)
	//   2. This field (fallback for ops who can't set env vars)
	//   3. Empty → triggers interactive OAuth (`bridge tsnet auth`
	//      prints an AuthURL the operator visits in a browser)
	//
	// Unused once tsnet has persisted state.
	AuthKey string `yaml:"authKey,omitempty"`

	// Hostname is the magic-DNS hostname tsnet will register with.
	// Empty falls back to the bridge's deviceName / library name.
	Hostname string `yaml:"hostname,omitempty"`
}

// TailscaleMode is the typed representation of TailscaleConfig.Mode.
type TailscaleMode string

const (
	TailscaleModeCLI      TailscaleMode = "cli"
	TailscaleModeTsnet    TailscaleMode = "tsnet"
	TailscaleModeDisabled TailscaleMode = "disabled"
)

// EffectiveMode resolves the configured mode, applying the "cli"
// default for empty values. Returns one of the three known modes
// or an error when the yaml carries something unrecognised — the
// safer shape than silently falling through to the default on a
// typo (e.g. `mode: tnset` would otherwise look like it worked).
func (t TailscaleConfig) EffectiveMode() (TailscaleMode, error) {
	switch t.Mode {
	case "", string(TailscaleModeCLI):
		return TailscaleModeCLI, nil
	case string(TailscaleModeTsnet):
		return TailscaleModeTsnet, nil
	case string(TailscaleModeDisabled):
		return TailscaleModeDisabled, nil
	}
	return "", fmt.Errorf("tailscale.mode: unknown value %q (want cli|tsnet|disabled)", t.Mode)
}

// UpscaleConfig governs the optional offline PCM-upscaling feature
// introduced in v1.2. Disabled by default — operators must explicitly
// opt in via `upscale.enabled: true`. When disabled:
//   - the `bridge upscale` CLI exits with a friendly "feature is
//     disabled" error.
//   - the `POST /v1/upscale` endpoint returns 503 `upscale_disabled`.
//   - the manifest provider does not splice `Variants` into Track
//     responses even if `track_variants` rows exist on disk.
//   - `/v1/health` reports `upscaleEnabled: false`.
//
// When enabled, additional safety: `bridge serve` runs an
// `exec.LookPath("sox")` probe at startup. Missing `sox` logs an
// error and overrides Enabled to false in-memory — feature
// gracefully degrades, the rest of the server keeps running.
//
// `track_variants` table is created unconditionally (additive
// schema, no harm in empty); only the read/write paths are gated.
// This means a user can: enable → run `bridge upscale` → variants
// populate → disable → variants disappear from manifest → re-enable
// → variants reappear without re-conversion.
type UpscaleConfig struct {
	// Enabled is the master toggle. Default false.
	Enabled bool `yaml:"enabled,omitempty"`

	// Workers is the size of the long-lived transcode worker pool
	// instantiated by `bridge serve` when Enabled. Zero (the default)
	// resolves to min(NumCPU-1, 4) at startup. Operators with beefy
	// hosts can raise; small-box deployments (Pi) should leave at
	// the default to avoid starving downloads / pairing requests.
	Workers int `yaml:"workers,omitempty"`

	// QueueCap is the hard cap on the worker pool's pending-job
	// channel. POST /v1/upscale enqueues are non-blocking and drop
	// to a 503 `queue_full` response when the channel can't accept
	// more — protects against a user spamming "Generate" on their
	// 50k-track library exhausting memory or wedging the HTTP path.
	// Zero (the default) resolves to DefaultUpscaleQueueCap.
	QueueCap int `yaml:"queueCap,omitempty"`

	// TargetRate selects the resampler output rate. "auto" (the
	// default) picks 176400 Hz for 44.1-family sources and 192000
	// Hz for 48-family sources. Explicit numeric values override
	// the auto pick (e.g. "352800" for DSD-friendly DACs that
	// prefer the next octave up). Sources at or above the chosen
	// rate are skipped — never downsample.
	TargetRate string `yaml:"targetRate,omitempty"`

	// TargetBits is the output bit depth. 24 (the default) is the
	// sweet spot for FLAC: lower ceiling than 32-bit float but
	// 96 dB above the audible noise floor and well below any
	// realistic dither quantisation noise. 32-bit-int output is
	// supported for completeness but rarely a sonic improvement.
	TargetBits int `yaml:"targetBits,omitempty"`
}

// EffectiveWorkers resolves the runtime worker count from the YAML
// field, applying the min(NumCPU-1, 4) default at zero. Centralised
// so the CLI and the serve-side pool can't disagree on the floor.
func (u UpscaleConfig) EffectiveWorkers() int {
	if u.Workers > 0 {
		return u.Workers
	}
	n := numCPUMinusOne()
	if n > 4 {
		n = 4
	}
	if n < 1 {
		n = 1
	}
	return n
}

// EffectiveQueueCap resolves the pending-job channel cap, defaulting
// to DefaultUpscaleQueueCap when the YAML field is zero.
func (u UpscaleConfig) EffectiveQueueCap() int {
	if u.QueueCap > 0 {
		return u.QueueCap
	}
	return DefaultUpscaleQueueCap
}

// EffectiveTargetBits resolves the output bit depth. Defaults to 24.
func (u UpscaleConfig) EffectiveTargetBits() int {
	switch u.TargetBits {
	case 16, 24, 32:
		return u.TargetBits
	default:
		return 24
	}
}

// EffectiveTargetRate returns the YAML field's literal value when
// set, or "auto" (the default) when empty. Validation happens at
// the call site in the transcode package, where the source rate
// is in scope.
func (u UpscaleConfig) EffectiveTargetRate() string {
	if u.TargetRate == "" {
		return "auto"
	}
	return u.TargetRate
}

// LibraryWatchConfig governs the optional fsnotify-based
// instant-update watcher. Off by default — the periodic scan
// (ScanIntervalSec) remains the safety net in either case. Power
// users with local-disk libraries flip this on to get
// Roon/Plex-style "drop a file in, it appears" responsiveness;
// NAS / spinning-disk users keep the periodic-only path so a
// flapping server can't trigger a thrash of incremental scans.
//
// Linux deployments with very large libraries should also raise
// `fs.inotify.max_user_watches` — `bridge doctor` warns when the
// kernel limit looks too low for the configured roots.
type LibraryWatchConfig struct {
	// Enabled is the master toggle. Default false.
	Enabled bool `yaml:"enabled,omitempty"`
	// DebounceSeconds is the per-directory event coalesce window.
	// 10 seconds is the documented default — long enough that a
	// large-folder copy doesn't trigger a scan-per-file storm,
	// short enough that the perceived "instant" feel survives.
	// Zero or omitted → DefaultLibraryWatchDebounceSeconds.
	DebounceSeconds int `yaml:"debounceSeconds,omitempty"`
}

// EffectiveDebounceSeconds resolves the runtime debounce window
// from the YAML field. Centralised so the watcher and the
// doctor check can't disagree.
func (l LibraryWatchConfig) EffectiveDebounceSeconds() int {
	if l.DebounceSeconds <= 0 {
		return DefaultLibraryWatchDebounceSeconds
	}
	return l.DebounceSeconds
}

// BackupConfig configures the periodic state-snapshot ticker that
// `bridge serve` runs alongside the manifest scanner. The CLI
// `bridge backup` / `bridge restore` work regardless of this section
// — these knobs only govern the in-process automatic schedule.
//
// Default state (section absent): IntervalHours=24, Keep=7 (one
// snapshot per day, a week retained). Setting `intervalHours: 0`
// explicitly disables the periodic ticker; the operator can still
// snapshot on-demand via the CLI or the admin console. To preserve
// the "omitted vs explicit-zero" distinction across YAML round-trips,
// `IntervalHours` is a `*int` — nil means "absent, use default", a
// pointer to 0 means "explicitly disabled".
type BackupConfig struct {
	// IntervalHours is the cadence for automatic snapshots.
	// nil/omitted → DefaultBackupIntervalHours.
	// 0 → disabled.
	// >0 → cadence in hours (negative values rejected at Validate).
	IntervalHours *int `yaml:"intervalHours,omitempty"`

	// Keep is the maximum number of snapshots to retain after a
	// rotation. Older snapshots beyond this count are deleted on
	// each periodic snapshot. Zero or negative disables pruning;
	// the operator manages backup-disk usage by hand in that case.
	// Defaults to DefaultBackupKeep when the entire backup section
	// is omitted; an operator who wants no pruning sets `keep: -1`
	// (a negative value won't trip Validate).
	Keep int `yaml:"keep,omitempty"`
}

// EffectiveIntervalHours resolves the runtime cadence from the
// pointer-typed config field. Caller code uses this rather than
// dereferencing IntervalHours directly so the nil-vs-zero contract
// stays in one place.
func (b BackupConfig) EffectiveIntervalHours() int {
	if b.IntervalHours == nil {
		return DefaultBackupIntervalHours
	}
	return *b.IntervalHours
}

// EffectiveKeep returns the rotation count to apply on each
// periodic snapshot. Mirrors EffectiveIntervalHours so call sites
// have one helper per field.
func (b BackupConfig) EffectiveKeep() int {
	if b.Keep == 0 {
		return DefaultBackupKeep
	}
	return b.Keep
}

// UpdateConfig configures the Phase C opt-in auto-installer. The
// safeties from Phase B (stream-active gate, signature verification,
// rollback marker) ALWAYS apply — these toggles only decide whether
// auto-install is attempted at all and within what time window.
//
// Default state: AutoInstall=false (operator-triggered only). The
// admin Settings UI exposes these for hand-edit; YAML-direct edits
// are also supported.
type UpdateConfig struct {
	// AutoInstall enables the poll-loop's automatic install attempt.
	// Off by default — the operator must opt in. Even when on, the
	// stream-active gate refuses if any /v1/download is in flight,
	// and quiet-hours (when set) restrict to the configured window.
	AutoInstall bool `yaml:"autoInstall,omitempty"`

	// QuietHours restricts auto-install to a daily window in
	// "HH:MM-HH:MM" form using the server's local time. Empty means
	// "any time". The window may wrap midnight (e.g.
	// "23:00-06:00") — see config_test.go for the wrap-around test
	// matrix. Any-clock format that fails to parse rejects at
	// Validate time so a bad config doesn't silently disable the
	// auto-installer.
	QuietHours string `yaml:"quietHours,omitempty"`

	// CheckIntervalHours overrides the default poll cadence (6 h).
	// Operator-tunable for installations on metered or rate-limited
	// uplinks. Values below 1h are clamped at runtime by the updater.
	// Zero = use the package default.
	CheckIntervalHours int `yaml:"checkIntervalHours,omitempty"`
}

// Defaults applied when a field is absent or zero-valued.
const (
	DefaultListenAddress = ":7788"
	DefaultAdminAddress  = "127.0.0.1:7789"
	DefaultDataDir       = "./data"
	// DefaultScanIntervalSec is 6h, not 1h. A 50k-track library on
	// mechanical NAS with the prior 1h cadence spun the disks every
	// hour preventing idle spindown — operator-facing wear hazard.
	// Operators with quiet libraries should set this higher; admin
	// console exposes on-demand triggers for ad-hoc rescans.
	// fsnotify integration would let us drop this further but has
	// cross-platform pitfalls (Windows path semantics, recursive
	// watch fan-out on large libraries) and deserves its own design
	// pass (PR #N).
	DefaultScanIntervalSec     = 21600
	DefaultLibraryName         = "1-bit Bridge"
	DefaultBackupIntervalHours = 24
	DefaultBackupKeep          = 7
	// DefaultLibraryWatchDebounceSeconds is the per-directory
	// event coalesce window when fsnotify-based watching is on.
	// 10 seconds matches the documented default and is long
	// enough that a large-folder copy doesn't trigger a scan-
	// per-file storm.
	DefaultLibraryWatchDebounceSeconds = 10
	// DefaultUpscaleQueueCap bounds the pending-job channel of the
	// long-lived transcode worker pool inside `bridge serve`. 5000
	// covers 2-3 average user libraries; smaller deployments stay
	// well under the cap, the user-spam-the-button case bounces
	// against a clean 503 instead of exhausting memory.
	DefaultUpscaleQueueCap = 5000
)

// Load parses a bridge.yaml file, fills defaults, resolves relative paths
// against the config file's directory, and validates. A returned *Config is
// ready to hand to downstream subsystems.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // typo-catcher: unknown YAML keys fail the load
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	cfg.applyDefaults()
	cfg.applyEnvOverrides()
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("abs config path %q: %w", path, err)
	}
	cfg.resolvePaths(filepath.Dir(absPath))
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyEnvOverrides lets a deployment override the YAML for the
// fields a Docker / Kubernetes / homelab operator typically wants
// to inject at runtime without rewriting the config file. Env
// wins over yaml; unset env = no change. Documented precedence:
// env > yaml > defaults.
//
// Why this exists: a `bridge serve` running inside a container
// has the binary baked in but `bridge.yaml` is the moving piece
// — bind addresses depend on the host's port-forward layout, the
// data dir is the persistent volume, and library roots are the
// bind-mounted music shares. Patching a YAML inside an image at
// runtime is doable but ugly; env overrides are the idiomatic
// container-config knob.
//
// Recognised variables (all optional):
//
//	BRIDGE_LISTEN_ADDRESS  — overrides ListenAddress
//	BRIDGE_ADMIN_ADDRESS   — overrides AdminAddress
//	BRIDGE_DATA_DIR        — overrides DataDir
//	BRIDGE_LIBRARY_NAME    — overrides LibraryName
//	BRIDGE_LIBRARY_ROOTS   — OS-native-PATH-separated; overrides
//	                         LibraryRoots. POSIX uses `:`,
//	                         Windows uses `;` so drive-letter
//	                         paths (`C:\Music`) aren't corrupted.
//
// Path-typed values (DataDir, LibraryRoots) still go through
// `resolvePaths` afterwards so a relative path inherits the same
// "relative-to-config-dir" semantics as a YAML field.
func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("BRIDGE_LISTEN_ADDRESS"); v != "" {
		c.ListenAddress = v
	}
	if v := os.Getenv("BRIDGE_ADMIN_ADDRESS"); v != "" {
		c.AdminAddress = v
	}
	if v := os.Getenv("BRIDGE_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("BRIDGE_LIBRARY_NAME"); v != "" {
		c.LibraryName = v
	}
	if v := os.Getenv("BRIDGE_LIBRARY_ROOTS"); v != "" {
		// Use the OS-native PATH-style separator: `:` on POSIX,
		// `;` on Windows. Pre-fix we hard-coded `:` everywhere,
		// which corrupted Windows drive-letter paths
		// (`C:\Music` parsed as `["C", "\Music"]`) and made the
		// follow-up filepath.Abs / Validate trip with a
		// confusing error (Qodo Bug post-merge on PR #84).
		// Container deployments are linux/amd64 + linux/arm64
		// where `os.PathListSeparator` evaluates to `:`, so
		// docker-compose / k8s manifests using colons keep
		// working unchanged.
		raw := strings.Split(v, string(os.PathListSeparator))
		out := make([]string, 0, len(raw))
		for _, p := range raw {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			c.LibraryRoots = out
		}
	}
}

func (c *Config) applyDefaults() {
	if c.ListenAddress == "" {
		c.ListenAddress = DefaultListenAddress
	}
	if c.AdminAddress == "" {
		c.AdminAddress = DefaultAdminAddress
	}
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir
	}
	if c.ScanIntervalSec == 0 {
		c.ScanIntervalSec = DefaultScanIntervalSec
	}
	if c.LibraryName == "" {
		c.LibraryName = DefaultLibraryName
	}
	// Backup section: pointer-typed IntervalHours preserves the
	// "omitted vs explicit-zero" distinction at YAML-round-trip
	// time, so an operator who writes `intervalHours: 0` genuinely
	// disables the ticker (matches the PROTOCOL.md spec). Defaults
	// are returned by the EffectiveIntervalHours / EffectiveKeep
	// helpers — applyDefaults intentionally leaves the raw fields
	// untouched so a Save+Load round-trip preserves operator intent.
}

func (c *Config) resolvePaths(baseDir string) {
	for i, r := range c.LibraryRoots {
		if r != "" && !filepath.IsAbs(r) {
			c.LibraryRoots[i] = filepath.Join(baseDir, r)
		}
	}
	if c.DataDir != "" && !filepath.IsAbs(c.DataDir) {
		c.DataDir = filepath.Join(baseDir, c.DataDir)
	}
	if c.TLSCertPath != "" && !filepath.IsAbs(c.TLSCertPath) {
		c.TLSCertPath = filepath.Join(baseDir, c.TLSCertPath)
	}
	if c.TLSKeyPath != "" && !filepath.IsAbs(c.TLSKeyPath) {
		c.TLSKeyPath = filepath.Join(baseDir, c.TLSKeyPath)
	}
}

// Validate checks invariants the server relies on. Called automatically by
// Load; exposed for tests and for callers that construct Config in memory.
func (c *Config) Validate() error {
	if len(c.LibraryRoots) == 0 {
		return errors.New("libraryRoots: must have at least one entry")
	}
	for _, r := range c.LibraryRoots {
		if r == "" {
			return errors.New("libraryRoots: entries must not be empty")
		}
		info, err := os.Stat(r)
		if err != nil {
			return fmt.Errorf("libraryRoots[%q]: %w", r, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("libraryRoots[%q]: not a directory", r)
		}
	}
	if c.ScanIntervalSec < 1 {
		return fmt.Errorf("scanIntervalSec: must be >= 1, got %d", c.ScanIntervalSec)
	}
	if (c.TLSCertPath == "") != (c.TLSKeyPath == "") {
		return errors.New("tlsCertPath and tlsKeyPath: must be set together, or both empty")
	}
	if _, _, err := net.SplitHostPort(c.ListenAddress); err != nil {
		return fmt.Errorf("listenAddress %q: %w", c.ListenAddress, err)
	}
	if err := validateLoopbackAddress(c.AdminAddress); err != nil {
		return fmt.Errorf("adminAddress %q: %w", c.AdminAddress, err)
	}
	if c.Update.QuietHours != "" {
		if _, _, err := ParseQuietHours(c.Update.QuietHours); err != nil {
			return fmt.Errorf("update.quietHours %q: %w", c.Update.QuietHours, err)
		}
	}
	if c.Update.CheckIntervalHours < 0 {
		return fmt.Errorf("update.checkIntervalHours: must be >= 0, got %d", c.Update.CheckIntervalHours)
	}
	if c.Backup.IntervalHours != nil && *c.Backup.IntervalHours < 0 {
		return fmt.Errorf("backup.intervalHours: must be >= 0 (0 disables, omit for default), got %d", *c.Backup.IntervalHours)
	}
	// backup.Keep: any non-positive value disables pruning. No
	// upper-bound check — an operator who wants 1000 retained
	// snapshots is making a disk-space choice we don't second-
	// guess.

	// CustomEndpoints: prune-and-warn. Accept HTTPS URLs only. We
	// silently drop malformed / non-HTTPS entries because cert SAN
	// generation downstream (PR feat/tls-broader-sans) treats every
	// kept entry as authoritative — a typo in one entry shouldn't
	// fail the whole `Save` and lock the operator out of the admin
	// console. Validate() never errors on CustomEndpoints; it
	// rewrites the slice in-place with the kept entries only.
	//
	// Per-entry warnings used to be discarded silently (Qodo bot
	// review on PR #92 — without observability, a bad entry just
	// disappeared). We now log each warning at `.warn` so the
	// operator sees the breadcrumb in the bridge logs even though
	// the patch / load doesn't fail.
	kept, warns := ValidateCustomEndpoints(c.CustomEndpoints)
	c.CustomEndpoints = kept
	for _, w := range warns {
		validateLogger.Warn("dropped invalid custom endpoint", "err", w)
	}
	return nil
}

// ValidateCustomEndpoints filters the input to entries that parse as
// absolute HTTPS URLs with a non-empty host. Returns (kept, warnings)
// where `warnings` is one error per dropped entry. Used by Validate()
// to scrub the persisted list and by the admin patch handler to
// surface per-entry errors back to the operator.
//
// Why HTTPS-only: iOS clients won't speak plain-HTTP to the bridge
// (ATS rejects it before our pinning runs even on a local-network
// allowlisted host), and the bridge itself only listens TLS. Allowing
// HTTP entries would just produce a confusing "advertised but
// unreachable" row in the Devices panel.
func ValidateCustomEndpoints(in []string) (kept []string, warnings []error) {
	kept = make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("customEndpoints[%q]: %w", raw, err))
			continue
		}
		if u.Scheme != "https" {
			warnings = append(warnings, fmt.Errorf("customEndpoints[%q]: scheme must be https, got %q", raw, u.Scheme))
			continue
		}
		if u.Host == "" {
			warnings = append(warnings, fmt.Errorf("customEndpoints[%q]: missing host", raw))
			continue
		}
		// Dedupe on the canonical URL string so two paste-friendly
		// equivalents (with vs without trailing slash) don't double
		// up the advertised list. URL.String() canonicalises a few
		// shapes for us; we preserve the operator's input form for
		// anything else (no path-normalisation, no port-normalisation).
		canonical := u.String()
		if seen[canonical] {
			continue
		}
		seen[canonical] = true
		kept = append(kept, raw)
	}
	return kept, warnings
}

// ParseQuietHours parses a "HH:MM-HH:MM" window into start and end
// minute-of-day values (0-1439 each). Returns an error for malformed
// input. The window may wrap midnight (start > end means "from
// start until midnight, then midnight until end").
//
// Used by Validate to catch bad config at load time, and by the
// updater's auto-install scheduler to decide whether the current
// wall-clock minute is inside the window.
func ParseQuietHours(s string) (startMin, endMin int, err error) {
	// Trim outer whitespace before splitting so YAML-clean values like
	// "23:00-06:00" and operator-friendly forms like "23:00 - 06:00" or
	// "  23:00-06:00  " all validate. parseHHMM trims its own input too
	// for the "23:00 - 06:00" case where the dash-split leaves a leading
	// or trailing space on each half.
	s = strings.TrimSpace(s)
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected HH:MM-HH:MM")
	}
	startMin, err = parseHHMM(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("start: %w", err)
	}
	endMin, err = parseHHMM(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("end: %w", err)
	}
	return startMin, endMin, nil
}

func parseHHMM(s string) (int, error) {
	s = strings.TrimSpace(s)
	// strings.Split (no limit) so an extra colon in the input
	// produces 3+ parts and fails — strings.SplitN(..., 2) would
	// silently accept "01:00:00" as ["01", "00:00"].
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%q: expected HH:MM", s)
	}
	// strconv.Atoi (not fmt.Sscanf) so trailing non-numeric input
	// is rejected — "12abc" parses cleanly with Sscanf as 12 but
	// errors with Atoi. PR #43 review caught the laxer Sscanf
	// behaviour as a quiet-hours validation gap.
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, fmt.Errorf("%q: hour must be 00-23", s)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q: minute must be 00-59", s)
	}
	return h*60 + m, nil
}

// IsInQuietHours returns true when minute-of-day `now` falls inside
// [startMin, endMin]. Handles midnight-wrap windows (start > end =
// the window crosses midnight, so the "inside" is [start, 1440) ∪
// [0, end]).
func IsInQuietHours(startMin, endMin, now int) bool {
	if startMin == endMin {
		// Degenerate: zero-length window matches only the exact
		// boundary minute. Treat as "always outside" to avoid
		// accidentally restricting to a single minute.
		return false
	}
	if startMin < endMin {
		return now >= startMin && now <= endMin
	}
	// Wraps midnight.
	return now >= startMin || now <= endMin
}

// validateLoopbackAddress enforces that the admin listener binds only to a
// loopback interface. Accepts "127.0.0.1:N", "[::1]:N", and "localhost:N" —
// an empty host (":N" = all interfaces) or any non-loopback IP is rejected.
// The admin console has no auth layer; loopback binding is the trust boundary.
func validateLoopbackAddress(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if port == "" {
		return errors.New("port must not be empty")
	}
	if host == "" {
		return errors.New("host must be a loopback address (127.0.0.1, ::1, or localhost); an empty host binds all interfaces")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("host %q is not a loopback address", host)
		}
		return nil
	}
	if host != "localhost" {
		return fmt.Errorf("host %q must be a loopback address (127.0.0.1, ::1, or localhost)", host)
	}
	return nil
}

// ScanInterval returns scanIntervalSec as a time.Duration.
func (c *Config) ScanInterval() time.Duration {
	return time.Duration(c.ScanIntervalSec) * time.Second
}

// Save atomically writes c as YAML to path (temp file + rename). Parent
// directory must exist. Comments and fields unknown to this schema are not
// preserved — callers that want to keep hand-authored comments should not
// use Save. `bridge init` and admin-console edits are the intended callers.
func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".bridge-*.yaml")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	// Panic-safety FD close. Registered AFTER the Remove defer so it
	// runs first (LIFO) — Windows requires the FD closed before
	// Remove can unlink. See internal/auth/auth.go for the rationale
	// in detail; pattern repeats across every atomic-write helper.
	defer func() { _ = tmp.Close() }()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	tmpName = "" // suppress defer cleanup
	return nil
}
