---
paths:
  - "internal/dlna/**"
  - "internal/upnp/**"
  - "internal/upnpingest/**"
  - "internal/upnpproxy/**"
  - "internal/mdns/**"
  - "internal/dsdtone/**"
  - "internal/baseurl/**"
  - "cmd/bridge/dlna_*.go"
  - "cmd/bridge/upnp_*.go"
  - "cmd/bridge/mdns_*.go"
  - "internal/manifest/store_upnp_routing*.go"
  - "internal/api/upnp_proxy*.go"
  - "internal/api/renderers*.go"
  - "internal/admin/handlers_upnp*.go"
---
# DLNA, UPnP and discovery

The DLNA MediaServer and its listener, SSDP, the renderer and server discovery clients, the upstream UPnP ingest and proxy, interface eligibility, and mDNS.

Moved verbatim from CLAUDE.md on 2026-10-18, under the section titles it had there, so a code comment citing "CLAUDE.md <title>" or "under **<section>**" means an entry here. The record behind each rule is in `ops/engineering-log.md`: grep it by symbol, file or PR number before changing anything a rule names.

## DLNA, UPnP and discovery

- **Every advertised device registers with serve's one SSDP subsystem (B470 PR 5a1). Close owns staged endpoints and withdrawals through their last socket release, under its one deadline; strict replies use their selected endpoint's local address AND outgoing interface, or send nothing and warn once, naming the stage, the operation and the errno and never a socket address (B604). Refuse strict endpoints without a valid non-unspecified, unmapped IPv4 Local and positive interface index before opening sockets. Clamp every owned sender's installed and future write deadlines to Close's under the same lock. Re-read before byebye to skip and report expiry. Every retirement checks expiry after sender/listener release and reader join against the re-read deadline; Withdraw checks again atomically with handle removal, adding a deadline error only if absent, so Close owns exactly the handles it saw. Keep bind/pin ordering tests portable and expect strict write refusal outside Linux/macOS/Windows. Normalize supported admission pointers before validation and refuse typed nils and unknown implementations before opening anything. Don't unlist a withdrawal before retirement finishes, give a rollback a fresh shutdown budget, use the MediaServer's routed reply path for a strict registration, or log a strict reply's error text.** → History-rules-dlna-upnp.md
- **The three spec-mandatory ContentDirectory introspection actions
  (`GetSearchCapabilities` / `GetSortCapabilities` / `GetSystemUpdateID`) must
  stay declared.** Strict control points poll `GetSystemUpdateID` between
  navigation steps and abandon the drill on a SOAPFault 401 — the symptom is
  "tap does nothing", with no error anywhere. Empirically validated against
  mconnect. (`Search` IS implemented now — check the code before believing any
  doc that says otherwise.)
- **ObjectIDs must stay numeric** (`FolderObjectID` hashes to a decimal string);
  a non-numeric prefix like `"f-"+hex` re-opens a silent int-parse rejection at
  every drill-down level. Root advertises exactly two
  containers and `BrowseMetadata`'s childCount must equal that count.
