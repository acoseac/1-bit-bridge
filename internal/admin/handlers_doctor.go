// GET /api/doctor — run the preflight checks from the console.
//
// `bridge doctor` has always been able to answer "is this install
// healthy", and cmd/bridge/doctor.go's own comment says the JSON envelope
// was shaped for exactly this surface. But it was reachable only from a
// shell on the host, and only really used at init time — so an operator
// working in the console had no way to ask.
//
// Re-runnable by design: the interesting cases are the ones that change
// while the bridge is up (a library root that went away with a NAS, a
// cert nearing expiry, sox uninstalled by a system upgrade), and a
// one-shot init-time report cannot show any of them.
//
// Rendered as a panel on the Diagnostics page rather than a page of its
// own. Both answer "is this bridge healthy"; splitting them would put two
// health surfaces in the top nav and leave the operator guessing which
// one to open.
package admin

import (
	"net/http"
)

// DoctorCheck is one line of the report. Admin-local wire DTO: cmd/bridge
// fills it from doctor.Check, so internal/admin needs no dependency on
// internal/doctor and a change to the domain type cannot silently reshape
// this response. Mirrors the CLI's jsonDoctorCheck field-for-field so the
// two reports stay comparable.
type DoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok | warn | fail
	Summary string `json:"summary"`
	Hint    string `json:"hint,omitempty"`
}

// DoctorReport is a whole run.
type DoctorReport struct {
	Checks []DoctorCheck `json:"checks"`
	OK     int           `json:"ok"`
	Warn   int           `json:"warn"`
	Fail   int           `json:"fail"`
}

// doctorResponse wraps the report so the handler can say WHY there is
// nothing to show, rather than returning an empty list that reads like a
// clean bill of health.
type doctorResponse struct {
	Available bool          `json:"available"`
	Reason    string        `json:"reason,omitempty"`
	Report    *DoctorReport `json:"report,omitempty"`
}

// apiDoctor handles GET /api/doctor.
//
// The checks touch the filesystem (config dir writability, library roots,
// cert files) and may exec sox/fpcalc for the toolchain checks, so this
// is click-driven only — never polled, and never on the SSE tick.
func (s *Server) apiDoctor(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.deps.DoctorRun == nil {
		// Not an error: a Server constructed without the closure (tests,
		// or a future embedding) simply has no doctor. Saying so beats a
		// 500 the UI would have to special-case anyway.
		writeJSON(w, http.StatusOK, doctorResponse{
			Available: false,
			Reason:    "doctor is not wired on this bridge",
		})
		return
	}
	rep := s.deps.DoctorRun(r.Context())
	if rep == nil {
		writeJSON(w, http.StatusOK, doctorResponse{
			Available: false,
			Reason:    "doctor produced no report",
		})
		return
	}
	writeJSON(w, http.StatusOK, doctorResponse{Available: true, Report: rep})
}
