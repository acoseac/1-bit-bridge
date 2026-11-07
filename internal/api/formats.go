package api

// The client format gate at the API (backlog B489). A request declares the
// gated formats it plays in X-Bridge-Formats (formatgate.Parse), and every
// catalog surface serves it that view: the manifest (full, paged and
// delta, its tombstones and its counts), search, the smart playlists, the
// playlist files and /v1/health's tracksIndexed. A request that declares
// none is every app from before WavPack and Monkey's Audio were indexed, and
// none of their rows reaches it. The filesystem views (/v1/list, /v1/stat,
// /v1/read, /v1/download) and the documents a device writes (playlists,
// favorites, history) are not gated: PROTOCOL.md, "Client formats".

import (
	"net/http"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/formatgate"
)

// requestFormats is the view the request declared in X-Bridge-Formats.
func requestFormats(r *http.Request) formatgate.Set {
	return formatgate.Parse(r.Header.Values(formatgate.HeaderName))
}

// varyOnFormats marks a response whose body depends on the formats the
// request declared, so a cache between the client and the bridge keys it on
// X-Bridge-Formats as well as the URL, and a validator one view was given is
// never answered from another view's copy.
func varyOnFormats(h http.Header) { addVary(h, formatgate.HeaderName) }

// addVary adds token to h's Vary, once, keeping the tokens already there.
func addVary(h http.Header, token string) {
	tokens := varyTokens(h)
	for _, t := range tokens {
		if strings.EqualFold(t, token) {
			return
		}
	}
	h.Set("Vary", strings.Join(append(tokens, token), ", "))
}

// dropVary removes token from h's Vary, and the header when nothing is left.
func dropVary(h http.Header, token string) {
	var kept []string
	for _, t := range varyTokens(h) {
		if !strings.EqualFold(t, token) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		h.Del("Vary")
		return
	}
	h.Set("Vary", strings.Join(kept, ", "))
}

// varyTokens are the tokens of h's Vary lines, trimmed, in order.
func varyTokens(h http.Header) []string {
	var out []string
	for _, line := range h.Values("Vary") {
		for _, t := range strings.Split(line, ",") {
			if t = strings.TrimSpace(t); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}
