// Package version exports the server build version and the wire protocol
// version. The protocol version is the source of truth that PROTOCOL.md
// documents and that iOS clients check during pairing.
package version

// ServerVersion is the build version of 1-bit-bridge. Bumped per release.
// goreleaser overrides this via -ldflags -X at build time
// (see .goreleaser.yaml); the source default is the dev placeholder.
var ServerVersion = "0.0.1"

// MinClientVersion is the minimum iOS app version this bridge build
// expects. Used by the updater's compatibility gate: an auto-install
// candidate whose MinClientVersion would orphan a currently-paired
// client (X-Client-Version header, persisted per token) is deferred
// with a warning unless the operator explicitly forces.
//
// Released bridges set this via -ldflags -X
// github.com/acoseac/1-bit-bridge/internal/version.MinClientVersion=<value>
// driven by an env var in .goreleaser.yaml so the maintainer states the
// floor at release time without a source-tree edit.
//
// "0.0.0" is the source default — meaning "no floor", any iOS client is
// considered compatible. Real releases tighten this.
var MinClientVersion = "0.0.0"

// Channel identifies the update channel this bridge build targets.
// Currently only "stable" is wired through; reserved for a future
// "beta" channel without a source-tree migration.
var Channel = "stable"

// ProtocolVersion is the wire-protocol version advertised in the
// X-Bridge-Protocol header and in GET /v1/health. A breaking wire
// change bumps this; additive changes keep it the same.
//
// If you bump ProtocolVersion, you MUST update PROTOCOL.md here and
// the mirror at com.acoseac.dsdplayer/docs/BridgeProtocol.md in the
// iOS repo, in the same PR cycle (see CONTRIBUTING.md — Mirror-PR
// rule).
const ProtocolVersion = 1
