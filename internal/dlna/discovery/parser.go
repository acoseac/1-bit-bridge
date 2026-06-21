package discovery

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ParsedSSDPHeaders is the subset of headers extracted from an SSDP
// M-SEARCH response or NOTIFY packet. All field values are
// header-case-normalized (the parser handles the wire's mixed-case
// shape). Empty strings denote absent headers.
type ParsedSSDPHeaders struct {
	// Location — URL to the device description XML. Required for
	// us to populate the renderer; a packet without it is dropped.
	Location string

	// USN — Unique Service Name carrying the UDN.
	// Format: "uuid:<udn>::urn:schemas-upnp-org:device:MediaRenderer:1"
	// We extract the bare UDN ("uuid:<udn>") via `UDNFromUSN`.
	USN string

	// ST — Search Target (M-SEARCH response only).
	ST string

	// NT — Notification Type (NOTIFY only).
	NT string

	// NTS — Notification Sub-Type (NOTIFY only). Values:
	// "ssdp:alive", "ssdp:byebye", "ssdp:update".
	NTS string

	// Server — informational vendor string. Not used for routing
	// but logged for telemetry.
	Server string
}

// ParseSSDPHeaders parses the raw bytes of an SSDP M-SEARCH response
// or NOTIFY packet into header fields. The first line (status line
// for responses, request line for NOTIFY) is consumed but not
// inspected by this helper — the caller's transport layer routes
// based on the listener's local addr (M-SEARCH response = unicast
// to our ephemeral port; NOTIFY = multicast on 239.255.255.250:1900).
//
// SSDP uses HTTP/1.1 header syntax — we wrap the body in a fake
// "HTTP/1.1 200 OK\r\n" line and let `textproto.MIMEReader` do the
// canonical-case parsing. The synthetic status line is necessary
// because real SSDP packets start with `HTTP/1.1 200 OK` (response)
// or `NOTIFY * HTTP/1.1` (NOTIFY) — both of which would confuse
// `http.ReadResponse` if we tried to use that directly.
func ParseSSDPHeaders(raw []byte) (ParsedSSDPHeaders, error) {
	if len(raw) == 0 {
		return ParsedSSDPHeaders{}, errors.New("empty SSDP packet")
	}
	// Find the end of the first line (request/status line) — skip
	// past it so the header parser sees a clean header block.
	idx := bytes.IndexByte(raw, '\n')
	if idx < 0 {
		return ParsedSSDPHeaders{}, errors.New("malformed SSDP packet: no LF in first line")
	}
	headerStart := idx + 1
	if headerStart >= len(raw) {
		return ParsedSSDPHeaders{}, errors.New("SSDP packet has no headers after first line")
	}
	// http.Header is happy with a Reader yielding "Key: Value\r\n"
	// lines terminated by a blank line. Walk the raw bytes manually
	// to avoid pulling in mime/textproto + handling its mid-parse
	// allocations.
	out := ParsedSSDPHeaders{}
	sc := bufio.NewScanner(bytes.NewReader(raw[headerStart:]))
	// Defensive — SSDP packets are typically <1KB but some
	// vendors stuff long Server strings; 8KB cap covers any
	// real case without unbounded growth.
	sc.Buffer(make([]byte, 0, 8192), 8192)
	for sc.Scan() {
		line := sc.Text()
		// Blank line terminates the header block.
		if line == "" || line == "\r" {
			break
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(line[:colon]))
		val := strings.TrimSpace(line[colon+1:])
		// Strip optional trailing \r left by the scanner on \r\n
		// line endings (Scanner.Text() drops \n but not \r).
		val = strings.TrimRight(val, "\r")
		switch key {
		case "location":
			out.Location = val
		case "usn":
			out.USN = val
		case "st":
			out.ST = val
		case "nt":
			out.NT = val
		case "nts":
			out.NTS = val
		case "server":
			out.Server = val
		}
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("SSDP header scan: %w", err)
	}
	return out, nil
}

