package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PUT /v1/outputs/{id}/dsd writes the DSD choice of the DAC the bridge's own
// player is bound to (B728 B9 PR 3): its mode, its cap and the user's
// consents, made only while the player is stopped. This package imports no
// player package: serve hands the route its write (BridgeDSDChoice), which
// takes plain values and answers an outcome word, the player's DSD as the
// write published it, and the cause of a failure, from which the handler
// builds the answer (PROTOCOL.md, the route's section).

// The outcomes a write of the DAC's choice answers with: the player's own,
// which serve passes on as they are, and DSDChoiceInternal, a write that
// panicked. Each is the answer's error code but DSDChoiceWritten.
const (
	DSDChoiceWritten                = "written"
	DSDChoiceBadRequest             = "bad_request"
	DSDChoicePlaybackNotStopped     = "playback_not_stopped"
	DSDChoiceDACUnavailable         = "dac_unavailable"
	DSDChoiceDACChanged             = "dac_changed"
	DSDChoiceStale                  = "stale"
	DSDChoiceModeNotOffered         = "mode_not_offered"
	DSDChoiceConfirmationRequired   = "dop_confirmation_required"
	DSDChoiceFirstUseUnacknowledged = "first_use_unacknowledged"
	DSDChoicePersistFailed          = "persist_failed"
	DSDChoicePersistUncertain       = "persist_uncertain"
	DSDChoiceStateUnreadable        = "state_unreadable"
	DSDChoiceInternal               = "internal"
)

const (
	// dsdChoiceDefaultBudget bounds the route's waits: the writes' turn, the
	// box's reading and a probe. The state's write, once begun, is not
	// interrupted.
	dsdChoiceDefaultBudget = 10 * time.Second
	// dsdChoiceMaxBody is the most a body may hold: one small object.
	dsdChoiceMaxBody = 4 << 10
)

// The route's answers' sentences, one for each outcome: none names a path, a
// serial or the DAC's strings.
const (
	dsdChoiceBodyMessage    = "request body must be one JSON object with dacKey, dacToken, baseRevision and mode"
	dsdChoiceWordsMessage   = "the choice's mode, cap or DAC token is not one the bridge takes"
	dsdChoiceNotStopped     = "Stop playback to change DSD"
	dsdChoiceNoDAC          = "no DAC is available to the bridge's player"
	dsdChoiceDACChanged     = "the DAC is not the one this choice was read for; read it again"
	dsdChoiceStaleMessage   = "the DAC's choice was written since it was read; read it again"
	dsdChoiceNotOffered     = "the DAC is not offered that mode"
	dsdChoiceConfirmDoP     = "DoP on this DAC needs the user's confirmation"
	dsdChoiceAcknowledge    = "the transport's first use on this DAC needs the user's acknowledgement"
	dsdChoiceStateMessage   = "the bridge cannot read or write its DSD state"
	dsdChoicePersistFailed  = "the bridge could not save the DAC's choice"
	dsdChoicePersistUnknown = "the bridge could not confirm that the DAC's choice was saved"
	dsdChoiceInternalError  = "the bridge encountered an internal error"
)

// BridgeDSDChoiceRequest is a write of the bound DAC's DSD choice as the
// route read it from the body: the DAC as the one dsdDAC the client holds
// names it (its key, its token and its choice's revision), the mode, the cap
// (0 for none), and the user's two consents, false unless given.
type BridgeDSDChoiceRequest struct {
	DACKey               string
	DACToken             string
	BaseRevision         uint64
	Mode                 string
	MaxBitRate           int
	ConfirmDoP           bool
	FirstUseAcknowledged bool
}

// BridgeDSDChoiceResult is a write's answer as serve gives it: the outcome
// word; Epoch, whether the answer carries the player's DSD as the write
// published it, which DSDPlayback, DSDDAC (nil while that names no DAC),
// DSDMode and AllowDoP then are, each as GET /v1/outputs' facts carry it; and
// Cause, the error a failure came from, logged and never sent.
type BridgeDSDChoiceResult struct {
	Outcome     string
	Epoch       bool
	DSDPlayback *OutputDSDPlayback
	DSDDAC      *OutputDSDDAC
	DSDMode     string
	AllowDoP    bool
	Cause       error
}

// BridgeDSDChoice is serve's player as PUT /v1/outputs/{id}/dsd writes
// through it.
type BridgeDSDChoice interface {
	// Lists is whether id names the player's element as GET /v1/outputs
	// lists it now.
	Lists(id string) bool
	// ChooseDSD writes the choice and answers how it went. It returns once
	// the write has, whatever ctx does: a state's write, once begun, runs to
	// its end, and the answer is what happened.
	ChooseDSD(ctx context.Context, req BridgeDSDChoiceRequest) BridgeDSDChoiceResult
}