- **`/dlna/artwork/{key}` is `/v1/artwork/{key}`'s read path under the DLNA
  mux, and the CDS emits `albumArtURI` ONLY when that route is mounted.**
  `api.(*Server).ServeArtwork` is the one implementation (the v1 handler is
  a one-line wrapper; `TestServeArtworkAnswersExactlyLikeTheV1Route` pins the
  two byte-for-byte across hit, the three miss shapes, bad key and alias);
  the dlna package takes it as `ServerConfig.Artwork` (an interface, so it
  never imports api) and gates BOTH the mount and the `<upnp:albumArtURI>`
  emission on that one field — never on a track merely carrying a key,
  because a strict renderer that 404s the URI may decline the whole item
  (the PR #560 duration lesson). The key is `artworkVersion ?? artworkMBID`
  (`TrackInfo.ArtworkKey`), i.e. the value iOS stores as `Album.artworkHash`,
  so the app's renderer-facing URI and the bridge's own DIDL compose the
  same URL; the URL is built per REQUEST against `r.Host` like `<res>`.
  Folder containers advertise the first keyed DIRECT child in path order —
  no descent, so a multi-disc parent does not inherit disc 1 by sort order.
  `dlnaArtwork` in `/v1/health` is AND-gated (`dlnaEnabled && artworkDirs`),
  and iOS must gate its own emission on it, not on `dlnaServer`. No demo-mode
  branch is needed: the listener never starts in public mode.
- **The DLNA listener answers only to a Host that names THIS host, with 421
  for any other** (2026-09-29, backlog B170). It has no authentication, and
  what kept it to the LAN is that it is reached on the LAN: a page a LAN
  browser loads from a name its author controls can re-point that name at
  this host's address (DNS rebinding), and the ContentDirectory builds every
  `<res>` and albumArtURI from `r.Host`, so its answers name whatever host the
  request did (measured with the real binary; the record is in the log).
  `ownHostOnly` (host_guard.go) passes, with any port
  or none: an EMPTY Host (an HTTP/1.0 renderer may send none; no browser
  does), `localhost` and a loopback literal, the host of every advertised
  LOCATION, of ServerURL and of a pinned listen address, NAME OR LITERAL
  (`knownOwnHosts`, folded), and any other address of this host's interfaces,
  asked at the request and only for a literal the configuration does not name
  (`Server.interfaceAddrs`, a per-server seam, nil is `net.InterfaceAddrs`).
  **A name not in that list is refused, this host's own included**: resolving
  it is what the page controls. The cost is a control point pointed at the
  server by a name by hand, and the iOS app's fallback that builds a renderer
  URL on the paired host when `/v1/health.endpoints` names no RFC 1918
  address (`BridgeDLNAURLResolver`, whose own doc says mDNS names already fail
  on some renderers). The check sits inside the telemetry middleware
  (`Server.handler`, what Start serves), so a refused request is recorded
  with its 421, and a refused name is logged once (16 at most).
- **The folder index is built LAZILY per Browse call** — pre-building it at the
  top of `handleBrowse` puts an O(N) walk on the flat-list hot path.
  `TrackInfo.RelativePath` is the load-bearing source in production; the
  LCP-derived fallback silently strips a top-level folder.
- **The ContentDirectory lists no SACD virtual track** (2026-10-16, backlog
  B415). The adapter's rebuild (`dlna_wiring.go`) leaves out a row
  `manifest.IsSACDVirtualPath` names unless `upnp_track_routing` has it:
  nothing is at a virtual path, since the bridge never demuxes, so a
  renderer could only fail to play it; the app vetoes casting one too.
  **Filter there, in the one list every view derives from** (All Tracks and
  `TrackCount`, the folder index, Search's path map, BrowseMetadata, the file
  route's lookup), never in one reader, or a childCount or a TotalMatches
  stops matching what it lists; **never re-implement the grammar; and never
  drop a routed row for its path's shape**, since the proxy serves it
  (`TestTheContentDirectoryListsNoSACDVirtualTrack`).
- **`upnpproxy` relays verbatim** (that IS its bit-exact contract) and sets
  `CheckRedirect: ErrUseLastResponse` — without it a rogue LAN upstream can aim
  a `<res>` fetch at the bridge's own no-auth loopback admin API, reachable
  unauthenticated. A caller needing a different Content-Type wraps the writer;
  don't change the package. Its client dials through
  `discovery.NewDeviceTransport` under the server's approval (the B36 bullet
  below), with no kept-alive connections.
- **A DISCOVERED description's service URLs stay on its own host** (external
  audit 2026-09-23, M3). `resolveServiceURL`
  (`internal/dlna/discovery/url_policy.go`) is the one home: every
  `<controlURL>` and `<eventSubURL>` must be http(s) with a host, and in a
  description found through SSDP (`SourceDiscovered`, the zero value and the
  default of `ParseDeviceDescription` / `FetchDeviceDescription`) it must be on
  the description URL's host, compared case-insensitively with the port
  ignored (`url.URL.Hostname`, so an IPv6 literal compares by address).
  **Read the host with `Hostname()`, never `Host`**: `http://:7789/x` has
  `Host ":7789"` and no hostname, and Go's client dials it on the local host,
  the bridge's own console (measured). The
  manual upstream parses with `SourceUserChosen`: the operator's URL is the
  approval the host rule stands in for, so another host is kept, another
  scheme never, and never this machine or a link-local address unless the
  manual URL itself is of that kind (the next bullet). A refused AVTransport control URL drops the renderer (it reads
  as "no AVTransport"); a refused optional URL (ConnectionManager,
  RenderingControl, any eventSubURL) is dropped alone, so GetProtocolInfo is
  POSTed only to a ConnectionManager URL that passed. An SSDP LOCATION that is
  not http(s) with a host reads as absent in `ParseSSDPHeaders` and is never
  fetched. **The upstream half matters most**: `LiveHost` derives every routed
  byte fetch's host:port from the cached ContentDirectory control URL and
  `upnpproxy` rewrites each stored `<res>` onto it, so a server that
  re-announced its UDN from a new address with a control URL on the loopback
  console steered `/v1/download` and `/dlna/file/{trackID}` there, with a path
  chosen at ingest (measured: the proxy relayed the console's 200).
  `TestAMovedServerCannotSteerTheCachedControlURLToAnotherHost` pins the
  cache half, through the real SSDP handler. **The HOST, not the origin**: an
  origin compare turned six existing upnp tests red, since serving control
  endpoints on another port of the description's host is ordinary. **A
  bound, not authentication**: a spoofer can still aim a
  server's fetches at the host that served the description, its own; the rule
  removes a THIRD host. It mirrors the app's `UPnPURLPolicy` /
  `DeviceDescriptionParser.resolveServiceURL` (iOS #1911), and since iOS
  #1998 (2026-09-29) the host-kind rule of the next bullet too:
  `UPnPURLPolicy.hostKind(of:)` and `hostKindAllowed` bound the app's
  `resolveServiceURL` for every source, over `cloudMetadataAddresses`, the
  same 19 addresses as `cloudMetadataAddrs` (a change to one list is a change
  to both). The app's check on
  relayed renderers (#1977) can compare only against the control URL, since
  `/v1/renderers` carries no description URL, so refusing a device that points
  EVERY service at one other host is the bridge's job. No description fetch
  follows a redirect (each dispatcher sets `ErrUseLastResponse`), stricter
  than the app's same-host redirect rule. A real device whose description
  names another host (a hostname where its LOCATION has an IP, say) drops out
  of discovery; a manual upstream URL is the escape hatch, and a renderer has
  none on the bridge.
- **…and an SSDP LOCATION leads the bridge to THIS machine or a link-local
  address only when the packet came from that address** (backlog B14,
  2026-09-28). The same-host rule bounds a description's service URLs to the
  host that served it and nothing bounded which host that is: a LAN peer
  answering an M-SEARCH, or re-announcing a known UDN (the move detector
  re-fetches), with LOCATION `http://127.0.0.1:7789/<path>` made the bridge GET
  its own console. **A host string cannot carry this rule alone** (measured,
  Go 1.27.1): a public DNS name pointed at 127.0.0.1 reached a loopback
  listener on macOS and on Linux, and `127.1`, `2130706433`, `0x7f000001` and
  `0` reached it on macOS, whose libc resolver takes inet_aton's spellings.
  So it is enforced twice, in `internal/dlna/discovery/url_policy.go`.
  `LocationFromSource`, in both handlers, refuses what the string shows (an IP
  literal on loopback, unspecified or link-local that is not the packet's
  source; a localhost name unless the source is loopback; a host ending in a
  number without being an IP literal, which no device writes) before any
  fetch, and a refused LOCATION reads as an absent one (a known UDN
  refreshed, an unknown one skipped, the move detector never fires).
  `NewDeviceFetchClient`, both clients' default client, checks the address
  EVERY connect targets after resolution (`net.Dialer.ControlContext`), with
  the packet's source carried in the request context
  (`WithAnnouncementSource`, on the description GET and a renderer's
  GetProtocolInfo POST alike). **Don't give that client a proxy** (the check
  would judge the proxy's address, and a proxy on 127.0.0.1 would refuse
  every fetch), **kept-alive connections** (a request could reuse one another
  packet's source allowed) **or a TLS dialer of its own** (it would connect
  around the check); a test pins all three. **The same host kinds bound a
  service URL, for every source**: one on this machine or a link-local address
  is kept only from a description URL of the same kind, so a manual upstream
  elsewhere cannot make the console (or a cloud VM's metadata service at
  169.254.169.254) its `LiveHost`, while one on this machine keeps its local
  control URL. `hostPortFromURL` reads `Hostname()` too. **The exception is the
  packet's own address, never "any address like it"**: a zero-configuration
  device announces from its link-local address, and a loopback source was
  sent on this machine. **Field data, 2026-09-28**: three root devices on one
  home LAN (two hosts) sent every LOCATION on the packet's source address, as
  an IP literal; two other LANs had none. The only LOCATIONs off their source
  were this repo's own `internal/dlna` test advertisers, which multicast a
  loopback LOCATION from the host's LAN address (until B38 bound them to the
  loopback interface, the test bullet further down). Three devices do not
  support the general rule (LOCATION host == source for every address, which
  would also bound names and tailnet addresses): multi-homed hosts and some
  NAS firmware are reported to break it, and a renderer has no escape hatch,
  so **measure before tightening further**. **Not covered**: a LOCATION on a
  tailnet or public address is still fetched, by the app as by the bridge.
  **The app makes this check too since iOS #1998** (2026-09-29):
  `UPnPURLPolicy.location(_:announcedFrom:)` is `LocationPermittedBy` rule
  for rule but for B49's link rule (three bullets down), which the app does
  not have (backlog B138), with no dial check behind it (`URLSession` offers
  no hook between resolving a name and connecting). This bullet said the app had no such
  check until that day, and `resolveServiceURL`'s docblock that the app had
  no host-kind rule: a claim about the other repo goes stale the day that
  repo merges, so a session that changes one side also updates, or files,
  what the other side says about it (here, backlog B78). (This bullet also
  said the later dials of a HOSTNAME control URL were not covered; the next
  bullet covers them.)
- **A host that, decoded, holds a Unicode control character or whitespace
  is malformed, and it is refused before any other kind** (2026-10-12,
  backlog B246; the app's twin is acoseac/1-bit#2042). `classifyHost` asks
  `hostIsMalformed` on the original host, before the trailing-dot trim and
  before `ParseAddr`. `unicode.IsControl` or `unicode.IsSpace` is the
  kind `hostMalformed`. The character is never stripped so the remainder
  can be classified: `127.0.0.1` plus U+0085 stays malformed, and a strip
  would call it this machine. Go's `url.Parse` already refuses an ASCII
  control or space in a host. What reaches the classifier is the non-ASCII
  set (U+0085, U+00A0, U+2028, U+3000). **An ASCII space inside an IPv6
  zone whose head parses as an address is kept**: Windows names the
  adapter `Wi-Fi 4`, and `TestParseDeviceDescription_KeepsNoServiceURLThatDoesNotParseBack`
  requires `http://[fe80::1%25Wi-Fi%204]:8080/d.xml`. A control or any
  other space in that zone is still malformed. A `%` whose head is not an
  IP is not a zone (the `%25` name). `hostKindAllowed` refuses the kind.
  `isFetchableURL` refuses it, so `fetchableLocation` reads such a
  LOCATION as absent. `resolveServiceURL` returns
  `errServiceURLMalformedHost` before `isFetchableURL` and again in the
  kind switch. `LocationPermittedBy` refuses it even when the packet came
  from loopback, ahead of the localhost arm that would otherwise keep a
  name it cannot parse. `OperatorChose` approves only this machine and
  link-local, so a malformed manual URL approves nothing.
  `callbackHostAllowed` already refuses one: it is not an address
  `ParseAddr` accepts. **Fullwidth digits (`１２７．０．０．１`) and a
  literal `%25` are not this kind.** IDNA maps the fullwidth host to
  `127.0.0.1`; `%25` decodes to a name holding `%`. Both stay elsewhere,
  and `refuseUnapprovedHostLocal` is the backstop that stops the dial.
  Do not drop that dial check because the string check now exists.
  (`TestAHostWithAControlOrASpaceIsMalformed`,
  `TestAMalformedHostIsNeverStrippedBeforeItIsClassified`,
  `TestAMalformedHostIsRefusedAtEverySiteThatJudgesOne`,
  `TestFullwidthDigitsAndAPercent25StayOnTheDialCheck`,
  `Test_callbackHostAllowed`.)
- **…and every LATER request to a device dials under the approval its URL
  came with, because a NAME in it resolves again at each dial** (backlog B36,
  2026-09-28). The ingest's SOAP Browse (`upnpUpstreamSOAPHTTPClient`, then on
  `http.DefaultTransport`) and every `upnpproxy` byte fetch dialled the cached
  control URL's host with no dial check, so a peer that passed discovery with
  a name answering its own LAN address and then answered 127.0.0.1 took both
  to the console, and the proxy relayed its 200 (measured, macOS and Linux:
  `CONSOLE POST /ctl`, `CONSOLE GET /api/stats`). What approved a local
  connect is `discovery.DialApproval`: `AnnouncedFrom(src)` (the packet's own
  address, #1069's rule) or `OperatorChose(manualURL)` (every address of the
  kind the URL's host names; **a NAME approves no local address**, so a manual
  URL naming this host by its host name, which Debian maps to 127.0.1.1, is
  refused; write `localhost`). It is recorded as `upnp.ServerInfo.DialApproval`
  beside the control URL, and **`Upsert` keeps and replaces the two together,
  never the approval alone**: a merged-alone approval outlives the URL it
  came with, or pairs one writer's URL with another's approval.
  `ResolveControlURL` and `LiveHost` return both from ONE lookup, and the
  ingest and the proxy carry the approval in each request's context. **Every
  client that sends a device a request is built on
  `discovery.NewDeviceTransport`**: the dial check (it replaces any
  `ControlContext` the dialer template carries), no proxy, **no kept-alive
  connections**, no TLS dialer. The keep-alive rule is measured, not
  argued: with the proxy's old pool a second fetch, approved only for a LAN
  address, rode the first fetch's idle connection to 127.0.0.1 past the check
  (`TestProxy_Serve_NeverCarriesARequestOnAConnectionAnotherApprovalOpened`),
  and net/http also hands a connection dialed for one request to another
  waiting one. Cost: about 65 µs a request on loopback, one round trip on a
  LAN. **Declined, on evidence**: requiring SSDP control URLs to be IP
  literals. Three devices on one LAN (#1069) are thin evidence against names,
  UDA 1.1 says LOCATION hosts are "normally" literals, not always, the dial
  check already covers the dangerous targets, and a literal can name a
  tailnet host anyway (the previous bullet), so the rule would bound no third
  host either. Tests resolve through `discovery.UseResolverForTest` (atomic)
  and `internal/dnstest`, **never by replacing `net.DefaultResolver`**, which
  every goroutine in the process reads unsynchronised. **Residual**: an SSDP
  source is not authenticated, and a peer on the same L2 segment can send a
  packet FROM a link-local address; the same-address exception then
  approves exactly that address, for the description fetch and the later
  dials, never a cloud metadata one (the next bullet), and since B49 only
  where the packet arrived on a zero-configuration link (the bullet after
  it). A loopback source is what RFC 1122 has a host discard from any other
  interface.
- **…and no device's say-so and no approval reaches a cloud metadata
  address** (CodeRabbit on #1074, 2026-09-28). The residual above said
  169.254.169.254 was included: a packet spoofed from it approved it for the
  description fetch and every later dial, whose answers the proxy relays to
  the unauthenticated DLNA listener (a cloud VM's credentials, on IMDSv1).
  `cloudMetadataAddrs` (`url_policy.go`) is the ONE list, from each
  provider's documentation (AWS's IMDS, DNS, NTP, ECS and EKS Pod Identity
  addresses in both families; the IPv6 metadata addresses of Google Cloud,
  Oracle, Linode, OpenStack and Scaleway; Scaleway's and Tencent's IPv4
  ones; Alibaba's 100.100.100.200; Azure's 168.63.129.16). `addrKind` names
  them first (`hostMetadata`), so the string check (`LocationPermittedBy`),
  the service-URL rule (`resolveServiceURL`, for every source) and the dial
  check (`DialApproval.Permits`) refuse them whatever approved the request.
  **Exact addresses, never a range**: a direct-cable device self-assigns
  anywhere in 169.254/16 and fe80::/10, and a /24 around 169.254.169.254
  would refuse one such device in 254 (the tests keep one at 169.254.7.7).
  Ten of them are not link-local (the fd00::/8 ones, 100.100.100.200,
  168.63.129.16) and were fetched on ANY device's say-so, exception or not.
  A tailnet node may hold 100.100.100.200 (it is in 100.64/10, one address
  in four million) and would lose its routed dials. The resolver's own DNS
  connects do not pass the dial check, so Azure's DNS on 168.63.129.16 keeps
  working. `TestCloudMetadataAddrsAreTheDocumentedOnes`
  holds the list to its sources, and
  `TestAPacketFromAMetadataAddressApprovesNoLaterDialThere` drives the chain
  through the real ingest and proxy. **A manual upstream's own description
  fetch is refused one too** (backlog B54; this bullet said it was not
  checked until then): it ran on a plain client, so a manual URL on a
  metadata address was sent a GET every poll, its answer parsed and dropped,
  and nothing said why the server never appeared. **Refused, not warned
  about and fetched**: it was the one request the rule did not reach, a GET
  to IMDS is the request the rule exists to prevent, no media server serves
  on one, and such a server could never walk anyway (its later dials are
  refused). The poller refuses a literal before any request
  (`discovery.NamesCloudMetadataAddr`) and fetches on `NewDeviceTransport`
  under `discovery.ManualDescriptionFetch()`, which permits every address
  BUT a metadata one: a name resolving to one is refused at the connect,
  while a name resolving to this machine is still fetched (#1069's
  decision; only the later dials need `localhost`). Either refusal warns
  ONCE per server, naming its host: a failed fetch is a Debug line, which
  the bridge never prints. The ingest's per-server error (the console's
  last walk error) says so for a literal, where it said "has not answered
  yet". The console refuses a typed literal (`AddServer`), and a config
  holding one still loads.
  `TestManualPollerNeverFetchesACloudMetadataDescription` holds both
  refusals and both controls (a direct-cable literal, a name answering
  127.0.0.1).
- **…and a manual URL's user information travels as a header, and no line
  names the URL** (backlog B66). A ContentDirectory control URL the
  description names relative to the manual URL INHERITS its user
  information (`url.ResolveReference` copies the base's), and the SOAP
  client built its request from it: `upnp: POST
  http://user:<password>@nas:8200/ctl: status 401` reached the journal at
  WARN on every failed walk ("UPnP upstream: per-server error", measured
  with the real binary), and the console shows the same text as the
  server's last walk error. `splitControlURL` (internal/upnp/client.go)
  takes it out of the request URL and `invoke` sends it with
  `SetBasicAuth`, the header net/http built from the URL byte for byte, so
  a server behind Basic auth keeps working; the dispatcher follows no
  redirect, so the header reaches no other host. B69's rule for the enrich
  bases, one package over. The poller's two Debug lines ("description
  fetch failed", "carries no ContentDirectory service") named the URL
  whole; they name the host (`descriptionHostForLog`), as its warnings do,
  and the fetch's error goes through `fetchErrorForLog`, which replaces the
  URL in each of its renderings: discovery's own (`GET <url>: …`, as
  passed) and net/http's `*url.Error` (as re-serialized, a password masked
  `***`, a token as the user name, a query and a fragment whole), read from
  the error's own `URL` field, never guessed, and **quoted as `%q` quotes
  it**: `url.Error.Error` is `%s %q: %s`, so a URL holding a `"`, a `\` or
  an unprintable rune appears escaped, and a search for it as written
  misses (measured; found checking a review's suggestion on #1116). A
  manual URL with no host `descriptionHostForLog` can read (it does not
  parse, or `user:pw@host` was written without a scheme) logs a fixed
  reason, never its error: net/http's `unsupported protocol scheme` names
  the scheme such a value parses with, its user name. The bridge prints no
  Debug line today (`logging.Init` fixes the level at Info), so that half
  would have bitten only the day one is added. A byte fetch through
  `upnpproxy` sends no such credential at all (backlog B143).
  `TestAControlURLsUserInformationTravelsAsBasicAuth` (net/http's header
  taken on every run), `TestAWalkErrorNamesNoControlURLUserInformation`
  (the ingest's per-server error) and
  `TestManualPollerDebugLinesNameTheHostAlone` (five ways the fetch fails,
  three URL shapes, a scheme-less one, every line searched).
- **…and a packet's LINK-LOCAL source approves itself only when the packet
  arrived on a zero-configuration IPv4 link** (backlog B49, 2026-09-29). The
  metadata list closed the costly case; the rest of the residual was any
  link-local neighbour: a peer on the segment answering an M-SEARCH "from"
  169.254.x.y, with a LOCATION on it, made the bridge GET that neighbour at
  a port and path the peer chose, and (by rebinding a LOCATION name) dial
  it for the ingest's SOAP and the proxy's byte fetches, whose answers the
  unauthenticated DLNA listener relays. Measured on main: the renderer
  client sent its GET and GetProtocolInfo POST to 169.254.7.7, and through
  the real ingest and proxy the proxy dialled it (`connect: host is down`,
  the ARP on the Mac's LAN). **What a direct-cable device needs is the link
  the exception exists for**: a zero-configuration link, where THIS host
  holds an IPv4 link-local address and no other IPv4 address
  (`discovery.ZeroConfIPv4Link`). That is what macOS and Windows self-assign
  when DHCP does not answer and what a Linux link-local connection holds,
  measured with the real `net.Interface.Addrs` on each (the dev Mac's USB
  link to an iPhone, Windows' APIPA adapters, a Linux namespace interface);
  every DHCP'd interface measured reads configured. **What the rule costs is
  measured too, and it is where the forgery lands**: a device stuck on
  169.254 on a configured LAN (failed DHCP, which UPnP requires it to keep
  retrying) is not fetched, and from such a LAN a connect to 169.254.7.7 is
  refused at once on Windows (`WSAENETUNREACH`: no 169.254 route) and on
  the Linux host measured (no 169.254 route: routed to the gateway), and
  leaves through the primary interface's 169.254 route onto the LAN on
  macOS (and on any host configured with such a route). A MediaServer
  there can still be configured by a manual URL (`OperatorChose`); a
  renderer has no such hatch. **Judged from the client's OWN interface** (the one its M-SEARCH
  goes out on), read when the client is built and again before every
  M-SEARCH (`discovery.AnnouncementLink.Refresh`, in both tick loops): not
  per packet (`Addrs` is a syscall, GetAdaptersAddresses on Windows), and
  not from the interface a packet arrived on, which x/net/ipv4 cannot
  report on Windows. **Declined**: "only for the interface the packet
  arrived on" alone, which leaves the reported shape open (peer and
  neighbour share the configured LAN); and a link that merely HOLDS a
  link-local address beside a routable one (RFC 3927 says a host SHOULD NOT
  have both; such a link is a DHCP LAN). **One approval, both checks**:
  `LocationPermittedBy` (the string check, which `LocationFromSource` now
  wraps) asks `DialApproval.Permits`, as the GENA guard does, so they
  cannot disagree; `AnnouncedOn(src, zeroConfLink)` is the approval and
  `AnnouncedFrom(src)` its configured-link form. An IPv6 link-local SSDP
  source approves nothing (both clients are udp4; IPv6 SSDP announces from
  fe80 on every link, so a rule of its own would be needed). **A GENA
  subscriber keeps its link-local address on any link** (`SubscribedFrom`):
  the SUBSCRIBE came over TCP, whose handshake shows the address to be the
  peer's own. A refusal only the link decided is one Warn per source,
  bounded at 64, naming the interface and this host's IPv4 there.
  **Residual**: a forged answer on a zero-configuration link itself, and
  one sent from a configured link to the ephemeral port of the client on
  another, zero-configuration interface, from the address of a device on
  that link (neither is visible from the sender's link). No SSDP client
  runs in public mode (config refuses `upnpUpstream.enabled`, DLNA and its
  renderer discovery are gated off; measured with the real binary), so the
  cloud-VM case was never reachable there. The app's mirror has no link
  rule (backlog B138). `TestHandlePacket_NeverFetchesALinkLocalLocationOffAZeroConfLink`,
  `TestServerLinkLocalSourceIsApprovedOnlyOnAZeroConfLink`,
  `TestAPacketFromALinkLocalAddressOffAZeroConfLinkApprovesNoLaterDialThere`
  (cmd/bridge, the chain), the per-link columns of
  `TestDefaultClientDialCheck` and `TestLocationPermittedBy`,
  `TestTheRendererClientReadsItsLinkBeforeEverySearch` and
  `TestTheServerClientReadsItsLinkBeforeEverySearch`.
- **A URL that names a port and no host (`https://:8443`) is not a URL of any
  host, and Go dials it on THIS machine**, so every validator reads
  `Hostname()` (backlog B36). `customEndpoints` prunes it (it was advertised
  to every phone); the harvest credential endpoint answers 400
  (`baseurl.CredentialBase`, which asks `baseurl.NamesHost`), and the
  harvest state store drops such a base, with the credential held against
  it, whenever it opens its file (backlog B97), so one stored before the
  endpoint checked is gone at the next start instead of dialled on every
  tick, as it was until then; a configured enrich or harvest base URL of that
  shape is WARNED about in `Normalize`, never refused, because it loaded
  before and a refusal stops a bridge from starting after an update. **Don't
  move the host test into `baseurl.CanonicalHTTPS`'s reduction**: a hostless pin
  would reduce to "" (unpinned) or, through `Validate`, refuse to load; as it
  stands it pins to a value no accepted credential can carry (`https://:443`
  too, since B97: it reduced to `https://`, which the handler's second
  reduction made "", unpinned). **A warning
  about a configured URL logs its scheme and host alone**
  (`urlOriginForLog`), never the value (review round 1 on #1074): an enrich
  base accepts userinfo, so `http://user:password@:5000` reached the journal
  whole, and a dropped custom endpoint was quoted whole, a parse failure
  twice (the parse error quotes it). `url.URL.Redacted` is not enough: it
  keeps a token written as the user name, and the query. The REFUSALS take
  the same rule since backlog B54 (the bullet under Config on naming a
  configured URL), and the manual upstream poller's warnings name the host
  alone.
- **…and a GENA callback on THIS machine or a link-local address gets the
  initial NOTIFY only when the SUBSCRIBE came from that address, and the
  NOTIFY follows no redirect** (backlog B39, 2026-09-28). The DLNA listener
  binds every interface, and `callbackHostAllowed` admitted a loopback or
  link-local callback from ANY source, so a LAN peer could aim the NOTIFY at
  the bridge's own loopback services, the unauthenticated console among
  them. Measured with the real binary (two containers on dido): a peer
  container's `CALLBACK: <http://127.0.0.1:9999/…>`, or the
  `[::ffff:127.0.0.1]` spelling, made the bridge NOTIFY a listener on its own
  loopback. **The redirect was the wider hole**: the NOTIFY client followed
  the callback's 3xx, a 307/308 re-sending the NOTIFY and a 301/302/303
  turning it into a GET, which the console's `csrfGuard` passes. A peer whose
  callback is its OWN address, which every rule admits (#818's narrow one
  included), steered the bridge to any URL, by address or by name (measured:
  `GET /redirected` on the bridge's loopback). **It was the one client
  sending to a LAN peer's URL that followed redirects**: `upnpproxy`, both
  discovery dispatchers, the upstream SOAP client and the manual poller all
  relay a 3xx. Now `callbackHostAllowed` asks the NOTIFY's own dial approval
  FIRST (`discovery.SubscribedFrom(src).Permits(cb)`, #1074's
  `DialApproval` with the SUBSCRIBE as the approving peer), so the guard and
  the dial check cannot disagree: a loopback or link-local callback only
  when it IS the SUBSCRIBE's address (#1069's rule for a LOCATION, and for
  the same reason: the subscriber's own address, never "any address like
  it"; a loopback callback names the host that SENDS the NOTIFY, and a
  link-local source proves nothing, since any device on the segment can take
  one), never the unspecified address or a cloud metadata address (#1074's
  list; `fd00:ec2::254` is a ULA, which the private arm admitted from any
  source before), and still a private address from any source. The NOTIFY goes out through
  `discovery.NewDeviceFetchClient` (no redirect, no proxy, no kept-alive
  connection, the dial check) under that approval
  (`discovery.WithDialApproval`), so each layer holds without the other:
  with the predicate reverted, `TestGENANotifyNeverReachesThisHostForAnotherAddressesSubscribe`
  stays green on the dial check, and with redirects followed,
  `TestGENASubscriberCannotRedirectTheNotifyOntoThisHost` does;
  `TestGENANotifyClientChecksTheConnectAgainstTheSubscriber` and
  `TestGENAInitialNotifyFollowsNoRedirect` pin each alone. **Don't drop the
  dial check because a callback is an IP literal**: it is what still holds
  when a later change lets a name or a redirect past the string check, as
  the redirect did. **Don't widen loopback to "any address of this host"**:
  the one shape that costs is a control point ON the bridge's host that
  subscribes over the host's LAN address with a loopback callback (measured:
  refused), and admitting it trusts every NAT that rewrites a peer's source
  to a local address. A refusal is a Warn once per (callback, source) pair
  (`noteCallbackRefusal`, bounded at 64 in a set of its own, since a peer
  reaches a refusal at will and a shared bound let refusals silence the
  divergence lines step two waits for), so a control
  point that needs the shape names itself. **#818's step two is only HALF
  done**: refusing a private callback other than the source stays held,
  because the observer it was gated on (`noteCallbackDivergence`, shipped in
  v0.2.0) has watched no SUBSCRIBE: public mode never starts the DLNA
  listener, the NUC's `/v1/health` carries no `dlnaServer` (2026-09-28), and
  home-pc has not been updated since before #818. The evidence needs a LAN
  bridge with `dlna.enabled` running the observer for a release. The iOS app
  subscribes to nothing (`subscribeGENA` is a stub), so no Mirror-PR.
- **The DLNA listener bounds the work one unauthenticated peer can start**
  (2026-10-09, backlog B220, B162). Five holes, each measured on the real
  listener before the bound.
  **The initial NOTIFY comes from a pool of `genaNotifyPool` (8).**
  `fireInitialNotify` used to start one goroutine per SUBSCRIBE. Forty-eight
  SUBSCRIBEs whose callbacks blocked grew the process by 190 goroutines
  (the 2026-09-30 review: 6,000 SUBSCRIBEs, 15,409 goroutines, 80 MiB of
  stack). `acquireNotifySlot` takes a slot before the spawn and drops the
  NOTIFY when the pool is full. The SUBSCRIBE has already answered 200;
  the initial NOTIFY stays best-effort. The B39 callback checks, and the
  return for a nil notify context, run before the slot is taken. `Stop`
  still cancels the notify context first and then waits the group. After
  the bound the same flood delivers 8 NOTIFYs and grows the process by
  31 to 33 goroutines (the pool, the callbacks it is serving, and the
  accept loop). A long-lived worker that exits when the notify context
  is already cancelled would hang `newGENATestServer`, which cancels that
  context before any SUBSCRIBE: the slot is a semaphore, acquired in the
  handler, released when the delivery returns.
  **Browse and Search return at most `maxCDSPage` (1000) entries.**
  `clampPage` treated RequestedCount 0 as the whole result. A browse of
  1,050 tracks answered `NumberReturned` 1050 in a 639,011-byte body (the
  review: 50,000 tracks, a 43 MiB body, 406 MiB allocated, eight at once
  1.7 GiB). A count of 0 or past 1000 is clipped to 1000 before the
  existing window arithmetic. `TotalMatches` stays the full set and
  `UpdateID` stays 1, so a control point that advances
  `StartingIndex` by `NumberReturned` collects every track. 1000 is the
  largest count a measured control point puts in one request (BubbleUPnP
  and foobar2000 playlist browse; JRiver's chunk is about the same). The
  bridge ingest and the 1-bit app ask for 200 (`DefaultPageSize`,
  `UPnPBrowsePager.defaultPageSize`), and BubbleUPnP's folder browse asks
  for 16 and then 500, so those requests come back whole. A page of 200
  would shorten the 500 and the 1000. The store still lists the library;
  the cap is the DIDL window. **Residual**: mconnect browsing All Tracks
  with RequestedCount 0 may show 1000 unless it pages. Album folders and
  the page sizes clients send are unchanged.
  **The telemetry ring keeps 100 runes of each string, and a header past
  16 KiB is refused.** `Record` stored whatever the request carried, and
  the listener left `MaxHeaderBytes` at net/http's 1 MiB default. Eight
  GETs with a 900 KiB User-Agent answered 200, stored 8 entries and
  retained 7,313,296 bytes (the review: 100 such requests, heap 4 MiB to
  92 MiB). `truncateRunes` cuts every stored string to
  `dlnaLoggedFieldRunes` (100), on a rune boundary. A cut is a copy
  (`strings.Clone`), so the entry does not keep the rest of the header;
  a string that already fits is stored as it arrived. The listener reads
  at most 16 KiB of headers. net/http's `initialReadLimitSize` (Go
  1.26.6 and 1.27.2) is `MaxHeaderBytes` plus 4 KiB of bufio lookahead, so
  `dlnaMaxHeaderBytes` is 16 KiB minus that 4 KiB and the read stops
  at 16 KiB. A header block past that read is 431 and is not stored;
  after the bound the same eight requests answered 431, stored
  nothing, and the heap did not grow. The SOAP body stays on its own
  1 MiB cap.
  **ReadTimeout is 60 s and IdleTimeout is 120 s, the API server's
  deadlines.** The listener set only `ReadHeaderTimeout`. An idle
  keep-alive and a SOAP body that stopped after one byte were both still
  open when the client's 2 s read gave up (4.01 s for the pair; the
  review left both open past 26 s). `WriteTimeout` stays unset: a
  renderer streams, as `/v1/download` does. A per-server override is
  what the close test shortens; production uses the consts.
  **Browse and Search INFO lines cut the client fields to those 100
  runes, on the request line only.** ObjectID, Filter, SortCriteria and
  SearchCriteria were logged whole, and ObjectID and SearchCriteria were
  logged again on the response and the fault (the review: two 0.9 MB
  requests wrote 2.46 MB of INFO lines, and the log file is not rotated
  on macOS or Windows). The response keeps the browse flag and the page
  counts. `remoteAddr` is the TCP peer and stays as it is.
  (`TestASubscribeFloodDoesNotSpawnAGoroutinePerNotify`,
  `TestBrowseAndSearchCapAZeroRequestedCountAndAClientCanPageTheRest`,
  `TestARequestedCountUpToThePageIsReturnedWhole`,
  `TestAClientPagingByNumberReturnedCollectsEveryCappedTrackOnce`,
  `TestTelemetryKeepsAHundredRunesOfEachHeader`,
  `TestRecordReleasesTheRestOfACutHeader`,
  `TestTheListenerRefusesAnOversizedHeader`,
  `TestAHeaderPastTheReadLimitIsRefusedAndAnOrdinaryOneIsAnswered`,
  `TestTheListenerUsesTheAPIDeadlines`,
  `TestTheListenerClosesAnIdleKeepAliveAndAStalledBody`,
  `TestBrowseAndSearchLogsTruncateClientFieldsAndDoNotRepeatThem`.)
