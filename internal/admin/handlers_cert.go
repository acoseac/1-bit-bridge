package admin

import (
	"errors"
	"io/fs"
	"net/http"
	"os"

	servertls "github.com/acoseac/1-bit-bridge/internal/tls"
)

// apiCertInfo: GET /api/cert
//
// Surfaces the live TLS cert's metadata so the dashboard can render
// an expiry badge (yellow ≤30 days, red ≤7) and the operator can
// see the fingerprint without SSHing in. The pinned fingerprint
// served here is the SAME value shown in the existing dashboard
// "TLS fingerprint" panel — duplicated for the cert tile so the
// JS doesn't have to cross-reference two endpoints.
//
// Cert rotation itself is a CLI-only operation (`bridge cert
// rotate`) — exposing it via the admin console would require a
// process restart hook the operator has to consciously trigger,
// which is exactly what the CLI flow already does without
// adding a "click here to rotate" surface that an idle browser tab
// could fire by accident.
func (s *Server) apiCertInfo(w http.ResponseWriter, r *http.Request) {
	certPath, _ := s.certPaths()
	// A MISSING cert is a distinct, reachable operator state — a deleted
	// data/tls/, a partial restore, a hand-assembled data dir — and it has
	// a specific remedy. Answering 500 with an opaque inspect error told
	// the operator only that something broke. (Found by the console smoke
	// pass, which hits every read-only route on a bridge that has never
	// minted one.)
	if _, statErr := os.Stat(certPath); errors.Is(statErr, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "no-certificate",
			"no TLS certificate at "+certPath+" — run `bridge cert rotate` to mint one "+
				"(note that rotating invalidates every existing iOS pairing)")
		return
	}
	info, err := servertls.Inspect(certPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "inspect-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// certPaths resolves the cert + key paths the running bridge uses,
// applying the same `cfg.TLSCertPath` / `cfg.TLSKeyPath` → defaults
// fallback that `serveCmd` does. Delegates to `servertls.DefaultPaths`
// rather than reconstructing the layout — single source of truth
// for "where the cert lives" (Gemini flagged the prior duplication
// on PR #46).
func (s *Server) certPaths() (certPath, keyPath string) {
	cfg := s.deps.CfgHolder.Load()
	certPath, keyPath = cfg.TLSCertPath, cfg.TLSKeyPath
	if certPath == "" || keyPath == "" {
		certPath, keyPath = servertls.DefaultPaths(cfg.DataDir)
	}
	return certPath, keyPath
}
