//go:build !(linux && (amd64 || arm64))

package alsapcm

import "runtime"

// Open refuses with ReasonUnsupported: the backend talks to the Linux
// kernel's ALSA interface, and only on linux/amd64 and linux/arm64, the
// two layouts its definitions are checked against. Nothing is opened.
func Open(ep Endpoint, cfg Config) (*Device, error) {
	if err := guarded(ep); err != nil {
		return nil, err
	}
	return nil, &Error{
		Reason:   ReasonUnsupported,
		Op:       "open",
		Endpoint: ep,
		Detail:   "the ALSA PCM backend runs on linux/amd64 and linux/arm64 only, and this is " + runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// Probe refuses with ReasonUnsupported, as Open does, every candidate not
// probed: nothing is opened. It is here so the packages every platform
// builds can call it (B728's B5 and B7), and it refuses what GuardNodeOpens
// guards first, as Open does.
func Probe(ep Endpoint, cfg ProbeConfig) (ProbeReport, error) {
	rep := notAsked(ep, cfg)
	if err := guarded(ep); err != nil {
		return rep, err
	}
	return rep, &Error{
		Reason:   ReasonUnsupported,
		Op:       "probe",
		Endpoint: ep,
		Detail:   "the ALSA PCM backend runs on linux/amd64 and linux/arm64 only, and this is " + runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// Supported reports whether this build can open an endpoint at all.
func Supported() bool { return false }

func openNativeDevice(ep Endpoint, cfg Config) (*Device, error) { return Open(ep, cfg) }