// UDNFromUSN extracts the bare UDN from a USN header value. SSDP
// USNs are formatted as `<udn>::<service-or-device-token>`; we
// take everything before the first `::`. Returns the input unchanged
// when no `::` separator is present (some renderers emit just the
// UDN as USN for their root-device announcement).
//
// Examples:
//
//	"uuid:abc::urn:schemas-upnp-org:device:MediaRenderer:1" → "uuid:abc"
//	"uuid:abc"                                              → "uuid:abc"
//	""                                                      → ""
func UDNFromUSN(usn string) string {
	if usn == "" {
		return ""
	}
	if idx := strings.Index(usn, "::"); idx >= 0 {
		return usn[:idx]
	}
	return usn
}

// DeviceDescription is the subset of fields we extract from
// /description.xml. Renderers stuff additional metadata in there
// (icons, vendor extensions) — we read only what the iOS-side
// `OutputPickerSheet` row + the GetProtocolInfo dispatcher need.
type DeviceDescription struct {
	UDN              string
	FriendlyName     string
	Manufacturer     string
	ModelDescription string
	ModelName        string
	// ServiceMap keys on UPnP service type; values are the
	// (controlURL, eventSubURL) for that service. Used by the
	// AVTransport / ConnectionManager dispatchers.
	Services map[string]ServiceURLs
}

// ServiceURLs carries the resolved absolute URLs for a single UPnP
// service. controlURL drives SOAP dispatches; eventSubURL drives
// GENA SUBSCRIBE.
type ServiceURLs struct {
	ControlURL  string
	EventSubURL string
}

// Standard UPnP service-type strings the discovery client cares
// about. The parser populates the `Services` map keyed on these
// when present; absent services produce no map entry.
const (
	ServiceAVTransport       = "urn:schemas-upnp-org:service:AVTransport:1"
	ServiceConnectionManager = "urn:schemas-upnp-org:service:ConnectionManager:1"
	ServiceRenderingControl  = "urn:schemas-upnp-org:service:RenderingControl:1"
)

// rawDeviceDescription is the XML unmarshaling target. Renderer
// XML is messy (mixed casing, optional namespaces, vendor
// extensions) — `xml.Unmarshal`'s case-insensitive matching +
// the explicit `xml:"..."` tags handle the common shape.
type rawDeviceDescription struct {
	XMLName xml.Name  `xml:"root"`
	Device  rawDevice `xml:"device"`
}

type rawDevice struct {
	UDN              string `xml:"UDN"`
	FriendlyName     string `xml:"friendlyName"`
	Manufacturer     string `xml:"manufacturer"`
	ModelDescription string `xml:"modelDescription"`
	ModelName        string `xml:"modelName"`
	ServiceList      struct {
		Services []rawService `xml:"service"`
	} `xml:"serviceList"`
}

type rawService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
	EventSubURL string `xml:"eventSubURL"`
}

