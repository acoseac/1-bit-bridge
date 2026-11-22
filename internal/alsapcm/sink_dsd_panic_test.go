package alsapcm

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
)

// A panic on the native sink's goroutines stays in the player (B728 B12 PR
// 0): its writer's, its opening worker's, and one in any close of its node,
// each recovered on the goroutine it came on, typed ReasonPanicked and logged
// once, never the process's end. A test cannot recover a panic on another
// goroutine, which ends the test binary: each case runs in a child of its
// test (RunEachCaseInAChild), and the parent reads from the child's output a
// panic that escaped.

// childCaseEnv names the case a child of a test that runs its cases apart
// runs.
const childCaseEnv = "BRIDGE_B12_PR0_CHILD_CASE"

// closePanicValue is what a close's panic carries beside sinkPanicValue, a
// second value no line of the log may show.
const closePanicValue = "a value the close's panic carries: /srv/music/Secret/Other.dsf"

// errCloseFailed is a close that closed the node and answered an error.
var errCloseFailed = errors.New("the node's close failed")

// panicLine is the start of the line Go writes as a panic ends a process.
var panicLine = regexp.MustCompile(`(?m)^panic: `)

// RunEachCaseInAChild runs each of cases in a child process of the test named
// name: the test binary run again with that test alone and the case named in
// its environment, which the child runs as a subtest. A child a panic ended
// fails its case, saying where the panic escaped; so does a child that timed
// out, failed or ran no case, and one that skipped skips it.
func RunEachCaseInAChild(t *testing.T, name string, cases map[string]func(t *testing.T)) {
	t.Helper()
	if c := os.Getenv(childCaseEnv); c != "" {
		run, ok := cases[c]
		if !ok {
			t.Fatalf("%s has no case %q", name, c)
		}
		t.Run(c, run)
		return
	}
	for _, c := range slices.Sorted(maps.Keys(cases)) {
		t.Run(c, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.v", "-test.count=1", "-test.timeout=2m", "-watchdog="+watchdog.String())
			cmd.Env = append(os.Environ(), childCaseEnv+"="+c)
			out, err := cmd.CombinedOutput()
			text, sub := string(out), name+"/"+strings.ReplaceAll(c, " ", "_")
			switch {
			case strings.Contains(text, "panic: test timed out"):
				t.Fatalf("the child timed out:\n%s", text)
			case panicLine.MatchString(text):
				t.Fatalf("the panic escaped %s and ended the process:\n%s", escapedFrom(text), text)
			case strings.Contains(text, "--- SKIP: "+sub+" "):
				t.Skipf("the child skipped the case:\n%s", text)
			case err != nil || !strings.Contains(text, "--- PASS: "+sub+" "):
				t.Fatalf("the child failed (%v):\n%s", err, text)
			}
		})
	}
}

// escapedFrom names the goroutine a panic that ended a child escaped from,
// by the frames of its trace.
func escapedFrom(trace string) string {
	switch {
	case strings.Contains(trace, "(*DSDSink).loopNative"):
		return "the native sink's writer (loopNative)"
	case strings.Contains(trace, "openNativeBounded.func"):
		return "the native sink's opening worker (openNativeBounded)"
	}
	return "a goroutine of the test"
}

// nativeLog keeps every record a native sink logs (logLines) and closes
// logged at the first line of a recovered panic.
type nativeLog struct {
	logLines
	once   sync.Once
	logged chan struct{}
}

func newNativeLog() *nativeLog { return &nativeLog{logged: make(chan struct{})} }

func (l *nativeLog) Handle(ctx context.Context, r slog.Record) error {
	err := l.logLines.Handle(ctx, r)
	if r.Message == nativePanicMessage {
		l.once.Do(func() { close(l.logged) })
	}
	return err
}

func (l *nativeLog) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *nativeLog) WithGroup(string) slog.Handler      { return l }

// panicsLogged requires a line of a recovered panic for each of where, in
// that order, each naming it, the endpoint and its stack, and no line of the
// log carrying a value a panic carried.
func (l *nativeLog) panicsLogged(t *testing.T, where ...string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []string
	for _, line := range l.lines {
		if strings.Contains(line, nativePanicMessage) {
			found = append(found, line)
		}
		if strings.Contains(line, sinkPanicValue) || strings.Contains(line, closePanicValue) {
			t.Errorf("a line of the log carries a panic's value: %s", line)
		}
	}
	if len(found) != len(where) {
		t.Fatalf("the log holds %d lines of a recovered panic, want %d, one each for %q: %q", len(found), len(where), where, found)
	}
	for i, w := range where {
		if !strings.Contains(found[i], "where="+w+" ") || !strings.Contains(found[i], "endpoint="+testEndpoint.String()) || !strings.Contains(found[i], "stack=") {
			t.Errorf("line %d of the recovered panics is %s; want it naming %q, its endpoint and its stack", i+1, found[i], w)
		}
	}
}

