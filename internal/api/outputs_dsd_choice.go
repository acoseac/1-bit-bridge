package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
)

// PUT /v1/outputs/{id}/dsd writes the bound DAC's DSD choice for the
// bridge's own player (B728 B9; the design's section 7.2): the body read whole
// within its bound and parsed strictly, then serve's player's write, whose
// outcome is the answer.

// BridgeDSDChooser is serve's player as PUT /v1/outputs/{id}/dsd asks it: a
// BridgeOutput that writes a choice. A player that writes none is answered
// as no output is, and /v1/health advertises no bridgeDSDChoice for it.
type BridgeDSDChooser interface {
	// OwnsOutput reports whether id is the id of the player's element as GET
	// /v1/outputs lists it now, reading its identity as the element does.
	OwnsOutput(id string) bool
	// ChooseDSD writes the choice and answers how it ended, with the bound
	// DAC's dsdDAC and dsdPlayback as the player published them then.
	ChooseDSD(ctx context.Context, req DSDChoiceRequest) DSDChoiceResult
}

// DSDChoiceRequest is a choice write's body, parsed.
type DSDChoiceRequest struct {
	DACKey               string
	BaseRevision         uint64
	Mode                 string
	MaxBitRate           int
	ConfirmDoP           bool
	FirstUseAcknowledged bool
	Interrupt            bool
}

// DSDChoiceResult is a choice write's outcome word and what the player
// published once it ended.
type DSDChoiceResult struct {
	Outcome     string
	DSDDAC      *OutputDSDDAC
	DSDPlayback *OutputDSDPlayback
}

// OutputDSDChoiceResponse is a written choice's answer: the player's dsdDAC
// and dsdPlayback, as GET /v1/outputs sends them in its element.
type OutputDSDChoiceResponse struct {
	DSDDAC      *OutputDSDDAC      `json:"dsdDAC,omitempty"`
	DSDPlayback *OutputDSDPlayback `json:"dsdPlayback"`
}

// outputDSDChoiceStale is a stale write's answer: the error envelope and the
// bound DAC's dsdDAC as the player publishes it, whose revision a write takes.
type outputDSDChoiceStale struct {
	Error   string        `json:"error"`
	Message string        `json:"message"`
	DSDDAC  *OutputDSDDAC `json:"dsdDAC,omitempty"`
}

// maxDSDChoiceBody bounds a choice's body, read whole before it is parsed.
const maxDSDChoiceBody = 4 << 10

// dsdChoiceModes and dsdChoiceCaps are the modes and caps a choice takes.
var (
	dsdChoiceModes = []string{"auto", "native", "dop", "convert", "off"}
	dsdChoiceCaps  = []int{0, 2822400, 5644800, 11289600}
)

// dsdChoiceKey is a dacKey's form: d1- and 16 lowercase hexadecimal digits.
var dsdChoiceKey = regexp.MustCompile(`^d1-[0-9a-f]{16}$`)

// dsdChooser is serve's player as the choice's route asks it: nil while the
// player's gate is shut, or while serve's player writes no choice.
func (s *Server) dsdChooser() BridgeDSDChooser {
	if !s.bridgeOutputOn() {
		return nil
	}
	c, _ := s.bridgeOutput.(BridgeDSDChooser)
	return c
}

