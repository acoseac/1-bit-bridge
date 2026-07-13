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

	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"github.com/acoseac/1-bit-bridge/internal/logging"
)

// numCPUMinusOne is the worker-pool size hint used by EffectiveWorkers.
// A package-level var (not a func declaration) so tests CAN swap it out
// without touching the runtime package directly — a func decl can't be
// reassigned, which made the original "so tests can override" rationale
// impossible to honor. Production code MUST NOT mutate it.
var numCPUMinusOne = func() int { return runtime.NumCPU() - 1 }

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
	CustomEndpoints []string             `yaml:"customEndpoints,omitempty"`
	Update          UpdateConfig         `yaml:"update,omitempty"`
	Backup          BackupConfig         `yaml:"backup,omitempty"`
	LibraryWatch    LibraryWatchConfig   `yaml:"libraryWatch,omitempty"`
	Upscale         UpscaleConfig        `yaml:"upscale,omitempty"`
	Analysis        AnalysisConfig       `yaml:"analysis,omitempty"`
	SmartPlaylists  SmartPlaylistsConfig `yaml:"smartPlaylists,omitempty"`
	Tailscale       TailscaleConfig      `yaml:"tailscale,omitempty"`
	Scanner         ScannerConfig        `yaml:"scanner,omitempty"`
	Limits          LimitsConfig         `yaml:"limits,omitempty"`
	Integrity       IntegrityConfig      `yaml:"integrity,omitempty"`
	Deployment      DeploymentConfig     `yaml:"deployment,omitempty"`
	Autocert        AutocertConfig       `yaml:"autocert,omitempty"`
	MDNS            MDNSConfig           `yaml:"mdns,omitempty"`
	DLNA            DLNAConfig           `yaml:"dlna,omitempty"`
	UPnPUpstream    UPnPUpstreamConfig   `yaml:"upnpUpstream,omitempty"`
	Enrich          EnrichConfig         `yaml:"enrich,omitempty"`
	Atlas           AtlasConfig          `yaml:"atlas,omitempty"`
	Artwork         ArtworkConfig        `yaml:"artwork,omitempty"`

	// DisableHTTP3 prevents the server from binding UDP ports and
	// advertising Alt-Svc headers for HTTP/3 upgrades. Defaults to false.
	DisableHTTP3 bool `yaml:"disableHttp3,omitempty"`
}

// UPnPUpstreamConfig governs the opt-in "ingest an upstream UPnP/DLNA
// MediaServer" feature — the bridge runs an SSDP M-SEARCH client for
// `device:MediaServer:1`, walks the discovered server's "Browse Folders"
// view into its own manifest, and proxies file reads to iOS over the
// bridge's authed/pinned HTTP transport. Companion to the DLNA
// MediaServer + Discovery sections — this one points OUTWARD, those
// inward (server self + renderer discovery).
//
// **LAN-only** by design: SSDP multicast can't traverse Tailscale. The
// upstream MediaServer must live on the same network segment as the
// bridge; the iOS client reaches the bridge over any transport (LAN /
// Tailscale / public mode) since the bridge proxies the file bytes.
//
// **Public-mode refusal**: Config.Validate refuses Enabled=true on a
// public bridge — SSDP multicast (LAN-only by spec) AND the upstream's
// RFC1918 byte URLs (e.g. http://192.168.0.62:8200/...) are both
// unreachable from a public VPS. Same gate class as mdns.enabled and
// DLNA.Enabled — see Validate() for the error string.
//
// **Source-of-truth for identity is the upstream's filesystem-tree
// view** ("Browse Folders" on MiniDLNA, container id "64"). The bridge
// reconstructs a stable path (`<prefix>/Artist/Album/NN - Title.ext`)
// from the directory chain, hashes that to the trackID iOS persists,
// and re-resolves the volatile <res> URL at fetch time.
type UPnPUpstreamConfig struct {
	// Enabled flips the feature on. Default false — discovery only
	// runs when an operator opts in (the M-SEARCH cadence is mild
	// but the feature is meaningless without at least one Server
	// entry, and we don't want to invite questions from users who
	// only have a single-direction DLNA setup).
	Enabled bool `yaml:"enabled,omitempty"`

	// Servers configures the upstream MediaServers the bridge should
	// ingest. Empty list means "discovery only, log what we find but
	// don't walk anything" — useful for an operator to learn the
	// UDN of their 2Go before adding a per-server entry.
	Servers []UPnPUpstreamServerConfig `yaml:"servers,omitempty"`

	// MSearchIntervalSeconds is the discovery cadence. Default 60s —
	// twice the renderer side's 30s because MediaServers are
	// always-on devices, not phones that flap online/offline.
	MSearchIntervalSeconds int `yaml:"msearchIntervalSeconds,omitempty"`

	// ServerTTLSeconds is the staleness window. Default 180s — ~3
	// missed M-SEARCH cycles of grace, strict enough that a server
	// gone for a few minutes drops from the live host:port
	// resolver. MUST be > MSearchIntervalSeconds (Validate
	// enforces).
	ServerTTLSeconds int `yaml:"serverTTLSeconds,omitempty"`
}

// UPnPUpstreamServerConfig is one per-upstream entry under
// UPnPUpstream.Servers.
type UPnPUpstreamServerConfig struct {
	// Name is the operator-visible label (logs, admin console). Required.
	Name string `yaml:"name"`

	// UDN matches the upstream's `uuid:<uuid>` so SSDP-discovered
	// host:port + control URL flow to this entry. Either UDN or
	// ManualDescriptionURL is required.
	UDN string `yaml:"udn,omitempty"`

	// ManualDescriptionURL is a fallback for networks where SSDP
	// multicast doesn't reach the bridge (operator pastes the
	// /rootDesc.xml URL from the upstream's admin page). Either
	// UDN or this is required.
	ManualDescriptionURL string `yaml:"manualDescriptionURL,omitempty"`

	// PathPrefix is prepended to every track's manifest Path so
	// multi-upstream setups can't collide. Defaults to a sanitized
	// form of Name when omitted. The trackID hashes on this prefix
	// — changing it after a scan re-keys every track.
	PathPrefix string `yaml:"pathPrefix,omitempty"`

	// RootObjectID is the ContentDirectory id whose subtree the
	// walker traverses. Defaults to "64" (MiniDLNA's Browse Folders
	// — the real filesystem view, the stable-identity source). Set
	// explicitly for non-MiniDLNA servers that expose the
	// filesystem view under a different id.
	RootObjectID string `yaml:"rootObjectID,omitempty"`

	// SkipTopLevelContainers is a case-insensitive list of folder
	// titles to skip at the root of the walk (e.g. "System Volume
	// Information" on the 2Go's FAT card).
	SkipTopLevelContainers []string `yaml:"skipTopLevelContainers,omitempty"`
}

// Default-rooting constants for UPnPUpstreamConfig — exported so the
// scanner/ingest layer reads the same constants the validator enforces.
const (
	DefaultUPnPUpstreamMSearchInterval = 60 * time.Second
	DefaultUPnPUpstreamServerTTL       = 180 * time.Second
	DefaultUPnPUpstreamRootObjectID    = "64"
)

// EffectiveMSearchInterval returns the operator value when positive,
// otherwise the default.
func (u UPnPUpstreamConfig) EffectiveMSearchInterval() time.Duration {
	if u.MSearchIntervalSeconds > 0 {
		return time.Duration(u.MSearchIntervalSeconds) * time.Second
	}
	return DefaultUPnPUpstreamMSearchInterval
}

// EffectiveServerTTL returns the operator value when positive,
// otherwise the default.
func (u UPnPUpstreamConfig) EffectiveServerTTL() time.Duration {
	if u.ServerTTLSeconds > 0 {
		return time.Duration(u.ServerTTLSeconds) * time.Second
	}
	return DefaultUPnPUpstreamServerTTL
}

// EffectiveRootObjectID returns the configured id or the MiniDLNA
// default ("64", Browse Folders).
func (s UPnPUpstreamServerConfig) EffectiveRootObjectID() string {
	if id := strings.TrimSpace(s.RootObjectID); id != "" {
		return id
	}
	return DefaultUPnPUpstreamRootObjectID
}