// neverRan says whether e answers a resume as one the writer never ran
// (errNativeActivationCancelled): a resume the writer's panic cut short ran,
// and answers that failure.
func neverRan(e error) bool { return errors.Is(e, errNativeActivationCancelled) }

// noValue fails the test when e names a value a panic carried.
func noValue(t *testing.T, e error, what string) {
	t.Helper()
	if e != nil && (strings.Contains(e.Error(), sinkPanicValue) || strings.Contains(e.Error(), closePanicValue)) {
		t.Errorf("%s names a panic's value: %v", what, e)
	}
}

// ioctlPanic is the fake's node whose next ioctl of req, once armed, panics
// with sinkPanicValue before it reaches the kernel.
type ioctlPanic struct {
	sys
	req   uint32
	armed atomic.Bool
}

func (p *ioctlPanic) ioctl(fd int, req uint32, arg unsafe.Pointer) error {
	if req == p.req && p.armed.CompareAndSwap(true, false) {
		panic(sinkPanicValue)
	}
	return p.sys.ioctl(fd, req, arg)
}

// panicSink opens a native sink on k with c, logging to l, its life the
// background's; the test's end revokes it.
func panicSink(t *testing.T, k *fakeKernel, c DSDConfig, l *nativeLog) *DSDSink {
	t.Helper()
	return panicSinkOn(t, k, k, c, l)
}

// panicSinkOn is panicSink with the node reached through node.
func panicSinkOn(t *testing.T, k *fakeKernel, node sys, c DSDConfig, l *nativeLog) *DSDSink {
	t.Helper()
	c.Log = slog.New(l)
	s, e := openDSDSink(context.Background(), c, nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(node, k.now, ep, c) }, now: k.now, at: k.nativeAt})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Revoke)
	return s
}

// errDropFailed is the errno a case has DROP answer: EIO, asm-generic's, an
// errno this package does not tell apart.
const errDropFailed errno = 5

// panickedErrors walks e's tree through both unwrap forms and returns each
// distinct *Error in it, by identity, that is itself typed ReasonPanicked:
// one a recovered panic answers. Two panics' errors carry the same details
// and the same ErrPanicked, so errors.Is cannot tell one from two.
func panickedErrors(e error) []*Error {
	var out []*Error
	seen := map[*Error]bool{}
	var walk func(error)
	walk = func(e error) {
		if pe, ok := e.(*Error); ok && pe.Reason == ReasonPanicked && !seen[pe] {
			seen[pe] = true
			out = append(out, pe)
		}
		switch u := e.(type) {
		case interface{ Unwrap() []error }:
			for _, c := range u.Unwrap() {
				walk(c)
			}
		case interface{ Unwrap() error }:
			if c := u.Unwrap(); c != nil {
				walk(c)
			}
		}
	}
	if e != nil {
		walk(e)
	}
	return out
}

// cleanupPanicked requires s's cleanup to say a close that panicked: left
// unconfirmed, typed ReasonPanicked, naming no value.
func cleanupPanicked(t *testing.T, s *DSDSink) {
	t.Helper()
	e := s.CleanupResult()
	if !errors.Is(e, ErrReleaseUnconfirmed) || ReasonOf(e) != ReasonPanicked || !errors.Is(e, ErrPanicked) {
		t.Errorf("the cleanup is %v; want a close that panicked, unconfirmed and typed %s", e, ReasonPanicked)
	}
	noValue(t, e, "the cleanup")
}

// endedTouchingNothing requires s's writer to have ended, every open of the
// node closed once and nothing after, and a command after the end to answer
// it, the panic and, where unconfirmed, the cleanup left unconfirmed, with no
// call to the kernel.
func endedTouchingNothing(t *testing.T, k *fakeKernel, s *DSDSink, unconfirmed bool) {
	t.Helper()
	await(t, s.Done(), "the writer's end")
	k.checkClosedEach(t)
	var calls, opens int
	k.inspect(func(k *fakeKernel) { calls, opens = len(k.calls), k.opens })
	e := s.Pause(context.Background())
	if !errors.Is(e, ErrPanicked) || errors.Is(e, ErrReleaseUnconfirmed) != unconfirmed {
		t.Errorf("a pause after the end answered %v; want the end, the panic and, unconfirmed %v, its cleanup", e, unconfirmed)
	}
	noValue(t, e, "a pause after the end")
	k.inspect(func(k *fakeKernel) {
		if len(k.calls) != calls || k.opens != opens {
			t.Errorf("a pause after the end reached the kernel: %d calls and %d opens, were %d and %d", len(k.calls), k.opens, calls, opens)
		}
	})
}