// WithBridgeDSDChoice wires the write of the player's DAC's DSD choice (B728
// B9 PR 3), before the listeners start; /v1/health advertises
// bridgeDSDChoice while it is wired and the player's gate holds.
func (s *Server) WithBridgeDSDChoice(c BridgeDSDChoice) *Server {
	s.bridgeDSDChoice = c
	return s
}

// dsdChoiceBudget is the route's budget: dsdChoiceDefaultBudget, or this
// server's own, a test's.
func (s *Server) dsdChoiceBudget() time.Duration {
	if s.dsdChoiceTimeout > 0 {
		return s.dsdChoiceTimeout
	}
	return dsdChoiceDefaultBudget
}

// outputDSDChoiceBody is the PUT's body. dacKey, dacToken, baseRevision and
// mode are required, pointers so that one missing is refused rather than
// read as a zero (a baseRevision of 0 matches a DAC with no choice);
// maxBitRate is 0, and each consent false, when absent. A member the bridge
// does not know is ignored, so a consent misspelled is a consent not given.
type outputDSDChoiceBody struct {
	DACKey               *string `json:"dacKey"`
	DACToken             *string `json:"dacToken"`
	BaseRevision         *uint64 `json:"baseRevision"`
	Mode                 *string `json:"mode"`
	MaxBitRate           optInt  `json:"maxBitRate"`
	ConfirmDoP           optBool `json:"confirmDoP"`
	FirstUseAcknowledged optBool `json:"firstUseAcknowledged"`
}

// optInt and optBool are the body's optional members: refused when null,
// which encoding/json would read as absent.
type (
	optInt  struct{ v int }
	optBool struct{ v bool }
)

func (o *optInt) UnmarshalJSON(b []byte) error  { return notNull(b, &o.v) }
func (o *optBool) UnmarshalJSON(b []byte) error { return notNull(b, &o.v) }

func notNull(b []byte, v any) error {
	if string(b) == "null" {
		return errDSDChoiceNull
	}
	return json.Unmarshal(b, v)
}

// OutputDSDChoiceAnswer is a written choice's answer, and the members a
// refused or failed write's error body carries beside its code: the player's
// DSD as the write published it, each as GET /v1/outputs' bridge object
// carries it. dsdPlayback, dsdMode and allowDoP are present together exactly
// when the answer carries the write's epoch, and dsdDAC while that epoch
// names a DAC; none is ever null.
type OutputDSDChoiceAnswer struct {
	DSDPlayback *OutputDSDPlayback `json:"dsdPlayback,omitempty"`
	DSDDAC      *OutputDSDDAC      `json:"dsdDAC,omitempty"`
	DSDMode     *string            `json:"dsdMode,omitempty"`
	AllowDoP    *bool              `json:"allowDoP,omitempty"`
}

// outputDSDChoiceError is a refused or failed write's body: the error
// envelope and OutputDSDChoiceAnswer's members.
type outputDSDChoiceError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	OutputDSDChoiceAnswer
}

var (
	errDSDChoiceTrailing = errors.New("the body holds more than one JSON value")
	errDSDChoiceMissing  = errors.New("the body lacks a required member")
	errDSDChoiceNull     = errors.New("an optional member of the body is null")
)