- **The DLNA listener serves a generated stereo DSD silence, capped at
  four streams** (2026-10-09, backlog B319). Pausing or stopping a
  DSD256 stream on a Chord 2go docked to a Hugo 2 (MPD 0.21) makes the
  Hugo 2 ring. A stereo DSD256 DSF of constant `0x69` stayed silent
  (measured 2026-10-09). The app switches MPD onto this file instead of
  a bare stop.
  **`GET` and `HEAD /dlna/silence/dsd/<fs>.dsf`**, beside
  `/dlna/silence.wav`. `<fs>` is 64·n × 44,100 or 64·n × 48,000 for
  n ∈ {1, 2, 4, 8}. Any other segment is 404.
  **The body is a 60 s stereo DSF, every data byte `0x69`**, pad of the
  last 4096-byte block included. `0x00` is full-scale offset. Layout
  matches `dsdtone.WriteDSF` (Sony DSF v1.01). Production does not
  import `internal/dsdtone`. `ServeContent` reads a seeker over a shared
  92-byte header and fills `0x69` in the read buffer. Nothing is written
  to disk. `Content-Type` is `audio/x-dsf` before `ServeContent`.
  **Four concurrent `GET`s, then 503.** One pause holds one stream for
  about ten seconds. The slot is a non-blocking channel, released when
  `ServeHTTP` returns. `HEAD`, a 404 and a 405 take no slot.
  **A reader that stops reading holds its slot until the write ends.**
  `WriteTimeout` stays unset (B220) so a track can stream. This GET
  sets a 120 s deadline (`dsdSilenceWriteBound`, shortened in tests)
  through `ResponseController` when the body starts: a real-time play
  of the file plus read-ahead, and the app stops it after about ten
  seconds. The response sends `Connection: close`. Go 1.26.6 and 1.27.2
  still clear a `ResponseController` deadline after the handler returns
  (`conn.serve` → `SetWriteDeadline(time.Time{})`) when `WriteTimeout`
  is 0, and the next request gets one only when `WriteTimeout` is
  positive; `Connection: close` is sent anyway. Telemetry (on by
  default) `Unwrap`s, or the deadline never reaches the socket.
  **B220 and B162 are unchanged** (notify pool 8, 16 KiB headers,
  `ReadTimeout` 60 s, `IdleTimeout` 120 s, `WriteTimeout` unset, page
  1000). The cap limits concurrent silence streams, and the deadline
  limits how long one stream may block.
  **`dsdSilence` is listed exactly when the listener is up**, the same
  gate as `dlnaServer`, between `dsdRender` and `favorites`. Public
  mode does not advertise it. `ProtocolVersion` stays 1.
  (`TestDSDSilenceHeaderAndSizeForEachRate`,
  `TestDSDSilenceDataBytesAreTheSilenceByte`,
  `TestDSDSilenceRangesAndHead`,
  `TestDSDSilenceUnknownRateIs404`,
  `TestDSDSilenceCapAnswers503AndFreesTheSlot`,
  `TestDSDSilenceCapFreesTheSlotWhenTheClientDisconnects`,
  `TestDSDSilence404DoesNotHoldASlot`,
  `TestTheDLNAListenerServesDSDSilence`,
  `TestDSDSilenceFFProbe`,
  `TestAStalledSilenceReaderFreesItsSlot`,
  `TestHealthAdvertisesDSDSilenceExactlyWhenDLNAIsEnabled`.)
