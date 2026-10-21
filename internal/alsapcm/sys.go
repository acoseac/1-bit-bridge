package alsapcm

import (
	"context"
	"runtime"
	"time"
	"unsafe"
)

// sys is the kernel as a Device uses it: the open, ioctl, poll and close
// calls, and nothing else. linuxSys is the real one, on linux/amd64 and
// linux/arm64; the tests drive a fake kernel through the same four calls.
// An error from any of them is an errno, errNotALSANode from open, or the
// context's error from poll.
type sys interface {
	// open opens a PCM node for playback: read-write, non-blocking (a
	// node another process holds is EBUSY at once, never a wait),
	// close-on-exec, and only when it is an ALSA character device.
	open(path string) (int, error)
	// ioctl issues one request. arg points at the request's argument,
	// which the caller keeps alive across the call (ioctlOn); the kernel
	// keeps nothing of it after it returns.
	ioctl(fd int, req uint32, arg unsafe.Pointer) error
	// ioctlValue issues a request whose argument is a value the kernel reads
	// as the argument itself, never through a pointer: PAUSE's 1 or 0. It
	// keeps that value out of an unsafe.Pointer, which must only ever hold
	// an address.
	ioctlValue(fd int, req uint32, arg uintptr) error
	// poll waits until fd has one of events, or an error condition, which
	// it reports whether asked or not (revents); until timeout passes
	// (revents 0, no error); or until ctx is done (ctx.Err()), which ends
	// the wait at once.
	poll(ctx context.Context, fd int, events int16, timeout time.Duration) (int16, error)
	close(fd int) error
}

// The poll events, with Linux's values (TestPollBitsAreLinuxs).
const (
	pollOut  int16 = 0x4
	pollErr  int16 = 0x8
	pollHup  int16 = 0x10
	pollNval int16 = 0x20
)

// ioctlOn issues req on d's node with arg pointing at v, and keeps v alive
// until the kernel has returned. It is the one way this package passes a Go
// pointer to the kernel.
func ioctlOn[T any](d *Device, req uint32, v *T) error {
	err := d.sys.ioctl(d.fd, req, unsafe.Pointer(v))
	runtime.KeepAlive(v)
	return err
}

// ioctlNone issues a request that takes no argument.
func (d *Device) ioctlNone(req uint32) error {
	return d.sys.ioctl(d.fd, req, nil)
}
