package upnp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// ContentDirectoryServiceType is the UPnP CDS:1 service type — the
// SOAPAction prefix + envelope xmlns:u for every ContentDirectory call.
const ContentDirectoryServiceType = "urn:schemas-upnp-org:service:ContentDirectory:1"

// browseResponseMaxBytes caps a Browse/Search SOAP response. A page of
// DefaultPageSize items can run large (hundreds of items × ~1 KB DIDL),
// so discovery's 64 KiB SOAP cap (sized for the tiny GetProtocolInfo
// Sink list) is too small here. 8 MiB covers any realistic page while
// still bounding a misbehaving server.
const browseResponseMaxBytes int64 = 8 * 1024 * 1024

// ContentDirectoryClient issues UPnP ContentDirectory control-point
// calls (Browse / Search / GetSystemUpdateID) to an upstream MediaServer
// and parses the DIDL-Lite results. Reuses discovery.SOAPDispatcher for
// the HTTP plumbing (test-injectable).
//
// Stateless + safe for concurrent use across DIFFERENT servers, but a
// single upstream MiniDLNA MUST be browsed SERIALLY — its libmicrohttpd
// pool is tiny and parallel Browse bursts cause socket timeouts that
// stall playback (the ingest layer enforces this).
type ContentDirectoryClient struct {
	dispatcher discovery.SOAPDispatcher
}

// NewContentDirectoryClient builds a client over the given dispatcher.
// Production passes a discovery.HTTPClientDispatcher wrapping a tuned
// *http.Client; tests pass a stub.
func NewContentDirectoryClient(dispatcher discovery.SOAPDispatcher) *ContentDirectoryClient {
	return &ContentDirectoryClient{dispatcher: dispatcher}
}

// Browse issues a single Browse call (one page). browseFlag is typically
// "BrowseDirectChildren" (children of objectID) or "BrowseMetadata"
// (objectID itself). filter "*" requests all metadata.
func (c *ContentDirectoryClient) Browse(
	ctx context.Context,
	controlURL, objectID, browseFlag, filter string,
	startingIndex, requestedCount int,
	sortCriteria string,
) (BrowseResult, error) {
	env := buildBrowseEnvelope(objectID, browseFlag, filter, startingIndex, requestedCount, sortCriteria)
	body, err := c.invoke(ctx, controlURL, "Browse", env)
	if err != nil {
		return BrowseResult{}, err
	}
	return ParseBrowseResponse(body)
}

// Search issues a single Search call (one page) against a container.
// searchCriteria is a UPnP SearchCriteria expression (e.g.
// `upnp:artist = "X" and dc:title = "Y"`).
func (c *ContentDirectoryClient) Search(
	ctx context.Context,
	controlURL, containerID, searchCriteria, filter string,
	startingIndex, requestedCount int,
	sortCriteria string,
) (BrowseResult, error) {
	env := buildSearchEnvelope(containerID, searchCriteria, filter, startingIndex, requestedCount, sortCriteria)
	body, err := c.invoke(ctx, controlURL, "Search", env)
	if err != nil {
		return BrowseResult{}, err
	}
	return ParseBrowseResponse(body)
}

// BrowseAll paginates Browse(BrowseDirectChildren) over a container,
// accumulating its direct-child containers + items. Serial by
// construction (one page at a time). Terminates per NextStartingIndex
// (empty page = EOF) and the MaxBrowseAllItems hard ceiling. ctx
// cancellation is honored between pages.
func (c *ContentDirectoryClient) BrowseAll(
	ctx context.Context,
	controlURL, objectID string,
) (containers []Container, items []Object, err error) {
	start := 0
	for {
		if err = ctx.Err(); err != nil {
			return nil, nil, err
		}
		page, perr := c.Browse(ctx, controlURL, objectID, "BrowseDirectChildren", "*", start, DefaultPageSize, "")
		if perr != nil {
			return nil, nil, perr
		}
		containers = append(containers, page.Containers...)
		items = append(items, page.Items...)
		next, more := NextStartingIndex(start, page.NumberReturned, page.TotalMatches)
		// Distinguish "natural EOF" from "hit our safety ceiling": only
		// the latter is a truncation the caller must not trust as the
		// complete view of the container.
		if len(containers)+len(items) >= maxBrowseAllItemsForTesting && more {
			return containers, items, ErrBrowseLimit
		}
		if !more {
			break
		}
		start = next
	}
	return containers, items, nil
}

// GetSystemUpdateID returns the server's ContentDirectory SystemUpdateID
// — used to skip a re-walk when the upstream library is unchanged.
// Returned verbatim (callers compare against a stored value); MiniDLNA
// may legitimately return "0", which the ingest layer treats as
// "untrusted, fall back to a time-based walk".
func (c *ContentDirectoryClient) GetSystemUpdateID(ctx context.Context, controlURL string) (string, error) {
	body, err := c.invoke(ctx, controlURL, "GetSystemUpdateID", buildGetSystemUpdateIDEnvelope())
	if err != nil {
		return "", err
	}
	return parseSystemUpdateID(body)
}

