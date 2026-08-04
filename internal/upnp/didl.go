package upnp

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ErrSOAPFault is returned when a ContentDirectory response body carries
// a SOAP <Fault> — distinguishes "server rejected the action" (e.g. a
// stale/unknown ObjectID → 701 NoSuchObject) from a transport error or a
// legitimately empty result.
var ErrSOAPFault = errors.New("upnp: SOAP fault in ContentDirectory response")

// ErrMissingResponseElement is returned when the SOAP envelope parses but
// the Body has no recognizable response element — surface a broken server
// loudly rather than masquerading it as an empty result.
var ErrMissingResponseElement = errors.New("upnp: SOAP body has no recognizable response element")

// ErrXMLTooDeep is returned when an untrusted XML payload's element nesting
// exceeds maxXMLDepth — rejected before the recursive decode runs.
var ErrXMLTooDeep = errors.New("upnp: XML nesting depth exceeds limit")

// maxXMLDepth bounds the element nesting depth the untrusted DIDL-Lite + SOAP
// envelope parsers accept. encoding/xml already self-limits recursion at its
// own maxUnmarshalDepth (10000), but a payload nested that deep — reachable
// within the 8 MiB body cap a hostile/compromised LAN media server could send
// — still spikes the goroutine stack + CPU on the way to that limit. Real
// ContentDirectory responses nest only a handful of levels
// (Envelope>Body>Response>Result, then DIDL-Lite>container/item>res); a few
// hundred is orders-of-magnitude headroom while bounding the worst case far
// below the stdlib backstop.
const maxXMLDepth = 256

// verifyXMLDepth scans the token stream ITERATIVELY (xml.Decoder.Token never
// recurses per nesting level, so this pass can't itself overflow) and rejects
// input whose element nesting exceeds maxDepth BEFORE the recursive
// xml.Unmarshal / Decoder.Decode runs. Well-formed input passes through
// untouched, so downstream parse results are identical; a tokenizer error is
// swallowed here (returns nil) so the real decoder surfaces its canonical
// parse error rather than this pass masking it.
func verifyXMLDepth(r io.Reader, maxDepth int) error {
	dec := xml.NewDecoder(r)
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil // EOF or malformed — let the real decode produce the verdict
		}
		switch tok.(type) {
		case xml.StartElement:
			depth++
			if depth > maxDepth {
				return ErrXMLTooDeep
			}
		case xml.EndElement:
			// Clamp at zero: an unmatched EndElement must never drive the
			// counter negative, or a following deeply-nested structure would
			// need maxDepth + |negative| opens to trip the guard — a bypass.
			// Strict-mode Token() already errors on a mismatched end tag (so
			// this isn't reachable today), but the clamp costs nothing and
			// keeps the bound sound if Strict is ever relaxed.
			if depth > 0 {
				depth--
			}
		}
	}
}

// Container is a <container> from a Browse result — a folder / Album /
// Artist node. IDs are server-defined + opaque (position-based on
// MiniDLNA); treat them as volatile locators, not stable identity.
type Container struct {
	ID         string
	ParentID   string
	Title      string
	ChildCount int // -1 when the childCount attribute is absent
	Class      string
}

// Object is an <item> (track) from a Browse result, carrying the <res>
// locator + the metadata an ingest needs without a per-file read.
type Object struct {
	ID            string
	ParentID      string
	Title         string
	Artist        string
	Album         string
	Creator       string
	Genre         string
	Class         string
	Date          string // dc:date (e.g. "2019-01-01")
	TrackNumber   int    // upnp:originalTrackNumber, 0 when absent
	Res           string // <res> element text — the file URL (raw, un-escaped)
	ProtocolInfo  string // <res protocolInfo>
	Size          int64  // <res size>, 0 when absent
	Duration      string // raw "H:MM:SS.mmm" from <res duration>
	SampleRate    int    // <res sampleFrequency>
	BitsPerSample int    // <res bitsPerSample>
	Channels      int    // <res nrAudioChannels>
	AlbumArtURI   string // upnp:albumArtURI
}

// BrowseResult is a parsed Browse/Search page: containers + items plus
// the count fields the pager needs.
type BrowseResult struct {
	Containers     []Container
	Items          []Object
	NumberReturned int
	TotalMatches   int
}

// --- SOAP envelope unmarshaling targets ---

// soapBrowseEnvelope matches a Browse/Search SOAP response. The `,any`
// wildcard on Response matches <BrowseResponse>/<SearchResponse>
// regardless of the xmlns:u prefix AND captures <Fault> (local name
// "Fault") for fault detection. Count fields are kept as strings +
// parsed leniently — some servers pad numbers with whitespace, which a
// direct `int` field would reject and fail the whole unmarshal.
type soapBrowseEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		Response struct {
			XMLName        xml.Name
			Result         string `xml:"Result"`
			NumberReturned string `xml:"NumberReturned"`
			TotalMatches   string `xml:"TotalMatches"`
		} `xml:",any"`
	} `xml:"Body"`
}