- **A service URL is kept only as a string that parses back to the scheme
  and host the policy judged** (2026-10-01, backlog B233). The nightly fuzz
  on f2bb02bd found `FuzzParseDeviceDescription` keeping
  `http://[fe80::1%25en0%B3…]:8080/t`, which `url.Parse` refuses (`invalid
  URL escape "%B3"`): net/url takes a raw non-ASCII byte in an IPv6 zone,
  `URL.String` writes it back as `%B3`, and a zone's unescape refuses an
  escape of a byte the host rules would escape. The same zone in UTF-8 in
  the XML does it too (`%C2%B3`), from a discovered description or a manual
  upstream. The policy judged the PARSED URL, and every later reader (the
  caches, `LiveHost`, the proxy, the ingest's SOAP, GetProtocolInfo's POST)
  parses the kept STRING, which named nothing. `resolveServiceURL` parses
  back the string it returns and refuses one whose scheme or `Host` differs
  (`errServiceURLDoesNotParseBack`; a refused control URL takes its service
  with it, an optional URL goes alone). **Refused, never repaired**: a zone
  names an interface of the host that reads the URL, which no device can
  know, and such a URL never worked, since no reader could parse it.
  **`Host`, not only `Hostname`**: it carries the port `LiveHost` dials. A
  zone that reads back is kept (ASCII, a Windows zone with a space,
  `%25Wi-Fi%204`), and so is non-ASCII in a host name outside a zone (its
  `%C3%A9` reads back). **The LOCATION path cannot keep this shape**:
  `fetchableLocation` and `LocationPermittedBy` keep the value as it
  arrived, which every reader parses as the policy did, and a description
  fetched from such a LOCATION keeps no service
  (`TestALocationIsKeptAsItArrived` fails if either returns `u.String()`).
  **A URL the bridge writes back from its parsed form and keeps is checked
  by parsing it back.** The minimised crasher is the seed
  (`testdata/fuzz/FuzzParseDeviceDescription/1bd8ceb8fcd09b36`);
  `TestParseDeviceDescription_KeepsNoServiceURLThatDoesNotParseBack`,
  `TestParseDeviceDescription_DropsAnOptionalURLThatDoesNotParseBackAlone`.
  The app's twin returns the `URL` value it judged, never a string parsed
  again (read, not measured), and B167 (`baseurl.CanonicalHTTPS`) is the
  same zone class, still open.
