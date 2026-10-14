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

// report logs err for upstream. A nil error ends an open streak: the
// caller is reporting that the upstream answered without an error value.
// A non-nil error never does, including a persistent one. A response
// that arrived is noted by the client (noteSettledAnswer) or by Atlas.
// A DNS failure, a TLS failure and a base that could not be used produced
// no response, so they leave the streak open.
//
// A nil receiver keeps no streak, so a transient failure warns every time
// and every other error is still logged. That is an Enricher built field
// by field, whose reports field NewEnricher never set.
func (r *upstreamReports) report(upstream, message string, err error, args ...any) {
	if err == nil {
		r.answered(upstream)
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

// noteSettledAnswer ends the streak when code is not a transient HTTP
// status. A 429 or a 5xx keeps it open. Every client uses this, so none
// of them copies the status test.
func noteSettledAnswer(fn func(), code int) {
	if transientHTTPStatus(code) {
		return
	}
	noteUpstreamAnswer(fn)
}

// carriedHTTP wraps a client's own error sentence with the response
// status. Error() stays that sentence. IsTransient reads the status
// through HTTPStatus, the same method *httpError carries, so Cover Art
// Archive, iTunes and Deezer 429s and 5xxs join the streak without
// becoming a MusicBrainz error.
type carriedHTTP struct {
	code int
	err  error
}

func (e *carriedHTTP) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *carriedHTTP) Unwrap() error { return e.err }

func (e *carriedHTTP) HTTPStatus() int {
	if e == nil {
		return 0
	}
	return e.code
}

// carryTransientStatus wraps err when code is a transient HTTP status.
// Any other code, and a nil err, is returned unchanged, so a 404 and a
// 4xx keep the sentence the client already built.
func carryTransientStatus(code int, err error) error {
	if err == nil || !transientHTTPStatus(code) {
		return err
	}
	return &carriedHTTP{code: code, err: err}
}

// logUpstream is the enricher's one route onto upstreamReports.
// NewEnricher sets reports and watchUpstreamAnswers points each client
// at it before any request. A map created here would open a streak those
// clients were never pointed at, so a nil field stays nil.
func (e *Enricher) logUpstream(upstream, message string, err error, args ...any) {
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
