package admin

import (
	"net/http"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// librarySpaceDTO backs the sidebar's space widget.
//
// Three numbers rather than one, because "free" alone cannot answer the
// question a quota user actually has. Trash does not free space until it is
// purged, so the reclaimable figure has to sit next to the free one or the
// operator is told they are stuck when they are one click from not being.
type librarySpaceDTO struct {
	Root string `json:"root"`
	// FreeBytes is the volume the library root lives on.
	FreeBytes int64 `json:"freeBytes"`
	// TotalBytes is that volume's capacity, so the UI can render a
	// proportion rather than a bare number.
	TotalBytes int64 `json:"totalBytes,omitempty"`
	// LibraryBytes is what the indexed tracks occupy.
	LibraryBytes int64 `json:"libraryBytes"`
	// ReclaimableBytes is what emptying the trash would return. Zero until
	// the trash exists.
	ReclaimableBytes int64 `json:"reclaimableBytes"`
	// MinFreeBytes is the floor uploads refuse below, so the UI can say WHY
	// a number that looks non-zero is nevertheless "full".
	MinFreeBytes int64 `json:"minFreeBytes"`
	// Configured reports whether a floor or quota is set. The widget renders
	// only when this is true or free space is already near the floor — an
	// operator with a 40 TB NAS should never see a meter for a number that
	// will never bind.
	Configured bool `json:"configured"`
	// Probed is false when the free-space probe failed. The UI shows a dash
	// rather than a confident zero, which would read as "disk full".
	Probed bool `json:"probed"`
}

// apiLibrarySpace reports free / used / reclaimable for the first library root.
func (s *Server) apiLibrarySpace(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.CfgHolder.Load()
	out := librarySpaceDTO{}
	if cfg == nil || len(cfg.LibraryRoots) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	root := cfg.LibraryRoots[0]
	out.Root = filepath.Base(root)
	out.MinFreeBytes = cfg.Upload.MinFreeBytes
	out.Configured = cfg.Upload.Enabled || cfg.Upload.MinFreeBytes > 0

	if free, err := transcode.AvailableDiskSpaceNearest(root); err == nil {
		out.FreeBytes, out.Probed = free, true
		// Capacity is what makes a fill bar mean something. Without it the
		// only available denominator is indexed library bytes, which on a
		// shared disk reads "almost empty" while the volume is nearly full.
		// A failure here is not fatal: the widget drops the bar and keeps
		// the free-space figure, which is the number that actually matters.
		if total, terr := transcode.TotalDiskSpace(root); terr == nil {
			out.TotalBytes = total
		} else {
			logger.Warn("library capacity probe", "root", root, "err", terr)
		}
	} else {
		// Deliberately not an error response: a missing number degrades the
		// widget, it does not break the page.
		logger.Warn("library free-space probe", "root", root, "err", err)
	}
	if s.deps.Manifest != nil {
		if bytes, err := s.deps.Manifest.TotalTrackBytes(r.Context()); err == nil {
			out.LibraryBytes = bytes
		} else {
			logger.Warn("library size query", "err", err)
		}
	}
	if s.deps.Trash != nil {
		out.ReclaimableBytes = s.deps.Trash(root)
	}
	writeJSON(w, http.StatusOK, out)
}
