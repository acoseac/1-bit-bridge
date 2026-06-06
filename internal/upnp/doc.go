// Package upnp is the bridge's UPnP/DLNA control-point client toward an
// UPSTREAM MediaServer (e.g. a Chord 2Go running MiniDLNA on the LAN).
// It issues ContentDirectory Browse / Search / GetSystemUpdateID SOAP
// calls and parses the DIDL-Lite results so the bridge can ingest the
// upstream's library into its own manifest and proxy file reads to iOS.
//
// Direction: this is the *inverse* of internal/dlna (the bridge's own
// ContentDirectory SERVER, which answers Browse FROM renderers) and a
// sibling of internal/dlna/discovery (whose control-point code finds +
// queries renderers). Here the control-point target is a MediaServer.
// The HTTP plumbing (SOAPDispatcher, body cap, status helper) is reused
// from internal/dlna/discovery.
//
// Bit-exact mission: this package reads only metadata + <res> locators —
// never audio bytes. Byte serving is a verbatim range-proxy elsewhere.
//
// MiniDLNA caveats baked into the design (validated against the 2Go):
//   - Drive Browse SERIALLY — the upstream's libmicrohttpd pool is tiny;
//     parallel Browse bursts cause socket timeouts that stall playback.
//   - Don't trust TotalMatches mid-index — paginate until an empty page.
//   - ObjectIDs are position-based / volatile; the stable identity is the
//     filesystem path reconstructed from the "Browse Folders" view
//     (handled by the ingest layer, not here).
package upnp