// soapSystemUpdateIDEnvelope matches a GetSystemUpdateID response.
type soapSystemUpdateIDEnvelope struct {
	XMLName xml.Name `xml:"Envelope"`
	Body    struct {
		Response struct {
			XMLName xml.Name
			ID      string `xml:"Id"`
		} `xml:",any"`
	} `xml:"Body"`
}

// --- DIDL-Lite unmarshaling targets ---
//
// encoding/xml matches elements + attributes by LOCAL name, ignoring the
// namespace prefix unless a tag specifies one — so `xml:"title"` matches
// <dc:title>, `xml:"class"` matches <upnp:class>, etc. (Same approach as
// internal/dlna/discovery's device-description parser, proven on real
// device XML.)

type didlLite struct {
	XMLName    xml.Name        `xml:"DIDL-Lite"`
	Containers []didlContainer `xml:"container"`
	Items      []didlItem      `xml:"item"`
}

type didlContainer struct {
	ID         string `xml:"id,attr"`
	ParentID   string `xml:"parentID,attr"`
	ChildCount string `xml:"childCount,attr"`
	Title      string `xml:"title"`
	Class      string `xml:"class"`
}

type didlItem struct {
	ID          string    `xml:"id,attr"`
	ParentID    string    `xml:"parentID,attr"`
	Title       string    `xml:"title"`
	Artist      string    `xml:"artist"`
	Album       string    `xml:"album"`
	Creator     string    `xml:"creator"`
	Genre       string    `xml:"genre"`
	Class       string    `xml:"class"`
	Date        string    `xml:"date"`
	TrackNumber string    `xml:"originalTrackNumber"`
	AlbumArtURI string    `xml:"albumArtURI"`
	Res         []didlRes `xml:"res"`
}

type didlRes struct {
	ProtocolInfo  string `xml:"protocolInfo,attr"`
	Size          string `xml:"size,attr"`
	Duration      string `xml:"duration,attr"`
	SampleFreq    string `xml:"sampleFrequency,attr"`
	BitsPerSample string `xml:"bitsPerSample,attr"`
	Channels      string `xml:"nrAudioChannels,attr"`
	URL           string `xml:",chardata"`
}

// ParseBrowseResponse parses a ContentDirectory Browse/Search SOAP
// response body into a BrowseResult. The <Result> element carries the
// DIDL-Lite document entity-escaped as a SOAP string value; the XML
// decoder un-escapes it on this first pass, then parseDIDL parses it.
// (Element values inside the DIDL that were themselves escaped — e.g. a
// title containing '&' — are un-escaped on the second pass, so the two
// passes compose correctly.)
func ParseBrowseResponse(body []byte) (BrowseResult, error) {
	// Bound nesting depth before the recursive unmarshal (defense-in-depth
	// against a hostile upstream — see verifyXMLDepth). The DIDL inside
	// <Result> is entity-escaped here (scanned as CharData), so it's bounded
	// separately by parseDIDL's own check after un-escaping.
	if err := verifyXMLDepth(bytes.NewReader(body), maxXMLDepth); err != nil {
		return BrowseResult{}, fmt.Errorf("upnp: parse SOAP envelope: %w", err)
	}
	var env soapBrowseEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		return BrowseResult{}, fmt.Errorf("upnp: parse SOAP envelope: %w", err)
	}
	// EqualFold so a loose server that emits a lowercase <s:fault> is still
	// caught rather than masquerading as an empty result. Matches the sibling
	// internal/dlna/discovery.ParseGetProtocolInfoResponse form.
	local := env.Body.Response.XMLName.Local
	if strings.EqualFold(local, "Fault") {
		return BrowseResult{}, ErrSOAPFault
	}
	if local == "" {
		return BrowseResult{}, ErrMissingResponseElement
	}
	res, err := parseDIDL(env.Body.Response.Result)
	if err != nil {
		return BrowseResult{}, err
	}
	res.NumberReturned = atoiOr(env.Body.Response.NumberReturned, 0)
	res.TotalMatches = atoiOr(env.Body.Response.TotalMatches, 0)
	return res, nil
}

