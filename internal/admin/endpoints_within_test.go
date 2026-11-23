package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/advertise"
	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// The endpoint list's provider runs on a goroutine of its own
// (endpointsWithin), so a panic in it has two possible reporters: the caller
// waiting for the list, which raises it again, and the goroutine, which logs
// it where no caller waits for it any more. Exactly one reports it, whatever
// the order of the panic, the request's end and the caller's choice (backlog
// B997), each order below held at the list's own steps (endpointsSeams).

// endpointsWaitBound bounds each wait of these cases for a step of the code.
const endpointsWaitBound = 10 * time.Second

// endpointsRig is a server whose endpoint list's provider panics with value
// once let go, the end of the provider's goroutine heard (endpointsSeams'
// ended) and the panic's line kept by rec.
type endpointsRig struct {
	s       *Server
	rec     *loggingtest.Recorder
	value   string
	asked   chan struct{}
	release chan struct{}
	ended   chan struct{}
	letGo   func()
	called  bool
}

// newEndpointsRig is a rig whose list's steps are seams', its ended step the
// rig's; its cleanup lets the provider go and waits for its goroutine's end.
func newEndpointsRig(t *testing.T, rec *loggingtest.Recorder, value string, seams endpointsSeams) *endpointsRig {
	t.Helper()
	r := &endpointsRig{rec: rec, value: value, asked: make(chan struct{}), release: make(chan struct{}), ended: make(chan struct{})}
	var released, endedOnce sync.Once
	r.letGo = func() { released.Do(func() { close(r.release) }) }
	seams.ended = func() { endedOnce.Do(func() { close(r.ended) }) }
	r.s = &Server{deps: Deps{Endpoints: r.provide}}
	r.s.testEndpoints.Store(&seams)
	t.Cleanup(func() {
		r.letGo()
		if !r.called {
			return
		}
		select {
		case <-r.ended:
		case <-time.After(endpointsWaitBound):
			t.Errorf("the provider's goroutine had not ended %v after it was let go", endpointsWaitBound)
		}
	})
	return r
}

// provide is the rig's provider: it says it was asked, waits to be let go,
// and panics with the rig's value.
func (r *endpointsRig) provide() []advertise.Endpoint {
	select {
	case <-r.asked:
	default:
		close(r.asked)
	}
	<-r.release
	panic(r.value)
}

// endpointsCall is one wait for the list on a goroutine of its own: what it
// returned, or the value it raised, once done is closed.
type endpointsCall struct {
	done   chan struct{}
	eps    []advertise.Endpoint
	err    error
	raised any
}

// call waits for the rig's list on ctx, on a goroutine of its own.
func (r *endpointsRig) call(ctx context.Context) *endpointsCall {
	r.called = true
	c := &endpointsCall{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer func() { c.raised = recover() }()
		c.eps, c.err = r.s.endpointsWithin(ctx)
	}()
	return c
}

// await waits for ch, bounded, failing the case with what did not come.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(endpointsWaitBound):
		t.Fatalf("%s within %v", what, endpointsWaitBound)
	}
}

// panicLines are the records of the panic's line the recorder kept.
func (r *endpointsRig) panicLines() []slog.Record {
	var out []slog.Record
	for _, rec := range r.rec.Records() {
		if rec.Message == endpointsPanicLine {
			out = append(out, rec)
		}
	}
	return out
}

// requireLoggedOnce fails unless the panic was logged once, with its value
// under panic and, under stack, the stack its recovery read: the provider's
// panicking frame below the provider's goroutine in endpointsWithin.
func (r *endpointsRig) requireLoggedOnce(t *testing.T) {
	t.Helper()
	lines := r.panicLines()
	if len(lines) != 1 {
		t.Fatalf("the panic was logged %d times, want once: %v", len(lines), r.rec.Lines(endpointsPanicLine))
	}
	var value, stack string
	lines[0].Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "panic":
			value = a.Value.String()
		case "stack":
			stack = a.Value.String()
		}
		return true
	})
	if value != r.value {
		t.Errorf("the panic was logged with the value %q under panic, want %q", value, r.value)
	}
	if !strings.Contains(stack, "(*endpointsRig).provide") || !strings.Contains(stack, "(*Server).endpointsWithin") {
		t.Errorf("the logged stack is not the provider's recovery's (the provider's panicking frame in endpointsWithin's goroutine):\n%s", stack)
	}
}

