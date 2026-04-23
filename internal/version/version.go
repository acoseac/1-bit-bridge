// Package version exports the server build version and the wire protocol
// version. The protocol version is the source of truth that PROTOCOL.md
// documents and that iOS clients check during pairing.
package version

const (
	// ServerVersion is the build version of 1-bit-bridge. Bumped per
	// release.
	ServerVersion = "0.0.1"

	// ProtocolVersion is the wire-protocol version advertised in the
	// X-Bridge-Protocol header and in GET /v1/health. A breaking wire
	// change bumps this; additive changes keep it the same.
	//
	// If you bump ProtocolVersion, you MUST update PROTOCOL.md here and
	// the mirror at com.acoseac.dsdplayer/docs/BridgeProtocol.md in the
	// iOS repo, in the same PR cycle (see CONTRIBUTING.md — Mirror-PR
	// rule).
	ProtocolVersion = 1
)
