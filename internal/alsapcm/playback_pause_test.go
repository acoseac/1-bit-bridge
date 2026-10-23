package alsapcm_test

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsa/sysfstest"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// pausedRig is a rig playing a, a second of 24-bit content at 48 kHz, paused
// 100 ms in, with a pause release of a second.
func pausedRig(t *testing.T, opts ...func(*rigOptions)) (*rig, []int32) {
	t.Helper()
	r := newRig(t, append([]func(*rigOptions){func(o *rigOptions) { o.pauseRelease = time.Second }}, opts...)...)
	a := pattern(1, 48000, 24)
	r.files.add("a", wav(48000, 24, a))
	r.insert("a")
	r.play()
	r.k.Await(t, "the writer to fill the hardware buffer and wait on the DAC", runningAndWaiting)
	r.k.Play(t, 100*time.Millisecond)
	if err := r.eng.Pause(bg); err != nil {
		t.Fatalf("pause: %v", err)
	}
	r.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
	return r, a
}

// releaseDevice lets the paused sink's pause release pass on the fake
// clock, and waits for the engine to learn the device was released.
func (r *rig) releaseDevice() {
	r.t.Helper()
	r.k.Grant(2 * time.Second)
	r.k.Await(r.t, "the paused sink to close the device", func(v alsapcm.KernelView) bool { return !v.Open })
	r.k.Revoke()
	r.waitSnap("the device released", func(s *engine.Snapshot) bool { return s.DeviceReleased() })
}

// heardAll plays the rest and fails the test unless the DAC heard want,
// every frame once, in order.
func (r *rig) heardAll(f format.ALSAFormat, bits int, want []int32) {
	r.t.Helper()
	r.k.Free()
	r.waitStopped(engine.ReasonEndOfQueue)
	if got := unpack(r.t, r.k.Heard(), f, bits); len(got) != len(want) || sumOf(got) != sumOf(want) {
		r.t.Fatalf("the DAC heard %d samples, not the %d of the entry, each once, in order", len(got), len(want))
	}
}

