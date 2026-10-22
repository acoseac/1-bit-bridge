//go:build linux

package hotplug

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// kernelGroup is the multicast group the kernel broadcasts its uevents on.
// udev rebroadcasts them, after its rules ran, on group 2 ("libudev"),
// which the watcher never joins: it hears the kernel itself.
const kernelGroup = 1

// receiveBuffer is the receive buffer the socket asks for. A hub plugged in
// with several devices behind it sends hundreds of events at once, which
// overflows the default (rmem_default); SO_RCVBUFFORCE passes rmem_max
// where the process may (CAP_NET_ADMIN), and SO_RCVBUF is capped at it.
const receiveBuffer = 4 << 20

// openKernelSource opens the kernel's uevent socket and reads it until ctx
// ends. A socket that cannot be opened (a container without the netlink
// family, a seccomp filter, a test) leaves a source that polls, saying why.
func openKernelSource(ctx context.Context) Source {
	sock, passcred, err := openLinuxSocket(ctx)
	if err != nil {
		return newPollingSource(ModePolling, err.Error())
	}
	return newKernelSource(ctx, sock, passcred)
}

// linuxSocket is a NETLINK_KOBJECT_UEVENT socket joined to the kernel's
// group, non-blocking, and an eventfd a cancel of its context writes to,
// so a wait for the kernel ends at once when the context does (as
// alsapcm's poll does).
type linuxSocket struct {
	fd, wake int
	oob      []byte
	// stop and fired are the cancel's AfterFunc: a write that has started
	// is waited for before the eventfd is closed, so it never lands on a
	// descriptor reused since.
	stop  func() bool
	fired chan struct{}
}

func openLinuxSocket(ctx context.Context) (*linuxSocket, bool, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, unix.NETLINK_KOBJECT_UEVENT)
	if err != nil {
		return nil, false, fmt.Errorf("the netlink socket could not be opened: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = unix.Close(fd)
		}
	}()
	if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, receiveBuffer) != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, receiveBuffer)
	}
	passcred := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PASSCRED, 1) == nil
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK, Groups: kernelGroup}); err != nil {
		return nil, false, fmt.Errorf("the netlink socket could not join the kernel's uevent group: %w", err)
	}
	wake, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return nil, false, fmt.Errorf("the netlink socket's wake-up could not be made: %w", err)
	}
	s := &linuxSocket{fd: fd, wake: wake, oob: make([]byte, unix.CmsgSpace(unix.SizeofUcred)), fired: make(chan struct{})}
	s.stop = context.AfterFunc(ctx, func() {
		defer close(s.fired)
		var one [8]byte
		binary.NativeEndian.PutUint64(one[:], 1)
		_, _ = unix.Write(s.wake, one[:])
	})
	ok = true
	return s, passcred, nil
}

// wait polls the socket and the eventfd with no timeout: the kernel's next
// message or the context's end. POLLERR is the kernel's ENOBUFS, which the
// next recv reports, so it is a reason to read, not to stop.
func (s *linuxSocket) wait(ctx context.Context) error {
	fds := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}, {Fd: int32(s.wake), Events: unix.POLLIN}}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := unix.Poll(fds, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return errnoFrom(err)
		}
		if fds[1].Revents != 0 {
			// Only the cancel writes the eventfd, which is never read, so
			// returning anything but an error here would spin.
			if err := ctx.Err(); err != nil {
				return err
			}
			return context.Canceled
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return errors.New("the netlink socket is no longer open")
		}
		return nil
	}
}

// recv reads one datagram with MSG_TRUNC, so n is its whole length even
// when it did not fit, and takes its sender's port id and credentials.
func (s *linuxSocket) recv(buf []byte) (received, error) {
	n, oobn, flags, from, err := unix.Recvmsg(s.fd, buf, s.oob, unix.MSG_TRUNC|unix.MSG_DONTWAIT)
	if err != nil {
		return received{}, errnoFrom(err)
	}
	r := received{n: n, truncated: n > len(buf) || flags&unix.MSG_TRUNC != 0, pid: ^uint32(0)}
	if sa, ok := from.(*unix.SockaddrNetlink); ok {
		r.pid = sa.Pid
	}
	if oobn > 0 && flags&unix.MSG_CTRUNC == 0 {
		if msgs, err := unix.ParseSocketControlMessage(s.oob[:oobn]); err == nil {
			for i := range msgs {
				if c, err := unix.ParseUnixCredentials(&msgs[i]); err == nil {
					r.cred = &credentials{pid: c.Pid, uid: c.Uid, gid: c.Gid}
				}
			}
		}
	}
	return r, nil
}

func (s *linuxSocket) close() error {
	if !s.stop() {
		<-s.fired
	}
	_ = unix.Close(s.wake)
	return unix.Close(s.fd)
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