// Validate the upnpUpstream block. Called from Config.Validate. Returns
// nil when the block is disabled or empty so an off-by-default operator
// pays no cost.
func (u UPnPUpstreamConfig) Validate() error {
	if !u.Enabled {
		return nil
	}
	// A negative cadence is always a typo — reject it loudly instead of
	// letting EffectiveMSearchInterval's `> 0` guard silently substitute
	// the default (the standardized negative-handling from the r1 review).
	if u.MSearchIntervalSeconds < 0 {
		return fmt.Errorf("upnpUpstream.msearchIntervalSeconds: must be >= 0 (0 uses the default), got %d", u.MSearchIntervalSeconds)
	}
	if u.ServerTTLSeconds < 0 {
		return fmt.Errorf("upnpUpstream.serverTTLSeconds: must be >= 0 (0 uses the default), got %d", u.ServerTTLSeconds)
	}
	// Compare EFFECTIVE durations — raw-field comparison would falsely
	// pass when one side is 0 (which silently falls back to the default),
	// e.g. raw {MS: 120, TTL: 0} satisfies "0 <= 120" yet the effective
	// {MS: 120s, TTL: 180s} pair is in fact valid. The effective-pair
	// form also surfaces an *inconsistent* operator-supplied pair like
	// raw {MS: 240, TTL: 0} → effective MS=240s, TTL=180s as the
	// genuine misconfiguration it is.
	ms := u.EffectiveMSearchInterval()
	ttl := u.EffectiveServerTTL()
	if ttl <= ms {
		return fmt.Errorf(
			"upnpUpstream: serverTTLSeconds (%v) must be > msearchIntervalSeconds (%v) — otherwise still-online servers evict between cycles",
			ttl, ms)
	}
	seenPrefix := make(map[string]int, len(u.Servers))
	seenUDN := make(map[string]int, len(u.Servers))
	for i, s := range u.Servers {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("upnpUpstream.servers[%d]: name is required", i)
		}
		hasUDN := strings.TrimSpace(s.UDN) != ""
		hasURL := strings.TrimSpace(s.ManualDescriptionURL) != ""
		if !hasUDN && !hasURL {
			return fmt.Errorf(
				"upnpUpstream.servers[%d] (%q): either udn or manualDescriptionURL is required",
				i, s.Name)
		}
		// Normalize the way the walker does: trim spaces AND surrounding
		// slashes. Otherwise "Chord 2Go" and "Chord 2Go/" would slip past
		// this check yet collide at walk time.
		prefix := strings.Trim(strings.TrimSpace(s.PathPrefix), "/")
		if prefix == "" {
			prefix = strings.Trim(strings.TrimSpace(s.Name), "/")
		}
		if prefix == "" {
			// Name/pathPrefix were only slashes/whitespace (e.g. "///").
			// An empty prefix would route this upstream's tracks at the
			// library root, tangling them with local paths — reject loudly.
			return fmt.Errorf(
				"upnpUpstream.servers[%d] (%q): resolved path prefix is empty (name and pathPrefix are only slashes/whitespace)",
				i, s.Name)
		}
		if other, dup := seenPrefix[strings.ToLower(prefix)]; dup {
			return fmt.Errorf(
				"upnpUpstream.servers[%d] (%q): pathPrefix %q collides with servers[%d]",
				i, s.Name, prefix, other)
		}
		seenPrefix[strings.ToLower(prefix)] = i
		if hasUDN {
			udn := strings.ToLower(strings.TrimSpace(s.UDN))
			if other, dup := seenUDN[udn]; dup {
				return fmt.Errorf(
					"upnpUpstream.servers[%d] (%q): udn duplicates servers[%d]",
					i, s.Name, other)
			}
			seenUDN[udn] = i
		}
	}
	return nil
}

// IntegrityConfig controls the proactive consistency watchers
// — today just the upscale-variant sweep. The library-scanner's
// own scheduling lives at the top-level `scanIntervalSec` for
// back-compat with v1.0 deploys; this block exists so future
// orthogonal integrity surfaces (artwork-cache reconcile, sidecar
// freshness re-validate) can join the same YAML node without
// scattering top-level fields.
// EnrichConfig overrides the upstream sources the background enricher
// queries. Empty fields fall back to the public services (MusicBrainz /
// Cover Art Archive). Point them at a self-hosted 1-bit-atlas mirror to
// keep enrichment on your own network: set musicbrainzBaseURL to
// "https://<atlas-host>/ws/2" and coverArtBaseURL to "https://<atlas-host>"
// — atlas implements exactly the release/artist search + lookup and the
// /release/{mbid}/front-{250,500,1200} cover endpoints the enricher uses,
// so no other change is needed. Env: BRIDGE_MUSICBRAINZ_BASE_URL /
// BRIDGE_COVERART_BASE_URL.
type EnrichConfig struct {
	// MusicBrainzBaseURL overrides the MusicBrainz ws/2 API root.
	// Default (empty): https://musicbrainz.org/ws/2.
	MusicBrainzBaseURL string `yaml:"musicbrainzBaseURL,omitempty"`
	// CoverArtBaseURL overrides the Cover Art Archive root.
	// Default (empty): https://coverartarchive.org.
	CoverArtBaseURL string `yaml:"coverArtBaseURL,omitempty"`
}

// AtlasConfig governs the optional rich-tier Atlas metadata integration
// (Phase 2). When Enabled, the bridge accepts metadata pushed by the
// closed-source iOS app via POST /v1/atlas-ingest (the app holds the Atlas
// read:bridge credential; the open-source bridge never does) and serves it
// back via GET /v1/atlas-meta/{release,artist}/{mbid} to all the user's
// devices. Disabled by default; restart-required (the endpoints + the
// `atlasEnrichment` health flag are wired once at `bridge serve` startup).
//
// Distinct from the `enrich` block: that points the artwork / MusicBrainz
// clients at an Atlas mirror (where cover art + MBIDs come from); `atlas`
// is the rich-tier ingest/meta surface (bios, descriptions, genres).
type AtlasConfig struct {
	// Enabled is the master toggle. Default false.
	Enabled bool `yaml:"enabled,omitempty"`
	// MetaTTLHours is the freshness window the bridge advertises for cached
	// metadata (as `ttlSeconds` in the /v1/atlas-meta response). The iOS app
	// re-fetches from Atlas + re-ingests once a cached entity is older than
	// this. Zero defaults to DefaultAtlasMetaTTLHours (720 h = 30 days).
	MetaTTLHours int `yaml:"metaTtlHours,omitempty"`
	// HarvestEnabled turns on the Phase-H bridge-driven bulk harvest: the bridge
	// accepts an iOS-provisioned bulk_harvest credential (POST
	// /v1/atlas-harvest/credential), submits the library's artist GIDs to Atlas,
	// and delta-syncs the harvested bios into the artist_atlas overlay. Default
	// false; the feature stays dormant until a credential is provisioned.
	HarvestEnabled bool `yaml:"harvestEnabled,omitempty"`
}

// DefaultAtlasMetaTTLHours is the metadata freshness window applied when
// AtlasConfig.MetaTTLHours is unset (30 days — editorial metadata is slow-
// moving, so re-fetches are rare).
const DefaultAtlasMetaTTLHours = 720

// EffectiveMetaTTL resolves the metadata freshness window. A non-positive
// MetaTTLHours falls back to DefaultAtlasMetaTTLHours.
func (a AtlasConfig) EffectiveMetaTTL() time.Duration {
	h := a.MetaTTLHours
	if h <= 0 {
		h = DefaultAtlasMetaTTLHours
	}
	return time.Duration(h) * time.Hour
}

// ArtworkConfig governs the on-disk artwork cache at <dataDir>/artwork/ —
// the shared directory where the scanner writes local- embedded/folder art,
// the enricher writes MusicBrainz / Cover Art Archive / iTunes covers, and
// (Atlas Phase 2) premium hi-res covers land. Distinct from `enrich` /
// `atlas`, which decide WHERE artwork is fetched from; this block bounds how
// much of it is kept on disk.
type ArtworkConfig struct {
	// CacheMaxBytes caps the total on-disk size of the artwork cache. When
	// the cache exceeds this, a periodic background sweeper evicts the
	// least-recently-modified cached covers until it is back under the cap.
	// Zero (the default) means unbounded — no eviction, the historical
	// behaviour. A library's covers are naturally bounded (one per album, a
	// few sizes) so a cap is rarely needed; it becomes advisable once premium
	// hi-res (1200px) covers are enabled, where whole-library covers can fill
	// a small host disk. A negative value is rejected by Validate.
	CacheMaxBytes int64 `yaml:"cacheMaxBytes,omitempty"`
}

type IntegrityConfig struct {
	// VariantSweepIntervalSec controls how often the
	// integrity.VariantWatcher walks `track_variants` and
	// stat()s each sidecar to detect external deletions.
	// Default 3600 s (1 h). Explicit zero disables the
	// watcher entirely — operators on minimal deploys who
	// run `bridge upscale --gc` manually opt out via this
	// knob.
	//
	// Pointer-typed so applyDefaults can distinguish
	// "missing field, use default" from "explicit zero,
	// disable". Same pattern LimitsConfig.RequestsPerMinute
	// uses for the same reason. Always read via
	// Config.VariantSweepInterval() below — never
	// dereference directly.
	//
	// Pair with the reactive serve-side cleanup in
	// internal/api.serveVariant: that path closes the
	// active-playback case the moment a client requests a
	// missing sidecar. The watcher catches the
	// not-currently-playing case at most VariantSweepInterval
	// later. Both publish the same `upscale.deleted` SSE
	// event so iOS reconciliation is uniform.
	VariantSweepIntervalSec *int `yaml:"variantSweepIntervalSec,omitempty"`

	// OrphanSidecarSweepIntervalSec controls how often the
	// integrity.OrphanSidecarSweeper walks `<variantsDir>/transcoded/`
	// for sidecar files that have no matching `track_variants` row
	// and unlinks them. The forward-sweep half of what the operator-
	// triggered `bridge upscale --gc` does today; the existing
	// VariantWatcher above handles the reverse half (DB rows whose
	// sidecar file no longer exists).
	//
	// **Default zero (disabled)** — opt-in feature. Operators on
	// minimal deploys or low-disk-pressure libraries can keep running
	// `--gc` manually; operators on libraries that churn variants
	// (frequent rip / re-tag passes that delete + re-add tracks under
	// the SAME relative path but produce different `track_variants.id`
	// rows) opt in via a non-zero value here.
	//
	// **Chunked + low-priority**: each tick processes at most
	// `gcChunkSize` filesystem entries (100, defined in the integrity
	// package). The operator-triggered `--gc` keeps its existing
	// unbounded-sweep semantics; this knob exists for the
	// hands-off-operator profile where chunking + cadence-spacing
	// matters.
	//
	// **Snapshot semantics**: each tick takes a `BEGIN DEFERRED`
	// snapshot of `track_variants.sidecar_path` BEFORE the
	// filesystem walk so a concurrent `UpsertVariant` writer can't
	// produce a false-positive orphan (the new sidecar lands on disk
	// before its row commits to the snapshot; pre-snapshot the
	// reverse race would have the sweeper unlink the file before the
	// row caught up).
	//
	// Pointer-typed to distinguish "missing field → use default
	// (disabled)" from "explicit zero → also disabled" — kept
	// symmetric with VariantSweepIntervalSec above even though they
	// resolve to the same behaviour here, so a future migration that
	// flips the default doesn't silently change operator-zeroed
	// configs.
	//
	// Always read via Config.OrphanSidecarSweepInterval() below.
	OrphanSidecarSweepIntervalSec *int `yaml:"orphanSidecarSweepIntervalSec,omitempty"`
}

