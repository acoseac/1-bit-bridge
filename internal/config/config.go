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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config mirrors the on-disk bridge.yaml shape. See config/bridge.yaml.example.
type Config struct {
	LibraryRoots    []string           `yaml:"libraryRoots"`
	ListenAddress   string             `yaml:"listenAddress"`
	AdminAddress    string             `yaml:"adminAddress,omitempty"`
	DataDir         string             `yaml:"dataDir"`
	TLSCertPath     string             `yaml:"tlsCertPath,omitempty"`
	TLSKeyPath      string             `yaml:"tlsKeyPath,omitempty"`
	ScanIntervalSec int                `yaml:"scanIntervalSec"`
	LibraryName     string             `yaml:"libraryName"`
	Update          UpdateConfig       `yaml:"update,omitempty"`
	Backup          BackupConfig       `yaml:"backup,omitempty"`
	LibraryWatch    LibraryWatchConfig `yaml:"libraryWatch,omitempty"`
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
	return nil
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
