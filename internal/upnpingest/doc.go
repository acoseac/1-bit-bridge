// Package upnpingest is the orchestration layer that turns a discovered
// upstream UPnP MediaServer into rows in the bridge's manifest store.
//
// Pipeline per configured server, per scan tick:
//
//  1. Resolve the server's live ContentDirectory controlURL — either
//     from the SSDP discovery cache (matched by UDN) or from a
//     manually-configured description URL fetched on demand.
//  2. Consult GetSystemUpdateID against a stored last-known value.
//     When the server returns a non-zero ID that matches storage, skip
//     the whole walk (MiniDLNA's GetSystemUpdateID is "0" today, which
//     this layer treats as untrusted — the time-based backstop drives
//     a periodic walk regardless).
//  3. Walk the configured root (defaults to "64" — MiniDLNA's Browse
//     Folders view) via internal/upnp.BrowseFoldersWalk and stream
//     each Walked record through a batch upsert into the manifest
//     store: Track row + UPnPRouting sidecar.
//  4. Reconcile: rows with last_seen_at older than the walk-start time
//     are deleted (the FK CASCADE drops the routing row alongside).
//
// Bit-exact mission: this package never reads audio bytes; <res> URLs
// are stored as resolution hints and the file proxy (separate PR)
// fetches them via HTTP Range passthrough.
package upnpingest
