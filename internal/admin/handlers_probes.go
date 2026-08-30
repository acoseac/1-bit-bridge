package admin

import (
	"net/http"
)

// Liveness and readiness probes for an orchestrator.
//
// Before these, an orchestrator had nothing to point at. Every path on
// the admin listener either redirected to /login (302) or answered
// `unauthenticated` (401) in public mode, so a load balancer's health
// check could not distinguish "the process is up" from "the process is
// wedged" — both look like a 302. Verified against the live bridge:
//
//	GET :7789/healthz  -> 302 /login
//	GET :7789/readyz   -> 302 /login
//	GET :7789/api/health -> 401 session required
//
// Two endpoints rather than one, because they answer different
// questions and a caller that conflates them makes a bad decision:
//
//   - /healthz is LIVENESS. The process is running and its HTTP stack
//     is answering. A failure here should restart the process.
//   - /readyz is READINESS. It is additionally able to serve a library:
//     the store is open and the first scan has completed. A failure
//     here should take the instance out of rotation WITHOUT restarting
//     it — a bridge doing its startup scan of a large library is
//     healthy and simply not ready yet, and restarting it would make
//     it start the scan again from the top, forever.
//
// Both disclose a status code and nothing else. That is deliberate:
// they sit ahead of the session gate, so they are the most exposed
// surface on the listener, and an unauthenticated endpoint that
// reports scan progress or track counts is an inventory (see the
// /v1/health split for the same argument at greater length). A probe
// needs one bit; it gets one bit.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// readyz reports whether this instance can serve the library.
//
// 503 rather than 200-with-a-body on the not-ready path: the whole
// point is that a load balancer, which reads status codes and not
// JSON, takes the instance out of rotation.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.ready() {
		// Retry-After is advisory, but a scanner-still-running answer is
		// a "come back shortly", not a "this will never work".
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ready is the readiness predicate, split out so it can be tested
// without an HTTP round trip.
//
// The nil checks are unreachable on the constructed path — New refuses
// a Deps without a Manifest and Scanner — and are kept anyway. This is
// the function an orchestrator polls, and an orchestrator's response to
// a failing liveness check is to RESTART the process: a nil dereference
// here turns the cheapest possible bug into a restart loop rather than
// a stack trace someone reads. Two lines of defence are worth more than
// the tidiness in the one function whose whole job is to answer
// honestly when something is wrong.
//
// The scan check is "has one ever finished", not "is one running now".
// A periodic rescan on a live bridge must not drop it out of rotation
// — it serves the previous manifest perfectly well throughout — so
// only the cold-start case, where there is no manifest yet to serve,
// answers not-ready.
func (s *Server) ready() bool {
	if s.deps.Manifest == nil || s.deps.Scanner == nil {
		return false
	}
	// A public bridge with no credential store cannot serve the console
	// to anyone. Liveness deliberately still answers 200 in that state
	// (a restart cannot conjure a credential file, so killing the
	// process only loops), which is exactly why readiness has to be the
	// one that reports it — otherwise a misconfigured instance stays in
	// rotation answering 503 to every real request.
	if cfg := s.deps.CfgHolder.Load(); cfg != nil && cfg.IsPublic() && s.deps.AdminAuth == nil {
		return false
	}
	return !s.deps.Scanner.LastFullScan().IsZero()
}
