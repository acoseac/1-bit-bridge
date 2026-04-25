package admin

import (
	"net/http"
	"path/filepath"

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
	info, err := servertls.Inspect(certPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "inspect-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// certPaths resolves the cert + key paths the running bridge uses,
// applying the same `cfg.TLSCertPath` / `cfg.TLSKeyPath` → defaults
// fallback that `serveCmd` does. The CLI mirror is in
// `cmd/bridge/cert.go::resolveCertPaths` — kept duplicated for now
// (admin shouldn't import cmd, and pulling the helper into
// internal/tls would force every consumer to plumb config). Two
// short identical functions are cheaper than the abstraction.
func (s *Server) certPaths() (certPath, keyPath string) {
	cfg := s.deps.Cfg
	certPath, keyPath = cfg.TLSCertPath, cfg.TLSKeyPath
	if certPath == "" || keyPath == "" {
		certPath = filepath.Join(cfg.DataDir, servertls.CertFileName)
		keyPath = filepath.Join(cfg.DataDir, servertls.KeyFileName)
	}
	return certPath, keyPath
}
