package alsapcm

import (
	"errors"
	"strconv"
	"strings"
)

// Reason is a stable code for why the backend refused an endpoint or
// stopped a stream.
type Reason string

const (
	// ReasonUnsupported is a platform the backend does not run on: anything
	// but linux/amd64 and linux/arm64.
	ReasonUnsupported Reason = "unsupported_platform"
	// ReasonBadEndpoint is a card or device number no endpoint can have.
	ReasonBadEndpoint Reason = "bad_endpoint"
	// ReasonBadConfig is a request the backend does not make: a format it
	// has no packer for, or a rate out of range.
	ReasonBadConfig Reason = "bad_config"
	// ReasonNoDevice is a playback node that does not exist.
	ReasonNoDevice Reason = "no_such_device"
	// ReasonPermission is a node this process may not open.
	ReasonPermission Reason = "permission_denied"
	// ReasonBusy is a node another process holds open: MPD playing, most
	// often. The backend never stops it.
	ReasonBusy Reason = "busy"
	// ReasonNotPCM is a node that is not the playback endpoint asked for:
	// not an ALSA device, or one the kernel says is another card, device
	// or direction.
	ReasonNotPCM Reason = "not_the_endpoint"
	// ReasonWrongDevice is a node the caller's check after open refused
	// (SinkConfig.Admit): the endpoint is now another unit than the one
	// the binding resolved, renumbered or swapped between the resolve and
	// the open. Nothing was written to it.
	ReasonWrongDevice Reason = "wrong_device"
	// ReasonNotLocated is a reopen whose SinkConfig.Locate named no
	// endpoint: the binding resolves to no unit now (missing, ambiguous, the
	// box not read). Nothing was opened.
	ReasonNotLocated Reason = "not_located"
	// ReasonProtocol is a kernel PCM protocol this backend does not open.
	ReasonProtocol Reason = "unsupported_protocol"
	// ReasonNotSupported is a rate, channel count or format the hardware
	// does not take.
	ReasonNotSupported Reason = "parameters_not_supported"
	// ReasonMismatch is a committed rate, channel count or format other
	// than the one asked for.
	ReasonMismatch Reason = "parameters_mismatch"
	// ReasonXrun is an underrun: the buffer ran empty (EPIPE). The stream
	// has stopped.
	ReasonXrun Reason = "xrun"
	// ReasonSuspended is a stream the system suspended (ESTRPIPE).
	ReasonSuspended Reason = "suspended"
	// ReasonDisconnected is a device that went away: unplugged, or its
	// driver unbound.
	ReasonDisconnected Reason = "disconnected"
	// ReasonStalled is a running stream that took no frames for as long as
	// a write waits for room (stallTimeout).
	ReasonStalled Reason = "stalled"
	// ReasonBadState is an operation the stream's state does not allow.
	ReasonBadState Reason = "bad_state"
	// ReasonDrainTimeout is a drain that did not finish by its deadline;
	// the stream was dropped.
	ReasonDrainTimeout Reason = "drain_timeout"
	// ReasonCancelled is an operation whose context ended.
	ReasonCancelled Reason = "cancelled"
	// ReasonClosed is an operation on a closed Device.
	ReasonClosed Reason = "closed"
	// ReasonPanicked is a sink whose writer panicked: the panic was
	// recovered and logged, the device closed, and the sink ended.
	ReasonPanicked Reason = "panicked"
	// ReasonKernel is any other error the kernel returned.
	ReasonKernel Reason = "kernel_error"
)

// Error is a refusal or a failure of one operation on one endpoint.
type Error struct {
	Reason   Reason
	Op       string // "open", "hw_params", "write", "drain", …
	Endpoint Endpoint
	Detail   string
	// Err is what the kernel returned (an errno) or the context's error,
	// when there is one.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("alsapcm: ")
	b.WriteString(e.Endpoint.String())
	if e.Op != "" {
		b.WriteString(": " + e.Op)
	}
	b.WriteString(": " + string(e.Reason))
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	if e.Err != nil {
		b.WriteString(" (" + e.Err.Error() + ")")
	}
	return b.String()
}

// Unwrap returns the kernel's errno or the context's error.
func (e *Error) Unwrap() error { return e.Err }

// ReasonOf returns the Reason of the *Error in err's chain, or "" when
// there is none.
func ReasonOf(err error) Reason {
	var e *Error
	if errors.As(err, &e) {
		return e.Reason
	}
	return ""
}

// errno is a Linux errno value. The real kernel seam returns a failed
// call's errno as one, and the fake kernel the same values, so the
// classification in this package runs, and is tested, on every OS. The
// values are asm-generic's, which linux/amd64 and linux/arm64 share;
// TestErrnoValuesAreLinuxs checks them against golang.org/x/sys/unix.
type errno uintptr

const (
	errnoEPERM    errno = 1
	errnoENOENT   errno = 2
	errnoEINTR    errno = 4
	errnoENXIO    errno = 6
	errnoEBADF    errno = 9
	errnoEAGAIN   errno = 11
	errnoEACCES   errno = 13
	errnoEBUSY    errno = 16
	errnoENODEV   errno = 19
	errnoEINVAL   errno = 22
	errnoENOTTY   errno = 25
	errnoEPIPE    errno = 32
	errnoENOSYS   errno = 38
	errnoELOOP    errno = 40
	errnoEBADFD   errno = 77
	errnoESTRPIPE errno = 86
)

// errnoNames names the errnos this package tells apart.
var errnoNames = map[errno]string{
	errnoEPERM:    "EPERM",
	errnoENOENT:   "ENOENT",
	errnoEINTR:    "EINTR",
	errnoENXIO:    "ENXIO",
	errnoEBADF:    "EBADF",
	errnoEAGAIN:   "EAGAIN",
	errnoEACCES:   "EACCES",
	errnoEBUSY:    "EBUSY",
	errnoENODEV:   "ENODEV",
	errnoEINVAL:   "EINVAL",
	errnoENOTTY:   "ENOTTY",
	errnoEPIPE:    "EPIPE",
	errnoENOSYS:   "ENOSYS",
	errnoELOOP:    "ELOOP",
	errnoEBADFD:   "EBADFD",
	errnoESTRPIPE: "ESTRPIPE",
}

func (e errno) Error() string {
	if name, ok := errnoNames[e]; ok {
		return name
	}
	return "errno " + strconv.FormatUint(uint64(e), 10)
}

// errNotALSANode is what the real kernel seam's open returns for a node at
// an endpoint's path that is not an ALSA character device.
var errNotALSANode = errors.New("not an ALSA character device")