// reopenRefusedItsClosePanicking resumes s, released, whose reopen the case
// has refused and whose reopened node's close then panics: the resume
// answers the refusal beside the close's panic, unconfirmed, and the sink
// ends as a close that panicked ends it.
func reopenRefusedItsClosePanicking(t *testing.T, k *fakeKernel, s *DSDSink, l *nativeLog) {
	t.Helper()
	k.inspect(func(k *fakeKernel) { k.closePanic = closePanicValue })
	e := s.Resume(context.Background())
	if !errors.Is(e, ErrReleaseUnconfirmed) || !errors.Is(e, ErrPanicked) {
		t.Errorf("the resume answered %v; want its refusal beside its close's panic, unconfirmed", e)
	}
	noValue(t, e, "the resume")
	cleanupPanicked(t, s)
	endedTouchingNothing(t, k, s, true)
	l.panicsLogged(t, "the native sink's close")
}

// TestANativeSinksCleanupThatPanicsStaysInThePlayer pins a close of the
// native sink's node that panics, at each of the writer's closes
// (closeNative's recover, which every caller takes): the writer's end after
// a release, after its own panic, at a pause's release and at each refusal
// of a reopen; a release whose DROP or HW_FREE panics, its close clean,
// failing or panicking, and one whose HW_FREE panics after DROP failed
// (releaseNative's recovers); a reopen whose setup panicked, its node's close
// failing or panicking (openNative's own close); and a reopen whose setup was
// refused, its close panicking. The writer's end runs, Done closes, the
// cleanup is unconfirmed and typed, holding every failure and an error of
// each panic apart, the node closed once, each panic is logged once with no
// value, and a later command answers the end.
func TestANativeSinksCleanupThatPanicsStaysInThePlayer(t *testing.T) {
	// releasedPanickingIn activates a native sink whose node's next req
	// panics once armed, edit making DROP or its close fail, or its close
	// panic too, releases it, and requires the release to answer the panic,
	// unconfirmed, the cleanup to hold an error of each of its panics apart,
	// and the sink to end as a close that panicked ends it.
	releasedPanickingIn := func(t *testing.T, req uint32, panics int, edit func(k *fakeKernel)) (*DSDSink, *nativeLog) {
		t.Helper()
		k, l := nativeKernel(t), newNativeLog()
		node := &ioctlPanic{sys: k, req: req}
		s := panicSinkOn(t, k, node, nativeConfig(), l)
		if e := s.Activate(context.Background(), 1); e != nil {
			t.Fatal(e)
		}
		node.armed.Store(true)
		if edit != nil {
			k.inspect(edit)
		}
		e := s.Release(context.Background(), Immediate)
		if !errors.Is(e, ErrReleaseUnconfirmed) || !errors.Is(e, ErrPanicked) {
			t.Errorf("the release answered %v; want its teardown's panic, unconfirmed", e)
		}
		noValue(t, e, "the release")
		cleanupPanicked(t, s)
		if got := panickedErrors(s.CleanupResult()); len(got) != panics {
			t.Errorf("the cleanup %v holds %d errors of a recovered panic, want %d, one for each", s.CleanupResult(), len(got), panics)
		}
		endedTouchingNothing(t, k, s, true)
		return s, l
	}
	RunEachCaseInAChild(t, "TestANativeSinksCleanupThatPanicsStaysInThePlayer", map[string]func(t *testing.T){
		"a release whose DROP panics": func(t *testing.T) {
			_, l := releasedPanickingIn(t, ioctlDrop, 1, nil)
			l.panicsLogged(t, "the native sink's close")
		},
		"a release whose HW_FREE panics": func(t *testing.T) {
			_, l := releasedPanickingIn(t, ioctlHWFree, 1, nil)
			l.panicsLogged(t, "the native sink's close")
		},
		"a release whose DROP fails, its HW_FREE panicking": func(t *testing.T) {
			s, l := releasedPanickingIn(t, ioctlHWFree, 1, func(k *fakeKernel) { k.fail[ioctlDrop] = errDropFailed })
			if e := s.CleanupResult(); !errors.Is(e, errDropFailed) {
				t.Errorf("the cleanup is %v; want DROP's failure beside HW_FREE's panic", e)
			}
			l.panicsLogged(t, "the native sink's close")
		},
		"a release whose DROP panics, its close failing": func(t *testing.T) {
			s, l := releasedPanickingIn(t, ioctlDrop, 1, func(k *fakeKernel) { k.closeErr = errCloseFailed })
			if e := s.CleanupResult(); !errors.Is(e, errCloseFailed) {
				t.Errorf("the cleanup is %v; want the close's failure beside DROP's panic", e)
			}
			l.panicsLogged(t, "the native sink's close")
		},
		"a release whose DROP panics, its close panicking": func(t *testing.T) {
			_, l := releasedPanickingIn(t, ioctlDrop, 2, func(k *fakeKernel) { k.closePanic = closePanicValue })
			l.panicsLogged(t, "the native sink's close", "the native sink's close")
		},
		"a reopen's setup refused, its close panicking": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			s := panicSink(t, k, nativeConfig(), l)
			pauseUntilReleased(t, k, s)
			k.inspect(func(k *fakeKernel) {
				k.commit = func(p *hwParams) { p.setInteger(hwParamChannels, 1) }
				k.closePanic = closePanicValue
			})
			e := s.Resume(context.Background())
			if ReasonOf(e) != ReasonMismatch || !errors.Is(e, ErrReleaseUnconfirmed) || !errors.Is(e, ErrPanicked) {
				t.Errorf("the resume answered %v; want its setup's refusal beside its close's panic, unconfirmed", e)
			}
			noValue(t, e, "the resume")
			if e := s.CleanupResult(); !errors.Is(e, ErrReleaseUnconfirmed) || !errors.Is(e, ErrPanicked) {
				t.Errorf("the cleanup is %v; want the refused node's close's panic, unconfirmed", e)
			}
			noValue(t, s.CleanupResult(), "the cleanup")
			endedTouchingNothing(t, k, s, true)
			l.panicsLogged(t, "the native setup's close")
		},
		"the close at a release": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			s := panicSink(t, k, nativeConfig(), l)
			if e := s.Activate(context.Background(), 1); e != nil {
				t.Fatal(e)
			}
			k.inspect(func(k *fakeKernel) { k.closePanic = closePanicValue })
			e := s.Release(context.Background(), Immediate)
			if !errors.Is(e, ErrReleaseUnconfirmed) || !errors.Is(e, ErrPanicked) {
				t.Errorf("the release answered %v; want its close's panic, unconfirmed", e)
			}
			noValue(t, e, "the release")
			cleanupPanicked(t, s)
			endedTouchingNothing(t, k, s, true)
			l.panicsLogged(t, "the native sink's close")
		},
		"the close after the writer panicked": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			var armed atomic.Bool
			c := nativeConfig()
			c.Check = func(NodeInfo) error {
				if armed.Load() {
					panic(sinkPanicValue)
				}
				return nil
			}
			s := panicSink(t, k, c, l)
			pauseUntilReleased(t, k, s)
			// The reopened node's check panics on the writer, and its close,
			// at the writer's end, panics too.
			armed.Store(true)
			k.inspect(func(k *fakeKernel) { k.closePanic = closePanicValue })
			e := s.Resume(context.Background())
			if ReasonOf(e) != ReasonPanicked || !strings.Contains(e.Error(), "the native sink's writer panicked") || !errors.Is(e, ErrReleaseUnconfirmed) || neverRan(e) {
				t.Errorf("the resume the panic cut short answered %v; want the writer's panic, and its close's, unconfirmed", e)
			}
			noValue(t, e, "the resume")
			cleanupPanicked(t, s)
			endedTouchingNothing(t, k, s, true)
			l.panicsLogged(t, "the native sink's writer", "the native sink's close")
		},
		"the close at a pause's release": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			s := panicSink(t, k, nativeConfig(), l)
			ctx := context.Background()
			if e := s.Activate(ctx, 1); e != nil {
				t.Fatal(e)
			}
			if e := s.Pause(ctx); e != nil {
				t.Fatal(e)
			}
			k.inspect(func(k *fakeKernel) { k.closePanic = closePanicValue })
			k.grant(31 * time.Second)
			await(t, s.Done(), "the writer's end at its pause's release")
			if e := s.Result(); !errors.Is(e, ErrReleaseUnconfirmed) || !errors.Is(e, ErrPanicked) {
				t.Errorf("the outcome is %v; want the release's close that panicked, unconfirmed", e)
			}
			cleanupPanicked(t, s)
			endedTouchingNothing(t, k, s, true)
			l.panicsLogged(t, "the native sink's close")
		},
		"the close of a reopen whose DoP precision is refused": func(t *testing.T) {
			k, l := dopKernel(t), newNativeLog()
			s := panicSink(t, k, dopConfig(), l)
			pauseUntilReleased(t, k, s)
			k.inspect(func(k *fakeKernel) { k.commit = func(p *hwParams) { p.msbits = 20 } })
			reopenRefusedItsClosePanicking(t, k, s, l)
		},
		"the close of a reopen whose geometry changed": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			s := panicSink(t, k, nativeConfig(), l)
			pauseUntilReleased(t, k, s)
			k.inspect(func(k *fakeKernel) {
				k.commit = func(p *hwParams) { p.setInteger(hwParamBufferSize, 7056); p.setInteger(hwParamPeriodSize, 1764) }
			})
			reopenRefusedItsClosePanicking(t, k, s, l)
		},
		"the close of a reopen its check refused": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			var refuse atomic.Bool
			c := nativeConfig()
			c.Check = func(NodeInfo) error {
				if refuse.Load() {
					return errors.New("the reopened node is another unit's")
				}
				return nil
			}
			s := panicSink(t, k, c, l)
			pauseUntilReleased(t, k, s)
			refuse.Store(true)
			reopenRefusedItsClosePanicking(t, k, s, l)
		},
		"the close of a reopen its fence refused": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			var fence atomic.Bool
			var s *DSDSink
			c := nativeConfig()
			c.Check = func(NodeInfo) error {
				if fence.Load() {
					// A pause while the reopened node is checked moves the
					// pause's epoch, which fences the resume after its check.
					return s.Pause(context.Background())
				}
				return nil
			}
			s = panicSink(t, k, c, l)
			pauseUntilReleased(t, k, s)
			fence.Store(true)
			reopenRefusedItsClosePanicking(t, k, s, l)
		},
		"a reopen's setup, its close failing": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			s := panicSink(t, k, nativeConfig(), l)
			pauseUntilReleased(t, k, s)
			k.inspect(func(k *fakeKernel) {
				k.commit = func(*hwParams) { panic(sinkPanicValue) }
				k.closeErr = errCloseFailed
			})
			e := s.Resume(context.Background())
			if ReasonOf(e) != ReasonPanicked || !errors.Is(e, ErrReleaseUnconfirmed) || !errors.Is(e, errCloseFailed) || neverRan(e) {
				t.Errorf("the resume answered %v; want its setup's panic beside its close's failure, unconfirmed", e)
			}
			noValue(t, e, "the resume")
			if e := s.CleanupResult(); !errors.Is(e, ErrReleaseUnconfirmed) || !errors.Is(e, errCloseFailed) || errors.Is(e, ErrPanicked) {
				t.Errorf("the cleanup is %v; want the reopened node's close's failure alone, unconfirmed", e)
			}
			endedTouchingNothing(t, k, s, true)
			l.panicsLogged(t, "the native sink's writer")
		},
		"a reopen's setup, its close panicking": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			s := panicSink(t, k, nativeConfig(), l)
			pauseUntilReleased(t, k, s)
			k.inspect(func(k *fakeKernel) {
				k.commit = func(*hwParams) { panic(sinkPanicValue) }
				k.closePanic = closePanicValue
			})
			e := s.Resume(context.Background())
			if ReasonOf(e) != ReasonPanicked || !strings.Contains(e.Error(), "the native sink's writer panicked") || !errors.Is(e, ErrReleaseUnconfirmed) || neverRan(e) {
				t.Errorf("the resume answered %v; want its setup's panic beside its close's, unconfirmed", e)
			}
			noValue(t, e, "the resume")
			cleanupPanicked(t, s)
			endedTouchingNothing(t, k, s, true)
			l.panicsLogged(t, "the native setup's close", "the native sink's writer")
		},
	})
}

