package decode

import (
	"errors"
	"fmt"
)

// Reason is a stable, user-presentable refusal code.
type Reason string

const (
	ReasonUnknownContainer    Reason = "unknown_container"
	ReasonLossy               Reason = "lossy_refused"
	ReasonFloatPCM            Reason = "float_pcm_refused"
	ReasonUnsupportedBits     Reason = "unsupported_bit_depth"
	ReasonUnsupportedChannels Reason = "unsupported_channel_count"
	ReasonUnsupportedComp     Reason = "unsupported_compression"
	ReasonTruncated           Reason = "truncated_or_corrupt"
	ReasonUnsupportedLayout   Reason = "unsupported_layout"
	ReasonNotYetSupported     Reason = "not_yet_supported" // ALAC/DSF/DFF in later PRs
)

// Refusal is a typed refusal the console and OpenHome layer can show.
type Refusal struct {
	Reason  Reason
	Message string
}

func (r *Refusal) Error() string {
	if r == nil {
		return "player decode: refused"
	}
	if r.Message == "" {
		return fmt.Sprintf("player decode: %s", r.Reason)
	}
	return fmt.Sprintf("player decode: %s: %s", r.Reason, r.Message)
}

// IsRefusal reports whether err is or wraps a *Refusal.
func IsRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}

// AsRefusal returns the *Refusal in err's chain, if any.
func AsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

func refuse(reason Reason, format string, args ...any) error {
	return &Refusal{Reason: reason, Message: fmt.Sprintf(format, args...)}
}
