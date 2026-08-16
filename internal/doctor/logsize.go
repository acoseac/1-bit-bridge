package doctor

import (
	"context"
	"fmt"
	"os"
	"runtime"
)

const checkNameLogSize = "log-file-size"

// logSizeWarnBytes is where the log-file check starts warning.
//
// 256 MiB is well past anything a healthy bridge produces and well short of
// a size that threatens a modern disk — it is chosen to catch the shape of
// problem that actually occurs (something logging in a loop) rather than to
// police disk usage. For scale: the author's own log reached 301 MB across 72
// days, of which ~99.5% was a single repeated warning from a send path with
// no streak suppression (fixed alongside this check). A bridge logging
// normally does not approach this in years.
const logSizeWarnBytes int64 = 256 << 20

// checkLogSize reports an oversized log file.
//
// # Why a warning and not rotation
//
// The bridge does not own its log file. logging.Init writes to STDERR, and
// the service unit templates redirect that to a path — so the file is opened
// by launchd / systemd / the Windows wrapper, not by us, and the bridge holds
// no descriptor it could roll over. Rotating from in here would mean either
// copy-truncate (which races the writer and drops the lines written between
// the copy and the truncate) or taking ownership of the file, which changes
// the unit templates on three platforms.
//
// Neither belongs in a point release, and neither is the actual fix: an
// unbounded log is a symptom, and the cause is something logging every tick.
// So the bridge names the condition and points at the platform's own tool,
// which is the thing already designed to do this and which an operator can
// apply without a new bridge build.
//
// Skipped entirely when the path is unset or absent — a foreground
// `bridge serve` has no log file at all, and reporting on one would be
// reporting on something that does not exist.
func checkLogSize(_ context.Context, d Deps) Check {
	if d.LogPath == "" {
		return ok(checkNameLogSize, "no log file configured (logs go to the terminal)")
	}
	info, err := os.Stat(d.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Normal for a foreground run, and for a service that has not
			// started yet. Not a problem to report.
			return ok(checkNameLogSize, "no log file yet")
		}
		return warn(checkNameLogSize, "cannot stat "+d.LogPath, err.Error())
	}
	if info.Size() < logSizeWarnBytes {
		return ok(checkNameLogSize, fmt.Sprintf("%s (%s)", humanBytes(info.Size()), d.LogPath))
	}
	return warn(checkNameLogSize,
		fmt.Sprintf("%s at %s", humanBytes(info.Size()), d.LogPath),
		"nothing rotates this file — the bridge writes to stderr and the service "+
			"redirects it, so rotation belongs to the OS. "+rotationHint()+
			" A log this size usually means something is logging on a timer; "+
			"`bridge logs -n 200` will show what.")
}

// rotationHint names the tool that already exists on this platform, rather
// than describing rotation in the abstract.
func rotationHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "On macOS, add a newsyslog.d entry (see `man newsyslog.conf`)."
	case "windows":
		return "On Windows, rotate it with a scheduled task or delete it while the service is stopped."
	default:
		return "On Linux, add a logrotate entry (see `man logrotate`), or use systemd's journal instead of a file."
	}
}

// humanBytes formats a size for an operator-facing line.
//
// Local to this package on purpose: internal/doctor imports nothing from the
// admin or CLI layers that already have a formatter, and a one-way dependency
// on either to save six lines would be the wrong trade.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}