// ScannerConfig controls the library scanner's resilience knobs.
//
// DeleteAfterMissingScans is the consecutive-missing-scan grace period
// before a row is deleted. The scanner increments per-row
// `missing_count` on every pass where the row is in the before-snapshot
// but NOT in the seen-set AND not under an errorSubtree. Rows reach
// the threshold only after that many CLEAN scans (no surfaced error)
// all failed to see them — defending against silent-empty-enumeration
// modes on network mounts (SMB re-auth flap, NFS brownout, libsmb2
// timeout returning an empty Readdir) that errorSubtrees can't catch
// because no error fired.
//
// Default is 3. Minimum is 1 (preserves the pre-resilience immediate-
// delete behaviour — useful for local-disk-only deployments where the
// failure modes don't apply). Maximum isn't enforced but values > 10
// only make sense in heavily flaky-mount environments; the trade-off
// is that a user-deleted track lingers in search until the threshold
// expires.
type ScannerConfig struct {
	DeleteAfterMissingScans int `yaml:"deleteAfterMissingScans,omitempty"`
}

// LimitsConfig groups operator-facing throttle knobs. Today: just the
// /v1/manifest rate limit. Lives at the top of the YAML so future
// per-endpoint or per-resource limits can join the same block instead
// of scattering across the config surface.
type LimitsConfig struct {
	Manifest ManifestLimitsConfig `yaml:"manifest,omitempty"`
}

// ManifestLimitsConfig controls the per-token-ID token bucket applied
// to /v1/manifest. See internal/api.manifestRateLimiter for the
// runtime shape.
//
// Pointer-typed RequestsPerMinute / Burst preserve the operator's
// intent across the (missing-field) vs (explicit-zero) distinction
// Go's bare `int` collapses. The documented opt-out path is
// `limits.manifest.requestsPerMinute: 0` — explicit zero MUST disable
// the limiter, while a missing field MUST pick up the default. Without
// the pointer, applyDefaults can't tell those apart and silently
// overrides operator-supplied zeros (Gemini HIGH + Greptile P1 on
// PR #194). Same shape as Backup.IntervalHours uses for the same
// reason. Always read via EffectiveRPM / EffectiveBurst below — never
// dereference the pointer fields directly.
type ManifestLimitsConfig struct {
	RequestsPerMinute *int `yaml:"requestsPerMinute,omitempty"`
	Burst             *int `yaml:"burst,omitempty"`
}

// EffectiveRPM returns the configured requests-per-minute. A missing
// field (pointer == nil) returns the default; an explicit zero is
// preserved verbatim — the limiter's constructor maps zero to "no
// budget" so callers fall open. That's the documented opt-out path.
func (m ManifestLimitsConfig) EffectiveRPM() int {
	if m.RequestsPerMinute == nil {
		return DefaultManifestRequestsPerMinute
	}
	return *m.RequestsPerMinute
}