- **Both discovery clients track in-flight detail fetches in a `WaitGroup`, and
  `cache.Clear()` runs UNDER `runMu` as `Stop`'s final act.** Without the group, a
  fetch that already passed its ctx check upserts AFTER `Stop` cleared the cache —
  a ghost server that never ages out. Without `Clear()` under the lock, a
  concurrent `Start()` slips through the gap, spins fresh loops that upsert, and
  the stale `Clear()` wipes them. Every fetch spawn goes through the
  `spawnDetailFetch` helper that does the `Add(1)`; a missed site panics with a
  negative counter.
- **…and both CLAIM a fetch per UDN from one bounded set, because a
  semaphore acquired INSIDE the spawned goroutine bounds the fetches that
  RUN, never the goroutines waiting to** (2026-09-28, backlog B37). The
  upstream MediaServer client spawned a fetch per announcement, queued on its
  two-slot semaphore: 10,000 packets cost 10,000 goroutines and 35 to 37 MiB
  of stack, for one UDN as for many, and a burst for one new server fetched
  its description once per packet (1,000 GETs for 1,000 packets). The
  renderer client's per-UDN `claimFetch` collapsed one device's burst and
  still spawned one goroutine per DISTINCT UDN, the shape a LAN peer that
  sees the M-SEARCH's source port can send. **A dedup is not a bound**: the
  backlog entry read that claim as guarding "exactly this", and it guarded
  one shape of two. `discovery.DetailFetchClaims` is now the one set both
  clients use, under each one's `locMu`: `Claim` refuses a UDN already
  claimed and any claim past `MaxPendingDetailFetches` (64). A refused
  dispatch records nothing, so the next announcement dispatches again (a new
  device is fetched then, and a moved one still reads as moved). Measured
  after: 1 goroutine and 1 fetch for one UDN's burst, 64 goroutines for
  10,000 distinct UDNs, in either client. The claim is taken before the
  spawn and released by the fetch's last deferred call (registered after
  `wg.Done`, so it runs first): `Stop`'s join releases every claim, and a
  restarted client skips no UDN. **The bound is on goroutines, not on the
  caches**: those have their own (the next bullet). A refusal at this bound
  is silent, and stays so (B47 measured it and decided, the next bullet).
- **…and both CACHES hold at most `discovery.MaxCachedDevices` (256), and a
  full cache never makes room by dropping a device it serves** (2026-09-28,
  backlog B47). A renderer whose LOCATION answered 4xx left a stub with a
  year-2999 `LastSeenAt` that no eviction pass reached, and a location
  record beside it, so a flood of distinct UDNs grew both for the life of
  the process (5,000 of 5,000 left an hour later, about 490 bytes a UDN),
  and a REAL renderer whose description failed once, while it booted say,
  was never fetched again from that address (one GET, then nothing in two
  hours of healthy announcements). The upstream cache kept every MediaServer
  serving a valid description (100,000 held 45 MB, and LiveHost's folded
  fallback copied all of them per routed byte fetch: 7.7 ms, 18 MB).
  **A structural stub lasts `structuralStubHold` (5 minutes) whatever the
  TTL**, stamped `hold - ttl` from the failure so `EvictStale` drops it at
  the hold: BEFORE the failure under a TTL longer than the hold, which
  `dlna.discovery.rendererTTLSeconds` admits up to a year. The first form
  clamped the stamp to the failure there and held the stub for the whole
  TTL (CodeRabbit on #1086). Nothing but eviction reads the stamp, and the
  earliest stamp is still the earliest expiry. A stub still costs a broken
  device one GET per hold, not per cycle. **The renderer
  cache makes room by evicting the stub that expires first**, and refuses a
  new UDN once only renderers it serves are left (`makeRoomLocked`): a
  served renderer is what a phone may be driving, and a new UDN is what any
  LAN peer can announce. **The server cache refuses a new SSDP server past
  the bound and never one the operator configured**: `UpsertConfigured` for
  the manual poller and for a UDN `DiscoveryConfig.Configured` names (the
  wiring's `upstreamDiscoveryConfig`, case-folded like `StableServerKey`), so
  a flood of fakes cannot keep the server the ingest walks out. **Don't
  switch either to LRU**: under a flood of valid fakes the least recently
  seen entry is the real device, refreshed every 30 to 60 s while the fakes
  are refreshed every second. A refused fetch leaves nothing behind (the
  renderer client drops the record it wrote, `store`; the upstream client
  records only after a stored Upsert), and the renderer client reaps its
  location records whenever they pass `maxLocationUDNs`, because the cache
  is shared by one client per interface and an evicting write cannot clean
  another client's map. **The upstream handler looks a server up and
  refreshes it in one step, `ServerCache.Touch`**: its Get then Upsert of
  `{UDN, LastSeenAt}` resurrected an entry `EvictStale` took in between,
  with no control URL, which was never fetched again (2 of 200,000 races on
  main). **The renderer handler's refresh is `RendererCache.Touch`** (backlog
  B165, 2026-10-15): the same Get then Upsert brought an evicted renderer
  back as a stub that Snapshot hides and the stub branch never fetches, so
  a live renderer was missing from /v1/renderers until the stub aged out.
  Touch refreshes only a renderer still cached with a ControlURL, so a stub
  that replaced it meanwhile keeps its LastSeenAt (the stub rule); **don't
  make it refresh a stub**. `beforeKnownRefresh` lands the eviction in
  that window (`TestHandlePacket_ARendererEvictedBeforeItsRefreshIsFoundAgainOnItsNextAnnouncement`). **Measured and left**: a dual-homed upstream server alternating its
  two addresses re-fetches twice per M-SEARCH cycle (120 GETs an hour, the
  control URL alternating between two valid addresses); porting the
  renderer's location set into that security-relevant move detector was not
  worth two GETs a minute. And a dispatch refused at the claims bound logs
  nothing, a result refused at a full cache only at Debug: neither bound is
  reached by a real LAN, a flood's refusals are the protection working, and
  a once-per-episode Warn needs a latch with its own flap rules, the second
  streak policy #1072 declined. `TestAFloodOfBrokenRenderersStaysBounded`,
  `TestARendererWhoseDescriptionFailedOnceComesBackAfterTheHold`,
  `TestAFloodNeverDisplacesACachedRenderer`,
  `TestAFloodOfFakeServersStaysBounded` and
  `TestAConfiguredServerIsCachedPastTheBound` pin it.
- **Both SSDP read loops share `discovery.HandleReadErr`** — timeout resets the
  streak, `net.ErrClosed`/ctx exits, anything else logs with a ctx-aware backoff
  and one escalation. A bare `return` on a transient error kills discovery for
  the process lifetime; no backoff hot-spins a core. Keep the policy in the
  shared helper.
- **The M-SEARCH SEND failure log is streak-suppressed, and BOTH discovery
  clients report through the one `discovery.SendFailureLog`** (first at Warn,
  one Error ten minutes into the streak, then silence until recovery, which
  logs the streak's length). It runs on a ticker and its failure mode is
  persistent by nature: unsuppressed it produced **199,078 of the last
  200,000 log lines**. The cost isn't disk — it's that every other line
  becomes unfindable. **The upstream MediaServer client discarded every send
  error until 2026-09-28** (backlog B32): pinned to the dev Mac's routeless
  `utun0`, its socket answered `sendto: can't assign requested address` on
  every tick and the client logged nothing in 20 ticks; it now logs the Warn.
  **Ten minutes is a DURATION, which each client turns into its own ticks**
  (`sendErrEscalateAt`: 20 at the renderer's 30 s, 10 at the upstream's
  60 s, never below 2). It was the renderer's constant 20, ten minutes at one
  cadence only, and both cadences are configurable. **Every line names the
  interface**: both wirings start one client per LAN-eligible interface, and
  a route can be gone on one of them while the others send. `Start` calls
  `Reset`. Don't give a client its own copy of the policy: this is the send
  side's one definition, as `HandleReadErr` is the read side's. **Each SSDP
  endpoint's NOTIFY bursts report through it too** (2026-09-28, backlog
  B47): a failed write logged at Debug alone, so an advertiser whose
  interface lost its route announced nothing and said nothing at the default
  level. It notes ONE result per burst (`SSDPHandle.noteAlive`, the first
  failure of its writes): noted per write, the Error lands inside the first
  burst (escalation is at the second failure, ten minutes of a 14-minute
  cadence). The endpoint's start (Register, or an Update's addition) builds
  the log, so each start is a fresh streak, and after it only the
  endpoint's timer goroutine notes (an Update's repeat is a kick to that
  goroutine, never a second writer); the byebye burst at a withdrawal notes
  nothing, and a burst the withdrawal's cancellation cut short notes
  nothing unless a write before the cut had failed for its own reason,
  which is then its result (B470 PR 5a1,
  `TestAFailureThatLandsDuringAWithdrawalIsStillReported`).
  `NewSendFailureLog` takes what it reports on ("M-SEARCH", "NOTIFY").
