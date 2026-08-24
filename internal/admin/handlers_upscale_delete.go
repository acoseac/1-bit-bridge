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
// Plus two admin-only IDENTITY shapes, which the public endpoint has
// no equivalent for because it has no library catalog to expand against:
//
//	DELETE /api/upscale/variants?albumId=<16-hex>    → variants for one album's tracks
//	DELETE /api/upscale/variants?artistId=<16-hex>   → variants for one artist's tracks
//
// These exist because an album is a SET of tracks, not a subtree: its
// directory is the common ancestor of its tracks and is routinely
// shared with other albums, so `?prefix=` would reclaim the
// neighbours' sidecars. Ids stay short on the wire where a 3,000-track
// artist's path list would not. See variantScope.
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
	"net/url"
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
	// Identity shapes carry an id, not paths; expand it here so the
	// deleter sees the same exact-path set the submit endpoint would.
	// The parser has already rejected a present-but-blank identity
	// parameter, so reaching here means there is something to expand.
	if scopeReq, present := identityParams(r.URL.Query()); present {
		scope, scopeErr := s.resolveVariantScope(r, scopeReq)
		if scopeErr != nil {
			writeError(w, scopeErr.Status, scopeErr.Code, scopeErr.Message)
			return
		}
		if len(scope.Paths) == 0 {
			// Defensive. A catalog album always has tracks and an
			// artist always has albums, so this is unreachable today —
			// but the alternative to handling it is passing a
			// SHAPE-LESS request down, and the shape-less request is
			// the one that means "delete every variant in the
			// manifest". RunVariantDelete's own guard would refuse it,
			// but a 500 from a guard is not the answer this deserves:
			// the request was well-formed and its post-condition
			// already holds.
			writeJSON(w, http.StatusOK, AdminVariantDeleteResponse{DeletedPaths: []string{}})
			return
		}
		req.Paths = scope.Paths
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
	identity, hasIdentity := identityParams(q)
	prefix := strings.TrimSpace(get("prefix"))
	pathParam := strings.TrimSpace(get("path"))
	confirm := strings.EqualFold(get("confirm"), "true")
	// Kind narrows the delete to one variant kind. Empty preserves
	// pre-feature behaviour (deletes BOTH kinds matching the path
	// scope). Mirrors api.parseVariantDeleteQuery's contract — both
	// parsers MUST stay in lockstep; the downstream RunVariantDelete
	// only filters once, and a mismatch here would either reject a
	// valid kind or accept an unknown one.
	kind := strings.ToLower(strings.TrimSpace(get("kind")))
	switch kind {
	case "", "upscale", "optimize":
		req.Kind = kind
	default:
		return req, "bad_request",
			`unknown kind: ` + kind + ` (expected "upscale" or "optimize")`
	}

	if code, msg := checkDeleteShapeExclusive(q, hasPrefix, hasPath, hasIdentity); code != "" {
		return req, code, msg
	}

	if hasIdentity {
		// A parameter that is PRESENT but blank is a malformed
		// identity request, not the absence of one. Letting it through
		// leaves every shape unset, and the shape-less request means
		// "delete every variant in the manifest" — so this has to be
		// an error here rather than something a downstream guard
		// happens to catch.
		if len(identity.AlbumIDs) == 0 && identity.ArtistID == "" {
			return req, "bad_request",
				"`albumId` / `artistId` must not be empty"
		}
		// The caller fills req.Paths after expanding the id against
		// the catalog. Returning early is what keeps an identity
		// request from falling through to the All shape below —
		// which, with no confirm, would 400, and with one would wipe
		// the whole cache.
		return req, "", ""
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

// checkDeleteShapeExclusive rejects every request that names more than
// one scope. Split out of the parser because these three checks are one
// idea — "exactly one shape" — and inlining them pushed
// parseVariantDeleteRequest past the cognitive-complexity gate for a
// function whose remaining branches each do something different.
//
// Guessing at a mixed request is not an option: one of the shapes it
// could resolve to deletes every variant in the manifest.
func checkDeleteShapeExclusive(q map[string][]string, hasPrefix, hasPath, hasIdentity bool) (code, msg string) {
	has := func(k string) bool { _, ok := q[k]; return ok }
	switch {
	case hasPrefix && hasPath:
		return "bad_request", "cannot combine `prefix` and `path` query parameters; pick one"
	case hasIdentity && (hasPrefix || hasPath):
		return "bad_request", "cannot combine `albumId` / `artistId` with `prefix` or `path`; pick one"
	case has("albumId") && has("artistId"):
		return "bad_request", "cannot combine `albumId` and `artistId`; pick one"
	}
	return "", ""
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

// identityParams reads the admin-only identity query parameters into
// the shared scopeRequest shape.
//
// `present` reports whether either parameter appeared AT ALL, which is
// deliberately not the same question as whether it carried a usable
// value. `?artistId=` is a malformed identity request; treating it as
// the absence of one leaves the request with no shape set, and a
// shape-less delete request is the one that clears the entire variant
// cache. The parser and the handler both key off `present` so they
// cannot disagree about which requests are identity-scoped.
//
// Blank entries are dropped from the id list rather than passed on to
// fail id validation one at a time; repeatable `albumId` is what lets a
// grid multi-select delete in one request, and `artistId` is single by
// construction.
//
// Kept in this file rather than variant_scope.go: it is the QUERY-side
// spelling of a scope, and only the delete route has one — the submit
// route names its scope in a JSON body.
func identityParams(q url.Values) (req scopeRequest, present bool) {
	rawAlbums, hasAlbum := q["albumId"]
	rawArtist, hasArtist := q["artistId"]
	if !hasAlbum && !hasArtist {
		return scopeRequest{}, false
	}
	for _, id := range rawAlbums {
		if v := strings.TrimSpace(id); v != "" {
			req.AlbumIDs = append(req.AlbumIDs, v)
		}
	}
	if len(rawArtist) > 0 {
		req.ArtistID = strings.TrimSpace(rawArtist[0])
	}
	return req, true
}