// EffectiveBurst returns the configured burst capacity. A missing
// field returns the default. An explicit zero / negative gets clamped
// to 1 by the limiter constructor — burst=0 in the rate package means
// "deny everything", which would lock out legitimate clients on a
// config typo. Operators opt out of the limiter via RequestsPerMinute=0,
// not via Burst.
func (m ManifestLimitsConfig) EffectiveBurst() int {
	if m.Burst == nil {
		return DefaultManifestBurst
	}
	return *m.Burst
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
//
// Tolerant of leading/trailing whitespace and case differences in
// the YAML value — hand-edited config files frequently pick up
// trailing spaces after a merge / format-on-save, and the
// resulting "unknown value" error trapped operators who couldn't
// see the invisible character. Normalisation is one-way: the
// error message preserves the ORIGINAL untrimmed `t.Mode` so the
// operator sees their actual input verbatim, which matters most
// for a typo path where the invisible-whitespace explanation
// would mislead.
func (t TailscaleConfig) EffectiveMode() (TailscaleMode, error) {
	mode := strings.ToLower(strings.TrimSpace(t.Mode))
	switch mode {
	case "", string(TailscaleModeCLI):
		return TailscaleModeCLI, nil
	case string(TailscaleModeTsnet):
		return TailscaleModeTsnet, nil
	case string(TailscaleModeDisabled):
		return TailscaleModeDisabled, nil
	}
	return "", fmt.Errorf("tailscale.mode: unknown value %q (want cli|tsnet|disabled)", t.Mode)
}

// DeploymentConfig selects the overall deployment posture. `loopback`
// (default) preserves the historical single-operator-on-trusted-LAN
// shape: admin console is loopback-only with no auth, mDNS advertises
// freely, Tailscale auto-pilot runs by default, the self-signed TLS
// cert is the pinning anchor for iOS clients.
//
// `public` opts into the public-VPS posture: admin console can bind
// non-loopback (auth-protected, see internal/adminauth — gated on
// this flag), mDNS suppressed by default, Tailscale defaults to
// disabled, the iOS pinning anchor shifts to a publicly-trusted LE
// cert minted via autocert on the operator's domain.
//
// The flip is deliberately coarse — a single posture switch rather
// than a cascade of independent knobs — and is RestartRequired (the
// admin bind address, TLS listener composition, and auto-pilot
// wiring all change shape; hot-applying mid-process is asking for
// incidents).
type DeploymentConfig struct {
	// Mode selects the posture: "loopback" (default) or "public".
	// Empty value falls back to "loopback" at load time.
	Mode string `yaml:"mode,omitempty"`

	// AdminTLSTerminatedByProxy lets operators run the bridge in
	// public mode behind a TLS-terminating reverse proxy (Caddy /
	// nginx) without having the bridge mint its own admin TLS cert.
	// When true: the admin listener serves plain HTTP on its bind
	// address (operator's responsibility to keep that bind on a
	// private interface or restrict it via firewall); session
	// cookies still carry Secure (so the operator's proxy MUST
	// front them with HTTPS — browsers refuse to send Secure
	// cookies over plain HTTP, surfacing a misconfigured setup as
	// a visible login failure rather than a silent leak). When
	// false (and Mode == public): the bridge wraps its admin
	// listener in tls.NewListener against the same Manager that
	// fronts the public API, so the admin console gets the LE
	// cert for the operator's domain SNI and a self-signed cert
	// for direct-IP / unknown SNI.
	AdminTLSTerminatedByProxy bool `yaml:"adminTLSTerminatedByProxy,omitempty"`
}

// DeploymentMode is the typed representation of DeploymentConfig.Mode.
type DeploymentMode string

const (
	DeploymentModeLoopback DeploymentMode = "loopback"
	DeploymentModePublic   DeploymentMode = "public"
)

// EffectiveMode resolves the configured posture, applying the
// "loopback" default for empty values. Returns one of the two known
// modes or an error when the yaml carries something unrecognised —
// surfaces the typo at config-load time rather than as silent
// loopback-fallback that masks public-mode intent.
//
// Tolerant of leading/trailing whitespace and case differences in
// the YAML value (mirrors TailscaleConfig.EffectiveMode).
func (d DeploymentConfig) EffectiveMode() (DeploymentMode, error) {
	mode := strings.ToLower(strings.TrimSpace(d.Mode))
	switch mode {
	case "", string(DeploymentModeLoopback):
		return DeploymentModeLoopback, nil
	case string(DeploymentModePublic):
		return DeploymentModePublic, nil
	}
	return "", fmt.Errorf("deployment.mode: unknown value %q (want loopback|public)", d.Mode)
}

// AutocertConfig wires native Let's Encrypt provisioning via
// `golang.org/x/crypto/acme/autocert` (TLS-ALPN-01 challenge on the
// same listener as the public API). When enabled in public mode, the
// bridge auto-mints + auto-renews an LE cert for `Domain` and serves
// it on matching SNI through the same `internal/tls.Manager` that
// already fronts the Tailscale magic-DNS cert path.
//
// **Hard ACME constraint**: TLS-ALPN-01 is validated by LE *only*
// on TCP/443. Either set `listenAddress: ":443"` directly OR
// configure an external port-forward / load-balancer mapping
// `WAN:443 → bridge:<listenPort>` and opt-in via
// `external443Mapping: true`. Validate enforces this in public
// mode + autocert.enabled.
type AutocertConfig struct {
	// Enabled toggles the autocert auto-pilot. False (default)
	// means the bridge serves only the self-signed cert. True
	// (in public mode + with a configured Domain + Email) means
	// the bridge mints + serves an LE cert for Domain.
	Enabled bool `yaml:"enabled,omitempty"`

	// Domain is the publicly-routable hostname the operator's iOS
	// clients (and the operator's browser) dial. Required when
	// `deployment.mode: public` is set; ignored otherwise.
	//
	// Consumed by:
	//   - Admin Origin allowlist (PR 2)
	//   - autocert.Manager.HostPolicy (PR 3) — only Domain is
	//     accepted as a host for cert minting
	//   - tls.Manager SNI gate (PR 3) — only requests whose SNI
	//     matches Domain hit the autocert path
	Domain string `yaml:"domain,omitempty"`

	// Email is the operator's contact address registered with the
	// ACME directory. LE uses this to send expiry warnings and
	// service-disruption notices. Required when Enabled.
	Email string `yaml:"email,omitempty"`

	// CacheDir is the on-disk directory where autocert stores the
	// account key, issued certs, and pending challenge state.
	// Empty defaults to `<dataDir>/acme` at load time. Persistence
	// across restarts is load-bearing: wiping this dir between
	// restarts burns the LE duplicate-cert rate-limit budget
	// (5/week per registered domain).
	CacheDir string `yaml:"cacheDir,omitempty"`

	// UseStaging routes the ACME client at LE's staging directory
	// instead of production. Untrusted cert (browsers warn) but
	// no rate limits — useful during deployment-tuning when the
	// production rate limits would otherwise burn fast. Default
	// false (production).
	UseStaging bool `yaml:"useStaging,omitempty"`

	// External443Mapping documents that the operator runs an
	// external port-forward / load balancer that maps `WAN:443`
	// to the bridge's `listenAddress`. Required when `Enabled`
	// AND `listenAddress` doesn't already end in `:443` — without
	// it, LE's TLS-ALPN-01 challenge cannot reach the bridge.
	// True is the operator's promise that the mapping is in
	// place; Validate cannot probe the WAN side.
	External443Mapping bool `yaml:"external443Mapping,omitempty"`
}

// listenAddrIsPort443 reports whether the configured ListenAddress
// binds TCP/443. Used by the autocert gate: LE's TLS-ALPN-01
// validator connects to the SNI host on TCP/443 exclusively, so
// either the bridge listens on :443 directly OR the operator has
// promised an external port-forward (via
// `autocert.external443Mapping: true`).
//
// Tolerant of the conventional bind shapes:
//   - ":443"               (any-interface)
//   - "0.0.0.0:443"        (explicit any-interface IPv4)
//   - "[::]:443"           (IPv6 any)
//   - "192.0.2.10:443"     (specific interface)
//   - "[2001:db8::1]:443"  (IPv6 literal)
func listenAddrIsPort443(addr string) bool {
	if addr == "" {
		return false
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return port == "443"
}

// EffectiveAutocertCacheDir returns the autocert account + cert
// cache directory. When Autocert.CacheDir is set, it's used
// verbatim; when empty, defaults to <dataDir>/acme. Caller
// resolves DataDir before consulting this so the default tracks
// the actual data root across deployments.
func (c *Config) EffectiveAutocertCacheDir() string {
	if c.Autocert.CacheDir != "" {
		return c.Autocert.CacheDir
	}
	return filepath.Join(c.DataDir, "acme")
}

// MDNSConfig controls the Bonjour `_onebit-bridge._tcp` service
// advertisement. Defaults differ by deployment posture:
//
//   - Loopback (default): mDNS is ON. Home LAN deployments rely on
//     it for iOS auto-discovery — without mDNS, every new device
//     would need a manual pair URL.
//   - Public: mDNS is OFF. A VPS has no local LAN to advertise on
//     (the bridge's interfaces are routable WAN addresses);
//     emitting Bonjour records would be a no-op at best and a
//     small attack-surface leak at worst (TXT records expose the
//     bridge's protocol version + library name).
//
// `Enabled` is a pointer so applyDefaults can distinguish
// "missing field, apply posture-default" from "explicit
// operator override" — same shape `IntegrityConfig.VariantSweepIntervalSec`
// uses for the same reason. Always read via `EffectiveMDNSEnabled()`;
// never dereference Enabled directly (nil-deref hazard).
type MDNSConfig struct {
	Enabled *bool `yaml:"enabled,omitempty"`
}

// EffectiveMDNSEnabled returns the resolved mDNS posture: explicit
// operator value when set, otherwise the deployment-mode default
// (true for loopback, false for public).
//
// Tolerates either form at the YAML layer; the pointer indirection
// is the single source of truth for missing-vs-explicit.
func (c *Config) EffectiveMDNSEnabled() bool {
	if c.MDNS.Enabled != nil {
		return *c.MDNS.Enabled
	}
	return !c.IsPublic()
}

// IsPublic reports whether the configured deployment mode is "public".
// Errors during EffectiveMode resolution (unknown values) are treated
// as non-public — Validate surfaces the typo separately at load time,
// and we don't want a typo to silently relax the loopback gate.
func (c *Config) IsPublic() bool {
	mode, err := c.Deployment.EffectiveMode()
	if err != nil {
		return false
	}
	return mode == DeploymentModePublic
}

// DLNAConfig governs the opt-in DLNA MediaServer the bridge exposes
// on its own parallel http.Server bound LAN/Tailnet-only. Disabled
// by default — operators who pair iOS with a DLNA renderer (Chord
// 2go, Lumin, dCS Network Bridge, etc.) flip Enabled to advertise.
//
// **Public deployment refusal**: even when Enabled is true here, the
// dlna.ShouldEnableDLNA gate in cmd/bridge/main.go REFUSES to bind
// the listener when Config.IsPublic() is true. SSDP multicast on a
// public-internet VPS subnet is undefined behaviour AND would
// advertise the operator's library to the open internet — the gate
// is non-negotiable. Document in operator-facing release notes.
type DLNAConfig struct {
	// Enabled flips the feature on. Default false.
	Enabled bool `yaml:"enabled,omitempty"`

	// ListenAddress is the TCP bind for the DLNA HTTP server.
	// Default ":7790" — chosen to sit alongside the existing
	// :7788 (API) / :7789 (admin) range without colliding with
	// anything else the bridge binds.
	ListenAddress string `yaml:"listenAddress,omitempty"`

	// FriendlyName is the DLNA-advertised device name surfaced
	// in renderer "select source" UIs. Defaults to "1-bit Bridge"
	// when empty.
	FriendlyName string `yaml:"friendlyName,omitempty"`

	// AllowTsnet opts into binding the DLNA listener on the
	// Tailscale interface alongside LAN. Default false because
	// SSDP multicast doesn't traverse Tailscale's point-to-point
	// tunnel (verified Phase 0 2026-05-26) — iOS clients on
	// cellular must use manual renderer entry (PR 3) or the
	// bridge-mediated discovery path (PR 5+6) for cross-network
	// playback. Setting this true is only useful for the
	// uncommon case where renderer + iOS are BOTH on the same
	// tailnet (renderer published via subnet router with
	// multicast forwarding hacks).
	AllowTsnet bool `yaml:"allowTsnet,omitempty"`

	// TelemetryEnabled wires the per-request middleware that
	// captures User-Agent / Range / status / bytes per renderer
	// connection into a bounded ring buffer. Default true —
	// overhead is sub-millisecond per request and the data feeds
	// vendor-profile refinement + the adaptive chunk allocator's
	// network multiplier coefficients. Operators on resource-
	// constrained deploys can opt out.
	TelemetryEnabled *bool `yaml:"telemetryEnabled,omitempty"`

	// Discovery configures the SSDP M-SEARCH client + renderer
	// cache that powers `GET /v1/renderers`. Wired in PR 5 of the
	// DLNA roadmap. Optional — defaults preserve the v1.4 behaviour
	// of no renderer-discovery surface when the operator hasn't
	// explicitly opted in.
	Discovery DLNADiscoveryConfig `yaml:"discovery,omitempty"`
}

// DLNADiscoveryConfig governs the opt-in SSDP renderer discovery
// surface — the bridge runs an M-SEARCH client on its LAN-eligible
// interface, caches discovered MediaRenderers, and exposes the cache
// via `GET /v1/renderers`. Companion to the DLNA MediaServer
// (Enabled-gate above): both have to be on for iOS clients to use
// bridge-mediated renderer discovery as a cross-network path.
//
// **Discovery is gated behind `DLNA.Enabled`** — running the
// discovery client without the MediaServer is a coherent
// configuration but not one we surface UX for (the
// `rendererDiscovery` feature flag in /v1/health depends on the
// DLNA package being initialized). Operators who want
// discovery-only deployments are an edge case for a future PR.
//
// **Public-mode refusal applies upstream** at the same chokepoint
// as the MediaServer — `dlna.ShouldEnableDLNA(...)` returning
// false skips Discovery startup regardless of this field's value.
type DLNADiscoveryConfig struct {
	// Enabled flips renderer discovery on. Default false — the
	// MediaServer can run without discovery, but discovery is
	// noisier (periodic multicast) and operators may not want it
	// surfaced when they're using SSDP-direct iOS pairing.
	Enabled bool `yaml:"enabled,omitempty"`

	// MSearchIntervalSeconds is the M-SEARCH cadence. Default
	// 30s — matches the v1 plan. Tunable for operators whose
	// renderers are slow to respond / whose LAN is saturated.
	MSearchIntervalSeconds int `yaml:"msearchIntervalSeconds,omitempty"`

	// RendererTTLSeconds is the staleness window. Default 60s —
	// must be > MSearchIntervalSeconds so one missed cycle
	// doesn't evict a still-online renderer.
	RendererTTLSeconds int `yaml:"rendererTTLSeconds,omitempty"`
}

// DefaultDLNADiscoveryMSearchInterval is the cadence used when
// DLNADiscoveryConfig leaves MSearchIntervalSeconds empty.
const DefaultDLNADiscoveryMSearchInterval = 30 * time.Second

// DefaultDLNADiscoveryRendererTTL is the staleness window used
// when DLNADiscoveryConfig leaves RendererTTLSeconds empty.
const DefaultDLNADiscoveryRendererTTL = 60 * time.Second

// EffectiveMSearchInterval returns the operator value (as time.Duration)
// when positive, otherwise the default.
func (d DLNADiscoveryConfig) EffectiveMSearchInterval() time.Duration {
	if d.MSearchIntervalSeconds > 0 {
		return time.Duration(d.MSearchIntervalSeconds) * time.Second
	}
	return DefaultDLNADiscoveryMSearchInterval
}

// EffectiveRendererTTL returns the operator value (as time.Duration)
// when positive, otherwise the default.
func (d DLNADiscoveryConfig) EffectiveRendererTTL() time.Duration {
	if d.RendererTTLSeconds > 0 {
		return time.Duration(d.RendererTTLSeconds) * time.Second
	}
	return DefaultDLNADiscoveryRendererTTL
}

// DefaultDLNAListenAddress is the TCP bind used when DLNAConfig
// leaves ListenAddress empty.
const DefaultDLNAListenAddress = ":7790"

// DefaultDLNAFriendlyName is the device name advertised when
// DLNAConfig leaves FriendlyName empty.
const DefaultDLNAFriendlyName = "1-bit Bridge"

// EffectiveDLNAListenAddress returns the operator value when set,
// otherwise DefaultDLNAListenAddress.
func (d DLNAConfig) EffectiveDLNAListenAddress() string {
	if a := strings.TrimSpace(d.ListenAddress); a != "" {
		return a
	}
	return DefaultDLNAListenAddress
}

// EffectiveDLNAFriendlyName returns the operator value when set,
// otherwise DefaultDLNAFriendlyName.
func (d DLNAConfig) EffectiveDLNAFriendlyName() string {
	if n := strings.TrimSpace(d.FriendlyName); n != "" {
		return n
	}
	return DefaultDLNAFriendlyName
}

// EffectiveDLNATelemetryEnabled defaults true when the YAML omits
// the field; explicit `false` honored.
func (d DLNAConfig) EffectiveDLNATelemetryEnabled() bool {
	if d.TelemetryEnabled == nil {
		return true
	}
	return *d.TelemetryEnabled
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

	// OptimizeEnabled gates the CarPlay-optimize feature
	// (`optimized-*` variant class — downsample to 16-bit /
	// 44.1k or 48k for fast cellular CarPlay playback).
	// Pointer-bool so the default is true when the field is
	// absent (`nil` → enabled); operators on storage-constrained
	// hosts can opt out via `optimizeEnabled: false`. Ignored
	// when `Enabled` is false (the master toggle covers both).
	OptimizeEnabled *bool `yaml:"optimizeEnabled,omitempty"`

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

	// VariantsDir lets operators relocate upscaled FLAC sidecars off
	// the bridge's data partition onto any writable mount they have
	// space on — external drive, secondary disk, dedicated NAS subdir.
	// Empty (the default) falls through to `<dataDir>/transcoded/`
	// (preserved historical layout — no existing install breaks).
	//
	// MUST be an absolute path. MUST NOT resolve under any library
	// root (catastrophic — variants tangled with source files would
	// confuse the scanner AND collide with the read-only library
	// invariant from PR #75). Validation in `Config.Validate` enforces
	// both constraints; the directory itself is created on first
	// upscale via `os.MkdirAll`.
	//
	// On-disk layout under VariantsDir is source-path-mirrored
	// (`<artist>/<album>/<basename>.<variantID>.flac` style) so
	// operators with write access to their library can `mv` the
	// variants dir contents into the library and slot variants next
	// to source files. The DB lookup remains the only path-resolution
	// mechanism — moves do NOT change the wire shape; iOS doesn't
	// care.
	VariantsDir string `yaml:"variantsDir,omitempty"`
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

// EffectiveOptimizeEnabled returns the optimize-feature gate value
// with nil-defaulting-to-true. Operators who never touch the field
// inherit the on-by-default behavior; `optimizeEnabled: false` in
// YAML explicitly opts out (storage-constrained hosts who only
// want upscale).
func (u UpscaleConfig) EffectiveOptimizeEnabled() bool {
	if u.OptimizeEnabled == nil {
		return true
	}
	return *u.OptimizeEnabled
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

// EffectiveBootstrapTargetRate resolves a concrete integer Hz value
// for seeding the DB-backed operator-controlled upscale target on
// first run. The legacy `TargetRate` field accepts "auto" (the
// source-rate-aware default) and arbitrary strings for the
// per-track CLI / HTTP path; the new v1.3 operator-driven model
// stores a fixed integer in scan_state that admin Settings edits.
//
// Resolution order:
//   - explicit numeric string (e.g. "192000") → parsed and returned
//   - "auto" or unset → DefaultBootstrapTargetRate (192000 Hz)
//   - parse failure → DefaultBootstrapTargetRate (don't fail boot
//     on a malformed YAML; admin Settings can correct via UI)
//
// Called once by cmd/bridge/main.go at boot: if scan_state has no
// upscale_target_hz row, seed it via Store.SetUpscaleTarget(this,
// EffectiveBootstrapTargetBits()).
func (u UpscaleConfig) EffectiveBootstrapTargetRate() int {
	target := strings.TrimSpace(u.TargetRate)
	if target == "" || target == "auto" {
		return DefaultBootstrapTargetRate
	}
	if n, err := strconv.Atoi(target); err == nil && n > 0 {
		return n
	}
	return DefaultBootstrapTargetRate
}

// EffectiveBootstrapTargetBits is the int counterpart to
// EffectiveBootstrapTargetRate for the scan_state seed. Reuses the
// existing EffectiveTargetBits resolver (16/24/32 with 24 default)
// so the operator-facing default matches the legacy CLI behaviour.
func (u UpscaleConfig) EffectiveBootstrapTargetBits() int {
	return u.EffectiveTargetBits()
}

// EffectiveVariantsDir resolves the absolute on-disk directory the
// transcode pool writes variant sidecars to. Explicit YAML setting
// wins; empty falls through to the legacy `<dataDir>/transcoded/`
// path so existing installs without a `variantsDir` setting are
// unaffected.
//
// Centralised so every consumer (transcode pool, admin "Stored at"
// surface, free-space probe, CLI `bridge variants move`) reads from
// one helper and can't drift. Mirrors the
// `EffectiveBootstrapTargetRate` convention.
//
// Pure / no I/O. Validation (must be absolute, must not be under a
// library root) lives in `Config.Validate` so a misconfiguration
// fails fast at boot — by the time this helper is called, the path
// has already been vetted.
func (u UpscaleConfig) EffectiveVariantsDir(dataDir string) string {
	if u.VariantsDir != "" {
		return u.VariantsDir
	}
	// Delegate to the transcode default helper via the
	// well-known subdir name. Inlined here to avoid a config →
	// transcode import (config is the foundation; transcode
	// imports config). The constant value MUST track
	// transcode.OutputDirSubdir.
	return filepath.Join(dataDir, defaultVariantsSubdir)
}

// AnalysisConfig governs the optional offline audio-analysis feature
// (`bridge analyze`) — Phase 1 computes a peak waveform sidecar per
// track for the iOS scrubber. Disabled by default; opt in here AND
// install sox (the same dependency upscaling uses — analysis decodes
// through it). A `true` config with sox missing degrades to feature-off
// in-memory at startup, like upscaling.
type AnalysisConfig struct {
	// Enabled is the master toggle. Default false.
	Enabled bool `yaml:"enabled,omitempty"`

	// Workers is the size of the long-lived analysis worker pool.
	// Zero (the default) resolves to max(1, NumCPU/2) — half the cores,
	// because each decoder streams a multi-MB PCM buffer and we leave
	// headroom for media streaming on Pi/NAS-class hosts.
	Workers int `yaml:"workers,omitempty"`

	// QueueCap bounds the pending-job channel; enqueues past it are
	// rejected (ErrQueueFull) rather than blocking. Zero resolves to
	// DefaultAnalysisQueueCap.
	QueueCap int `yaml:"queueCap,omitempty"`
}

// EffectiveWorkers resolves the analysis worker count: explicit YAML
// value wins; zero defaults to max(1, NumCPU/2). Lower than upscaling's
// floor by design — analysis is heavier per job (full decode) and runs
// as a background batch.
func (a AnalysisConfig) EffectiveWorkers() int {
	if a.Workers > 0 {
		return a.Workers
	}
	n := runtime.NumCPU() / 2
	if n < 1 {
		n = 1
	}
	return n
}

// EffectiveQueueCap resolves the pending-job channel cap, defaulting to
// DefaultAnalysisQueueCap when the YAML field is zero.
func (a AnalysisConfig) EffectiveQueueCap() int {
	if a.QueueCap > 0 {
		return a.QueueCap
	}
	return DefaultAnalysisQueueCap
}

// SmartPlaylistsConfig governs the optional server-side smart/dynamic
// playlist generator (Heavy Rotation, Auto Mix, Forgotten Favorites,
// time-of-day, Daily Mix, The Finish Line). Disabled by default; opt in
// here. Composes with the analysis + history features it draws from: the
// listening families work from playback history alone, while the harmonic
// Auto Mix + Daily Mix discovery self-omit unless `analysis.enabled` is on
// (the generator gates them internally). Results are precomputed on a daily
// cadence into the `smart_playlists` cache table and served by
// GET /v1/smart-playlists.
type SmartPlaylistsConfig struct {
	// Enabled is the master toggle. Default false.
	Enabled bool `yaml:"enabled,omitempty"`

	// RegenerateIntervalSec is the background-regeneration cadence. Zero
	// (the default) resolves to 24h — these families move on a daily scale
	// (Heavy Rotation / Daily Mix are day-cadence concepts), and a daily
	// snapshot keeps GET /v1/smart-playlists a single fast cache read while
	// staying stable within the day so the homepage doesn't reshuffle.
	RegenerateIntervalSec int `yaml:"regenerateIntervalSec,omitempty"`
}

// EffectiveRegenerateInterval resolves the regeneration cadence, defaulting
// to 24h when the YAML field is zero.
func (c SmartPlaylistsConfig) EffectiveRegenerateInterval() time.Duration {
	if c.RegenerateIntervalSec > 0 {
		return time.Duration(c.RegenerateIntervalSec) * time.Second
	}
	return 24 * time.Hour
}

// defaultVariantsSubdir mirrors transcode.OutputDirSubdir. Kept as
// a duplicated constant to avoid a config → transcode import; both
// values MUST stay in lockstep. Validated by a same-package test
// that asserts the string matches transcode.OutputDirSubdir's
// observable behaviour (`OutputDirFor("a") == filepath.Join("a", defaultVariantsSubdir)`).
const defaultVariantsSubdir = "transcoded"

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
	// rotation. Older snapshots beyond this count are deleted on each
	// periodic snapshot. A NEGATIVE value (e.g. `keep: -1`) disables
	// pruning — the operator manages backup-disk usage by hand in that
	// case (a negative value won't trip Validate).
	//
	// Zero or an omitted `backup` section BOTH fall back to
	// DefaultBackupKeep: `keep,omitempty` can't distinguish "absent" from
	// "explicit 0", and EffectiveKeep() maps zero to the default. So
	// `keep: 0` does NOT disable pruning — use `keep: -1` for that.
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

	// DefaultDeleteAfterMissingScans is the grace period before the
	// scanner deletes a row that's been missing from successive scans.
	// 3 is a balance: short enough that a user-deleted track disappears
	// from search inside a typical day's rescan cadence (6h × 3 = 18h),
	// long enough to absorb the silent-empty-enumeration failure modes
	// network mounts produce occasionally. Operators on local-disk-only
	// deployments can override to 1 to preserve pre-resilience behaviour.
	DefaultDeleteAfterMissingScans = 3

	// DefaultManifestRequestsPerMinute / DefaultManifestBurst configure
	// the per-token /v1/manifest rate limit. 6 rpm + 3 burst lets the
	// first three calls fire instant (typical paginated scan: pull
	// page, process, pull next) and then paces the steady state at one
	// call every ~10 s. Tuned for the realistic worst case — an iOS
	// client doing a full-manifest re-pull after a long offline window
	// where it caches nothing on disk. Operators with bursty traffic
	// can raise; operators tightening defence-in-depth can lower.
	// Setting RequestsPerMinute to a negative value (or zero with
	// burst > 0) disables the limit entirely — see manifestRateLimiter.
	DefaultManifestRequestsPerMinute = 6
	DefaultManifestBurst             = 3
	DefaultBackupKeep                = 7
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
	// DefaultAnalysisQueueCap bounds the pending-job channel of the
	// offline audio-analysis worker pool. Same shape + rationale as
	// DefaultUpscaleQueueCap — a whole-library `bridge analyze` enqueue
	// bounces against a clean rejection rather than exhausting memory.
	DefaultAnalysisQueueCap = 5000
	// DefaultBootstrapTargetRate is the integer Hz value used to seed
	// scan_state.upscale_target_hz on first run when the YAML field
	// is unset or "auto". 192000 covers most 44.1- and 48-family
	// sources without resampling artifacts; admin Settings can edit
	// at runtime via the v1.3 Library Inspector. Per
	// `UpscaleConfig.EffectiveBootstrapTargetRate`.
	DefaultBootstrapTargetRate = 192000
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
//	BRIDGE_MUSICBRAINZ_BASE_URL — overrides Enrich.MusicBrainzBaseURL
//	BRIDGE_COVERART_BASE_URL    — overrides Enrich.CoverArtBaseURL
//	BRIDGE_LIBRARY_ROOTS   — OS-native-PATH-separated; overrides
//	                         LibraryRoots. POSIX uses `:`,
//	                         Windows uses `;` so drive-letter
//	                         paths (`C:\Music`) aren't corrupted.
//
// Path-typed values (DataDir, LibraryRoots) still go through
// `resolvePaths` afterwards so a relative path inherits the same
// "relative-to-config-dir" semantics as a YAML field.
// applyBoolEnv sets *dst from the boolean env var `key` when it is present
// and parseable via strconv.ParseBool. A present-but-unparseable value
// (e.g. `yes`, `on`, or a typo) is logged at Warn and ignored — the
// YAML/default value stands, so a mistyped override can't silently flip a
// feature to false. An unset / empty var is a no-op.
func applyBoolEnv(key string, dst *bool) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	if b, err := strconv.ParseBool(v); err == nil {
		*dst = b
	} else {
		validateLogger.Warn("ignoring unparseable boolean env var", "var", key, "value", v, "err", err)
	}
}

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
	if v := os.Getenv("BRIDGE_MUSICBRAINZ_BASE_URL"); v != "" {
		c.Enrich.MusicBrainzBaseURL = v
	}
	if v := os.Getenv("BRIDGE_COVERART_BASE_URL"); v != "" {
		c.Enrich.CoverArtBaseURL = v
	}
	// Boolean toggles, via the shared applyBoolEnv helper: a malformed
	// value is logged at Warn and ignored (the YAML/default stands)
	// rather than coerced to false — a typo like `yes`/`on` must not
	// silently flip a feature off. BRIDGE_UPSCALE_ENABLED /
	// BRIDGE_ANALYSIS_ENABLED are the only env path to enable those two
	// sox/ffmpeg-backed features (otherwise YAML-only); the startup sox
	// probe (soxFeatureReady) still AND-gates actual activation, so
	// setting them true on a host without a usable sox degrades to
	// feature-off with a log line rather than a hard failure. Only the
	// master Enabled bool is exposed; workers / queueCap keep their
	// runtime.NumCPU-derived defaults.
	applyBoolEnv("BRIDGE_DISABLE_HTTP3", &c.DisableHTTP3)
	applyBoolEnv("BRIDGE_UPSCALE_ENABLED", &c.Upscale.Enabled)
	applyBoolEnv("BRIDGE_ANALYSIS_ENABLED", &c.Analysis.Enabled)
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
	// AdminAddress defaults to loopback ONLY in loopback mode. In
	// public mode the operator must explicitly configure the bind
	// (e.g. `0.0.0.0:7789` or a private-interface IP behind a
	// reverse proxy) — silently defaulting to loopback in public
	// mode would leave the admin console unreachable from outside
	// the host and the operator would have a broken setup with no
	// clear breadcrumb. Validate refuses public+empty-adminAddress.
	if c.AdminAddress == "" && !c.IsPublic() {
		c.AdminAddress = DefaultAdminAddress
	}
	// Tailscale mode posture default: public-mode VPS deployments
	// typically don't have Tailscale installed; defaulting to
	// "cli" would spawn fork-execs of a missing binary every 30 s
	// on the /v1/health hot path (see PR #95 cache). Default to
	// "disabled" in public mode + warn at load so the operator
	// sees the explicit-default-applied breadcrumb. Loopback
	// installs keep the historical "cli" default (empty Mode
	// resolves to TailscaleModeCLI in EffectiveMode).
	if strings.TrimSpace(c.Tailscale.Mode) == "" && c.IsPublic() {
		c.Tailscale.Mode = string(TailscaleModeDisabled)
		validateLogger.Warn("tailscale.mode defaulted to disabled in public mode (no implicit CLI probe on the public bridge)")
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
	if c.Scanner.DeleteAfterMissingScans <= 0 {
		c.Scanner.DeleteAfterMissingScans = DefaultDeleteAfterMissingScans
	}
	// Limits section: pointer-typed RequestsPerMinute / Burst preserve
	// the "omitted vs explicit-zero" distinction at YAML-round-trip
	// time, so an operator who writes `requestsPerMinute: 0` genuinely
	// disables the limiter (matches PROTOCOL.md). Defaults are returned
	// by the EffectiveRPM / EffectiveBurst helpers; applyDefaults
	// intentionally leaves the raw fields untouched so a Save+Load
	// round-trip preserves operator intent (same convention as
	// Backup.IntervalHours).

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
	// Autocert cache dir follows the same config-relative path
	// contract as the other on-disk paths above. Without this,
	// a relative `autocert.cacheDir: "acme-cache"` would resolve
	// against the process CWD, which (a) creates a different
	// ACME cache location every time the bridge is launched
	// from a different shell, and (b) forces LE to re-issue
	// against rate-limit quota (CodeRabbit Major on PR #293).
	if c.Autocert.CacheDir != "" && !filepath.IsAbs(c.Autocert.CacheDir) {
		c.Autocert.CacheDir = filepath.Join(baseDir, c.Autocert.CacheDir)
	}
}

// Validate checks invariants the server relies on. Called automatically by
// Load; exposed for tests and for callers that construct Config in memory.
// normalizeBaseURL trims surrounding whitespace and any trailing slash from
// an optional enrich override base URL, and validates that a non-empty value
// is an absolute http(s) URL. Empty stays empty (the enrich client falls back
// to its public default). Trimming the trailing slash prevents double-slash
// request paths (e.g. "https://host/ws/2/" + "/release/..." → "...//release").
func normalizeBaseURL(field, raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	v = strings.TrimRight(v, "/")
	u, err := url.Parse(v)
	if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%s: must be an absolute http(s) URL, got %q", field, raw)
	}
	return v, nil
}

func (c *Config) Validate() error {
	// Surface deployment.mode typos at load time rather than letting
	// them silently fall through to "loopback" (IsPublic-returns-false
	// on unknown values would mask public-mode intent).
	if _, err := c.Deployment.EffectiveMode(); err != nil {
		return err
	}

	// LibraryRoots: loopback installs require at least one root at
	// load time. Public installs may legitimately boot with no roots
	// (operator hasn't mounted the FUSE drive yet, will add via
	// `bridge library add` after the bridge is up) — log a warn so
	// the breadcrumb is visible without failing startup.
	if len(c.LibraryRoots) == 0 {
		if !c.IsPublic() {
			return errors.New("libraryRoots: must have at least one entry")
		}
		validateLogger.Warn("no library roots configured — add via admin or `bridge library add`")
	}
	for _, r := range c.LibraryRoots {
		if r == "" {
			return errors.New("libraryRoots: entries must not be empty")
		}
	}
	// NOTE: existence + is-directory checks for LibraryRoots have
	// deliberately been pulled OUT of Validate() and into
	// CheckLibraryRootsAccessible. Rationale (see CLAUDE.md "Things
	// that have bitten before"): public-mode VPS deployments run the
	// daemon as a non-root user against a FUSE-mounted library that
	// only the daemon user can stat. `sudo bridge update` / `sudo
	// bridge status` then run as root and the stat returns EACCES,
	// taking down the entire CLI path including update + status
	// regardless of whether those subcommands actually need the
	// library. Validate() is now a pure shape check; runtime
	// accessibility is the caller's decision — `bridge serve`
	// invokes CheckLibraryRootsAccessible at startup with
	// mode-dependent strictness, mutation paths (`bridge library
	// add`, admin's apiRootsAdd) already stat independently.
	if c.ScanIntervalSec < 1 {
		return fmt.Errorf("scanIntervalSec: must be >= 1, got %d", c.ScanIntervalSec)
	}
	// Enrich upstream base URLs: normalize (trim whitespace + trailing
	// slash) and require an absolute http(s) URL — surfaces typos at load
	// time instead of as silent runtime enrichment failures, and prevents
	// double-slash request paths against a self-hosted mirror.
	mbBase, err := normalizeBaseURL("enrich.musicbrainzBaseURL", c.Enrich.MusicBrainzBaseURL)
	if err != nil {
		return err
	}
	c.Enrich.MusicBrainzBaseURL = mbBase
	caaBase, err := normalizeBaseURL("enrich.coverArtBaseURL", c.Enrich.CoverArtBaseURL)
	if err != nil {
		return err
	}
	c.Enrich.CoverArtBaseURL = caaBase
	// Artwork cache cap: a negative value is almost certainly a typo. Zero
	// is the valid "unbounded" sentinel; positive is a byte cap. Surface a
	// negative at load time rather than letting the sweeper silently treat
	// it as unbounded.
	if c.Artwork.CacheMaxBytes < 0 {
		return fmt.Errorf("artwork.cacheMaxBytes: must be >= 0 (0 = unbounded), got %d", c.Artwork.CacheMaxBytes)
	}
	// DLNA discovery: TTL must be strictly greater than the
	// M-SEARCH interval — otherwise we'd evict still-online
	// renderers between cycles (the M-SEARCH cycle is the only
	// observation source per the discovery client's
	// no-NOTIFY-listener design). Per CodeRabbit MAJOR round-1
	// on PR #305 — the prior shape documented the invariant in
	// the field docblock but didn't enforce it.
	if c.DLNA.Discovery.Enabled {
		// Standardized negative-interval handling (r1 review): reject a
		// negative cadence/TTL loudly rather than letting EffectiveX's
		// `> 0` guards silently substitute the default. Co-located with
		// the existing TTL-vs-interval check + gated on Enabled, matching
		// the upnpUpstream block's pattern.
		if c.DLNA.Discovery.MSearchIntervalSeconds < 0 {
			return fmt.Errorf("dlna.discovery.msearchIntervalSeconds: must be >= 0 (0 uses the default), got %d", c.DLNA.Discovery.MSearchIntervalSeconds)
		}
		if c.DLNA.Discovery.RendererTTLSeconds < 0 {
			return fmt.Errorf("dlna.discovery.rendererTTLSeconds: must be >= 0 (0 uses the default), got %d", c.DLNA.Discovery.RendererTTLSeconds)
		}
		ms := c.DLNA.Discovery.EffectiveMSearchInterval()
		ttl := c.DLNA.Discovery.EffectiveRendererTTL()
		if ttl <= ms {
			return fmt.Errorf(
				"dlna.discovery: rendererTTLSeconds (%v) must be > msearchIntervalSeconds (%v) — otherwise still-online renderers evict between cycles",
				ttl, ms,
			)
		}
	}
	if err := c.UPnPUpstream.Validate(); err != nil {
		return err
	}
	// A negative regeneration cadence is always a typo — reject it loudly
	// instead of letting EffectiveRegenerateInterval's `> 0` guard silently
	// substitute the 24h default (same standardized handling as the
	// upnpUpstream cadences above).
	if c.SmartPlaylists.RegenerateIntervalSec < 0 {
		return fmt.Errorf("smartPlaylists.regenerateIntervalSec: must be >= 0 (0 uses the default 24h), got %d", c.SmartPlaylists.RegenerateIntervalSec)
	}
	// Public-mode refusal for upnpUpstream. The feature relies on SSDP
	// multicast (LAN-only by spec) AND a route to the upstream's
	// RFC1918 byte URLs (e.g. http://192.168.0.62:8200/MediaItems/...
	// — unreachable from a public VPS). Enabling it on a public bridge
	// would burn CPU on a multicast loop that hears nothing AND fail
	// every proxy fetch. Refuse loudly at load time, matching the
	// mdns.enabled / DLNA.Enabled patterns above.
	if c.IsPublic() && c.UPnPUpstream.Enabled {
		return errors.New("upnpUpstream.enabled: must be false in public mode (SSDP multicast is LAN-only AND the upstream's RFC1918 byte URLs are unreachable from a public VPS)")
	}
	if (c.TLSCertPath == "") != (c.TLSKeyPath == "") {
		return errors.New("tlsCertPath and tlsKeyPath: must be set together, or both empty")
	}
	if _, _, err := net.SplitHostPort(c.ListenAddress); err != nil {
		return fmt.Errorf("listenAddress %q: %w", c.ListenAddress, err)
	}
	// AdminAddress: loopback installs enforce the historical loopback
	// trust boundary (admin console has no auth in loopback mode, so
	// loopback binding IS the security boundary). Public installs
	// require a parseable host:port but allow non-loopback binds —
	// the admin auth layer (gated on IsPublic) becomes the trust
	// boundary instead. See internal/adminauth and the
	// `deployment.adminTLSTerminatedByProxy` knob for the
	// reverse-proxy posture.
	if c.IsPublic() {
		if c.AdminAddress == "" {
			return errors.New("adminAddress: must not be empty in public mode")
		}
		if _, _, err := net.SplitHostPort(c.AdminAddress); err != nil {
			return fmt.Errorf("adminAddress %q: %w", c.AdminAddress, err)
		}
		// Trim before the empty check so a whitespace-only
		// value ("   ") fails fast with the same error message
		// (CodeRabbit Minor review post-PR-#292). Persist the
		// trimmed form back into the config so downstream
		// consumers (tlsacme.New, the admin Origin allowlist,
		// the SNI gate) all see the canonical value.
		c.Autocert.Domain = strings.TrimSpace(c.Autocert.Domain)
		if c.Autocert.Domain == "" {
			return errors.New("autocert.domain: must be set in public mode (the publicly-routable hostname iOS clients dial)")
		}
		// Admin-TLS gate: either the bridge terminates TLS itself
		// (autocert.enabled, using the same cert via the SNI
		// switcher), or an external reverse proxy terminates TLS
		// and forwards plain HTTP to the bridge's admin listener.
		// Serving the admin console over plain HTTP without a
		// proxy in front would leak session cookies + credentials
		// over the open internet — and `Secure` cookies (mandatory
		// in public mode) would refuse to be sent over HTTP
		// anyway, surfacing as a silent login failure.
		if !c.Deployment.AdminTLSTerminatedByProxy && !c.Autocert.Enabled {
			return errors.New("public mode requires either deployment.adminTLSTerminatedByProxy: true OR autocert.enabled: true — admin console cannot be served over plain HTTP")
		}
		// mDNS in public mode is a misconfiguration: a VPS has no
		// LAN to advertise on (interfaces are routable WAN
		// addresses), and the Bonjour TXT records leak the
		// bridge's protocol version + library name to anyone
		// scanning the local subnet at the VPS provider. The
		// EffectiveMDNSEnabled default for public mode is false,
		// so an operator who hasn't touched the field is fine;
		// only an EXPLICIT `mdns.enabled: true` reaches here.
		if c.MDNS.Enabled != nil && *c.MDNS.Enabled {
			return errors.New("mdns.enabled: must be false in public mode (no LAN to advertise on; TXT records leak library metadata)")
		}
		// ACME / autocert prerequisites.
		if c.Autocert.Enabled {
			if c.Autocert.Email == "" {
				return errors.New("autocert.email: must be set when autocert.enabled (LE registers the account key against this address)")
			}
			// TLS-ALPN-01 is validated by LE only on TCP/443.
			// The operator must either bind :443 directly OR
			// confirm an external port-forward maps WAN:443 →
			// the bridge's listenAddress.
			if !listenAddrIsPort443(c.ListenAddress) && !c.Autocert.External443Mapping {
				return fmt.Errorf("autocert.enabled requires listenAddress on port :443 OR autocert.external443Mapping: true — got listenAddress %q (LE's TLS-ALPN-01 challenge is only validated on TCP/443)", c.ListenAddress)
			}
		}
	} else if err := validateLoopbackAddress(c.AdminAddress); err != nil {
		return fmt.Errorf("adminAddress %q: %w", c.AdminAddress, err)
	}
	if c.Update.QuietHours != "" {
		if _, _, err := ParseQuietHours(c.Update.QuietHours); err != nil {
			return fmt.Errorf("update.quietHours %q: %w", c.Update.QuietHours, err)
		}
	}
	if err := validateVariantsDir(c.Upscale.VariantsDir, c.LibraryRoots); err != nil {
		return fmt.Errorf("upscale.variantsDir %q: %w", c.Upscale.VariantsDir, err)
	}
	if c.Update.CheckIntervalHours < 0 {
		return fmt.Errorf("update.checkIntervalHours: must be >= 0, got %d", c.Update.CheckIntervalHours)
	}
	if c.Backup.IntervalHours != nil && *c.Backup.IntervalHours < 0 {
		return fmt.Errorf("backup.intervalHours: must be >= 0 (0 disables, omit for default), got %d", *c.Backup.IntervalHours)
	}
	// Standardize negative-interval handling: reject loudly rather than
	// silently clamp-to-disabled (the EffectiveX helpers still clamp as
	// defense-in-depth, but a negative is always a misconfiguration the
	// operator should hear about — matching CheckIntervalHours /
	// IntervalHours above). 0 = "disabled", nil/omitted = "use default".
	if c.Integrity.VariantSweepIntervalSec != nil && *c.Integrity.VariantSweepIntervalSec < 0 {
		return fmt.Errorf("integrity.variantSweepIntervalSec: must be >= 0 (0 disables, omit for default), got %d", *c.Integrity.VariantSweepIntervalSec)
	}
	if c.Integrity.OrphanSidecarSweepIntervalSec != nil && *c.Integrity.OrphanSidecarSweepIntervalSec < 0 {
		return fmt.Errorf("integrity.orphanSidecarSweepIntervalSec: must be >= 0 (0 disables, omit for default), got %d", *c.Integrity.OrphanSidecarSweepIntervalSec)
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

	// tailscale.mode: surface typos at config-load time rather than
	// deep inside the lifecycle wiring. Without this gate a typo'd
	// `mode: tnset` would silently fall through to `bridge serve`'s
	// EffectiveMode call which prints the error and exits 2 — already
	// safe, but the operator gets the error from the same surface as
	// every other field's validation here. Gemini medium on PR #249.
	if _, err := c.Tailscale.EffectiveMode(); err != nil {
		return err
	}
	return nil
}

// validateVariantsDir checks the `upscale.variantsDir` setting
// against two constraints that, if violated, would catastrophically
// tangle variants with source files OR produce a path the transcode
// pool can't write to:
//
//  1. MUST be absolute. A relative path would be resolved against
//     the bridge process's working directory — which differs between
//     `bridge serve` (typically the launchd / systemd cwd) and
//     `bridge upscale` (operator-launched, varies). Variants written
//     in one context would be unfindable in the other.
//  2. MUST NOT resolve under any library root. The library is
//     read-only by design (CLAUDE.md PR #75 invariant); writing
//     variants into it would break the contract AND surface to
//     scanner re-scans as mystery audio files. The check uses
//     `filepath.Rel` to handle symlink-cleanup edge cases.
//
// Empty `variantsDir` is the documented "use the default
// <dataDir>/transcoded" path — always valid; nothing to check.
// Directory existence is NOT validated here; `os.MkdirAll` on first
// upscale creates it as needed.
//
// Pure / no I/O beyond the optional Stat for the conflict check.
func validateVariantsDir(variantsDir string, libraryRoots []string) error {
	if variantsDir == "" {
		return nil
	}
	if !filepath.IsAbs(variantsDir) {
		return errors.New("must be an absolute path")
	}
	// Containment check with symlink resolution on both sides (CodeRabbit
	// Major on PR D1): a lexical-only check could be bypassed by a symlink in
	// either the variantsDir OR a library root that resolves into the other
	// tree. fsutil.IsUnderAny is the single canonical form, shared with the
	// admin handler + the `bridge variants move` CLI so all three stay in
	// lockstep.
	if matched := fsutil.IsUnderAny(variantsDir, libraryRoots); matched != "" {
		return fmt.Errorf("must not be under library root %q (variants would tangle with source files)", matched)
	}
	return nil
}

// maxCustomEndpointHostLen caps the per-entry hostname at the RFC 1035
// maximum FQDN length. Each accepted hostname is added to the
// generated TLS certificate's SAN list (via internal/advertise's
// CertSANConfig); without a cap, a typo'd or hostile entry could bloat
// the cert binary unboundedly. IPv6 string-form fits comfortably below
// this ceiling (max ~45 chars), so a single bound covers both forms.
//
// Unexported because it's an implementation detail of
// ValidateCustomEndpoints — only this file and its same-package tests
// should reference it.
const maxCustomEndpointHostLen = 255

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
		if hostLen := len(u.Hostname()); hostLen > maxCustomEndpointHostLen {
			warnings = append(warnings, fmt.Errorf("customEndpoints[%q]: hostname is %d characters, exceeds %d-character limit", raw, hostLen, maxCustomEndpointHostLen))
			continue
		}
		// Dedupe on a canonical form so two paste-friendly equivalents
		// collapse. url.String() does NOT treat an empty path
		// ("https://host") and a root path ("https://host/") as equal,
		// so normalise a bare "/" path to "" before building the key —
		// that's the common trailing-slash paste case. Deeper paths are
		// compared verbatim (no further path/port normalisation); the
		// operator's exact input form is what we keep in `kept`.
		cu := *u
		if cu.Path == "/" {
			cu.Path = ""
		}
		canonical := cu.String()
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

// LibraryRootError pairs an inaccessible library-root path with the
// underlying filesystem error. Returned by CheckLibraryRootsAccessible
// so callers can distinguish "not a directory" from "permission
// denied" from "no such file" without parsing error strings.
type LibraryRootError struct {
	Path string
	Err  error
}

func (e *LibraryRootError) Error() string {
	return fmt.Sprintf("libraryRoots[%q]: %v", e.Path, e.Err)
}

func (e *LibraryRootError) Unwrap() error { return e.Err }

// CheckLibraryRootsAccessible stats every configured LibraryRoot
// and returns one LibraryRootError per failing entry. Empty result
// means all roots are present + are directories.
//
// Deliberately separate from Validate(): the stat call is what
// trips public-mode deployments where the daemon user runs the
// bridge against a FUSE-mounted library inaccessible to root, and
// `sudo bridge update` / `sudo bridge status` / etc. would
// otherwise refuse to load the config. Move-out predates the
// runtime check; the runtime check is owned by `bridge serve`
// (mode-dependent strictness) and the mutation handlers (`bridge
// library add`, admin's apiRootsAdd) — both stat independently
// before persisting.
//
// Callers should treat the returned slice as advisory and pick
// their own response: loopback `bridge serve` refuses to start
// when any root is unreachable (typo'd YAML protection); public
// `bridge serve` logs warnings and continues (the bridge can
// still come up serving cached state while a FUSE mount catches
// up, and the scanner's PR-#74 error-subtree machinery prevents
// the deletion pass from wiping the manifest of a momentarily-
// unreadable root).
func (c *Config) CheckLibraryRootsAccessible() []*LibraryRootError {
	var errs []*LibraryRootError
	for _, r := range c.LibraryRoots {
		if r == "" {
			continue // shape-checked in Validate; skip the noisy duplicate here
		}
		info, err := os.Stat(r)
		if err != nil {
			errs = append(errs, &LibraryRootError{Path: r, Err: err})
			continue
		}
		if !info.IsDir() {
			errs = append(errs, &LibraryRootError{Path: r, Err: errors.New("not a directory")})
		}
	}
	return errs
}

// ScanInterval returns scanIntervalSec as a time.Duration.
func (c *Config) ScanInterval() time.Duration {
	return time.Duration(c.ScanIntervalSec) * time.Second
}

// DefaultVariantSweepIntervalSec is the default cadence (1 h) for
// the upscale-variant integrity watcher. See IntegrityConfig
// docstring for the rationale (closes the operator-rm / backup-
// software / disk-failure cases without exhausting filesystem IO).
const DefaultVariantSweepIntervalSec = 3600

// VariantSweepInterval returns the configured sweep cadence as a
// time.Duration. Missing field (pointer nil) returns the default
// (1 h); an explicit zero opt-out is preserved verbatim and the
// integrity package treats ≤ 0 as "watcher disabled".
//
// Negative values are clamped to zero (disabled) — a typo'd
// negative would otherwise create a busy-loop ticker that
// stat()s every variant row continuously and saturates IOPS.
func (c *Config) VariantSweepInterval() time.Duration {
	if c.Integrity.VariantSweepIntervalSec == nil {
		return time.Duration(DefaultVariantSweepIntervalSec) * time.Second
	}
	secs := *c.Integrity.VariantSweepIntervalSec
	if secs < 0 {
		secs = 0
	}
	return time.Duration(secs) * time.Second
}

// OrphanSidecarSweepInterval returns the configured forward-sweep
// cadence (orphan sidecar files on disk → unlink). Default zero
// (DISABLED); explicit zero or negative also map to disabled.
// Symmetric to VariantSweepInterval but with a different default
// because the forward sweep is opt-in (see IntegrityConfig.
// OrphanSidecarSweepIntervalSec docstring for the rationale).
//
// Negative values are clamped to zero — a typo'd negative would
// otherwise create a busy-loop ticker. Same hardening as
// VariantSweepInterval.
func (c *Config) OrphanSidecarSweepInterval() time.Duration {
	if c.Integrity.OrphanSidecarSweepIntervalSec == nil {
		return 0
	}
	secs := *c.Integrity.OrphanSidecarSweepIntervalSec
	if secs < 0 {
		secs = 0
	}
	return time.Duration(secs) * time.Second
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
	if err := atomicwrite.RenameWithRetry(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	tmpName = "" // suppress defer cleanup
	return nil
}
