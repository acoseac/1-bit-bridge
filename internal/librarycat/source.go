package librarycat

// Source identity for the player's library-source facet.
//
// A track's source is either this bridge's own filesystem or one
// upstream UPnP MediaServer. The catalog already carries the routing
// key per row (Row.RoutedUDN); this file turns it into a public id the
// browse routes can validate and filter on.
//
// WHAT THE ROUTING KEY ACTUALLY IS. `upnp_track_routing.server_udn`
// stores the ingest's `upnpingest.StableServerKey`, NOT the device's
// raw UDN: it is the LOWERCASED UDN for a UDN-configured upstream, and
// `"manual:<sha256(url)>"` for one configured by description URL only.
// That distinction matters because the SSDP cache — the thing that
// answers "is it online right now" — is keyed on the RAW UDN as the
// device reported it. The two keys are equal only for a device whose
// UDN is already lowercase, so anything that wants both liveness and
// membership has to carry the pair rather than assume one lookup
// works for both. admin.UPnPSource is that pair.

// LocalSourceID is the facet id of the bridge's own filesystem library.
//
// A magic token beside the hashed ids rather than a hash of its own,
// for the same reason `quality` accepts "all"/"dsd" beside its enum
// names: it is a fixed member of the vocabulary, not a value that
// varies per library. It cannot collide with a SourceID — those are 16
// lowercase hex characters and this is not.
const LocalSourceID = "local"

// SourceID maps an upstream's routing key to its public facet id.
//
// Hashed for the same reason album and artist ids are: a routing key
// is `uuid:...` or `manual:<hex>`, so it carries a colon and cannot be
// validated by the bounded-alphabet regex the routes check BEFORE any
// lookup. The "source:" prefix keeps this id space provably disjoint
// from the album/artist/axis one, so a routing key can never hash onto
// an album id and be accepted by the wrong filter.
func SourceID(routingKey string) string { return HashID("source:" + routingKey) }