// TestANativeWritersOwnPanicIsTypedAndLogged pins a panic on the native
// sink's writer whose cleanup succeeds, as it holds a pause and in the check
// of a node it reopens: the sink ends typed ReasonPanicked, the backend's own
// fault and never the device's, logged once with no value (the value only
// for the runtime's own error), its cleanup confirmed, and a resume the panic
// cut short answers that failure.
func TestANativeWritersOwnPanicIsTypedAndLogged(t *testing.T) {
	// asItHoldsAPause pauses s and makes the writer's next read of the clock,
	// in its pause, run f.
	asItHoldsAPause := func(t *testing.T, k *fakeKernel, s *DSDSink, f func()) {
		t.Helper()
		ctx := context.Background()
		if e := s.Activate(ctx, 1); e != nil {
			t.Fatal(e)
		}
		if e := s.Pause(ctx); e != nil {
			t.Fatal(e)
		}
		k.atNow(func() bool { return true }, f)
		k.grant(time.Second)
	}
	typed := func(t *testing.T, s *DSDSink) {
		t.Helper()
		await(t, s.Done(), "the writer's end")
		if e := s.Result(); ReasonOf(e) != ReasonPanicked || !errors.Is(e, ErrPanicked) || errors.Is(e, ErrReleaseUnconfirmed) {
			t.Errorf("the outcome is %v; want the writer's panic, typed %s, its cleanup confirmed", e, ReasonPanicked)
		}
		noValue(t, s.Result(), "the outcome")
		if e := s.CleanupResult(); e != nil {
			t.Errorf("the cleanup is %v; want it confirmed", e)
		}
	}
	RunEachCaseInAChild(t, "TestANativeWritersOwnPanicIsTypedAndLogged", map[string]func(t *testing.T){
		"as it holds a pause": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			s := panicSink(t, k, nativeConfig(), l)
			asItHoldsAPause(t, k, s, func() { panic(sinkPanicValue) })
			typed(t, s)
			endedTouchingNothing(t, k, s, false)
			l.panicsLogged(t, "the native sink's writer")
		},
		"the runtime's own error, as it holds a pause": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			s := panicSink(t, k, nativeConfig(), l)
			asItHoldsAPause(t, k, s, func() {
				var none []int
				_ = none[len(none)]
			})
			typed(t, s)
			endedTouchingNothing(t, k, s, false)
			l.panicsLogged(t, "the native sink's writer")
			l.mu.Lock()
			defer l.mu.Unlock()
			if !slices.ContainsFunc(l.lines, func(line string) bool {
				return strings.Contains(line, nativePanicMessage) && strings.Contains(line, "error=runtime error: index out of range")
			}) {
				t.Errorf("the runtime's own error is not in the panic's line: %q", l.lines)
			}
		},
		"a reopen's check": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			var armed atomic.Bool
			c := nativeConfig()
			c.Check = func(NodeInfo) error {
				if armed.Load() {
					panic(sinkPanicValue)
				}
				return nil
			}
			s := panicSink(t, k, c, l)
			pauseUntilReleased(t, k, s)
			armed.Store(true)
			e := s.Resume(context.Background())
			if ReasonOf(e) != ReasonPanicked || errors.Is(e, ErrReleaseUnconfirmed) || neverRan(e) {
				t.Errorf("the resume the panic cut short answered %v; want the writer's panic, its cleanup confirmed", e)
			}
			noValue(t, e, "the resume")
			typed(t, s)
			endedTouchingNothing(t, k, s, false)
			l.panicsLogged(t, "the native sink's writer")
		},
	})
}