// ParseDeviceDescription parses the XML body of a renderer's
// /description.xml and resolves the (controlURL, eventSubURL)
// fields to ABSOLUTE URLs using `baseURL` (the Location header
// from the SSDP packet, also where the description was fetched
// from). UPnP devices commonly use relative URLs in their
// service entries; absolute is what the SOAP / GENA dispatcher
// actually dials.
//
// Returns an error when the XML is malformed, when no AVTransport
// service is present, OR when the AVTransport service has no control
// URL (a renderer that can't be SetAVTransportURI-driven can't be an
// audio target — surfacing it would mislead the user). The partial
// `desc` is returned WITH the error so a caller that tolerates "not a
// renderer" (upstream MediaServer discovery) can still read
// desc.Services. ConnectionManager / RenderingControl absence is
// non-fatal (Sink list resolution would silently degrade, but
// the renderer is still SetAVTransportURI-drivable).
func ParseDeviceDescription(body []byte, baseURL string) (DeviceDescription, error) {
	var raw rawDeviceDescription
	if err := xml.Unmarshal(body, &raw); err != nil {
		return DeviceDescription{}, fmt.Errorf("parse XML: %w", err)
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return DeviceDescription{}, fmt.Errorf("parse baseURL %q: %w", baseURL, err)
	}
	desc := DeviceDescription{
		UDN:              raw.Device.UDN,
		FriendlyName:     raw.Device.FriendlyName,
		Manufacturer:     raw.Device.Manufacturer,
		ModelDescription: raw.Device.ModelDescription,
		ModelName:        raw.Device.ModelName,
		Services:         make(map[string]ServiceURLs, len(raw.Device.ServiceList.Services)),
	}
	for _, s := range raw.Device.ServiceList.Services {
		stype := strings.TrimSpace(s.ServiceType)
		if stype == "" {
			continue
		}
		ctrl, ctrlErr := resolveServiceURL(base, s.ControlURL)
		if ctrlErr != nil {
			// Silently dropping the service marks an otherwise-usable renderer
			// structurally broken with no breadcrumb. Log it so a malformed
			// controlURL (e.g. unescaped spaces from older hardware) is
			// diagnosable. (external review r3 — observability only; no
			// speculative sanitization here.)
			packageLogger.Debug("discovery: dropping service with unresolvable controlURL",
				"serviceType", stype, "controlURL", s.ControlURL, "err", ctrlErr.Error())
			continue
		}
		ev, _ := resolveServiceURL(base, s.EventSubURL) // eventSubURL is optional; ignore error
		desc.Services[stype] = ServiceURLs{
			ControlURL:  ctrl,
			EventSubURL: ev,
		}
	}
	av, ok := desc.Services[ServiceAVTransport]
	if !ok {
		return desc, fmt.Errorf("device %q has no AVTransport service", desc.FriendlyName)
	}
	// AVTransport present but with no resolvable control URL is just as
	// undrivable as no AVTransport at all — SetAVTransportURI has nowhere
	// to go. Treat it as a parse failure so the discovery layer marks the
	// renderer structurally unusable rather than caching a stub it can
	// never drive (which would otherwise retry forever). The partial
	// `desc` is still returned so error-tolerating callers keep
	// desc.Services. (Gemini HIGH on PR #361.)
	if av.ControlURL == "" {
		return desc, fmt.Errorf("device %q AVTransport service has no control URL", desc.FriendlyName)
	}
	return desc, nil
}

// resolveServiceURL turns a possibly-relative service URL into an
// absolute one by resolving against the device description's base.
// `relativeRef` may be "" (e.g. eventSubURL absent on a renderer
// that doesn't surface event subscriptions) — handled by returning
// "" + nil so the caller writes the empty value into the map.
func resolveServiceURL(base *url.URL, relativeRef string) (string, error) {
	ref := strings.TrimSpace(relativeRef)
	if ref == "" {
		return "", nil
	}
	resolved, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	abs := base.ResolveReference(resolved)
	return abs.String(), nil
}

// ParseGetProtocolInfoResponse extracts the `Sink` argument value
// from a SOAP `GetProtocolInfoResponse` envelope. The Sink value
// is a comma-separated list of protocolInfo strings the renderer
// accepts (e.g. "http-get:*:audio/x-dsf:*,http-get:*:audio/x-flac:*").
// We split on commas + trim whitespace; empty entries are skipped.
//
// Returns the slice on success, error on malformed XML / missing
// element. An empty Sink (renderer responded but advertises no
// protocols) returns ([], nil) — degenerate but legal.
type rawSOAPEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		// Match any Response element via wildcard local name (the
		// element is `<u:GetProtocolInfoResponse>` with a vendor-
		// specific xmlns:u; xml.Unmarshal handles the namespace
		// transparently with this wildcard tag). The wildcard ALSO
		// matches `<s:Fault>` which is what we want for fault
		// detection — see `Response.XMLName` consult below.
		Response struct {
			XMLName xml.Name
			Sink    string `xml:"Sink"`
		} `xml:",any"`
	} `xml:"Body"`
}