// invoke POSTs a SOAP envelope to the ContentDirectory control URL and
// returns the (capped) response body. Mirrors
// discovery.FetchGetProtocolInfo's request shape + defensive nil-guards.
// A 500 (SOAP fault) is read through so ParseBrowseResponse can surface
// the typed ErrSOAPFault; other non-200 statuses are hard errors.
func (c *ContentDirectoryClient) invoke(ctx context.Context, controlURL, action string, soapBody []byte) ([]byte, error) {
	if c == nil || c.dispatcher == nil {
		return nil, errors.New("upnp: nil dispatcher")
	}
	if strings.TrimSpace(controlURL) == "" {
		return nil, errors.New("upnp: empty ContentDirectory controlURL")
	}
	// NewRequestWithContext so cancellation/deadlines propagate via
	// req.Context() too — the dispatcher path already passes ctx, but
	// binding it on the request is the idiomatic Go shape and protects
	// any downstream middleware that consults req.Context().
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controlURL, bytes.NewReader(soapBody))
	if err != nil {
		return nil, fmt.Errorf("upnp: build POST request: %w", err)
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+ContentDirectoryServiceType+"#"+action+`"`)
	req.Header.Set("User-Agent", "1-bit-bridge/upnp")

	resp, err := c.dispatcher.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("upnp: POST %s: %w", controlURL, err)
	}
	if resp == nil {
		return nil, errors.New("upnp: dispatcher returned nil response without error")
	}
	// Body-nil check BEFORE the defer — a nil resp.Body panics on Close.
	if resp.Body == nil {
		return nil, errors.New("upnp: dispatcher returned response with nil Body")
	}
	defer func() { _ = resp.Body.Close() }()

	if !discovery.IsHTTPStatusOK(resp.StatusCode) && resp.StatusCode != http.StatusInternalServerError {
		return nil, fmt.Errorf("upnp: POST %s: status %d", controlURL, resp.StatusCode)
	}
	body, err := discovery.ReadResponseBodyCapped(resp.Body, browseResponseMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("upnp: read body: %w", err)
	}
	return body, nil
}

// --- SOAP request envelope builders ---

const soapHeader = `<?xml version="1.0" encoding="utf-8"?>` +
	`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
	`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`

const soapFooter = `</s:Body></s:Envelope>`

func buildBrowseEnvelope(objectID, browseFlag, filter string, startingIndex, requestedCount int, sortCriteria string) []byte {
	var b bytes.Buffer
	b.WriteString(soapHeader)
	b.WriteString(`<u:Browse xmlns:u="` + ContentDirectoryServiceType + `">`)
	writeArg(&b, "ObjectID", objectID)
	writeArg(&b, "BrowseFlag", browseFlag)
	writeArg(&b, "Filter", filter)
	fmt.Fprintf(&b, "<StartingIndex>%d</StartingIndex>", startingIndex)
	fmt.Fprintf(&b, "<RequestedCount>%d</RequestedCount>", requestedCount)
	writeArg(&b, "SortCriteria", sortCriteria)
	b.WriteString(`</u:Browse>`)
	b.WriteString(soapFooter)
	return b.Bytes()
}

func buildSearchEnvelope(containerID, searchCriteria, filter string, startingIndex, requestedCount int, sortCriteria string) []byte {
	var b bytes.Buffer
	b.WriteString(soapHeader)
	b.WriteString(`<u:Search xmlns:u="` + ContentDirectoryServiceType + `">`)
	writeArg(&b, "ContainerID", containerID)
	writeArg(&b, "SearchCriteria", searchCriteria)
	writeArg(&b, "Filter", filter)
	fmt.Fprintf(&b, "<StartingIndex>%d</StartingIndex>", startingIndex)
	fmt.Fprintf(&b, "<RequestedCount>%d</RequestedCount>", requestedCount)
	writeArg(&b, "SortCriteria", sortCriteria)
	b.WriteString(`</u:Search>`)
	b.WriteString(soapFooter)
	return b.Bytes()
}

// getSystemUpdateIDEnvelope is the fixed SOAP body for GetSystemUpdateID (no
// per-call args), built once instead of re-allocated per poll. The returned
// slice is SHARED and MUST NOT be mutated; the only caller wraps it in a
// read-only bytes.NewReader.
var getSystemUpdateIDEnvelope = []byte(soapHeader +
	`<u:GetSystemUpdateID xmlns:u="` + ContentDirectoryServiceType + `"/>` +
	soapFooter)

func buildGetSystemUpdateIDEnvelope() []byte {
	return getSystemUpdateIDEnvelope
}

// writeArg writes <name>escaped(val)</name>. Argument values are
// XML-escaped (ObjectIDs are safe ASCII but a SearchCriteria carries
// quotes — escape defensively so the envelope is always valid).
func writeArg(b *bytes.Buffer, name, val string) {
	b.WriteString("<" + name + ">")
	b.WriteString(escapeXMLArg(val))
	b.WriteString("</" + name + ">")
}

// escapeXMLArg escapes a SOAP argument value using NAMED XML entities.
// MiniDLNA's rigid SearchCriteria parser rejects NUMERIC character
// references — encoding/xml's xml.EscapeText emits `&#34;` for a quote,
// which the 2Go answers with a 708 InvalidSearchCriteria fault, whereas
// the named `&quot;` returns results (A/B-confirmed against the live
// device). Mirrors the iOS SOAPEnvelopeBuilder.escapeXML named-entity set.
func escapeXMLArg(s string) string {
	// Fast path: the vast majority of titles/artists/IDs carry none of
	// the five escapable runes, so skip the Builder allocation + copy
	// entirely and return the input unchanged. Gemini r4.
	if !strings.ContainsAny(s, "&<>\"'") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case '\'':
			b.WriteString("&apos;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