// openPanicking opens a native sink through its opening worker on k, with c
// logging to l and each edit given its seams.
func openPanicking(ctx context.Context, k *fakeKernel, c DSDConfig, l *nativeLog, edits ...func(*nativeSeams)) (*DSDSink, error) {
	c.Log = slog.New(l)
	h := nativeSeams{open: func(ep Endpoint, c Config) (*Device, error) { return openNative(k, k.now, ep, c) }, now: k.now, at: k.nativeAt}
	for _, edit := range edits {
		edit(&h)
	}
	return openNativeBounded(ctx, c, h)
}

// openPanicked requires an open's answer to be no sink and the panic it
// recovered, typed ReasonPanicked, its node's cleanup unconfirmed as
// unconfirmed says, naming no value, and every open of the node closed once.
func openPanicked(t *testing.T, k *fakeKernel, s *DSDSink, e error, unconfirmed bool) {
	t.Helper()
	if s != nil {
		t.Fatalf("the open answered a sink, and %v", e)
	}
	if ReasonOf(e) != ReasonPanicked || !errors.Is(e, ErrPanicked) || errors.Is(e, ErrReleaseUnconfirmed) != unconfirmed {
		t.Errorf("the open answered %v; want its panic typed %s, its cleanup unconfirmed %v", e, ReasonPanicked, unconfirmed)
	}
	noValue(t, e, "the open's answer")
	k.checkClosedEach(t)
}