// ErrSOAPFault is returned by `ParseGetProtocolInfoResponse` when
// the body carries a SOAP Fault element. Distinguishes "renderer
// returned an error" from "renderer advertised an empty sink list"
// — the prior shape conflated the two, masking SOAP faults +
// malformed responses as legitimate empty-sink renderers. Per
// CodeRabbit MAJOR round-1 on PR #305.
var ErrSOAPFault = errors.New("SOAP fault in GetProtocolInfo response")

// ErrMissingResponseElement is returned when the SOAP envelope
// parses successfully but the Body contains no recognisable
// Response element (vendor returned a bare `<Body/>` or some
// other unexpected shape). Same rationale as ErrSOAPFault —
// surface "renderer is broken" loudly, don't silently treat as
// empty sink.
var ErrMissingResponseElement = errors.New("SOAP body has no GetProtocolInfoResponse element")

func ParseGetProtocolInfoResponse(body []byte) ([]string, error) {
	var env rawSOAPEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parse SOAP envelope: %w", err)
	}
	// Detect SOAP Fault — the inner element name is "Fault" in the
	// SOAP envelope namespace. The xml package's `,any` wildcard
	// captures the local name regardless of the xmlns:s prefix the
	// responder used; EqualFold so a loose renderer that emits a
	// lowercase <s:fault> is still caught rather than masquerading as
	// an empty sink list. (goreview F10)
	if strings.EqualFold(env.Body.Response.XMLName.Local, "Fault") {
		return nil, ErrSOAPFault
	}
	// Detect missing/empty Body element — a renderer returning a
	// shape we don't recognise (e.g. `<Body></Body>` or an
	// element name that doesn't carry a Sink field) should surface
	// as an error rather than mascarading as a successful empty
	// sink list. The wildcard `,any` leaves Response.XMLName.Local
	// empty when no child element was matched.
	if env.Body.Response.XMLName.Local == "" {
		return nil, ErrMissingResponseElement
	}
	raw := strings.TrimSpace(env.Body.Response.Sink)
	if raw == "" {
		// Legitimately empty Sink — renderer responded but
		// advertises no protocols. Degenerate but legal per the
		// UPnP ConnectionManager spec.
		return []string{}, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// ReadResponseBodyCapped reads up to `maxBytes` from `r` and returns
// the bytes. Used by the HTTP fetchers (`fetchDeviceDescription`,
// `fetchGetProtocolInfo`) to defend against a misbehaving renderer
// that returns a multi-MB body — the parsers all expect <100 KB.
//
// Returns an error when the limit is hit (we deliberately do NOT
// silently truncate; truncated XML / SOAP would parse-fail
// confusingly downstream).
func ReadResponseBodyCapped(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("maxBytes must be > 0")
	}
	// Overflow guard: the maxBytes+1 peek below wraps to a negative value at
	// maxBytes == math.MaxInt64. io.LimitReader returns EOF immediately on a
	// negative N, so an overflowed cap would silently yield an EMPTY body
	// instead of the data. Unreachable today (callers pass small constants).
	if maxBytes >= 1<<63-1 {
		return nil, errors.New("maxBytes too large")
	}
	// LimitReader + a peek-past trick: read maxBytes+1; if we got
	// maxBytes+1 bytes, body exceeds the cap.
	lr := io.LimitReader(r, maxBytes+1)
	buf, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > maxBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxBytes)
	}
	return buf, nil
}

// IsHTTPStatusOK returns true for 200 OK and false otherwise.
// Trivial wrapper but extracted so future relaxation (e.g. accept
// 206 for some renderers' GetProtocolInfo) is a single edit.
func IsHTTPStatusOK(status int) bool {
	return status == http.StatusOK
}