// parseDIDL parses an (already un-escaped) DIDL-Lite document into its
// containers + items. An empty/whitespace document yields an empty
// result (legal: a Browse of an empty container).
func parseDIDL(didl string) (BrowseResult, error) {
	if strings.TrimSpace(didl) == "" {
		return BrowseResult{Containers: []Container{}, Items: []Object{}}, nil
	}
	// Bound nesting depth before the recursive decode (defense-in-depth
	// against a hostile upstream — see verifyXMLDepth).
	if err := verifyXMLDepth(strings.NewReader(didl), maxXMLDepth); err != nil {
		return BrowseResult{}, fmt.Errorf("upnp: parse DIDL-Lite: %w", err)
	}
	var doc didlLite
	// Decode straight from the string via strings.NewReader to avoid a
	// []byte(didl) copy — a Browse page can run up to browseResponseMaxBytes
	// (8 MiB), so duplicating the full payload would burn allocator time
	// on every ingest sweep.
	if err := xml.NewDecoder(strings.NewReader(didl)).Decode(&doc); err != nil {
		return BrowseResult{}, fmt.Errorf("upnp: parse DIDL-Lite: %w", err)
	}
	out := BrowseResult{
		Containers: make([]Container, 0, len(doc.Containers)),
		Items:      make([]Object, 0, len(doc.Items)),
	}
	for _, c := range doc.Containers {
		out.Containers = append(out.Containers, Container{
			ID:         strings.TrimSpace(c.ID),
			ParentID:   strings.TrimSpace(c.ParentID),
			Title:      strings.TrimSpace(c.Title),
			ChildCount: atoiOr(c.ChildCount, -1),
			Class:      strings.TrimSpace(c.Class),
		})
	}
	for _, it := range doc.Items {
		obj := Object{
			ID:          strings.TrimSpace(it.ID),
			ParentID:    strings.TrimSpace(it.ParentID),
			Title:       strings.TrimSpace(it.Title),
			Artist:      strings.TrimSpace(it.Artist),
			Album:       strings.TrimSpace(it.Album),
			Creator:     strings.TrimSpace(it.Creator),
			Genre:       strings.TrimSpace(it.Genre),
			Class:       strings.TrimSpace(it.Class),
			Date:        strings.TrimSpace(it.Date),
			TrackNumber: atoiTrackNumber(it.TrackNumber, 0),
			AlbumArtURI: strings.TrimSpace(it.AlbumArtURI),
		}
		if r, ok := pickAudioRes(it.Res); ok {
			obj.Res = strings.TrimSpace(r.URL)
			obj.ProtocolInfo = strings.TrimSpace(r.ProtocolInfo)
			obj.Size = atoi64Or(r.Size, 0)
			obj.Duration = strings.TrimSpace(r.Duration)
			obj.SampleRate = atoiOr(r.SampleFreq, 0)
			obj.BitsPerSample = atoiOr(r.BitsPerSample, 0)
			obj.Channels = atoiOr(r.Channels, 0)
		}
		out.Items = append(out.Items, obj)
	}
	return out, nil
}

// parseSystemUpdateID extracts the <Id> from a GetSystemUpdateID
// response. Returned verbatim — callers compare against a stored value;
// MiniDLNA may legitimately return "0".
func parseSystemUpdateID(body []byte) (string, error) {
	// Bound nesting depth before the recursive unmarshal (defense-in-depth
	// against a hostile upstream — see verifyXMLDepth).
	if err := verifyXMLDepth(bytes.NewReader(body), maxXMLDepth); err != nil {
		return "", fmt.Errorf("upnp: parse GetSystemUpdateID: %w", err)
	}
	var env soapSystemUpdateIDEnvelope
	if err := xml.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("upnp: parse GetSystemUpdateID: %w", err)
	}
	// Match ParseBrowseResponse's response-discrimination: surface a
	// broken server (no Fault, no Response element) loudly instead of
	// returning "" as if it were a valid "unknown ID". EqualFold so a
	// lowercase <s:fault> is still caught.
	local := env.Body.Response.XMLName.Local
	if strings.EqualFold(local, "Fault") {
		return "", ErrSOAPFault
	}
	if local == "" {
		return "", ErrMissingResponseElement
	}
	return strings.TrimSpace(env.Body.Response.ID), nil
}

// pickAudioRes selects the <res> to use for a track. An item can carry
// multiple <res> (transcode profiles); prefer one whose protocolInfo is
// an audio MIME, else the first with a non-empty URL.
func pickAudioRes(res []didlRes) (didlRes, bool) {
	var fallback *didlRes
	for i := range res {
		if strings.TrimSpace(res[i].URL) == "" {
			continue
		}
		if fallback == nil {
			fallback = &res[i]
		}
		if strings.Contains(strings.ToLower(res[i].ProtocolInfo), ":audio/") {
			return res[i], true
		}
	}
	if fallback != nil {
		return *fallback, true
	}
	return didlRes{}, false
}

func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}

// atoiTrackNumber parses `upnp:originalTrackNumber`, tolerating the
// non-conformant "N/M" form some third-party servers emit.
//
// The spec types this element as xsd:int and our own DIDL writer emits a bare
// %d, as does MiniDLNA — so this is defence against an arbitrary upstream, not
// a bug in anything we produce. But `strconv.Atoi("5/12")` fails, and atoiOr's
// zero default then silently strips a perfectly good track number: the ingest
// leaves Track.TrackNumber nil and the walker drops its "%02d - " filename
// prefix.
//
// Deliberately NOT folded into atoiOr. Its other call sites are
// NumberReturned / TotalMatches / ChildCount / sampleFrequency /
// bitsPerSample / nrAudioChannels, where a '/' is genuinely malformed and
// should stay a parse failure rather than being silently truncated.
func atoiTrackNumber(s string, def int) int {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}

func atoi64Or(s string, def int64) int64 {
	if v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
		return v
	}
	return def
}