// pendingOpen is an open's answer once its caller has gone.
type pendingOpen interface {
	Completion() <-chan struct{}
	CompletionResult() error
}

// openAfterItsCallerHasGone opens a native sink on k with c, held in the
// kernel's open until its caller's context has ended and the caller has its
// answer, edit given the seams, and returns that answer; let lets the open go
// on.
func openAfterItsCallerHasGone(t *testing.T, k *fakeKernel, c DSDConfig, l *nativeLog, edits ...func(*nativeSeams)) (p pendingOpen, let func()) {
	t.Helper()
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	let = func() { once.Do(func() { close(gate) }) }
	t.Cleanup(let)
	k.inspect(func(k *fakeKernel) { k.openParked, k.openGate = entered, gate })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	answer := make(chan error, 1)
	go func() {
		_, e := openPanicking(ctx, k, c, l, edits...)
		answer <- e
	}()
	await(t, entered, "the open, held in the kernel's open")
	cancel()
	e := receive(t, answer, "the open's answer once its caller has gone")
	p, ok := e.(pendingOpen)
	if !ok || !errors.Is(e, ErrReleaseUnconfirmed) {
		t.Fatalf("the open whose caller has gone answered %v; want its completion pending", e)
	}
	return p, let
}

// completedPanicked requires the completion of an open whose caller has gone
// to say the panic its worker recovered, typed, its cleanup confirmed, and
// every open of the node closed once.
func completedPanicked(t *testing.T, k *fakeKernel, p pendingOpen) {
	t.Helper()
	await(t, p.Completion(), "the opening worker's end")
	e := p.CompletionResult()
	if ReasonOf(e) != ReasonPanicked || !errors.Is(e, ErrPanicked) || errors.Is(e, ErrReleaseUnconfirmed) {
		t.Errorf("the completion is %v; want the worker's panic, typed %s, its cleanup confirmed", e, ReasonPanicked)
	}
	noValue(t, e, "the completion")
	k.checkClosedEach(t)
}

