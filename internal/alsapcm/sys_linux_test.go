//go:build linux && (amd64 || arm64)

package alsapcm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

// TestErrnoValuesAreLinuxs pins the errno values the classification uses,
// which are written out so it runs on every OS, to the kernel's own.
func TestErrnoValuesAreLinuxs(t *testing.T) {
	for _, c := range []struct {
		ours errno
		unix unix.Errno
	}{
		{errnoEPERM, unix.EPERM}, {errnoENOENT, unix.ENOENT}, {errnoEINTR, unix.EINTR},
		{errnoENXIO, unix.ENXIO}, {errnoEBADF, unix.EBADF}, {errnoEAGAIN, unix.EAGAIN},
		{errnoEACCES, unix.EACCES}, {errnoEBUSY, unix.EBUSY}, {errnoENODEV, unix.ENODEV},
		{errnoEINVAL, unix.EINVAL}, {errnoENOTTY, unix.ENOTTY}, {errnoEPIPE, unix.EPIPE},
		{errnoELOOP, unix.ELOOP}, {errnoEBADFD, unix.EBADFD}, {errnoESTRPIPE, unix.ESTRPIPE},
	} {
		if uintptr(c.ours) != uintptr(c.unix) {
			t.Errorf("%v is %d here and %d in the kernel", c.ours, uintptr(c.ours), uintptr(c.unix))
		}
	}
}

// TestPollBitsAreLinuxs pins the poll events the device reads to the
// kernel's.
func TestPollBitsAreLinuxs(t *testing.T) {
	if pollOut != unix.POLLOUT || pollErr != unix.POLLERR || pollHup != unix.POLLHUP || pollNval != unix.POLLNVAL {
		t.Errorf("poll bits %#x %#x %#x %#x, the kernel's %#x %#x %#x %#x",
			pollOut, pollErr, pollHup, pollNval, unix.POLLOUT, unix.POLLERR, unix.POLLHUP, unix.POLLNVAL)
	}
}

// TestLinuxPollEndsAtOnceWhenCancelled pins the real poll's cancel: a wait
// on a descriptor that never becomes ready returns the context's error, not
// a timeout, when the context is cancelled, however the cancel and the
// system call interleave; a later wait on a live context is not woken by
// it; and a descriptor that is ready reports its events.
func TestLinuxPollEndsAtOnceWhenCancelled(t *testing.T) {
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(p[0]); _ = unix.Close(p[1]) }()
	var s linuxSys

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		rev, err := s.poll(ctx, p[0], unix.POLLIN, time.Minute)
		if err == nil {
			err = errors.New("poll returned events " + pollEvents(rev).Error() + " without a cancel")
		}
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled poll returned %v, want context.Canceled", err)
	}

	rev, err := s.poll(context.Background(), p[0], unix.POLLIN, 10*time.Millisecond)
	if err != nil || rev != 0 {
		t.Errorf("a later poll on a live context: %v, %v; want a timeout", rev, err)
	}

	if _, err := unix.Write(p[1], []byte{1}); err != nil {
		t.Fatal(err)
	}
	rev, err = s.poll(context.Background(), p[0], unix.POLLIN, time.Minute)
	if err != nil || rev&unix.POLLIN == 0 {
		t.Errorf("a ready descriptor: %#x, %v; want POLLIN", rev, err)
	}

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := s.poll(cancelled, p[0], unix.POLLIN, time.Minute); !errors.Is(err, context.Canceled) {
		t.Errorf("a poll on a context already done: %v", err)
	}
}

// TestLinuxOpenRefusesANodeThatIsNotALSA pins that the real open looks at
// the node first: a character device that is not ALSA's, a regular file and
// a symlink are refused without being opened, and a missing node is ENOENT.
func TestLinuxOpenRefusesANodeThatIsNotALSA(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("/dev/null", link); err != nil {
		t.Fatal(err)
	}
	var s linuxSys
	for _, path := range []string{"/dev/null", file, link} {
		if fd, err := s.open(path); !errors.Is(err, errNotALSANode) {
			if err == nil {
				_ = unix.Close(fd)
			}
			t.Errorf("open(%s) = %v, want errNotALSANode", path, err)
		}
	}
	if _, err := s.open(filepath.Join(dir, "missing")); !errors.Is(err, errnoENOENT) {
		t.Errorf("a missing node: %v, want ENOENT", err)
	}
}

// TestAloopNegotiatesAndCloses runs the real backend against the kernel's
// snd-aloop virtual card when one is loaded and this user may open it
// (`modprobe snd-aloop`), and is skipped otherwise. It proves negotiation
// and lifecycle only: an exact open, a short write, a drain, a close that
// leaves the substream closed, and the busy refusal once every substream
// of the device is held. It proves nothing about a real DAC.
func TestAloopNegotiatesAndCloses(t *testing.T) {
	b, err := os.ReadFile("/proc/asound/cards")
	if err != nil {
		t.Skipf("no ALSA here: %v", err)
	}
	cards, _ := alsa.ParseCards(b)
	card := -1
	for _, c := range cards {
		if c.Driver == "Loopback" {
			card = c.Index
			break
		}
	}
	if card < 0 {
		t.Skip("no snd-aloop card: modprobe snd-aloop to run this")
	}
	ep := Endpoint{Card: card, Device: 0}
	d, err := Open(ep, Config{RateHz: 48000, Format: format.S16LE})
	if r := ReasonOf(err); r == ReasonPermission || r == ReasonBusy {
		t.Skipf("the loopback card is not free for this user: %v", err)
	}
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	p := d.Params()
	if p.RateHz != 48000 || p.Format != format.S16LE || p.Channels != 2 || p.RateNum != 48000 || p.RateDen != 1 {
		t.Errorf("committed %+v", p)
	}
	t.Logf("snd-aloop granted %d frames of buffer (%v) in %d periods of %d", p.BufferFrames, p.BufferTime(), p.Periods, p.PeriodFrames)
	silence := make([]byte, p.FrameBytes()*p.BufferFrames)
	if n, err := d.Write(context.Background(), silence); err != nil || n != p.BufferFrames {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if err := d.Drain(context.Background(), p.BufferTime()+2*time.Second); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var held []*Device
	defer func() {
		for _, h := range held {
			_ = h.Close()
		}
	}()
	for range 64 {
		h, err := Open(ep, Config{RateHz: 48000, Format: format.S16LE})
		if err != nil {
			if ReasonOf(err) != ReasonBusy {
				t.Fatalf("with %d substreams held: %v, want %s", len(held), err, ReasonBusy)
			}
			return
		}
		held = append(held, h)
	}
	t.Fatalf("opened %d substreams and none was busy", len(held))
}
