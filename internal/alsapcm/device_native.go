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
			err = nativeCleanupError(err, d.releaseNative())
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
func (d *Device) releaseNative() error {
	if d.closed {
		return nil
	}
	d.closed = true
	var e error
	if d.hwSet {
		e = errors.Join(d.ioctlNone(ioctlDrop), d.ioctlNone(ioctlHWFree))
	}
	return errors.Join(e, d.sys.close(d.fd))
}
