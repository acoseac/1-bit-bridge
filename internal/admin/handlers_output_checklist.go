// GET /api/output/checklist — the player checklist (backlog B473): Player,
// DAC, Music and Phone, the four lines `bridge doctor` prints for a bridge
// that is a player, in the same words, from the same source
// (internal/outputcheck, gathered in cmd/bridge over the serving process's
// own GET /v1/outputs builder). The dashboard's checklist panel renders it.
//
// Loaded with the dashboard and on its Check again button, never polled:
// a build can ask MPD to find up to twenty tracks per root, which the
// outputs builder caches, and an SSE tick would keep its session open.
package admin

import "net/http"

// OutputCheckLine is one line of the checklist. Admin-local wire DTO:
// cmd/bridge fills it from outputcheck.Line, so internal/admin needs no
// dependency on that package and a change there cannot reshape this
// response. Fix is one sentence on a line that is not ok, and absent on a
// green one.
type OutputCheckLine struct {
	Key     string `json:"key"`
	Title   string `json:"title"`
	Status  string `json:"status"` // ok | warn | fail
	Summary string `json:"summary"`
	Fix     string `json:"fix,omitempty"`
}

// outputChecklistResponse says whether this bridge is a player at all.
// Inactive is a bridge with output.mpd resolving off, or not on Linux, or
// a console with no checklist wired: the panel stays hidden, and Lines is
// [] rather than a list that reads as a clean bill.
type outputChecklistResponse struct {
	Active bool              `json:"active"`
	Lines  []OutputCheckLine `json:"lines"`
}

// apiOutputChecklist handles GET /api/output/checklist.
func (s *Server) apiOutputChecklist(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out := outputChecklistResponse{Lines: []OutputCheckLine{}}
	if s.deps.OutputChecklist != nil {
		if lines, ok := s.deps.OutputChecklist(r.Context()); ok {
			out.Active = true
			out.Lines = append(out.Lines, lines...)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
