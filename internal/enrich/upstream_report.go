package enrich

import (
	"context"
	"log/slog"
	"sync"
)

// One name per upstream. MusicBrainz artist search, release search,
// release-group lookup and the fingerprint artist search share one streak.
const (
	upstreamMusicBrainz = "musicbrainz"
	upstreamCoverArt    = "coverart"
	upstreamITunes      = "itunes"
	upstreamDeezer      = "deezer"
	upstreamAtlas       = "atlas"

	msgUpstreamAnswered = "upstream answered"
)

// upstreamReports keeps one consecutive-failure streak per upstream.
// A transient failure warns once when the streak starts and stays quiet
// while it continues. The next answer from that upstream is one Info
// line. A clean miss is Debug. Anything else stays Error.
//
// The level never decides whether the track is retried or stamped.
// IsTransient still does.
type upstreamReports struct {
	mu     sync.Mutex
	streak map[string]int
}

func newUpstreamReports() *upstreamReports {
	return &upstreamReports{streak: make(map[string]int)}
}

// report logs err for upstream. A nil error only ends an open streak.
// A nil receiver keeps no streak, so a transient failure warns every time
// and every other error is still logged. That is a client built outside
// an enricher.
func (r *upstreamReports) report(upstream, message string, err error, args ...any) {
	if err == nil || !IsTransient(err) {
		r.answered(upstream)
	}
	if err == nil {
		return
	}
	if IsTransient(err) {
		r.noteBusy(upstream, message, withErr(args, err)...)
		return
	}
	level := slog.LevelError
	if IsNotFound(err) {
		level = slog.LevelDebug
	}
	logger.Log(context.Background(), level, message, withErr(args, err)...)
}

// noteBusy warns on the first consecutive transient failure of upstream
// and stays quiet after that. Atlas uses it for a 5xx that is not an
// error value.
func (r *upstreamReports) noteBusy(upstream, message string, args ...any) {
	if r == nil || r.begin(upstream) {
		logger.Warn(message, args...)
	}
}

// answered ends upstream's streak. One Info line names how many
// consecutive failures it covered. A quiet upstream logs nothing.
func (r *upstreamReports) answered(upstream string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	n := r.streak[upstream]
	if n > 0 {
		r.streak[upstream] = 0
	}
	r.mu.Unlock()
	if n > 0 {
		logger.Info(msgUpstreamAnswered, "upstream", upstream, "failures", n)
	}
}

// begin records one more failure and reports whether it starts the streak.
func (r *upstreamReports) begin(upstream string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.streak[upstream]
	r.streak[upstream] = n + 1
	return n == 0
}

func withErr(args []any, err error) []any {
	out := make([]any, len(args)+2)
	copy(out, args)
	out[len(args)] = "err"
	out[len(args)+1] = err
	return out
}

// noteUpstreamAnswer ends this client's failure streak when a request
// is answered. fn is nil when the client is built outside an enricher.
func noteUpstreamAnswer(fn func()) {
	if fn != nil {
		fn()
	}
}

// logUpstream is the enricher's one route onto upstreamReports.
// An Enricher whose reports field is nil grows the map on first use.
// NewEnricher sets it before any request runs.
func (e *Enricher) logUpstream(upstream, message string, err error, args ...any) {
	if e.reports == nil {
		e.reports = newUpstreamReports()
	}
	e.reports.report(upstream, message, err, args...)
}

// watchUpstreamAnswers points each client at this enricher's streak.
func (e *Enricher) watchUpstreamAnswers() {
	if e.reports == nil {
		return
	}
	if e.mb != nil {
		e.mb.noteAnswered = func() { e.reports.answered(upstreamMusicBrainz) }
	}
	if e.caa != nil {
		e.caa.noteAnswered = func() { e.reports.answered(upstreamCoverArt) }
	}
	if e.deezer != nil {
		e.deezer.noteAnswered = func() { e.reports.answered(upstreamDeezer) }
	}
	if e.itunes != nil {
		e.itunes.noteAnswered = func() { e.reports.answered(upstreamITunes) }
	}
}