// putOutputDSD is PUT /v1/outputs/{id}/dsd. Its checks run in PROTOCOL.md's
// order: the write's gate, the id, the body, then serve's write, whose
// outcome names the answer. It writes an answer on every way out, the
// budget's end included: withCtxTimeout writes none, and a handler that
// returned without writing would send an empty 200.
func (s *Server) putOutputDSD(w http.ResponseWriter, r *http.Request) {
	if !s.bridgeOutputOn() || s.bridgeDSDChoice == nil {
		// GET's words: output_not_found while the outputs route answers (the
		// player's gate, or MPD's as it last answered, asked without a probe),
		// local_outputs_not_supported while it does not.
		if s.bridgeOutputOn() || s.LocalOutputsActiveNow() {
			writeError(w, http.StatusNotFound, "output_not_found", "no output with that id on this bridge")
			return
		}
		writeError(w, http.StatusNotFound, "local_outputs_not_supported", "local outputs are not enabled on this bridge")
		return
	}
	if !s.bridgeDSDChoice.Lists(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "output_not_found", "no output with that id on this bridge")
		return
	}
	req, err := readDSDChoice(w, r)
	if err != nil {
		writeErrorLog(w, r, http.StatusBadRequest, "bad_request", dsdChoiceBodyMessage, err)
		return
	}
	res := s.bridgeDSDChoice.ChooseDSD(r.Context(), req)
	switch res.Outcome {
	case DSDChoiceWritten:
		s.answerDSDChoice(w, r, http.StatusOK, DSDChoiceWritten, "", res)
	case DSDChoiceBadRequest:
		writeError(w, http.StatusBadRequest, DSDChoiceBadRequest, dsdChoiceWordsMessage)
	case DSDChoicePlaybackNotStopped:
		s.answerDSDChoice(w, r, http.StatusConflict, DSDChoicePlaybackNotStopped, dsdChoiceNotStopped, res)
	case DSDChoiceDACUnavailable:
		s.answerDSDChoice(w, r, http.StatusConflict, DSDChoiceDACUnavailable, dsdChoiceNoDAC, res)
	case DSDChoiceDACChanged:
		s.answerDSDChoice(w, r, http.StatusConflict, DSDChoiceDACChanged, dsdChoiceDACChanged, res)
	case DSDChoiceStale:
		s.answerDSDChoice(w, r, http.StatusConflict, DSDChoiceStale, dsdChoiceStaleMessage, res)
	case DSDChoiceModeNotOffered:
		s.answerDSDChoice(w, r, http.StatusConflict, DSDChoiceModeNotOffered, dsdChoiceNotOffered, res)
	case DSDChoiceConfirmationRequired:
		s.answerDSDChoice(w, r, http.StatusConflict, DSDChoiceConfirmationRequired, dsdChoiceConfirmDoP, res)
	case DSDChoiceFirstUseUnacknowledged:
		s.answerDSDChoice(w, r, http.StatusConflict, DSDChoiceFirstUseUnacknowledged, dsdChoiceAcknowledge, res)
	case DSDChoiceStateUnreadable:
		s.answerDSDChoice(w, r, http.StatusConflict, DSDChoiceStateUnreadable, dsdChoiceStateMessage, res)
	case DSDChoicePersistFailed:
		// A client that went away is recorded 499 at Info, with the plain
		// envelope nobody reads; the route's deadline, the server's, is a 500
		// with the members, at Error (B364).
		if clientWentAway(r, res.Cause) {
			writeErrorLog(w, r, http.StatusInternalServerError, DSDChoicePersistFailed, dsdChoicePersistFailed, res.Cause)
			return
		}
		s.answerDSDChoice(w, r, http.StatusInternalServerError, DSDChoicePersistFailed, dsdChoicePersistFailed, res)
	case DSDChoicePersistUncertain:
		s.answerDSDChoice(w, r, http.StatusInternalServerError, DSDChoicePersistUncertain, dsdChoicePersistUnknown, res)
	case DSDChoiceInternal:
		writeError(w, http.StatusInternalServerError, DSDChoiceInternal, dsdChoiceInternalError)
	default:
		writeErrorLog(w, r, http.StatusInternalServerError, DSDChoiceInternal, dsdChoiceInternalError,
			fmt.Errorf("the write answered an outcome this build does not know: %q", res.Outcome))
	}
}

// readDSDChoice reads the PUT's body: one JSON object of at most
// dsdChoiceMaxBody bytes, nothing after it, and every required member.
func readDSDChoice(w http.ResponseWriter, r *http.Request) (BridgeDSDChoiceRequest, error) {
	var body outputDSDChoiceBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, dsdChoiceMaxBody))
	if err := dec.Decode(&body); err != nil {
		return BridgeDSDChoiceRequest{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return BridgeDSDChoiceRequest{}, errDSDChoiceTrailing
	}
	if body.DACKey == nil || body.DACToken == nil || body.BaseRevision == nil || body.Mode == nil {
		return BridgeDSDChoiceRequest{}, errDSDChoiceMissing
	}
	return BridgeDSDChoiceRequest{
		DACKey:               *body.DACKey,
		DACToken:             *body.DACToken,
		BaseRevision:         *body.BaseRevision,
		Mode:                 *body.Mode,
		MaxBitRate:           body.MaxBitRate.v,
		ConfirmDoP:           body.ConfirmDoP.v,
		FirstUseAcknowledged: body.FirstUseAcknowledged.v,
	}, nil
}

// answerDSDChoice writes a write's answer under status, code and msg: the
// members while res carries the write's epoch, none otherwise, and the error
// envelope beside them on every status but 200. A cause is logged at the
// status's level, never sent. Every call passes a constant status and code,
// which the error table's guard reads at each call.
func (s *Server) answerDSDChoice(w http.ResponseWriter, r *http.Request, status int, code, msg string, res BridgeDSDChoiceResult) {
	var m OutputDSDChoiceAnswer
	if res.Epoch && res.DSDPlayback != nil {
		mode, dop := res.DSDMode, res.AllowDoP
		m = OutputDSDChoiceAnswer{
			DSDPlayback: dsdPlaybackLists(res.DSDPlayback),
			DSDDAC:      dsdDACLists(res.DSDDAC),
			DSDMode:     &mode,
			AllowDoP:    &dop,
		}
	}
	if status == http.StatusOK {
		writeJSON(w, http.StatusOK, m)
		return
	}
	if res.Cause != nil {
		logRequestFailed(r, status, code, res.Cause)
	}
	writeJSON(w, status, outputDSDChoiceError{Error: code, Message: msg, OutputDSDChoiceAnswer: m})
}
