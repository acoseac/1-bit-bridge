package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

// Drain-aware restart.
//
// `POST /api/restart` used to answer 202 and exit 100 ms later, cutting
// whatever was streaming. That was survivable while a restart meant an
// operator standing at the console deciding to bounce their own bridge:
// they knew what they were interrupting, and it was their call.
//
// It stops being survivable under the hosted product. There, settings
// arrive from a control plane, and the fix for a restart-bound field is
// for the control plane to bounce the process on the operator's behalf —
// at which point the person whose stream just cut never asked for
// anything and has no idea why the music stopped. A restart nobody can
// see has to be a restart nobody notices.
//
// So the handler waits for in-flight /v1/read + /v1/download requests to
// finish first, and REPORTS whether it managed it. The machinery already
// existed — updater.Tracker, which the auto-installer has consulted
// before swapping a binary since PR #42 — it simply was not wired here.
//
// # The wait costs nothing when nothing is playing
//
// The common case is zero in-flight, which returns immediately. Only an
// actual stream makes anyone wait, which is the case worth waiting for.
//
// # Bounded, and honest when the bound is hit
//
// A stream can outlive any deadline (a long album over a slow link), so
// the wait is capped and the response says plainly whether it drained or
// gave up with N still in flight. Reporting a clean drain we could not
// verify would be worse than not draining at all: the control plane would
// record a graceful restart and never learn it interrupted anyone.

const (
	// defaultRestartDrainWait is the cap when the caller does not set one.
	// Long enough for a track to finish over a slow link, short enough
	// that a control plane applying a setting is not left hanging.
	defaultRestartDrainWait = 30 * time.Second
	// maxRestartDrainWait bounds what a caller may ask for. The admin
	// server deliberately sets no WriteTimeout (PR #75), so nothing else
	// would stop a caller pinning a request open for an hour.
	maxRestartDrainWait = 5 * time.Minute
	// restartDrainPoll is how often the counter is re-read while waiting.
	restartDrainPoll = 250 * time.Millisecond
	// restartRespondDelay gives the response time to reach the client
	// before the process exits. Pre-existing behaviour, kept.
	restartRespondDelay = 100 * time.Millisecond
)

// restartRequest is the optional body. Absent fields take the defaults,
// so a bodyless POST — what the console has always sent — now drains.
type restartRequest struct {
	// Drain waits for in-flight file serving before restarting.
	// Defaults to TRUE: a caller that did not think about it gets the
	// safer behaviour, and the only cost when nothing is playing is one
	// counter read.
	Drain *bool `json:"drain,omitempty"`
	// MaxWaitSec caps the wait. Zero or absent uses the default; the
	// value is clamped to maxRestartDrainWait.
	MaxWaitSec *int `json:"maxWaitSec,omitempty"`
}

type restartResponse struct {
	Restarting bool `json:"restarting"`
	// Drained is true only when the in-flight count was OBSERVED to reach
	// zero. False means either the wait timed out with streams still
	// running, draining was not requested, or the count is unknown —
	// Reason says which.
	Drained bool `json:"drained"`
	// Inflight is the count at the moment the decision was made. -1 when
	// no tracker is wired, which is a different fact from 0.
	Inflight int64 `json:"inflight"`
	WaitedMs int64 `json:"waitedMs"`
	// Reason is present when Drained is false, naming why.
	Reason string `json:"reason,omitempty"`
}

func (s *Server) apiRestart(w http.ResponseWriter, r *http.Request) {
	req, err := decodeRestartRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, errCodeBadJSON, err.Error())
		return
	}

	resp := s.drainForRestart(r, req)
	resp.Restarting = true

	writeJSON(w, http.StatusAccepted, resp)
	// Exit after the response has had a moment to reach the client —
	// pre-existing behaviour. s.restart() routes through the same
	// graceful-shutdown closure SIGINT uses, so the pools drain, the auth
	// debounce flushes and the DB checkpoints; it must never os.Exit.
	go func() {
		time.Sleep(restartRespondDelay)
		s.restart()
	}()
}

// drainForRestart performs the wait and reports what happened.
func (s *Server) drainForRestart(r *http.Request, req restartRequest) restartResponse {
	if req.Drain != nil && !*req.Drain {
		return restartResponse{Inflight: s.inflightOrUnknown(), Reason: "drain not requested"}
	}
	if s.deps.InflightSessions == nil {
		// Honest absence: a bridge whose session tracker is not wired
		// cannot know what it is interrupting, and saying "drained"
		// would have a control plane record a graceful restart that may
		// have cut a stream.
		return restartResponse{Inflight: -1, Reason: "no session tracker wired on this bridge"}
	}

	wait := clampDrainWait(req)

	start := time.Now()
	// Deliberately NOT tied to r.Context(): the restart was already
	// requested, and a control plane whose HTTP client gives up mid-drain
	// still wants the bounce it asked for. Only the deadline stops us.
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(restartDrainPoll)
	defer tick.Stop()

	for {
		n := s.deps.InflightSessions()
		if n <= 0 {
			return restartResponse{
				Drained:  true,
				Inflight: 0,
				WaitedMs: time.Since(start).Milliseconds(),
			}
		}
		select {
		case <-deadline.C:
			return restartResponse{
				Inflight: s.deps.InflightSessions(),
				WaitedMs: time.Since(start).Milliseconds(),
				Reason:   "timed out waiting for in-flight streams to finish",
			}
		case <-tick.C:
		}
	}
}

func (s *Server) inflightOrUnknown() int64 {
	if s.deps.InflightSessions == nil {
		return -1
	}
	return s.deps.InflightSessions()
}

// decodeRestartRequest tolerates an absent body — the console has always
// POSTed without one, and that path must keep working.
func decodeRestartRequest(r *http.Request) (restartRequest, error) {
	var req restartRequest
	if r.Body == nil {
		return req, nil
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, adminMaxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return req, nil // empty body: all defaults
		}
		return req, err
	}
	return req, nil
}

// clampDrainWait resolves the requested cap. Absent or non-positive uses
// the default; anything larger than maxRestartDrainWait is clamped,
// because the admin server sets no WriteTimeout (PR #75) and nothing else
// would stop a caller holding a request open indefinitely.
func clampDrainWait(req restartRequest) time.Duration {
	if req.MaxWaitSec == nil || *req.MaxWaitSec <= 0 {
		return defaultRestartDrainWait
	}
	if w := time.Duration(*req.MaxWaitSec) * time.Second; w < maxRestartDrainWait {
		return w
	}
	return maxRestartDrainWait
}