// TestPlaybackPausesAndResumesContinued pins a pause that holds the device:
// the kernel's PAUSE, the position frozen, and the resume continued on the
// same open, every frame heard once.
func TestPlaybackPausesAndResumesContinued(t *testing.T) {
	r, a := pausedRig(t)
	v := r.k.View()
	if v.State != "PAUSED" || v.Pauses != 1 || !v.Open {
		t.Fatalf("paused: the stream is %s after %d PAUSEs (open %v)", v.State, v.Pauses, v.Open)
	}
	if err := r.eng.Play(bg); err != nil {
		t.Fatalf("resume: %v", err)
	}
	s := r.waitSnap("playing again", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	if how, why := s.LastResume(); how != engine.ResumeContinued || why != "" {
		t.Errorf("the resume went on %s (%q), want continued", how, why)
	}
	r.heardAll(format.S243LE, 24, a)
	if v := r.k.View(); v.Opens != 1 {
		t.Errorf("%d opens for a pause that held the device", v.Opens)
	}
}

// TestPlaybackReleasesAPausedDeviceAndReopensIt pins the pause release: once
// the pause release has passed, the sink closes the device itself, the
// backend reports it (DeviceReleased), and the resume opens it again,
// answered reopened, writing first the frames the DAC had not played: every
// frame heard once, across the two opens.
func TestPlaybackReleasesAPausedDeviceAndReopensIt(t *testing.T) {
	r, a := pausedRig(t)
	r.releaseDevice()
	if n := count(r.rec.events(), "device released", 0); n != 1 {
		t.Errorf("DeviceReleased %d times, want once", n)
	}
	if err := r.eng.Play(bg); err != nil {
		t.Fatalf("resume: %v", err)
	}
	s := r.waitSnap("playing again", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	if how, _ := s.LastResume(); how != engine.ResumeReopened {
		t.Errorf("the resume went on %s, want reopened", how)
	}
	r.heardAll(format.S243LE, 24, a)
	if v := r.k.View(); v.Opens != 2 || v.Closes != 2 {
		t.Errorf("%d opens and %d closes; want the release's close and the reopen", v.Opens, v.Closes)
	}
	var kinds []string
	for len(r.notes) > 0 {
		kinds = append(kinds, (<-r.notes).Kind.String())
	}
	if got := strings.Join(kinds, " "); got != "opened released reopened closed" {
		t.Errorf("the sink's notes: %q, want opened released reopened closed", got)
	}
}

// goBar is the GO bar of the Pi 5 tree (internal/alsa/testdata/sysfs), at
// card 5, the fake node's card.
const goBar = "sys/devices/platform/axi/1000120000.pcie/1f00200000.usb/xhci-hcd.0/usb1/1-1"

// TestPlaybackReopensOnARenumberedCard pins the reopen at the endpoint the
// binding resolves to now, with 4a's own resolve and check after open over
// a box's trees: the GO bar, bound by serial, plays on card 5, pauses and is
// let go; it is unplugged and plugged in again while the device is free and
// comes back as card 6; the resume locates it there, opens card 6's node,
// checks it is the bound unit, and plays every frame once.
func TestPlaybackReopensOnARenumberedCard(t *testing.T) {
	root := sysfstest.Build(t, filepath.Join("..", "alsa", "testdata", "sysfs", "pi5-go-bar.txt"))
	loc := playback.BindingLocator{
		Device: config.PlayerDevice{Transport: "usb", Binding: config.BindingSerial, VendorID: "20b1", ProductID: "3009",
			Serial: usbid.New("GOBAR-7Q2X-91KZ-4471")},
		Observer: binding.Observer{Proc: alsa.Reader{ProcRoot: filepath.Join(root, "proc")}, Sysfs: alsa.SysfsReader{Root: filepath.Join(root, "sys")}},
	}
	r, a := pausedRig(t, func(o *rigOptions) { o.locator = loc })
	r.releaseDevice()
	sysfstest.Apply(t, root, "unplug "+goBar+"\n"+
		"usbdev "+goBar+" vendor=20b1 product=3009 serial=GOBAR-7Q2X-91KZ-4471 devnum=3\n"+
		"usbcard 6 "+goBar+"/1-1:1.0\n"+
		"proccard 6 Audio \"iFi (by AMR) HD USB Audio\" usb=20b1:3009 pcm=0.0\n")
	r.k.Renumber(6)
	if err := r.eng.Play(bg); err != nil {
		t.Fatalf("resume on the renumbered card: %v", err)
	}
	s := r.waitSnap("playing again", func(s *engine.Snapshot) bool { return s.State() == engine.Playing })
	if how, _ := s.LastResume(); how != engine.ResumeReopened {
		t.Errorf("the resume went on %s, want reopened", how)
	}
	if v := r.k.View(); len(v.Opened) != 2 || v.Opened[0] != "/dev/snd/pcmC5D0p" || v.Opened[1] != "/dev/snd/pcmC6D0p" {
		t.Fatalf("opened %q; want card 5's node, then card 6's", v.Opened)
	}
	r.heardAll(format.S243LE, 24, a)
}

// TestPlaybackRefusesABusyResumeAndStaysPaused pins a resume whose reopen
// finds another process holding the device (MPD, say): refused, typed
// busy, the session left paused with the reason, and nothing lost: the next
// resume, once the device is free, reopens and plays every frame once.
func TestPlaybackRefusesABusyResumeAndStaysPaused(t *testing.T) {
	r, a := pausedRig(t)
	r.releaseDevice()
	r.k.Busy(true)
	err := r.eng.Play(bg)
	var re *playback.ResumeError
	if !errors.As(err, &re) || re.Code != "busy" || alsapcm.ReasonOf(err) != alsapcm.ReasonBusy {
		t.Fatalf("a resume of a busy device: %v; want a ResumeError, busy", err)
	}
	s := r.eng.Snapshot()
	if _, why := s.LastResume(); s.State() != engine.Paused || !strings.Contains(why, "busy") {
		t.Fatalf("after the refused resume: %s, the reason %q; want paused, busy", s.State(), why)
	}
	r.k.Busy(false)
	if err := r.eng.Play(bg); err != nil {
		t.Fatalf("resume once the device is free: %v", err)
	}
	r.heardAll(format.S243LE, 24, a)
}

// TestPlaybackRefusesAReopenTheCheckRefusesAndKeepsItsFrames pins a resume
// whose reopened node the check after open refuses: the node is closed with
// nothing written, the resume refused (wrong_device), the session paused,
// the frames kept; a later resume on the bound unit plays every one once.
func TestPlaybackRefusesAReopenTheCheckRefusesAndKeepsItsFrames(t *testing.T) {
	r, a := pausedRig(t)
	r.releaseDevice()
	before := r.k.View()
	r.unit.mu.Lock()
	r.unit.refuse = 1
	r.unit.mu.Unlock()
	err := r.eng.Play(bg)
	var re *playback.ResumeError
	if !errors.As(err, &re) || re.Code != "wrong_device" {
		t.Fatalf("a resume the check refused: %v; want a ResumeError, wrong_device", err)
	}
	v := r.k.View()
	if v.Open || v.Opens != before.Opens+1 || v.Closes != before.Closes+1 || v.GivenBytes != before.GivenBytes {
		t.Fatalf("the refused node: open %v, %d opens, %d closes, %d bytes given (had %d); want opened, closed, never written to",
			v.Open, v.Opens, v.Closes, v.GivenBytes, before.GivenBytes)
	}
	if s := r.eng.Snapshot(); s.State() != engine.Paused {
		t.Fatalf("after the refused reopen: %s, want paused", s.State())
	}
	if err := r.eng.Play(bg); err != nil {
		t.Fatalf("resume on the bound unit: %v", err)
	}
	r.heardAll(format.S243LE, 24, a)
}

// timers is a Config.AfterFunc a test fires by hand.
type timers struct {
	mu sync.Mutex
	f  []func()
}

func (tm *timers) afterFunc(_ time.Duration, f func()) func() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.f = append(tm.f, f)
	return func() bool { return true }
}

func (tm *timers) fire(t *testing.T) {
	t.Helper()
	tm.mu.Lock()
	fs := tm.f
	tm.f = nil
	tm.mu.Unlock()
	if len(fs) == 0 {
		t.Fatal("no pause release timer was armed")
	}
	for _, f := range fs {
		f()
	}
}

// TestPlaybackPausesBetweenTwoTransportsWithNoDeviceHeld pins a pause that
// finds no device to pause: a's sink plays out its last frames for the
// change to b's transport, which no pause interrupts, so the session pauses
// after them with the device let go; it reports the release once the pause
// release passes; and the resume opens b's sink itself, answered reopened,
// and b plays: the DAC heard a in one open and b in the next.
func TestPlaybackPausesBetweenTwoTransportsWithNoDeviceHeld(t *testing.T) {
	tm := &timers{}
	r := newRig(t, func(o *rigOptions) { o.pauseRelease, o.afterFunc = time.Second, tm.afterFunc })
	a, b := pattern(1, 1000, 16), pattern(2, 3000, 24)
	r.files.add("a", wav(44100, 16, a))
	r.files.add("b", wav(48000, 24, b))
	r.insert("a", "b")
	r.play()
	// a, 1000 frames, is shorter than the hardware buffer: the change of
	// transport drains it, and the kernel's drain waits for time.
	r.k.Await(t, "a's sink to play out its last frames", func(v alsapcm.KernelView) bool { return v.Drains == 1 && v.Starved })
	if err := r.eng.Pause(bg); err != nil {
		t.Fatalf("a pause during the last frames of a transport: %v", err)
	}
	r.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
	r.k.Free()
	r.k.Await(t, "a's sink to close", func(v alsapcm.KernelView) bool { return v.Closes == 1 && !v.Open })
	tm.fire(t)
	r.waitSnap("the device released", func(s *engine.Snapshot) bool { return s.DeviceReleased() })
	if v := r.k.View(); v.Opens != 1 {
		t.Fatalf("b's sink opened during the pause (%d opens)", v.Opens)
	}
	if err := r.eng.Play(bg); err != nil {
		t.Fatalf("resume: %v", err)
	}
	s := r.waitStopped(engine.ReasonEndOfQueue)
	if how, _ := s.LastResume(); how != engine.ResumeReopened {
		t.Errorf("the resume went on %s, want reopened", how)
	}
	v := r.k.View()
	if v.Opens != 2 || len(v.OpenAt) != 2 {
		t.Fatalf("%d opens, want a's and b's", v.Opens)
	}
	given := r.k.Given()
	if got := unpack(t, given[:v.OpenAt[1]], format.S16LE, 16); sumOf(got) != sumOf(a) {
		t.Errorf("a's open was given %d samples, not a's", len(got))
	}
	if got := unpack(t, given[v.OpenAt[1]:], format.S243LE, 24); sumOf(got) != sumOf(b) {
		t.Errorf("b's open was given %d samples, not b's", len(got))
	}
}

// TestPlaybackPausesTheTransportCurrentWhenItDecides pins a pause that a
// sink playing out its last frames refuses: by the time the pause decides,
// the producer may have drained that sink, opened the next transport's and
// started it, and the pause then acts on the sink current at its decision,
// never on the one it found first. The seam holds the pause between the
// refusal and its decision while a's drain ends and b's stream starts.
func TestPlaybackPausesTheTransportCurrentWhenItDecides(t *testing.T) {
	parked, letGo := make(chan struct{}), make(chan struct{})
	var parkOnce, goOnce sync.Once
	d := newDirectWith(t, nil, func(cfg *playback.Config) {
		cfg.Seams = &playback.Seams{PauseRefused: func() {
			parkOnce.Do(func() {
				close(parked)
				<-letGo
			})
		}}
	})
	release := func() { goOnce.Do(func() { close(letGo) }) }
	t.Cleanup(release)
	d.files.add("a", wav(44100, 16, pattern(1, 1000, 16)))
	d.files.add("b", wav(48000, 24, pattern(2, 480000, 24)))
	if err := d.start(1, "a"); err != nil {
		t.Fatal(err)
	}
	next := occurrence(102, 2, "b")
	if err := d.be.Prepare(bg, 1, 101, &next); err != nil {
		t.Fatal(err)
	}
	d.k.Await(t, "a's sink to play out its last frames", func(v alsapcm.KernelView) bool { return v.Drains == 1 && v.Starved })
	paused := make(chan error, 1)
	go func() { paused <- d.be.Pause(bg, 1) }()
	receiveIn(t, parked, "the pause, refused by a's sink, held before it decides")
	// a's drain ends; the producer opens b's sink, fills it and starts it.
	d.k.Grant(100 * time.Millisecond)
	d.k.Await(t, "b's stream to start", func(v alsapcm.KernelView) bool { return v.Opens == 2 && v.State == "RUNNING" })
	release()
	if err := receiveIn(t, paused, "the pause"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if v := d.k.View(); v.State != "PAUSED" {
		t.Errorf("after the pause b's stream is %s, want PAUSED: the pause acted on a's sink, which was gone", v.State)
	}
	d.release(1)
	d.k.CheckClosedEach(t)
}

// TestPlaybackReleaseWaitsForAResumesOpenBetweenTransports pins the release
// of a session whose resume is opening the next transport's sink: paused
// between two transports, the resume opens b's sink itself, and a Stop while
// that open is under way (b's node open, its check after open held, as one
// reading a slow sysfs is, so no sink exists yet to close the node with the
// session's life) releases the session. The release is not acknowledged
// while the node is open: it waits for the resume's open, which closes what
// it opened, unwritten, before the session lets go.
func TestPlaybackReleaseWaitsForAResumesOpenBetweenTransports(t *testing.T) {
	tm := &timers{}
	r := newRig(t, func(o *rigOptions) { o.pauseRelease, o.afterFunc = time.Second, tm.afterFunc })
	r.files.add("a", wav(44100, 16, pattern(1, 1000, 16)))
	r.files.add("b", wav(48000, 24, pattern(2, 3000, 24)))
	r.insert("a", "b")
	r.play()
	r.k.Await(t, "a's sink to play out its last frames", func(v alsapcm.KernelView) bool { return v.Drains == 1 && v.Starved })
	if err := r.eng.Pause(bg); err != nil {
		t.Fatalf("a pause during the last frames of a transport: %v", err)
	}
	r.waitSnap("paused", func(s *engine.Snapshot) bool { return s.State() == engine.Paused })
	r.k.Free()
	r.k.Await(t, "a's sink to close", func(v alsapcm.KernelView) bool { return v.Closes == 1 && !v.Open })
	held, letGo := r.unit.holdNextAdmit()
	t.Cleanup(letGo)
	resumed := make(chan error, 1)
	go func() { resumed <- r.eng.Play(bg) }()
	receiveIn(t, held, "the check after the resume's open of b's sink to hold, the node open")
	if v := r.k.View(); !v.Open || v.Opens != 2 {
		t.Fatalf("the resume's open: open %v after %d opens, want b's node open", v.Open, v.Opens)
	}
	if err := r.eng.Stop(bg); err != nil {
		t.Fatal(err)
	}
	// Acknowledged, the session holds nothing; past its deadline the engine
	// holds the release unconfirmed while the open goes on.
	s := r.waitSnap("the release acknowledged, or past its deadline", func(s *engine.Snapshot) bool { return s.State() != engine.Releasing })
	if _, unconfirmed := s.ReleaseUnconfirmed(); !unconfirmed && r.k.View().Open {
		t.Fatal("the release was acknowledged while the resume's open held b's node open")
	}
	letGo()
	receiveIn(t, resumed, "the resume's play command to be answered")
	r.waitSnap("the release acknowledged", func(s *engine.Snapshot) bool {
		_, unconfirmed := s.ReleaseUnconfirmed()
		return s.State() == engine.Stopped && !unconfirmed
	})
	if v := r.k.View(); v.Open || v.Closes != 2 || v.GivenBytes != 1000*2*2 {
		t.Errorf("after the release: open %v, %d closes, %d bytes given; want b's node closed unwritten, a's frames alone given", v.Open, v.Closes, v.GivenBytes)
	}
}
