//go:build linux && (amd64 || arm64)

package alsapcm

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// alsaMajor is the character-device major of every ALSA node
// (CONFIG_SND_MAJOR, include/sound/core.h).
const alsaMajor = 116

// Open opens the endpoint's playback node and negotiates cfg with the
// kernel: the protocol and identity checks, rate, two channels and format
// exactly, a buffer near cfg.BufferTime, the software parameters, PREPARE.
// The Device it returns is prepared: its first writes fill the buffer, and
// the stream starts once the buffer is full (Params.StartThreshold). Every
// refusal is an *Error, and a refused open leaves nothing open.
func Open(ep Endpoint, cfg Config) (*Device, error) {
	if err := guarded(ep); err != nil {
		return nil, err
	}
	return open(linuxSys{}, time.Now, ep, cfg)
}

// Supported reports whether this build can open an endpoint at all.
func Supported() bool { return true }

// linuxSys is the kernel itself, through golang.org/x/sys/unix: no cgo.
type linuxSys struct{}

func (linuxSys) open(path string) (int, error) {
	// The node is looked at before it is opened, so nothing but an ALSA
	// device is ever opened, and looked at again once it is, so what was
	// opened is what was looked at.
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		return -1, errnoFrom(err)
	}
	if !isALSANode(&st) {
		return -1, errNotALSANode
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, errnoFrom(err)
	}
	if err := unix.Fstat(fd, &st); err != nil || !isALSANode(&st) {
		_ = unix.Close(fd)
		if err != nil {
			return -1, errnoFrom(err)
		}
		return -1, errNotALSANode
	}
	return fd, nil
}

// isALSANode reports whether st is an ALSA character device.
func isALSANode(st *unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFCHR && unix.Major(uint64(st.Rdev)) == alsaMajor
}

func (linuxSys) ioctl(fd int, req uint32, arg unsafe.Pointer) error {
	// The pointer is converted in the call's own argument list, the one
	// place the compiler keeps what it points at alive and in place for
	// the length of the system call (unsafe.Pointer's rule 4).
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(arg)); e != 0 {
		return errno(e)
	}
	return nil
}

func (linuxSys) ioctlValue(fd int, req uint32, arg uintptr) error {
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), arg); e != 0 {
		return errno(e)
	}
	return nil
}

// poll waits on fd and on an eventfd of its own, which a cancel of ctx
// writes to, so a cancelled context ends the wait at once rather than at
// the timeout. The eventfd lives for this one call; a cancel that fires
// as the call returns is waited for before the eventfd is closed, so its
// write never lands on a reused descriptor.
func (linuxSys) poll(ctx context.Context, fd int, events int16, timeout time.Duration) (int16, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return 0, errnoFrom(err)
	}
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		var one [8]byte
		binary.NativeEndian.PutUint64(one[:], 1)
		_, _ = unix.Write(wake, one[:])
	})
	defer func() {
		if !stop() {
			<-fired
		}
		_ = unix.Close(wake)
	}()
	fds := []unix.PollFd{{Fd: int32(fd), Events: events}, {Fd: int32(wake), Events: unix.POLLIN}}
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if left < 0 {
			left = 0
		}
		n, err := unix.Poll(fds, pollMillis(left))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, errnoFrom(err)
		}
		if fds[1].Revents != 0 {
			return 0, ctx.Err()
		}
		if n == 0 {
			return 0, nil
		}
		return fds[0].Revents, nil
	}
}

// pollMillis is a timeout in poll's milliseconds, rounded up so a wait is
// never cut short, at most the largest poll takes.
func pollMillis(d time.Duration) int {
	ms := (d + time.Millisecond - 1) / time.Millisecond
	if ms > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(ms)
}

func (linuxSys) close(fd int) error {
	if err := unix.Close(fd); err != nil {
		return errnoFrom(err)
	}
	return nil
}

// errnoFrom turns an error from golang.org/x/sys/unix into this package's
// errno, keeping any other error as it is.
func errnoFrom(err error) error {
	var e unix.Errno
	if errors.As(err, &e) {
		return errno(e)
	}
	return err
}

func openNativeDevice(ep Endpoint, cfg Config) (*Device, error) {
	if e := guarded(ep); e != nil {
		return nil, e
	}
	return openNative(linuxSys{}, time.Now, ep, cfg)
}
