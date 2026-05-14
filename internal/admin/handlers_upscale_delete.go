// Package admin — DELETE /api/upscale/variants handler.
//
// Admin-console mirror of the public `DELETE /v1/upscale/variants`
// endpoint, registered at the admin port (loopback-only, no bearer
// token; CSRF guard via Content-Type + Origin validation in
// `csrfGuard`). Shares the api package's `RunVariantDelete` through
// an adapter wired in `cmd/bridge/main.go`, so the admin UI and
// the iOS app converge on exactly one list/unlink/DB-delete/SSE
// path. The shared path is what guarantees the
// `upscale.deleted` SSE event fans out identically whether the
// operator clicks Delete in the admin console or the user clicks
// the iOS app's Bridge upscale management Section — paired clients
// reconcile their local state regardless of origin.
//
// Three query-param shapes (mirror of the public endpoint):
//
//	DELETE /api/upscale/variants?confirm=true        → every variant
//	DELETE /api/upscale/variants?prefix=<rel-path>   → variants under a path prefix
//	DELETE /api/upscale/variants?path=<rel-path>     → variants for one exact source path
//
// The unscoped form's `?confirm=true` gate is enforced by the
// shared parser; the admin UI additionally requires a typed-phrase
// modal confirmation on the "Clear all variants" button so
// accidental clicks on the settings page can't wipe the cache in
// one tap (matches the `bridge artwork --gc --confirm GC-ARTWORK`
// CLI convention).

package admin

import (
	"errors"
	"net/http"
	"path"
	"strings"
)

// apiUpscaleVariantsDelete handles DELETE /api/upscale/variants.
// Mirrors `apiUpscaleBatchCancel`'s shape: nil-deps short-circuit
// to 503, then defer to the wired adapter. The adapter
// (cmd/bridge/main.go's `adminVariantDeleterAdapter`) wraps
// `api.Server.RunVariantDelete` so the destructive path is shared
// with the public endpoint — the admin console adds the UI but
// never duplicates the underlying delete loop.
func (s *Server) apiUpscaleVariantsDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.VariantDeleter == nil {
		writeError(w, http.StatusServiceUnavailable, errCodeUpscaleDisabled,
			errMsgUpscalingNotEnabled)
		return
	}

	req, errCode, errMsg := parseVariantDeleteRequest(r.URL.Query())
	if errCode != "" {
		writeError(w, http.StatusBadRequest, errCode, errMsg)
		return
	}

	resp, err := s.deps.VariantDeleter.Delete(r.Context(), req)
	if err != nil {
		if errors.Is(err, ErrAdminVariantDeleterUnavailable) {
			// Deleter was wired at admin-Server construction but
			// the underlying api.Server lost it mid-life — same
			// surface as the nil-deps case at the top, distinct
			// only in that we got here via the adapter rather
			// than the short-circuit. Keep the response shape
			// stable so the JS handler doesn't fork.
			writeError(w, http.StatusServiceUnavailable, errCodeUpscaleDisabled,
				errMsgUpscalingNotEnabled)
			return
		}
		// SQLite enumeration failure during the list phase, or
		// any other adapter-layer error. The shared loop logs
		// per-row failures itself; this branch is the bulk-list
		// path failing entirely.
		writeError(w, http.StatusInternalServerError, "delete-failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

// parseVariantDeleteRequest validates the three mutually-exclusive
// query-param shapes and returns a normalised
// `AdminVariantDeleteRequest`. The error code on rejection matches
// the public endpoint's wire shape (`bad_request`) so the JS
// fetch handler can render the JSON error body uniformly.
//
// Mirrors `api.parseVariantDeleteQuery` but stays in the admin
// package so the admin package compiles without importing the api
// package — same decoupling rule the `AdminBatchCoordinator`
// interface follows. The semantics MUST stay in sync; the
// adapter in cmd/bridge translates between the two
// `*VariantDeleteRequest` types and the downstream
// `api.RunVariantDelete` re-validates against `validateRelativePath`,
// so any drift in this parser would surface as a 500 from the
// adapter rather than letting an unsafe path reach the deleter
// (defense in depth).
func parseVariantDeleteRequest(q map[string][]string) (req AdminVariantDeleteRequest, errCode string, errMsg string) {
	get := func(k string) string {
		if vs := q[k]; len(vs) > 0 {
			return vs[0]
		}
		return ""
	}
	has := func(k string) bool { _, ok := q[k]; return ok }

	hasPrefix := has("prefix")
	hasPath := has("path")
	prefix := strings.TrimSpace(get("prefix"))
	pathParam := strings.TrimSpace(get("path"))
	confirm := strings.EqualFold(get("confirm"), "true")

	if hasPrefix && hasPath {
		return req, "bad_request",
			"cannot combine `prefix` and `path` query parameters; pick one"
	}

	if !hasPrefix && !hasPath && !confirm {
		return req, "bad_request",
			"deleting all variants requires `?confirm=true` to be set explicitly"
	}

	if hasPrefix {
		v, ok := validateAdminRelativePath(prefix)
		if !ok {
			return req, "bad_request",
				"`prefix` must be a clean relative path (no leading `/`, no `..`)"
		}
		req.Prefix = v
		return req, "", ""
	}
	if hasPath {
		v, ok := validateAdminRelativePath(pathParam)
		if !ok {
			return req, "bad_request",
				"`path` must be a clean relative path (no leading `/`, no `..`)"
		}
		req.Path = v
		return req, "", ""
	}
	req.All = true
	return req, "", ""
}

// validateAdminRelativePath enforces the same "no leading `/`,
// no `..`, no `\` separator, must round-trip through path.Clean"
// contract as `api.validateRelativePath`. Defense in depth: the
// downstream `api.RunVariantDelete` re-validates, so a transient
// skew between the two implementations would surface as a 500
// from the adapter rather than letting an unsafe path reach the
// deleter.
func validateAdminRelativePath(p string) (string, bool) {
	if p == "" {
		return "", false
	}
	if strings.HasPrefix(p, "/") {
		return "", false
	}
	if strings.Contains(p, `\`) {
		return "", false
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != p {
		return "", false
	}
	return cleaned, true
}
