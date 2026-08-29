package admin

import (
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/transcode"
)

// spaceTTL bounds how often the space snapshot is recomputed.
//
// The sidebar widget asks for this on EVERY admin page load, and the snapshot
// costs two statfs calls plus a SUM(size) over `tracks` — which has no index on
// size, so it is a full scan. On a 19k-track library behind a FUSE mount that
// is not free, and none of the three numbers moves fast enough to notice a few
// seconds of staleness. Deletes and purges invalidate the reclaimable half
// through the trash manager's own cache, which this reads live.
const spaceTTL = 15 * time.Second

// diskFacts are the expensive, slow-moving half of the snapshot.
type diskFacts struct {
	root    string
	free    int64
	total   int64
	library int64
}

var (
	spaceMu   sync.Mutex
	spaceSnap diskFacts
	spaceAt   time.Time
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

	// Only the DISK facts are cached. Everything config-derived
	// (Configured, MinFreeBytes, Root) is recomputed every call: an operator
	// who enables uploads and reloads must not find the widget still hidden
	// because a snapshot from ten seconds ago said the question was not live.
	if f, ok := cachedDiskFacts(root); ok {
		out.FreeBytes, out.TotalBytes, out.LibraryBytes, out.Probed = f.free, f.total, f.library, true
		if s.deps.Trash != nil {
			out.ReclaimableBytes = s.deps.Trash(root)
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

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
	// Only a PROBED snapshot is cached. Caching a failed probe would pin
	// "unknown" — which the widget renders as absent — for the whole TTL.
	if out.Probed {
		spaceMu.Lock()
		spaceSnap = diskFacts{root: root, free: out.FreeBytes, total: out.TotalBytes, library: out.LibraryBytes}
		spaceAt = time.Now()
		spaceMu.Unlock()
	}
	writeJSON(w, http.StatusOK, out)
}

func cachedDiskFacts(root string) (diskFacts, bool) {
	spaceMu.Lock()
	defer spaceMu.Unlock()
	if spaceAt.IsZero() || spaceSnap.root != root || time.Since(spaceAt) >= spaceTTL {
		return diskFacts{}, false
	}
	return spaceSnap, true
}

// resetSpaceCacheForTest clears the snapshot between tests, which would
// otherwise leak across them through the package-level cache.
func resetSpaceCacheForTest() {
	spaceMu.Lock()
	spaceSnap, spaceAt = diskFacts{}, time.Time{}
	spaceMu.Unlock()
}