// parseDSDChoice is body as a choice, or why it is none: one JSON object of
// the route's members alone, each of its type, dacKey, baseRevision and mode
// present, a mode and a cap the route takes, nothing after it. An explicit
// null is no member's value (B728 B9 round 2): decoded into a value, a null
// is taken as the member left out, so a client's null for confirmDoP,
// firstUseAcknowledged or interrupt would ask nothing it did not say, and the
// optional members are kept as their bytes and decoded only when they are
// not null.
func parseDSDChoice(body []byte) (DSDChoiceRequest, error) {
	var in struct {
		DACKey               *string         `json:"dacKey"`
		BaseRevision         *uint64         `json:"baseRevision"`
		Mode                 *string         `json:"mode"`
		MaxBitRate           json.RawMessage `json:"maxBitRate"`
		ConfirmDoP           json.RawMessage `json:"confirmDoP"`
		FirstUseAcknowledged json.RawMessage `json:"firstUseAcknowledged"`
		Interrupt            json.RawMessage `json:"interrupt"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return DSDChoiceRequest{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return DSDChoiceRequest{}, errors.New("data after the body")
	}
	var out DSDChoiceRequest
	for _, m := range []struct {
		raw json.RawMessage
		to  any
	}{{in.MaxBitRate, &out.MaxBitRate}, {in.ConfirmDoP, &out.ConfirmDoP}, {in.FirstUseAcknowledged, &out.FirstUseAcknowledged}, {in.Interrupt, &out.Interrupt}} {
		if err := optionalMember(m.raw, m.to); err != nil {
			return DSDChoiceRequest{}, err
		}
	}
	switch {
	case in.DACKey == nil || !dsdChoiceKey.MatchString(*in.DACKey):
		return DSDChoiceRequest{}, errors.New("dacKey is not a DAC key")
	case in.BaseRevision == nil:
		return DSDChoiceRequest{}, errors.New("baseRevision is missing")
	case in.Mode == nil || !slices.Contains(dsdChoiceModes, *in.Mode):
		return DSDChoiceRequest{}, errors.New("mode is not a mode")
	case !slices.Contains(dsdChoiceCaps, out.MaxBitRate):
		return DSDChoiceRequest{}, errors.New("maxBitRate is neither 0 nor a DSD rate")
	}
	out.DACKey, out.BaseRevision, out.Mode = *in.DACKey, *in.BaseRevision, *in.Mode
	return out, nil
}

// optionalMember decodes an optional member's bytes into to: nothing for a
// member left out, a refusal for an explicit null, and otherwise the value,
// of to's type alone.
func optionalMember(raw json.RawMessage, to any) error {
	switch {
	case raw == nil:
		return nil
	case bytes.Equal(bytes.TrimSpace(raw), []byte("null")):
		return errors.New("an optional member is null")
	}
	return json.Unmarshal(raw, to)
}

// putOutputDSD is PUT /v1/outputs/{id}/dsd: 404 local_outputs_not_supported
// while neither of GET /v1/outputs' gates holds, 404 output_not_found for an
// id that is not the player's element's now, every id while the player's
// gate is shut, then 400 bad_request for a body past its bound or that is not
// a choice, read and parsed before the player is asked anything; then the
// player's outcome, each its status and its word.
func (s *Server) putOutputDSD(w http.ResponseWriter, r *http.Request) {
	c := s.dsdChooser()
	if c == nil {
		if !s.bridgeOutputOn() && (s.localOutputs == nil || !s.localOutputs.Active(r.Context())) {
			writeError(w, http.StatusNotFound, "local_outputs_not_supported",
				"local outputs are not enabled on this bridge")
			return
		}
		writeError(w, http.StatusNotFound, "output_not_found",
			"no output with that id on this bridge takes a DSD choice")
		return
	}
	if !c.OwnsOutput(r.PathValue("id")) {
		writeError(w, http.StatusNotFound, "output_not_found",
			"no output with that id on this bridge takes a DSD choice")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDSDChoiceBody+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the body could not be read")
		return
	}
	if len(body) > maxDSDChoiceBody {
		writeError(w, http.StatusBadRequest, "bad_request", "the body is larger than a DSD choice")
		return
	}
	req, err := parseDSDChoice(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the body is not a DSD choice")
		return
	}
	res := c.ChooseDSD(r.Context(), req)
	switch res.Outcome {
	case "written":
		writeJSON(w, http.StatusOK, OutputDSDChoiceResponse{DSDDAC: dsdDACLists(res.DSDDAC), DSDPlayback: dsdPlaybackLists(res.DSDPlayback)})
	case "stale":
		writeJSON(w, http.StatusConflict, outputDSDChoiceStale{Error: "stale",
			Message: "the DAC's choice was written since the revision the write was made from", DSDDAC: dsdDACLists(res.DSDDAC)})
	case "bad_request":
		writeError(w, http.StatusBadRequest, "bad_request", "the body is not a DSD choice")
	case "dac_unavailable":
		writeError(w, http.StatusConflict, "dac_unavailable", "the player has no fresh reading of a bound DAC now")
	case "dac_changed":
		writeError(w, http.StatusConflict, "dac_changed", "dacKey is not the bound DAC's")
	case "mode_not_offered":
		writeError(w, http.StatusConflict, "mode_not_offered", "the DAC offers the mode at no rate")
	case "dop_confirmation_required":
		writeError(w, http.StatusConflict, "dop_confirmation_required", "DoP at a rate that needs the user's confirmation, without confirmDoP")
	case "first_use_unacknowledged":
		writeError(w, http.StatusConflict, "first_use_unacknowledged", "the transport's first use is not acknowledged, without firstUseAcknowledged")
	case "would_interrupt":
		writeError(w, http.StatusConflict, "would_interrupt", "the write would interrupt what the player plays, without interrupt")
	case "state_unreadable":
		writeError(w, http.StatusConflict, "state_unreadable", "the player's DSD state could not be read and is not written over until it is moved")
	case "persist_failed":
		writeError(w, http.StatusInternalServerError, "persist_failed", "the DSD choice was not written: nothing changed")
	case "persist_uncertain":
		writeError(w, http.StatusInternalServerError, "persist_uncertain", "the DSD choice reached the state's file and is not known to be durable")
	default:
		writeError(w, http.StatusInternalServerError, "internal", "the bridge encountered an internal error")
	}
}