- **…and a send Stop's close cut short is a STOP, not a failure**
  (2026-09-28). `sendMSearch` snapshots the socket and then writes, and Stop
  can close it in between: the write fails with `net.ErrClosed`, which logged
  "M-SEARCH send failed … use of closed network connection" and took the
  streak to 1 on 4 of 6,000 plain Start→Stop cycles on macOS (43 under
  `-race`) and 24 of 4,000 on Linux under `-race`. `SendFailureLog.Note`
  drops it before the streak sees it (0 of 12,000 after, on each host). **The ERROR
  decides, never the run's context**: the socket is the client's own and only
  Stop closes it, so `net.ErrClosed` names the stop exactly, while a write
  takes no context and a genuine failure that lands during Stop is still a
  failure (#998's second condition, under The CLI and the serve wiring).
  `TestSendMSearchReportsAFailureThatLandsDuringStop` goes red if the check is
  widened to `ctx.Err() != nil`. `HandleReadErr`'s ctx arm is no precedent: it
  decides whether the READ loop exits, not what a result means. The drop is in
  the shared log, so the upstream client has it too (its socket is likewise
  its own, and only its Stop closes it). An SSDP endpoint's alive burst
  (`SSDPHandle.sendAliveAll`) ends on the same error rather than logging a
  Debug line per target left, since only its withdrawal closes its sender,
  and returns it, which the shared log drops; a write that fails for its own reason still
  logs one Debug line per target, and the burst's result reaches the
  default level through the log.
- **A test that starts an SSDP advertiser binds it to the loopback
  interface, and a discovery test client sends no M-SEARCH** (2026-09-28,
  backlog B38). Two `internal/dlna` tests bound the OS default, so one run of
  the package multicast about 310 NOTIFYs of a fake MediaServer with a
  loopback LOCATION to every device and bridge on the LAN (measured with a
  listener joined on en0, three runs; about 300 of them from
  `Test_SSDPAdvertiser_StartStopRaceFree`'s 1 ms advertise interval), and
  each package's `TestStopWaitsForInFlightFetch` sent a real M-SEARCH.
  `loopbackInterface(t)` pins the join, the sends and the listener to
  loopback and keeps the race and lifecycle coverage (both tests PASS, not
  skip, on macOS and in a Linux container); `newTestClient` and
  `newServerDiscoveryTestClient` install a `writeMSearch` that sends nothing.
  Measured after: 0 datagrams on en0 in three runs of each package, while a
  lo0 listener heard the advertisers' NOTIFYs; on Linux 0 on the Docker
  bridge, where main put 299. **A new test that starts an advertiser or a
  client does the same.** **The skip is decided by the PIN, not only by
  Start's error**: Start only warns when `SetMulticastInterface` fails, and
  sends on the OS default interface, so a test whose Start succeeded would
  multicast onto the LAN on such a host. `loopbackInterface` first pins a
  socket of its own with Start's own call (`pinMulticastInterface`) and
  skips where that fails (CodeRabbit on #1086). The fallback itself stays:
  a production advertiser whose pin fails still advertises. **Measure what
  leaves a Linux host from ANOTHER network namespace, or with tcpdump on
  the bridge, never with a listener
  in the same one**: Go binds a multicast listener to the WILDCARD address
  and the group's port (this said "the group address" until 2026-09-29),
  and with Linux's default `IP_MULTICAST_ALL` a socket joined on one
  interface also receives the group's datagrams arriving on any interface
  another socket joined (a listener on eth0 heard the loopback advertisers'
  NOTIFYs, and one on lo heard eth0's). A test's own listener keeps that
  default; the advertisers' listeners turn it off since backlog B71 (the
  next bullet). On the Windows runner the join and the pin both take on
  "Loopback Pseudo-Interface 1": the tests run there, and its log shows them
  starting with no pin warning. What left a Windows host was measured on
  2026-09-29 (B71, on a Windows 11 host with Tailscale, with a listener on
  the same LAN): nothing reached the LAN, because the advertiser's
  connected NOTIFY sender took the Tailscale route, which its pin did not
  move. The sender is unconnected since.
- **Each SSDP advertiser hears only the multicast M-SEARCHes that arrive on
  its own interface, and announces from that interface's address** (2026-09-29,
  backlog B71). The DLNA server runs one SSDP endpoint per LAN interface,
  each with that interface's LOCATION (#328), on one listener per interface
  that every device of the SSDP subsystem shares (B470 PR 5a1, the
  subsystem's bullet below), and by the eligibility rule a Linux
  host running Docker is such a host: `docker0` and every user bridge carry
  a private IPv4.
  **The listener**: `net.ListenMulticastUDP` binds the wildcard address
  (net's `listenDatagram` rewrites the group before the bind), and Linux's
  default `IP_MULTICAST_ALL = 1` delivers the group's datagrams to it from
  every interface where any socket on the host joined, so every advertiser
  answered every interface's M-SEARCHes with its own LOCATION. Measured with
  the real binary in three network namespaces: each M-SEARCH from either
  subnet (6 of 6) got two answers, one naming the other subnet, the wrong
  one first in 3. macOS and Windows deliver per joined interface
  (measured). `listenSSDP` (ssdp_listen_linux.go) sets `IP_MULTICAST_ALL =
  0` in the `ListenConfig`'s Control, **before the bind**: from the bind on
  a wildcard socket takes other interfaces' group datagrams, so setting it
  after `ListenMulticastUDP` returns leaves a window. A kernel that refuses
  the option keeps the old listener with one Warn, never a refusal that
  takes DLNA down. **The sender is an unconnected socket, pinned before
  anything is sent, writing each NOTIFY to the group** (`openNotifySender`).
  `net.DialUDP` fixed its route and source address at the connect, along
  the group's route, and a pin set afterwards did not move the source (Linux,
  macOS) or even the route (Windows). Measured: the second interface's
  NOTIFYs came from the first interface's address, and a renderer with no
  route back received 0 of 5 (5 of 5 now); on Windows with Tailscale
  (home-pc's shape: Windows' SSDP service holds the group on every
  interface and Tailscale's route has metric 5) the connect took the
  Tailscale address and route, and a sender pinned to Ethernet put 0 of 4
  datagrams on the LAN (2 of 2 now). **And on a host with no route to the
  group (no default route) the connect failed ("network is unreachable"),
  no advertiser could start, and DLNA did not start at all**; an
  unconnected socket needs no route. **Don't connect it, and don't pin a
  socket after connecting it.** A single-interface host sees no change:
  its advertiser's one membership is where every search arrives, and its
  source is that interface's address either way. The discovery clients
  take nothing from `IP_MULTICAST_ALL` (an ephemeral port, no membership);
  the upstream client's MediaServer search is answered by the bridge's own
  advertisers, with both LOCATIONs before and one now (a looped-back
  search, measured), and only a configured server is ever walked. Pinned
  by `TestAnSSDPListenerHearsOnlyTheInterfaceItJoined` (Linux, and macOS
  where the host sends its datagram: GitHub's macOS runner answers `no
  route to host` for en0, and the test skips there),
  `TestAStartedAdvertisersListenerHearsOnlyItsOwnInterface` and
  `TestAListenerTheKernelWillNotConfineStillListensAndSaysSo` (Linux),
  `TestAStartedAdvertiserWritesItsNotifiesFromAnUnconnectedSocket` (all),
  `TestAnAdvertiserOnLoopbackNotifiesFromALoopbackAddress` (macOS, Windows:
  Linux gives a lo-pinned multicast another interface's address either way,
  127.0.0.1 being host-scoped), and
  `TestSSDPAdvertisersKeepToTheirOwnInterfaces`, the real server over veths
  in a network namespace of the test's own, with and without a route to the
  group (root, or a user namespace: dido, not CI). **Still open**: a unicast
  M-SEARCH to port 1900 reaches ONE listener's socket (Linux: the last
  bound; macOS, Windows: the first) and the MediaServer answers it with its
  endpoint on that listener, whichever served address it was sent to
  (backlog B119, which B470 PR 5a1 keeps exactly; since
  B48 one sent to an address the server does not serve is not answered at
  all, the bullet after the Tailscale one below); on Windows the
  advertiser hears no search sent from its own host (B120); and a pinned
  `dlna.listenAddress` advertises on the picker's interface, which need
  not hold that address (B121).
- **`upnp_track_routing.server_udn` holds the ingest's `StableServerKey`, NOT the
  device's raw UDN.** They are equal only for a device whose UDN is already
  lowercase, and never for a manually-configured server (`manual:<sha256(url)>`).
  The SSDP cache is keyed on the raw UDN, so handing a routing key to a raw-UDN
  lookup reports "offline" about upstreams that are up. Anything wanting both
  membership and liveness must carry BOTH spellings — one lookup cannot serve
  both. `librarycat.SourceID` prefixes `"source:"` before hashing so a routing
  key can never collide with an album or artist id.
- **`ServerCache.Upsert`'s merge must carry EVERY descriptive field, and the
  test that pins it is reflective.** The SSDP handler refreshed a known UDN
  with `{UDN, LastSeenAt}` on every announcement (it uses `Touch` since
  2026-09-28) and the merge preserved the
  cached fields one by one — `DeviceUDN` was added to `ServerInfo` and not to
  the list, so any partial refresh blanked it. Latent today (only the
  manual-URL poller sets it, under a key no SSDP refresh lands on), and the
  next field would have met the same list. The merge is `mergeServerInfo`,
  which `Upsert` and `UpsertConfigured` share.
  `TestServerCacheUpsertPreservesEveryDescriptiveField` fills every string
  field by reflection, so a field is covered the day it is declared.
- **A path component that sanitizes to NOTHING is a folder that collapses onto
  its parent.** `sanitizePathComponent` returned "" for a control-only title
  and `joinPath` skipped the empty component. Unreachable through the DIDL
  walk — `encoding/xml` refuses every byte the sanitizer drops except the
  whitespace `TrimSpace` removes first — but it is a plain function any future
  caller can hand a raw string. Now: the floor is `"_"`, and both callers use
  `sanitizePathComponentOrEmpty` + the ObjectID fallback AFTER sanitizing
  (`containerPathComponent`, `synthesizeFilename`), which is unique where the
  floor is not. Pin the fallback through the helper, not the walk: the walk
  fixture cannot construct the input.
- **A manually-configured upstream is cached under that same `StableServerKey`**,
  which is what makes routing rows, telemetry, `LiveHost` and the online chip all
  work from one insertion point. Implementing that path took three surfaces, not
  one — the walk, `LiveHost` for playback, and status. (Until PR #824 `ingestOne`
  refused it outright; **this entry still said "CONFIGURED, VALIDATED and
  UNIMPLEMENTED" a day after that shipped** — the fourth stale claim of the kind.
  Check the code.) **No SSDP packet can name that key** (backlog B163,
  2026-10-15): both discovery clients key their caches by the UDN a USN carries,
  and one alive whose USN was `manual:<the key>`, sent from another host,
  re-fetched the description there and gave the manual server's entry the
  sender's control URL, description URL and dial approval (the ingest and
  `LiveHost` then went there), and refreshed its `LastSeenAt` so a down server
  showed online; a byebye removed the entry. `discovery.UDNFromUSN` answers ""
  for a UDN in `discovery.ManualKeyPrefix` (any case, after any leading space),
  so both clients drop the packet, and `StableServerKey` builds the key from
  the same constant. **Don't refuse every UDN outside `uuid:`**: UPnP UDA's
  UDNs begin `uuid:`, but odd firmware exists, and only `manual:` is the
  bridge's own. **Don't give the ingest a second synthetic key space** without
  refusing it in `UDNFromUSN` too (`TestAManualServersKeyIsOneNoSSDPPacketCanName`).
- **UPnP ingest is skip-if-unchanged**, and `walkFieldsEqual`'s exclusions are
  load-bearing: `ModTime` (stamped at walkStart — including it defeats the skip)
  and the enricher-owned MBID fields (including them marks every enriched row
  changed forever). The ROUTING row is still upserted every walk. Baseline load
  failure degrades to nil, never fatal.
- **A reap grace expires for a fact about the UPSTREAM, never about our own
  READ.** `walkLooksImplausible` is true for two reasons — the upstream reported
  far fewer tracks than we last saw, and `ListUPnPTracksByServer` failed so the
  baseline is unknown — and `reapAuthorized` gave both the same 6 h window. At
  the default 6 h cadence the second or third tick authorized a reap against a
  baseline nobody had seen, which is the guard's own docblock inverted; pair it
  with a silently-partial walk (a container that Browses empty mid-tree: no
  error, no `stats.Truncated`) and the sweep deletes every unvisited row. The
  reasons are now named and only `implausibleShape` can expire. Same rule as the
  scanner's, one layer up.
- **An upstream walk stops at a container budget and a time budget, and skips a child
  container that is its own ancestor** (backlog B164, 2026-10-15). `recurse` was bounded only
  by `MaxDepth` (12, which bounds a chain) and `MaxItems` (audio items only), so a tree of
  empty containers (four children a level is 16.7M containers by depth 12) or a cycle with
  two ways round walked until a restart, and `upnpingest.Run` holds `runMu` across every
  server, so every other upstream's walk and "Rescan now" waited behind it. The walk now ends
  at `MaxContainers` (default 50,000 containers) or `MaxDuration` (default 4 h; the walk runs
  under a context with that deadline, so it also ends one container's pages, which
  `BrowseAll` follows up to `MaxBrowseAllItems`) with
  `ErrWalkContainerBudget` / `ErrWalkTimeBudget`, each an `ErrWalkTruncated` to `errors.Is`,
  so the ingest reaps nothing and walks again next tick; and a child whose id is an ancestor
  of the container listing it is skipped (`WalkStats.CyclesSkipped`), while a container under
  two parents is still walked under each. **Don't make a budget error anything but an
  `ErrWalkTruncated`** (a partial walk must never reap), **and don't widen the cycle check to
  every container already visited**: a container reachable from two parents is not a cycle,
  and its second path is rows of its own.
- **Routed rows fill artist/album from the container path via `dupes.Resolve`,
  never manifest's `fillFromPath`** — the scanner's two-directories-up rule puts
  "CD1" in the album field, while `dupes` strips disc folders and is what the
  catalog and iOS already resolve through, so the fill writes the values already
  on screen and cannot regroup anything. **Fill only; never rewrite a field the
  upstream supplied**, and never persist the `dupes.UnknownArtist` /
  `UnknownAlbum` display sentinels. Persisted, they become the enricher's search
  terms, which resolves *some* release for "Unknown Artist" and attributes its
  cover, bio and booklet to an arbitrary track — a wrong answer that looks like
  a right one, so nobody reports it.
- **A routed track's variant-skip reason says "routed", and that case comes
  FIRST** — DIDL supplies no bit depth, so every routed FLAC otherwise reports
  "format unreadable" about a file whose format is perfectly well known.
- **Chord 2Go's file fetch identifies as generic MPD**, so the `chordFamily`
  matchers never fire on real 2Go traffic — it lands on `mpdGeneric`. Harmless
  today (identical MIME maps, no enforcement), but **any future enforcement keyed
  on the Chord profile silently won't apply to real Chord hardware**. Don't
  "fix" it with a version-anchored `Music Player Daemon 0.21` UA matcher, which
  rots on the next firmware.
- **The renderer cache's controlURL refresh is NOT a copy of the server-side
  fix** — a failed re-fetch upserts a stub whose merge advances `LastSeenAt`
  while keeping the dead URL, pinning it forever.
- **`IsLANEligibleInterface` scans all addresses then decides**
  (`hasPrivate || (hasLinkLocal && !hasPublic)`). The obvious simplification
  regresses the no-usable-address cases, and disqualifying on any public IPv4
  breaks dual-stack home LANs where SLAAC hands out a public IPv6.
- **A Tailscale interface is eligible ONLY through the opt-in
  (`EligibilityOpts.TsnetIfaceName`), and its ULA is what had defeated it**
  (2026-09-28). A Tailscale interface carries an address in
  `fd7a:115c:a1e0::/48`, and one in 100.64/10 where the tailnet has IPv4. The
  100.64/10 address always counted as public; the ULA is
  inside fc00::/7, so `net.IP.IsPrivate` counted it as a LAN address and
  admitted the interface with no opt-in (and no production caller sets one).
  Measured: the dev Mac's `utun12` and dido's `tailscale0` were eligible with
  their addresses and not without the ULA. The real bridge on the Mac then
  ran an SSDP advertiser on utun12 (LOCATION on its 100.x address) and a
  renderer- and a UPnP-discovery client sending into it, and on dido each
  of those consumers' first step (the group join, an M-SEARCH send)
  succeeded on tailscale0. On Windows (reasoned from the ranking and pinned
  by a row, not measured) the Wintun adapter, which has no point-to-point
  flag, outranked a zero-config LAN as the mDNS responder's single pick.
  `isTailscaleULA` sorts the ULA out BEFORE `IsPrivate` and counts it as
  public, so it admits nothing AND keeps the zero-config arm from admitting
  a tailnet with IPv4 switched off (fe80 plus the ULA). **Address-based, not
  name-based**: macOS numbers its utuns, and Windows' adapter carries no
  flag that says tunnel. 100.64/10 needed no change: a LAN genuinely
  numbered in CGNAT space was refused before and still is. `tailscaleULA`
  is pinned to `tsaddr.TailscaleULARange()` by
  `TestTailscaleULAIsTailscalesRange`. Multicast written to the tailnet
  interface reached no peer in the one tailnet measured (no exit node, no
  subnet router): 0 of 32 datagrams each way between the Mac and dido,
  beside 32 of 32 unicast controls.
- **…and the DLNA listener and SSDP serve only the LAN-eligible
  interfaces' addresses, judged per connection and per datagram**
  (2026-10-16, backlog B48; the owner's decision of 2026-10-15: DLNA is for
  the home network only). The rule above kept a tailnet interface out of the
  pickers only. The listener binds `dlna.listenAddress` (`:7790` by default)
  and each SSDP listener `0.0.0.0:1900`, so both took a
  connection or a unicast datagram on every address of the host, and every
  tailnet peer, a node shared from another account included, reached the
  unauthenticated surface: measured from a peer in the B15 session, a GET of
  `/dlna/description.xml` on the host's tailnet address answered 200 and a
  unicast M-SEARCH there on 1900 was answered with the LAN LOCATION, and
  `config_gate.go` said to reach DLNA over Tailscale. The sockets stay the
  wildcard ones. `servedOnlyListener` closes, before reading a byte, a
  connection whose LOCAL address is not served, so no handler (the Host
  check included) sees it, and the MediaServer's SSDP admission
  (`ssdpServedAdmission.admits`, the advertiser's `searchedFor` until B470
  PR 5a1) drops an M-SEARCH whose DESTINATION is neither the SSDP group nor
  a served address.
  The destination is IP_PKTINFO on Linux and IP_RECVPKTINFO on macOS (x/net's
  `FlagDst`), and on Windows IP_PKTINFO through `WSARecvMsg` (`ReadMsgUDP`;
  x/net reads no control message there), decoded by `parseWSAPktinfo`, which
  is tested on every platform. **What is served is `servedAddrs`**:
  loopback (the host itself); an address of a member of the multicast set
  (`pickAllLANInterfaces`, no opt-in) of a LAN kind by the eligibility check's
  own classes (`lanAddrKindOf`: RFC 1918, a ULA outside Tailscale's,
  link-local, an IPv6 one only with its member as zone); and a member's
  address the server itself advertises, whatever its kind (its LOCATION may
  name a public IPv4 on an interface eligible through a ULA). So not a
  tailnet address (its interface is no member, and its addresses count as
  public anyway: either half alone refuses them), not another interface's
  fe80 (a docker veth, awdl0, a link-local tunnel), and not a member's public
  address the server does not advertise (a dual-stack LAN's SLAAC address,
  which a router may let the internet reach). A snapshot of the interfaces
  answers, rebuilt at `servedMaxAge` (10 s) and, for an address it refuses,
  at `servedMissRebuild` (1 s), so a new LAN address is served within a
  second and a flood of refusals lists the interfaces once a second; a
  listing that fails keeps the last snapshot. A refusal is one Warn per kind
  and address, naming the interface that holds it (bounded at 32), **and
  never the peer**, for a connection and an M-SEARCH alike: `admit` is not
  handed one (2026-10-30, B604, the 0.2.2 logging audit; the line named the
  peer that connected or searched until then, `from`, which no rule
  allowed, and a tailnet peer or a scanner is exactly whom it refuses). **A
  `dlna.listenAddress` pinned to an address that is not served warns once at
  Start** (`warnIfPinnedOffTheLAN`, the owner's ask of 2026-10-16): the
  listener binds that address alone and refuses every connection on it, so
  DLNA serves no one, and before this the first refused connection was the
  only line that said so. It judges the address the listener bound, so a
  pinned name counts, and a wildcard is no pin. **It warns and starts; don't
  make it refuse** (an address can become served later), and the
  per-connection Warn keeps its cap beside it.
  **Chosen over binding each served address**, which needs a rebind loop
  with a window where a new address is unreachable, zoned binds for IPv6
  link-local, retries for Windows' tentative addresses, and a new SSDP
  listener: a socket bound to a unicast address hears no multicast on Linux
  or macOS, and Windows refuses a bind to the group. Its cost: a refused
  peer completes the TCP handshake, so a scanner sees the port open and then
  a close. **`dlna.allowTsnet` is reserved and has no effect**: nothing reads
  it and no caller passes `EligibilityOpts.TsnetIfaceName`; it stays because
  Load refuses an unknown key (`TestADLNAConfigThatSetsAllowTsnetStillLoads`).
  **Don't serve a tailnet address again without a real opt-in, don't widen
  the served set to every address of an eligible interface, don't move the
  check into a handler or the Host check, don't answer a datagram whose
  destination was asked for and not reported** (only a listener that cannot
  be armed answers as before, with one Warn), **don't judge a group
  datagram by its destination** (the group is no address of the host; B71's
  membership keeps it to its interface), **and don't name the peer in a
  refusal**. **Residuals**: a Docker container
  on a bridge network sees a published port's connections on its own eth0
  address after DNAT, so that path works as before and is reachable from
  wherever the HOST's port is, its tailnet included (publish on the LAN
  address, `-p <LAN IP>:7790:7790`, as `docs/docker.md` says under
  "Publishing the DLNA port"); a host that is a Tailscale subnet
  router for its own LAN carries tailnet peers to its LAN address; a VPN
  tunnel numbered in RFC 1918 space is a member (eligibility cannot tell it
  from a LAN); and a laptop on a café's private network serves that network.
  **An operator's probe of `:7790` runs on the host or from the LAN; over
  the tailnet it is now refused.** B119's LOCATION choice for a served
  unicast is unchanged, and the destination read is what it needs.
  (`TestServedAddrsAreTheLANAddressesOfTheMulticastSet`,
  `TestServedAddrsServeLoopbackWithoutReadingTheInterfaces`,
  `TestServedAddrsServeANewLANAddressASecondAfterItsFirstRefusal`,
  `TestServedAddrsStopServingAnAddressWithinTheMaxAge`,
  `TestRefusedAddressesRebuildTheSnapshotAtMostOnceASecond`,
  `TestAFailedListingKeepsTheSnapshotItWouldReplace`,
  `TestARefusalIsLoggedOncePerKindAndAddress`, `TestTheRefusalLogIsBounded`,
  `TestTheDLNAListenerRefusesAConnectionToAnAddressItDoesNotServe`,
  `TestTheDLNAListenerLogsARefusedConnection`,
  `TestARefusedConnectionNamesItsAddressAndInterfaceAndNeverThePeer` (IPv4,
  IPv6 and a mapped peer), `TestARefusedMSearchNamesItsDestinationAndNeverItsSource`,
  `TestAListenerPinnedToAnAddressItDoesNotServeWarnsOnceAtStart`,
  `TestTheStartupCheckWarnsOnlyForAPinnedAddressItDoesNotServe`,
  `TestTheSSDPListenerReadsEachDatagramsDestination`, `TestParseWSAPktinfo`,
  `TestWSAPktinfoConstantsAreWinsocks` (Windows),
  `TestTheSSDPAdvertiserAnswersOnlyASearchSentToAnAddressItServes`,
  `TestSearchedForAnswersTheGroupAndJudgesEveryOtherDestination`,
  `TestTheSSDPAdvertiserAnswersASearchSentToTheGroup` (Linux, macOS),
  `TestEveryAdvertiserAnswersByTheServersServedAddrs`.)
- **Eligibility is the allowlist; SELECTION prefers a real LAN among what it
  admits, and the two stay separate** (2026-09-27). `PickLANEligibleInterface`
  returned the FIRST eligible interface, and macOS enumerates system utuns
  ahead of `en0`: on the dev Mac that was `utun0` (`UP,POINTOPOINT,RUNNING,
  MULTICAST`, one fe80 address), which the zero-config arm admits. The mDNS
  responder pinned itself there, its IPv4 listen failed without a word
  (hashicorp/mdns drops that error, and utun0 has no IPv4), and neither
  hashicorp/mdns's client on `en0` nor `dns-sd -B` found the bridge, while
  the NUC's bridge on the same LAN was found. The picker now ranks what
  eligibility admits (`lanPreference`): not point-to-point first, then a
  private IPv4, then any other usable address (a ULA, the opted-in tsnet
  interface), then link-local only, and enumeration order among equals, so
  a host whose first eligible interface already ranks best keeps it (dido
  measured: `enp1s0f0` and the same five-member set as before; three
  members since the next paragraph's rule and the Tailscale bullet above).
  `PickAllLANEligibleInterfaces` keeps its members and order but leaves out
  a point-to-point interface whose only addresses are link-local whenever
  anything else is eligible: six such utuns sat in the dev Mac's set, and
  the renderer and UPnP-upstream discovery clients start one SSDP client per
  member, while an IPv4 multicast send pinned to such a tunnel fails
  (`sendto: can't assign requested address`, hashicorp/mdns's client on
  utun0). **Don't fold the preference into `IsLANEligibleInterface`**:
  dropping every point-to-point interface, or the zero-config arm, from
  ELIGIBILITY takes away an opted-in tunnel and a direct-cable renderer,
  and the bullet above says why each arm is there. **Don't reduce the key
  to either half**: the flag alone misses Windows' Wintun adapter
  (WireGuard's, and Tailscale's when opted in; `IF_TYPE_PROP_VIRTUAL`,
  which Go gives no point-to-point flag) and a Mac's `bridge0` holding a
  self-assigned address, and the class alone ties a WireGuard tunnel's
  private 10.x with `en0` and lets enumeration order pick the tunnel (each
  half turns rows red that the other leaves green). One case moves the
  other way: a host whose LAN is zero-config (169.254 / fe80 only) beside a
  private-IPv4 bridge (`docker0`, a VM's) now binds the bridge, where the
  first eligible used to win if it came first. The tables drive
  `pickLANInterface` and `pickAllLANInterfaces`, the seam both exported
  pickers call (`TestPickLANInterfacePrefersANonTunnelWithAPrivateIPv4`,
  `TestPickAllLANInterfacesDropsALinkLocalOnlyTunnel`), and
  `TestTheExportedPickersRunTheSelection` compares the exported pair with it
  on the host, which can fail only where the first eligible interface does
  not rank best (the dev Mac, not a typical Linux runner).
- **…and then leaves out a member with no IPv4 address, whenever one with
  an IPv4 address remains** (2026-09-28). Every consumer of the set runs
  SSDP over IPv4 (udp4, 239.255.255.250): the advertisers, which already
  skipped such a member (`gatherAdvertiseEndpoints`), and the renderer and
  UPnP-upstream discovery clients, which did not. On the dev Mac awdl0 and
  llw0 (fe80 only, not point-to-point) each got a renderer client whose every
  M-SEARCH failed with `can't assign requested address` (a WARN per client
  and an ERROR ten minutes on; since #1072 the UPnP-upstream clients on the
  same members report theirs as well); on dido each
  docker veth (fe80 only, a port of `docker0` or a user bridge, both members
  with an IPv4 address) got two clients, whose sends Linux lets out. **The
  rule is the SET's, not the SSDP call sites'**: all three consumers are IPv4
  SSDP, and it sits beside the tunnel rule #1051 justified by the same
  failed send. A 169.254 address counts (the direct-cable renderer).
  **It runs AFTER the tunnel rule, and neither rule empties the set**, so a
  host whose tunnel-ruled set holds no IPv4 member (an IPv6-only LAN) keeps
  that set exactly, and UPnP upstream, whose manual-URL poller starts only
  where an SSDP client does, still starts there. The single pick can then
  be a member the set leaves out (an IPv6-only LAN beside a tunnel holding a
  private IPv4, or awdl0 beside an opted-in tunnel, since a non-tunnel
  ranks first): `assertPickIsInTheSet` allows exactly that, a pick carrying
  no IPv4 while a member does, because the responder answers over IPv6 too.
  `TestPickAllLANInterfacesLeavesOutAMemberWithNoIPv4` and
  `TestPickersLeaveOutATailnetInterfaceWithoutTheOptIn` drive both rules.
- **The mDNS rebind loop compares the ADVERTISEMENT, never the host's
  addresses** (2026-09-28). `maybeRebind` rebuilt the responder whenever
  `ipsForAdvertise()`, every up interface's addresses, differed from the set
  cached at the last rebuild, while the records carry only the pinned
  interface's. So an address coming or going on another interface rebuilt
  it with the same records on the same interface. Sampled at the loop's
  cadence for 80 minutes: 21 such changes on dido, every one a docker veth
  (a container starting or stopping), and 1 on the dev Mac, a new utun
  coming up; the pick and its addresses changed 0 times on either.
  Each tick now builds the advertisement a rebuild would make
  (`advertisementNow`: the InterfaceSource's pick, and the addresses
  narrowed to it, or all of them when nothing is pinned) and rebuilds only
  when it is not `same` as the running one. **By name AND index**: an
  adapter re-created under a new index has none of the old sockets' group
  memberships. That asks the InterfaceSource every tick, where it was asked
  only on a rebuild, so the responder follows a better interface as soon as
  the picker names it, **and the source must not print per call**:
  cmd/bridge's `lanInterfaceSource` prints a failed pick once per streak (a
  host with no LAN-eligible interface would print a line a minute), and
  `TestMDNSInterfaceSourceIsTheOncePerStreakOne` requires the Config literal
  to take it. The rebind tests pin the responder to the loopback interface,
  which hashicorp/mdns binds on macOS and on Linux (measured).

- **A folder's children sort by `RelativePath`, with `AbsolutePath` only
  as the tie-break.** Every UPnP-routed track has an EMPTY `AbsolutePath`
  (`dlna_wiring.go` leaves it so the proxy fast-path takes over), so a sort
  keyed on it alone left a routed folder's children — and the "first keyed
  direct child" its `albumArtURI` comes from — in arrival order, under a
  comment that said "sorted by path". (#919)
- **The file route's serving half is shared with the loopback track route** (B473 B2c): `serveOpenedFile` (the headers, the adaptive writer, `http.ServeContent`) serves `/dlna/file/` on this listener and the same path on the loopback listener the MPD beside the bridge fetches from, so MPD gets the same type and bytes from either; it was extracted from `serveFromFilesystem` unchanged. The loopback route is not a second DLNA listener: no SSDP, no CDS, no artwork, no renditions, no routed tracks, a stricter containment check, a stricter Host rule (`127.0.0.1`, `[::1]` or `localhost` only) and no forwarded request, a per-request `output.mpd` gate, and typed JSON refusals. Its rules are in `output.md`. **Don't add a serving step to one route that the other does not get, and don't mount another DLNA route on the loopback listener.**

- **`dlna.Server.AcceptsOn` answers whether the listener takes a connection on an address now, by the listener's own served set (`servedAddrs`) and its bound address (a pinned listener its address alone, `0.0.0.0` IPv4 alone), and nothing once Stop begins or before Start binds; the player's OpenHome Insert takes such an address as one of the bridge's own** (B470 PR 5a4; the confinement's bullet in `openhome.md`). **Don't answer it from a set or a cache of its own, or for a listener that is not up.** `TestAcceptsOnIsTheListenersServedSet`, `TestAcceptsOnFollowsTheServedSnapshot` and `TestAcceptsOnIsTheRunningListeners` pin it.


## Source discovery (B470 PR 5a2b)

- **Strict multicast selects the first active alias on the receiving listener that Source admits, after capability and destination/arrival proofs** (B470 PR 5a2b). Overlap yields one matched set, and a second disjoint prefix remains discoverable. Unicast first matches exact Local plus arrival index, then Source, with no destination fallback. **Don't choose firstOn before the strict source hook or change legacySelect.** `TestStrictMulticastChoosesAnAdmittedAlias`, `FuzzSSDPDispatch`, and Source's refusal tests pin it. The instance `SSDPNetwork` seam defaults to host transport and preserves real registration/dispatch/retirement in cross-package fake-network tests. Nil Delay and zero IntervalFloor keep existing timing defaults (`TestSSDPNetworkTimingOverridesPreserveDefaults`); the delay guard is preventive while the production default remains nil. → History-rules-dlna-upnp.md

- **An optional `SSDPRegistration.SendBarrier` linearizes lifetime invalidation with alive and reply writes, including initial Register writes before a handle exists** (B470 PR 5a2b, round 3). Check cancellation and barrier state under its read lock, held through one write under the installed one-second burst deadline; Invalidate takes the write lock and joins those writes. A fresh registration gets a fresh barrier. **Do not gate byebye, hold surface/publication locks while joining, or give MediaServer a Source barrier.** `TestSourceHTTPExitJoinsAnAliveAlreadyWriting` and `TestSSDPSendBarrierJoinsRepliesAndLeavesByebyeAndMediaServer` pin the join, subsequent refusal, byebye and MediaServer independence. → History-rules-dlna-upnp.md