// TestANativeOpenThatPanicsStaysInThePlayer pins a panic on the native
// sink's opening worker (openNativeBounded's recover): in the check after
// open, in the kernel's setup, each with its node's close clean, failing or
// panicking; in a refusal's close; after the open's caller has gone; in the
// cancellation arm, a sink made and not delivered, its close clean or
// failing; and after the hand-off, a sink delivered, which is never revoked.
// The node a panic comes to is closed once before the answer, the answer is
// typed ReasonPanicked beside a close that failed or panicked, unconfirmed,
// the caller's or its CompletionResult, and each panic is logged once with
// no value.
func TestANativeOpenThatPanicsStaysInThePlayer(t *testing.T) {
	checkPanics := func() DSDConfig {
		c := nativeConfig()
		c.Check = func(NodeInfo) error { panic(sinkPanicValue) }
		return c
	}
	setupPanics := func(k *fakeKernel) {
		k.inspect(func(k *fakeKernel) { k.commit = func(*hwParams) { panic(sinkPanicValue) } })
	}
	// inTheCancellationArm opens a native sink on k whose caller has gone,
	// with its watcher, which revokes it once its life has ended, held
	// before it does (its end, if it takes that first, has nothing to
	// revoke), so the clock's first read once the open goes on is the
	// worker's revocation in the cancellation arm, which panics there: no
	// other goroutine reads it. It returns the open's pending answer.
	inTheCancellationArm := func(t *testing.T, k *fakeKernel, l *nativeLog) pendingOpen {
		t.Helper()
		watcher := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(watcher) }) })
		p, let := openAfterItsCallerHasGone(t, k, nativeConfig(), l, func(h *nativeSeams) {
			h.lifeEnded = func() { <-watcher }
		})
		panicAtNextClock(k, sinkPanicValue)
		let()
		return p
	}
	RunEachCaseInAChild(t, "TestANativeOpenThatPanicsStaysInThePlayer", map[string]func(t *testing.T){
		"the check after open": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			s, e := openPanicking(context.Background(), k, checkPanics(), l)
			openPanicked(t, k, s, e, false)
			l.panicsLogged(t, "the native sink's opener")
		},
		"the check after open, and its close": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			k.inspect(func(k *fakeKernel) { k.closePanic = closePanicValue })
			s, e := openPanicking(context.Background(), k, checkPanics(), l)
			openPanicked(t, k, s, e, true)
			l.panicsLogged(t, "the native open's close", "the native sink's opener")
		},
		"the kernel's setup": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			setupPanics(k)
			s, e := openPanicking(context.Background(), k, nativeConfig(), l)
			openPanicked(t, k, s, e, false)
			l.panicsLogged(t, "the native sink's opener")
		},
		"the kernel's setup, its close failing": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			setupPanics(k)
			k.inspect(func(k *fakeKernel) { k.closeErr = errCloseFailed })
			s, e := openPanicking(context.Background(), k, nativeConfig(), l)
			openPanicked(t, k, s, e, true)
			if !errors.Is(e, errCloseFailed) {
				t.Errorf("the open answered %v; want its close's failure beside its setup's panic", e)
			}
			l.panicsLogged(t, "the native sink's opener")
		},
		"the kernel's setup, its close panicking": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			setupPanics(k)
			k.inspect(func(k *fakeKernel) { k.closePanic = closePanicValue })
			s, e := openPanicking(context.Background(), k, nativeConfig(), l)
			openPanicked(t, k, s, e, true)
			l.panicsLogged(t, "the native setup's close", "the native sink's opener")
		},
		"a refusal's close": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			refused := errors.New("the node is another unit's")
			c := nativeConfig()
			c.Check = func(NodeInfo) error { return refused }
			k.inspect(func(k *fakeKernel) { k.closePanic = closePanicValue })
			s, e := openPanicking(context.Background(), k, c, l)
			if s != nil || !errors.Is(e, refused) || !errors.Is(e, ErrReleaseUnconfirmed) || !errors.Is(e, ErrPanicked) {
				t.Errorf("the open answered %v, %v; want no sink, its refusal beside its close's panic, unconfirmed", s, e)
			}
			noValue(t, e, "the open's answer")
			k.checkClosedEach(t)
			l.panicsLogged(t, "the native open's close")
		},
		"after its caller has gone": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			p, let := openAfterItsCallerHasGone(t, k, checkPanics(), l)
			let()
			completedPanicked(t, k, p)
			l.panicsLogged(t, "the native sink's opener")
		},
		"in the cancellation arm, its sink made": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			p := inTheCancellationArm(t, k, l)
			completedPanicked(t, k, p)
			l.panicsLogged(t, "the native sink's opener")
		},
		"in the cancellation arm, its sink made, its close failing": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			k.inspect(func(k *fakeKernel) { k.closeErr = errCloseFailed })
			p := inTheCancellationArm(t, k, l)
			await(t, p.Completion(), "the opening worker's end")
			// The worker's panic beside the result of the sink it revoked, whose
			// writer's close failed.
			e := p.CompletionResult()
			if ReasonOf(e) != ReasonPanicked || !errors.Is(e, ErrPanicked) || !errors.Is(e, ErrReleaseUnconfirmed) || !errors.Is(e, errCloseFailed) {
				t.Errorf("the completion is %v; want the worker's panic, typed %s, beside its sink's close's failure, unconfirmed", e, ReasonPanicked)
			}
			noValue(t, e, "the completion")
			k.checkClosedEach(t)
			l.panicsLogged(t, "the native sink's opener")
		},
		"after the hand-off, its sink delivered": func(t *testing.T) {
			k, l := nativeKernel(t), newNativeLog()
			ended := make(chan struct{})
			s, e := openPanicking(context.Background(), k, nativeConfig(), l, func(h *nativeSeams) {
				h.delivered = func() { panic(sinkPanicValue) }
				h.openEnded = func() { close(ended) }
			})
			if e != nil || s == nil {
				t.Fatalf("the open answered %v, %v; want its sink", s, e)
			}
			t.Cleanup(s.Revoke)
			// The worker's end, past its recover: whatever that recover would do
			// to the sink is done before the sink is used.
			await(t, ended, "the opening worker's end, past its recover")
			l.panicsLogged(t, "the native sink's opener")
			// The sink delivered is its caller's: the worker's recover leaves it
			// as it was, and it plays.
			ctx := context.Background()
			if e := s.Activate(ctx, 1); e != nil {
				t.Errorf("the sink delivered before the panic refused its activation: %v", e)
			}
			if e := s.Release(ctx, Immediate); e != nil {
				t.Errorf("the sink delivered before the panic answered its release %v", e)
			}
			k.checkClosedEach(t)
		},
	})
}