// TestAPanicAfterItsRequestEndedIsLoggedWithItsValue: the caller has returned
// its context's error before the provider panics, so no caller raises the
// panic again, and the goroutine logs it, with its value and its stack.
func TestAPanicAfterItsRequestEndedIsLoggedWithItsValue(t *testing.T) {
	r := newEndpointsRig(t, loggingtest.Record(t), "the provider's panic after its request ended", endpointsSeams{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	call := r.call(ctx)
	await(t, r.asked, "the provider was not asked")
	cancel()
	await(t, call.done, "the caller did not return when its request ended")
	if call.raised != nil || !errors.Is(call.err, context.Canceled) {
		t.Fatalf("the caller returned (%v, %v) raising %v, want its context's error", call.eps, call.err, call.raised)
	}
	r.letGo()
	await(t, r.ended, "the provider's goroutine did not end")
	r.requireLoggedOnce(t)
}

// TestAPanicWhileItsCallerWaitsIsRaisedAgainAndNotLogged: the caller still
// waits when the provider panics, so it raises the panic again, and nothing
// logs it, which the case asks once the provider's goroutine has ended.
func TestAPanicWhileItsCallerWaitsIsRaisedAgainAndNotLogged(t *testing.T) {
	r := newEndpointsRig(t, loggingtest.Record(t), "the provider's panic while its caller waits", endpointsSeams{})
	call := r.call(context.Background())
	await(t, r.asked, "the provider was not asked")
	r.letGo()
	await(t, call.done, "the caller did not return once the provider panicked")
	if call.raised != r.value {
		t.Fatalf("the caller raised %v and returned (%v, %v), want the provider's panic %q raised again", call.raised, call.eps, call.err, r.value)
	}
	await(t, r.ended, "the provider's goroutine did not end")
	if got := r.rec.Carrying(r.value); len(got) != 0 {
		t.Fatalf("a panic its caller raised again was logged too: %v", got)
	}
}

// TestACancellationBetweenTheRecoveryAndTheHandOffIsReportedOnce: the
// provider's goroutine is held after its recovery, immediately before its
// claim (endpointsSeams' claiming), while the request ends and its caller
// returns; let go, it finds no caller to hand the panic to, and logs it.
// Exactly one side reports it, and here, the caller gone, the log.
func TestACancellationBetweenTheRecoveryAndTheHandOffIsReportedOnce(t *testing.T) {
	parked, unpark := make(chan struct{}), make(chan struct{})
	var unparked sync.Once
	letClaim := func() { unparked.Do(func() { close(unpark) }) }
	r := newEndpointsRig(t, loggingtest.Record(t), "the provider's panic recovered as its request ended",
		endpointsSeams{claiming: func() { close(parked); <-unpark }})
	t.Cleanup(letClaim)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	call := r.call(ctx)
	await(t, r.asked, "the provider was not asked")
	r.letGo()
	await(t, parked, "the provider's goroutine did not come to its claim")
	cancel()
	await(t, call.done, "the caller did not return when its request ended")
	letClaim()
	await(t, r.ended, "the provider's goroutine did not end")
	raised := call.raised != nil
	logged := len(r.panicLines())
	if raised == (logged == 1) || logged > 1 {
		t.Fatalf("the panic was raised again: %v, and logged %d times; want exactly one report", raised, logged)
	}
	if raised || !errors.Is(call.err, context.Canceled) {
		t.Fatalf("the caller raised %v and returned %v once its request had ended, want its context's error", call.raised, call.err)
	}
	r.requireLoggedOnce(t)
}

// TestAPanicReadyBesideTheRequestsEndBeforeTheChoiceIsReportedOnce: the caller
// is held just before its choice (endpointsSeams' choosing) while its request
// ends and the provider panics and its goroutine ends, so the panic's answer
// and the context's end are both ready when it chooses, and the select takes
// either. The answer was handed over before the caller stopped waiting, so
// the caller raises it again and nothing logs it, whichever the select took:
// sixteen rounds, each a fresh wait.
func TestAPanicReadyBesideTheRequestsEndBeforeTheChoiceIsReportedOnce(t *testing.T) {
	rec := loggingtest.Record(t)
	for round := range 16 {
		atChoice, choose := make(chan struct{}), make(chan struct{})
		var chose sync.Once
		letChoose := func() { chose.Do(func() { close(choose) }) }
		r := newEndpointsRig(t, rec, fmt.Sprintf("the provider's panic ready beside its request's end, round %d", round),
			endpointsSeams{choosing: func() { close(atChoice); <-choose }})
		t.Cleanup(letChoose)
		before := len(r.panicLines())
		ctx, cancel := context.WithCancel(context.Background())
		call := r.call(ctx)
		await(t, atChoice, "the caller did not come to its choice")
		cancel()
		r.letGo()
		await(t, r.ended, "the provider's goroutine did not end")
		letChoose()
		await(t, call.done, "the caller did not return once it chose")
		raised := call.raised == r.value
		logged := len(r.panicLines()) - before
		if raised == (logged == 1) || logged > 1 {
			t.Fatalf("round %d: the panic was raised again: %v, and logged %d times; want exactly one report", round, raised, logged)
		}
		if !raised {
			t.Fatalf("round %d: the caller returned (%v, %v), want the panic handed over before it chose raised again", round, call.eps, call.err)
		}
	}
}
