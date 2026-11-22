package alsapcm

import (
	"errors"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// nativeFormats is deliberately inaccessible to PCM negotiation.
var nativeFormats = map[format.ALSAFormat]sampleFormat{
	format.DSDU16LE: {code: formatDSDU16LE, bytes: 2},
	format.DSDU32LE: {code: formatDSDU32LE, bytes: 4},
	format.DSDU32BE: {code: formatDSDU32BE, bytes: 4},
}

// NativeFrameBytes sizes native frames independently of the PCM packing table.
func (p Params) NativeFrameBytes() int { return nativeFormats[p.Format].bytes * p.Channels }

func openNative(s sys, now func() time.Time, ep Endpoint, cfg Config) (_ *Device, err error) {
	if err = ep.validate(); err != nil {
		return nil, err
	}
	sf, ok := nativeFormats[cfg.Format]
	if cfg.Format == format.S32LE {
		sf, ok = sampleFormats[format.S32LE]
	}
	if !ok {
		return nil, errors.New("native format required")
	}
	want, err := cfg.resolveSample(ep, sf)
	if err != nil {
		return nil, err
	}
	fd, err := s.open(ep.Path())
	if err != nil {
		return nil, openError(ep, err)
	}
	d := &Device{sys: s, now: now, fd: fd, native: true, params: Params{Endpoint: ep}}
	set := false
	defer func() {
		if !set {
			ce := releaseRecovered(d, cfg.log, "the native setup's close")
			if err = nativeCleanupError(err, ce); ce != nil {
				if r := recover(); r != nil { // a setup's panic goes on with its close's failure (B728 B12 PR 0)
					panic(&setupPanic{value: r, cleanup: ce})
				}
			}
		}
	}()
	if err = d.setUp(want); err != nil {
		return nil, err
	}
	set = true
	return d, nil
}

func nativeCleanupError(cause, cleanup error) error {
	if cleanup != nil {
		return errors.Join(cause, ErrReleaseUnconfirmed, cleanup)
	}
	return cause
}

// releaseNative preserves every teardown failure; PCM release is unchanged.
// The node is closed from a defer, once, whatever DROP and HW_FREE do; given
// recovered (releaseRecovered's), a panic of theirs and one of the close each
// answer as their failure, beside the other's (B728 B12 PR 0).
func (d *Device) releaseNative(recovered ...func(any) error) (err error) {
	if d.closed {
		return nil
	}
	d.closed = true
	answer := func(e *error) {
		if len(recovered) > 0 {
			if r := recover(); r != nil {
				*e = errors.Join(*e, recovered[0](r))
			}
		}
	}
	defer func() {
		err = errors.Join(err, func() (ce error) {
			defer answer(&ce)
			return d.sys.close(d.fd)
		}())
	}()
	defer answer(&err)
	if d.hwSet {
		return errors.Join(d.ioctlNone(ioctlDrop), d.ioctlNone(ioctlHWFree))
	}
	return nil
}
