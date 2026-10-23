package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// playOut is the run's standard output: one line at a time, each stamped
// with the time it was printed.
type playOut struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

func (o *playOut) line(format string, args ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	fmt.Fprintf(o.w, "%s %s\n", o.now().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

// playTee is the backend as the engine sees it in the harness: each
// session's events printed on their way to the engine, and each occurrence
// the engine hands off kept, to name it.
type playTee struct {
	playBackend
	r *playRun
}

func (t playTee) Start(ctx context.Context, req engine.StartRequest) error {
	t.r.noteOcc(req.First)
	t.r.out.line("session %d: start with %s", req.Session, t.r.name(req.First.ID))
	req.Events = &playEvents{next: req.Events, r: t.r}
	err := t.playBackend.Start(ctx, req)
	if err != nil {
		t.r.out.line("session %d: the start did not play: %v", req.Session, err)
	}
	return err
}

func (t playTee) Prepare(ctx context.Context, s engine.SessionID, after engine.OccurrenceID, next *engine.Occurrence) error {
	if next != nil {
		t.r.noteOcc(*next)
		t.r.out.line("session %d: %s follows %s", s, t.r.name(next.ID), t.r.name(after))
	} else {
		t.r.out.line("session %d: nothing follows %s", s, t.r.name(after))
	}
	err := t.playBackend.Prepare(ctx, s, after, next)
	if err != nil {
		t.r.out.line("session %d: the backend took no successor: %v", s, err)
	}
	return err
}

func (t playTee) Cut(ctx context.Context, s engine.SessionID, from engine.OccurrenceID) (bool, error) {
	ok, err := t.playBackend.Cut(ctx, s, from)
	switch {
	case err != nil:
		t.r.out.line("session %d: cut at %s: %v", s, t.r.name(from), err)
	case ok:
		t.r.out.line("session %d: cut at %s in time: none of its frames reaches the DAC", s, t.r.name(from))
	default:
		t.r.out.line("session %d: cut at %s too late: its frames are the kernel's, and playback stops when it is heard", s, t.r.name(from))
	}
	return ok, err
}

func (t playTee) Release(ctx context.Context, s engine.SessionID) error {
	err := t.playBackend.Release(ctx, s)
	if err != nil {
		t.r.out.line("session %d: release not confirmed by its deadline (%v): nothing opens the device until it is", s, err)
	} else {
		t.r.out.line("session %d: released: the device and the decoders closed", s)
	}
	return err
}

// playEvents prints each event a session reports and passes it on.
type playEvents struct {
	next engine.Events
	r    *playRun

	mu      sync.Mutex
	printed time.Time // the last position line: one a second at most
}

func (v *playEvents) Prepared(s engine.SessionID, o engine.OccurrenceID, j engine.Join) {
	how := "gapless: its frames follow with nothing between"
	if !j.Gapless {
		how = "another transport (" + strings.Join(j.Differs, ", ") + "): drain, reopen and refill, an honest gap"
	}
	v.r.out.line("session %d: %s prepared, %s", s, v.r.name(o), how)
	v.next.Prepared(s, o, j)
}

func (v *playEvents) Refused(s engine.SessionID, o engine.OccurrenceID, why engine.Unplayable) {
	v.r.out.line("session %d: %s refused before any of it played: %s", s, v.r.name(o), why.Error())
	v.next.Refused(s, o, why)
}

func (v *playEvents) Submitted(s engine.SessionID, o engine.OccurrenceID) {
	v.r.out.line("session %d: %s submitted: its first frames are in the sink", s, v.r.name(o))
	v.next.Submitted(s, o)
}

func (v *playEvents) Audible(s engine.SessionID, o engine.OccurrenceID) {
	v.r.out.line("session %d: boundary: %s is audible", s, v.r.name(o))
	v.next.Audible(s, o)
}

func (v *playEvents) Position(s engine.SessionID, p engine.Position) {
	v.mu.Lock()
	print := p.Measured.Sub(v.printed) >= time.Second || p.Measured.Before(v.printed)
	if print {
		v.printed = p.Measured
	}
	v.mu.Unlock()
	if print {
		v.r.out.line("session %d: %s at %.3f s (%d frames)", s, v.r.name(p.Occurrence), p.Seconds(), p.Frames)
	}
	v.next.Position(s, p)
}

func (v *playEvents) Fault(s engine.SessionID, f engine.Fault) {
	v.r.out.line("session %d: fault: %s at %s, %d frames in: %s", s, f.Reason, v.r.name(f.Occurrence), f.Frames, f.Detail)
	v.next.Fault(s, f)
}

func (v *playEvents) Finished(s engine.SessionID) {
	v.r.out.line("session %d: finished: everything handed off has played", s)
	v.next.Finished(s)
}

func (v *playEvents) DeviceReleased(s engine.SessionID) {
	v.r.out.line("session %d: the pause let the device go", s)
	v.next.DeviceReleased(s)
}

func (v *playEvents) Released(s engine.SessionID) {
	v.r.out.line("session %d: released, late: the device and the decoders are closed now", s)
	v.next.Released(s)
}

// playSeen is what the run has printed of the engine's snapshots.
type playSeen struct {
	gen         engine.Generation
	state       engine.State
	selected    uint32
	faults      int
	skips       map[uint32]engine.Skip
	stop        engine.StopInfo
	unconfirmed bool
	devReleased bool
	resumed     engine.ResumeResult
	resumeErr   string
	output      engine.OutputState
	maint       bool
}

// diff prints what changed in s since the last snapshot it printed.
func (r *playRun) diff(s *engine.Snapshot) {
	r.mu.Lock()
	seen := r.seen
	r.mu.Unlock()
	if seen == nil {
		seen = &playSeen{skips: map[uint32]engine.Skip{}}
	}
	if g := s.Generation(); g != seen.gen {
		seen.gen = g
		starting := ""
		if s.Starting() {
			starting = ", starting"
		}
		r.out.line("engine: %s%s, generation %s, selected entry %d", s.State(), starting, g, s.Selected())
	}
	seen.state, seen.selected = s.State(), s.Selected()
	faults := s.Faults()
	for _, f := range faults[min(seen.faults, len(faults)):] {
		r.out.line("engine: fault recorded: %s in entry %d, %d frames in: %s", f.Reason, f.Entry, f.Frames, f.Detail)
	}
	seen.faults = len(faults)
	for id, sk := range s.Skips() {
		if old, ok := seen.skips[id]; !ok || old != sk {
			seen.skips[id] = sk
			r.out.line("engine: entry %d skipped: %s: %s", id, sk.Reason, sk.Detail)
		}
	}
	if st := s.LastStop(); st != seen.stop {
		seen.stop = st
		p := s.Position()
		r.out.line("engine: playback stopped: %s (%s), on entry %d, the position at %.3f s (%d frames)", st.Reason, st.Detail, st.Entry, p.Seconds(), p.Frames)
	}
	if u, ok := s.ReleaseUnconfirmed(); ok != seen.unconfirmed {
		seen.unconfirmed = ok
		if ok {
			r.out.line("engine: session %d's release is unconfirmed: nothing opens the device until it is", u.Session)
		} else {
			r.out.line("engine: the unconfirmed release was acknowledged")
		}
	}
	if d := s.DeviceReleased(); d != seen.devReleased {
		seen.devReleased = d
		if d {
			r.out.line("engine: paused, the device released")
		}
	}
	if how, why := s.LastResume(); how != seen.resumed || why != seen.resumeErr {
		seen.resumed, seen.resumeErr = how, why
		if why != "" {
			r.out.line("engine: the resume was refused, still paused: %s", why)
		} else if how != 0 {
			r.out.line("engine: resumed: %s", how)
		}
	}
	if o := s.Output(); o != seen.output {
		seen.output = o
		if o.Available {
			r.out.line("engine: the output is available (report %d)", o.Generation)
		} else {
			r.out.line("engine: the output is unavailable (report %d): %s", o.Generation, o.Reason)
		}
	}
	if m, ok := s.Maintenance(); ok != seen.maint {
		seen.maint = ok
		if ok {
			r.out.line("engine: in maintenance (%s), the release %s", m.Reason, map[bool]string{true: "confirmed", false: "unconfirmed"}[m.ReleaseConfirmed])
		} else {
			r.out.line("engine: maintenance ended: stopped")
		}
	}
	r.mu.Lock()
	r.seen = seen
	r.mu.Unlock()
}
