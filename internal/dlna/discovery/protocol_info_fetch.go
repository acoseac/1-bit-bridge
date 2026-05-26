package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
)

// DefaultDescriptionMaxBytes caps the body size read from a renderer's
// /description.xml. 256 KiB covers every real-world device description
// observed in Phase 0 (typically <8 KB; vendor extension chunks push
// 30-40 KB). Defensive against a misbehaving renderer streaming MB of
// garbage and pinning bridge memory.
const DefaultDescriptionMaxBytes int64 = 256 * 1024

// DefaultSOAPMaxBytes caps the body size read from a renderer's
// GetProtocolInfo SOAP response. 64 KiB covers every observed
// renderer's Sink list (typically <2 KB).
const DefaultSOAPMaxBytes int64 = 64 * 1024

// SOAPDispatcher is the abstraction over `http.Client` used by the
// fetchers. The default implementation wraps `http.DefaultClient`;
// tests inject a stub that returns canned responses without standing
// up an HTTP server.
//
// **Why an interface, not a function**: a future PR may want to share
// connection pooling / TLS config across multiple fetchers; the
// interface form lets the orchestrator hold a single dispatcher and
// pass it to both the description fetcher AND the SOAP fetcher.
type SOAPDispatcher interface {
	// Do executes the request and returns the response. Caller is
	// responsible for closing resp.Body.
	Do(ctx context.Context, req *http.Request) (*http.Response, error)
}

// HTTPClientDispatcher is the production implementation backed by
// an *http.Client. Caller supplies the client so timeouts /
// transport-level config are tunable at construction time.
type HTTPClientDispatcher struct {
	Client *http.Client
}

// Do implements SOAPDispatcher. Threads the context into the
// request so cancellation propagates.
func (d *HTTPClientDispatcher) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if d == nil || d.Client == nil {
		return nil, errors.New("HTTPClientDispatcher has no http.Client")
	}
	return d.Client.Do(req.WithContext(ctx))
}

// FetchDeviceDescription fetches + parses the renderer's
// /description.xml at the given URL. Returns the parsed
// `DeviceDescription` on success.
//
// Failure modes:
//   - Network error (renderer offline, DNS fail) → wrapped error
//   - Non-200 HTTP status → error with status code
//   - Body exceeds `DefaultDescriptionMaxBytes` → error
//   - XML parse failure / missing AVTransport → error from
//     ParseDeviceDescription
//
// Caller (typically `SSDPDiscoveryClient`) logs the failure at
// `.notice` level and skips populating the cache entry — the next
// M-SEARCH cycle gets another chance to fetch.
func FetchDeviceDescription(
	ctx context.Context,
	dispatcher SOAPDispatcher,
	url string,
) (DeviceDescription, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return DeviceDescription{}, fmt.Errorf("build GET request: %w", err)
	}
	// Identify ourselves so renderer logs show "bridge discovery"
	// rather than an opaque user-agent — operator-debug friendliness.
	req.Header.Set("User-Agent", "1-bit-bridge/discovery")
	resp, err := dispatcher.Do(ctx, req)
	if err != nil {
		return DeviceDescription{}, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !IsHTTPStatusOK(resp.StatusCode) {
		return DeviceDescription{}, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	body, err := ReadResponseBodyCapped(resp.Body, DefaultDescriptionMaxBytes)
	if err != nil {
		return DeviceDescription{}, fmt.Errorf("read body: %w", err)
	}
	return ParseDeviceDescription(body, url)
}

// FetchGetProtocolInfo dispatches a SOAP `GetProtocolInfo` call to
// the renderer's ConnectionManager controlURL + returns the parsed
// Sink protocolInfo list. Returns an empty slice (NOT nil) on
// success with no advertised protocols.
//
// Failure modes:
//   - Network / status / body-cap errors as above.
//   - Renderer returns a SOAP fault → propagated as parse error.
//   - ConnectionManager absent on the renderer (caller checks
//     `desc.Services[ServiceConnectionManager]` first; this fetcher
//     trusts the caller's preflight).
func FetchGetProtocolInfo(
	ctx context.Context,
	dispatcher SOAPDispatcher,
	connectionManagerControlURL string,
) ([]string, error) {
	if connectionManagerControlURL == "" {
		return nil, errors.New("empty ConnectionManager controlURL")
	}
	soapBody := buildGetProtocolInfoEnvelope()
	req, err := http.NewRequest(
		http.MethodPost,
		connectionManagerControlURL,
		bytes.NewReader(soapBody),
	)
	if err != nil {
		return nil, fmt.Errorf("build POST request: %w", err)
	}
	// SOAP-required headers + our identifier.
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set(
		"SOAPAction",
		`"urn:schemas-upnp-org:service:ConnectionManager:1#GetProtocolInfo"`,
	)
	req.Header.Set("User-Agent", "1-bit-bridge/discovery")
	resp, err := dispatcher.Do(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", connectionManagerControlURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !IsHTTPStatusOK(resp.StatusCode) {
		return nil, fmt.Errorf(
			"POST %s: status %d",
			connectionManagerControlURL,
			resp.StatusCode,
		)
	}
	body, err := ReadResponseBodyCapped(resp.Body, DefaultSOAPMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return ParseGetProtocolInfoResponse(body)
}

// buildGetProtocolInfoEnvelope assembles the SOAP request body for
// `GetProtocolInfo`. The envelope is fixed (no per-call parameters
// — GetProtocolInfo takes no inputs), so the body bytes are
// effectively a constant. Defined as a function rather than a
// `const` byte slice for clarity at the call site.
func buildGetProtocolInfoEnvelope() []byte {
	return []byte(`<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body>` +
		`<u:GetProtocolInfo xmlns:u="urn:schemas-upnp-org:service:ConnectionManager:1"/>` +
		`</s:Body>` +
		`</s:Envelope>`)
}
