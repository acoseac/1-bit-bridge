//go:build nativeintegration

package playback

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/atomicwrite"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
)

type nativeFiles map[string][]byte

func (f nativeFiles) Open(_ context.Context, r engine.Reference) (Content, error) {
	b := f[r.String()]
	return Content{Data: bytes.NewReader(b), Size: int64(len(b))}, nil
}

type nativeLocator struct{}

func (nativeLocator) Locate(context.Context) (Unit, error) {
	return Unit{Endpoint: alsapcm.Endpoint{Card: 5, Device: 0}}, nil
}

type nativeEvents struct {
	mu      sync.Mutex
	records []string
	done    chan struct{}
	once    sync.Once
}

func (e *nativeEvents) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.records = append(e.records, s)
}
func (e *nativeEvents) Prepared(_ engine.SessionID, o engine.OccurrenceID, _ engine.Join) {
	e.add(fmt.Sprintf("prepared:%d", o))
}
func (e *nativeEvents) Refused(_ engine.SessionID, o engine.OccurrenceID, u engine.Unplayable) {
	e.add(fmt.Sprintf("refused:%d:%s", o, u.Reason))
}
func (e *nativeEvents) Submitted(_ engine.SessionID, o engine.OccurrenceID) {
	e.add(fmt.Sprintf("submitted:%d", o))
}
func (e *nativeEvents) Audible(_ engine.SessionID, o engine.OccurrenceID) {
	e.add(fmt.Sprintf("audible:%d", o))
}
func (e *nativeEvents) Details(_ engine.SessionID, o engine.OccurrenceID, d engine.Details) {
	e.add(fmt.Sprintf("details:%d:%d:%d:%s:%t", o, d.Bits, d.RateHz, d.Codec, d.Lossless))
}
func (e *nativeEvents) Position(_ engine.SessionID, p engine.Position) {
	e.add(fmt.Sprintf("position:%d:%d:%d", p.Occurrence, p.Frames, p.RateHz))
}
func (e *nativeEvents) Fault(_ engine.SessionID, f engine.Fault) {
	e.add("fault:" + string(f.Reason))
	e.once.Do(func() { close(e.done) })
}
func (e *nativeEvents) Finished(engine.SessionID) {
	e.add("finished")
	e.once.Do(func() { close(e.done) })
}
func (e *nativeEvents) DeviceReleased(engine.SessionID) {}
func (e *nativeEvents) Released(engine.SessionID)       { e.add("released") }

func nativeDSF(t *testing.T, bits int) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("..", "decode", "testdata", "dsd-goldens", "lsb_dsd64_partial_block.dsf"))
	if e != nil {
		t.Fatal(e)
	}
	binary.LittleEndian.PutUint64(b[64:72], uint64(bits))
	return b
}
func nativeTestPlan(d format.NativeDSD) (engine.NativePlan, error) {
	return engine.NativePlan{Tuple: engine.NativeTuple{Format: string(format.DSDU32BE), BitRate: d.BitRateHz}, PolicyID: "test-policy", Pause: "kernel", Tail: time.Millisecond}, nil
}
func TestNativeIntegrationTinyBackend(t *testing.T) {
	for _, bits := range []int{1, 7, 15, 31} {
		t.Run(fmt.Sprint(bits), func(t *testing.T) {
			k := alsapcm.NewFakeKernel(t)
			k.HardwareFormats(49, 50, 52)
			k.Free()
			b, e := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, bits)}, Locator: nativeLocator{}})
			if e != nil {
				t.Fatal(e)
			}
			AdmitNativeForIntegration(b, nativeTestPlan, k.OpenNativeSink)
			ev := &nativeEvents{done: make(chan struct{})}
			r, _ := engine.NewReference("a")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if e = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Entry: 1, Ref: r}, Events: ev}); e != nil {
				t.Fatal(e)
			}
			if e = b.Prepare(ctx, 1, 1, nil); e != nil {
				t.Fatal(e)
			}
			select {
			case <-ev.done:
			case <-ctx.Done():
				t.Fatal("tiny accepted bits did not reach explicit final end")
			}
			if e = b.Release(ctx, 1); e != nil {
				t.Fatal(e)
			}
			ev.mu.Lock()
			defer ev.mu.Unlock()
			want := fmt.Sprintf("position:1:%d:2822400", bits)
			found := false
			for _, r := range ev.records {
				if r == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("logical position absent: %v", ev.records)
			}
			detail := false
			for _, r := range ev.records {
				if r == "details:1:1:2822400:DSD:true" {
					detail = true
				}
			}
			if !detail {
				t.Fatalf("native details absent: %v", ev.records)
			}
			k.CheckClosedEach(t)
		})
	}
}

type nativeTee struct {
	engine.Events
	log *nativeEvents
}

func (v nativeTee) Details(s engine.SessionID, o engine.OccurrenceID, d engine.Details) {
	v.log.Details(s, o, d)
	v.Events.(engine.DetailsEvents).Details(s, o, d)
}
func (v nativeTee) Audible(s engine.SessionID, o engine.OccurrenceID) {
	v.log.Audible(s, o)
	v.Events.Audible(s, o)
}
func (v nativeTee) Position(s engine.SessionID, p engine.Position) {
	v.log.Position(s, p)
	v.Events.Position(s, p)
}
func (v nativeTee) NativePlan(s engine.SessionID, o engine.OccurrenceID, p engine.NativePlan) {
	v.Events.(engine.NativePlanEvents).NativePlan(s, o, p)
}
func (v nativeTee) Retiring(t engine.ReleaseToken, n engine.NativeEvent) {
	v.Events.(engine.NativeReleaseEvents).Retiring(t, n)
}

type nativeRecordingBackend struct {
	*Backend
	events *nativeEvents
}

func (b nativeRecordingBackend) Start(c context.Context, r engine.StartRequest) error {
	r.Events = nativeTee{r.Events, b.events}
	return b.Backend.Start(c, r)
}

// nativeEngineStorage isolates playback tests from a TMPDIR on tmpfs. The
// medium seam is process-global: callers must not run in parallel, and must
// register engine/backend cleanup after this so they join before its restore.
func nativeEngineStorage(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the engine keeps its IDs durable with a directory fsync, which Windows has not: the player runs on Linux")
	}
	t.Cleanup(atomicwrite.SetMediumForTest(func(string) error { return nil }))
}

func TestNativeIntegrationTinyThroughEngine(t *testing.T) {
	nativeEngineStorage(t)
	for _, bits := range []int{1, 7, 15, 31} {
		t.Run(fmt.Sprint(bits), func(t *testing.T) {
			k := alsapcm.NewFakeKernel(t)
			k.HardwareFormats(49, 50, 52)
			k.Free()
			b, e := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, bits)}, Locator: nativeLocator{}})
			if e != nil {
				t.Fatal(e)
			}
			AdmitNativeForIntegration(b, nativeTestPlan, k.OpenNativeSink)
			events := &nativeEvents{done: make(chan struct{})}
			dir := t.TempDir()
			eng, e := engine.New(engine.Config{Backend: nativeRecordingBackend{b, events}, PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e", DataDir: dir, QueuePath: filepath.Join(dir, "queue.json")})
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = eng.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				<-done
				c, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if e := b.Wait(c); e != nil {
					t.Error(e)
				}
				k.CheckClosedEach(t)
			})
			ref, _ := engine.NewReference("a")
			a, e := eng.InsertAfter(ctx, 0, ref, nil)
			if e != nil {
				t.Fatal(e)
			}
			_, e = eng.InsertAfter(ctx, a, ref, nil)
			if e != nil {
				t.Fatal(e)
			}
			if e = eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true}); e != nil {
				t.Fatal(e)
			}
			if e = eng.Play(ctx); e != nil {
				t.Fatal(e)
			}
			for {
				ch := eng.Changed()
				if eng.Snapshot().State() == engine.Stopped {
					break
				}
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("tiny engine stalled")
				}
			}
			if k.View().Opens != 1 {
				t.Errorf("natural compatible advance reopened: %d", k.View().Opens)
			}
			if n := eng.Snapshot().TrackCount(); n != 2 {
				t.Fatalf("heard %d occurrences, want two", n)
			}
			events.mu.Lock()
			defer events.mu.Unlock()
			for _, id := range []int{1, 2} {
				for _, want := range []string{fmt.Sprintf("details:%d:1:2822400:DSD:true", id), fmt.Sprintf("audible:%d", id), fmt.Sprintf("position:%d:%d:2822400", id, bits)} {
					if !slices.Contains(events.records, want) {
						t.Errorf("missing %s: %v", want, events.records)
					}
				}
			}
		})
	}
}

func TestNativeIntegrationReleaseCleanup(t *testing.T) { testNativeReleaseCleanup(t, false) }
func testNativeReleaseCleanup(t *testing.T, dop bool) {
	plan := nativeTestPlan
	if dop {
		plan = dopTestPlan
	}
	for _, where := range []string{"drop", "hw_free", "close"} {
		t.Run(where, func(t *testing.T) {
			k := alsapcm.NewFakeKernel(t)
			k.HardwareFormats(10, 49, 50, 52)
			b, e := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
			if e != nil {
				t.Fatal(e)
			}
			AdmitNativeForIntegration(b, plan, func(ctx context.Context, c alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
				return k.OpenNativeSinkWithCleanupError(ctx, c, where)
			})
			ev := &nativeEvents{done: make(chan struct{})}
			r, _ := engine.NewReference("a")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if e = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Entry: 1, Ref: r}, Events: ev}); e != nil {
				t.Fatal(e)
			}
			e = b.ReleaseNative(ctx, engine.ReleaseRequest{Token: engine.ReleaseToken{Session: 1, Generation: 1}, Mode: engine.ReleaseImmediate})
			if !errors.Is(e, engine.ErrReleaseUnconfirmed) {
				t.Fatalf("failed cleanup acknowledged: %v", e)
			}
			if e = b.Wait(ctx); e != nil {
				t.Fatal(e)
			}
			ev.mu.Lock()
			defer ev.mu.Unlock()
			fault := false
			for _, r := range ev.records {
				if strings.HasPrefix(r, "fault:") {
					fault = true
				}
				if r == "released" {
					t.Fatal("failed cleanup sent success acknowledgement")
				}
			}
			if !fault {
				t.Fatalf("cleanup fault not delivered: %v", ev.records)
			}
			if e = b.hold(newSession(b, 2)); !errors.Is(e, engine.ErrReleaseUnconfirmed) {
				t.Fatalf("failed cleanup authorized successor: %v", e)
			}
			if k.View().Opens != 1 {
				t.Fatal("failed cleanup opened a successor")
			}
		})
	}
}

func TestNativeIntegrationReleasePrefix(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint(late), func(t *testing.T) {
			k := alsapcm.NewFakeKernel(t)
			k.HardwareFormats(49, 50, 52)
			k.Free()
			b, e := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
			if e != nil {
				t.Fatal(e)
			}
			AdmitNativeForIntegration(b, nativeTestPlan, k.OpenNativeSink)
			ev := &nativeEvents{done: make(chan struct{})}
			r, _ := engine.NewReference("a")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if e = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Entry: 1, Ref: r}, Events: ev}); e != nil {
				t.Fatal(e)
			}
			parked, unpark := k.ParkNextWrite()
			defer unpark()
			if e = b.Prepare(ctx, 1, 1, &engine.Occurrence{ID: 2, Entry: 2, Ref: r}); e != nil {
				t.Fatal(e)
			}
			select {
			case <-parked:
			case <-ctx.Done():
				t.Fatal("mixed word did not enter write")
			}
			relctx, stop := context.WithCancel(ctx)
			answer := make(chan error, 1)
			go func() {
				answer <- b.ReleaseNative(relctx, engine.ReleaseRequest{Token: engine.ReleaseToken{Session: 1, Generation: 1}, Mode: engine.ReleaseTrackChange})
			}()
			// Observe the producer lifetime cancellation: BeginRelease has already
			// fenced every native sink while the partial hardware write is parked.
			<-b.known(1).ctx.Done()
			if late {
				stop()
				if e = <-answer; !errors.Is(e, engine.ErrReleaseUnconfirmed) {
					t.Fatal(e)
				}
			}
			unpark()
			if !late {
				if e = <-answer; e != nil {
					t.Fatal(e)
				}
			}
			stop()
			if e = b.Wait(ctx); e != nil {
				t.Fatal(e)
			}
			ev.mu.Lock()
			defer ev.mu.Unlock()
			want := []string{"details:1:1:2822400:DSD:true", "audible:1", "details:2:1:2822400:DSD:true", "audible:2", "position:2:1:2822400"}
			at := 0
			for _, r := range ev.records {
				if at < len(want) && r == want[at] {
					at++
				}
			}
			if at != len(want) {
				t.Fatalf("accepted two-occurrence prefix lost: %v", ev.records)
			}
			if late {
				lastPosition, firstAck := -1, len(ev.records)
				for i, r := range ev.records {
					if strings.HasPrefix(r, "position:") {
						lastPosition = i
					}
					if r == "released" && i < firstAck {
						firstAck = i
					}
				}
				if firstAck < lastPosition {
					t.Fatalf("ack preceded final progress: %v", ev.records)
				}
			}
			if late && ev.records[len(ev.records)-1] != "released" {
				t.Fatalf("ack preceded final progress: %v", ev.records)
			}
		})
	}
}

func (v nativeTee) CurrentOccurrence(s engine.SessionID, o engine.OccurrenceID, p *engine.NativePlan) {
	v.Events.(engine.CurrentOccurrenceEvents).CurrentOccurrence(s, o, p)
}

func TestNativeIntegrationMixedPolicy(t *testing.T) {
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(2, 10, 49, 50, 52)
	k.Free()
	second := nativeDSF(t, 31)
	binary.LittleEndian.PutUint32(second[56:60], 5644800)
	b, e := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31), "b": second}, Locator: nativeLocator{}})
	if e != nil {
		t.Fatal(e)
	}
	AdmitNativeForIntegration(b, func(d format.NativeDSD) (engine.NativePlan, error) {
		p, _ := nativeTestPlan(d)
		p.PolicyID = fmt.Sprint(d.BitRateHz)
		return p, nil
	}, k.OpenNativeSink)
	ev := &nativeEvents{done: make(chan struct{})}
	r, _ := engine.NewReference("a")
	r2, _ := engine.NewReference("b")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Entry: 1, Ref: r}, Events: ev}); e != nil {
		t.Fatal(e)
	}
	if e = b.Prepare(ctx, 1, 1, &engine.Occurrence{ID: 2, Entry: 2, Ref: r2}); e != nil {
		t.Fatal(e)
	}
	s := b.known(1)
	for {
		s.mu.Lock()
		o := s.find(2)
		done := o != nil && o.opened()
		var ch <-chan struct{}
		if o != nil {
			ch = o.done
		}
		s.mu.Unlock()
		if done {
			break
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("successor open stalled")
		}
	}
	if e = b.Prepare(ctx, 1, 2, nil); e != nil {
		t.Fatal(e)
	}
	select {
	case <-ev.done:
	case <-ctx.Done():
		t.Fatal("mixed transition stalled")
	}
	s.mu.Lock()
	cur := s.cur
	id := cur.policyID
	s.mu.Unlock()
	if id != "5644800" {
		t.Fatalf("mixed boundary changed policy ID: %q", id)
	}
	if e = b.Release(ctx, 1); e != nil {
		t.Fatal(e)
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	found := false
	for _, r := range ev.records {
		if r == "position:2:31:5644800" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mixed boundary lost logical position: %v", ev.records)
	}
}

type nativeStalledReader struct {
	*bytes.Reader
	entered, gate chan struct{}
	once          sync.Once
}

func (r *nativeStalledReader) ReadAt(p []byte, off int64) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.gate
	return r.Reader.ReadAt(p, off)
}

type nativeStalledResolver struct {
	first []byte
	held  map[string]*nativeStalledReader
}

func (r nativeStalledResolver) Open(_ context.Context, ref engine.Reference) (Content, error) {
	if s := r.held[ref.String()]; s != nil {
		return Content{Data: s, Size: s.Size()}, nil
	}
	return Content{Data: bytes.NewReader(r.first), Size: int64(len(r.first))}, nil
}
func TestNativeIntegrationStalledReservations(t *testing.T) { testNativeStalledReservations(t, false) }
func testNativeStalledReservations(t *testing.T, dop bool) {
	plan := nativeTestPlan
	if dop {
		plan = dopTestPlan
	}
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(10, 49, 50, 52)
	data := nativeDSF(t, 31)
	resolver := nativeStalledResolver{first: data, held: map[string]*nativeStalledReader{}}
	for i := 2; i <= 4; i++ {
		resolver.held[fmt.Sprint(i)] = &nativeStalledReader{Reader: bytes.NewReader(data), entered: make(chan struct{}), gate: make(chan struct{})}
	}
	var free sync.Once
	unblock := func() {
		free.Do(func() {
			for _, r := range resolver.held {
				close(r.gate)
			}
		})
	}
	defer unblock()
	b, e := New(Config{Resolver: resolver, Locator: nativeLocator{}})
	if e != nil {
		t.Fatal(e)
	}
	AdmitNativeForIntegration(b, plan, k.OpenNativeSink)
	budget := engine.NewOccurrenceBudget(4)
	var leases []*engine.OccurrenceReservation
	take := func(id int) engine.Occurrence {
		r := budget.Reserve()
		if r == nil {
			t.Fatal("early budget refusal")
		}
		leases = append(leases, r)
		ref, _ := engine.NewReference(fmt.Sprint(id))
		return engine.Occurrence{ID: engine.OccurrenceID(id), Entry: uint32(id), Ref: ref, Reservation: r}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev := &nativeEvents{done: make(chan struct{})}
	if e = b.Start(ctx, engine.StartRequest{Session: 1, First: take(1), Events: ev}); e != nil {
		t.Fatal(e)
	}
	for i := 2; i <= 4; i++ {
		o := take(i)
		if e = b.Prepare(ctx, 1, 1, &o); e != nil {
			t.Fatal(e)
		}
		select {
		case <-resolver.held[fmt.Sprint(i)].entered:
		case <-ctx.Done():
			t.Fatal("reader did not stall")
		}
		o.Reservation.ReleaseEngine()
		o.Reservation.ReleaseEngine()
	}
	if extra := budget.Reserve(); extra != nil {
		t.Fatal("fifth allocation while cancelled readers retained four slots")
	}
	unblock()
	if e = b.Release(ctx, 1); e != nil {
		t.Fatal(e)
	}
	if e = b.Wait(ctx); e != nil {
		t.Fatal(e)
	}
	for _, r := range leases {
		r.ReleaseEngine()
	}
	if budget.Used() != 0 {
		t.Fatalf("reservation leak: %d", budget.Used())
	}
}

// These are regression pins for the newly connected backend boundary. The
// lower sink's cancellation and mixed-word rules were pinned before this phase.
func TestNativeIntegrationCarryCancel(t *testing.T) {
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	k.Free()
	b, err := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
	if err != nil {
		t.Fatal(err)
	}
	AdmitNativeForIntegration(b, nativeTestPlan, k.OpenNativeSink)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref, _ := engine.NewReference("a")
	ev := &nativeEvents{done: make(chan struct{})}
	if err = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}, Events: ev}); err != nil {
		t.Fatal(err)
	}
	out := b.known(1).cur
	if err = b.ReleaseNative(ctx, engine.ReleaseRequest{Token: engine.ReleaseToken{Session: 1, Generation: 1}, Mode: engine.ReleaseImmediate}); err != nil {
		t.Fatal(err)
	}
	if k.View().Writes != 0 || out.native.Position().Written != 0 {
		t.Fatal("cancel emitted uncommitted carry or tail")
	}
}
func TestNativeIntegrationTypedRevoke(t *testing.T) {
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	k.Free()
	b, err := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
	if err != nil {
		t.Fatal(err)
	}
	AdmitNativeForIntegration(b, nativeTestPlan, k.OpenNativeSink)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref, _ := engine.NewReference("a")
	ev := &nativeEvents{done: make(chan struct{})}
	if err = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}, Events: ev}); err != nil {
		t.Fatal(err)
	}
	out := b.known(1).cur
	b.Revoke(engine.RevokeInput{Session: 1, Reason: "test revocation"})
	n, err := out.native.WriteDSD(ctx, 2, []byte{255}, []byte{255}, 0, 1)
	if n != 0 || err == nil {
		t.Errorf("revoked session accepted bits: %d %v", n, err)
	}
	if err = b.Release(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if k.View().Writes != 0 {
		t.Fatal("revocation emitted bytes")
	}
}
func TestNativeIntegrationCarryMixedCut(t *testing.T) {
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	k.Free()
	b, err := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
	if err != nil {
		t.Fatal(err)
	}
	AdmitNativeForIntegration(b, nativeTestPlan, k.OpenNativeSink)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref, _ := engine.NewReference("a")
	ev := &nativeEvents{done: make(chan struct{})}
	if err = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}, Events: ev}); err != nil {
		t.Fatal(err)
	}
	parked, unpark := k.ParkNextWrite()
	defer unpark()
	if err = b.Prepare(ctx, 1, 1, &engine.Occurrence{ID: 2, Ref: ref}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parked:
	case <-ctx.Done():
		t.Fatal("mixed word did not enter kernel")
	}
	// Kernel ownership starts before the parked write returns. Cutting through
	// its two-occurrence word is refused even though accepted-prefix is pending.
	out := b.known(1).cur
	ok, err := out.native.Cut(31)
	if err != nil || ok {
		t.Errorf("mixed committed word was cut: %t %v", ok, err)
	}
	unpark()
	if err = b.Release(ctx, 1); err != nil {
		t.Fatal(err)
	}
}

func TestNativeIntegrationPauseKeepsPlan(t *testing.T) { testNativePauseKeepsPlan(t, false) }
func testNativePauseKeepsPlan(t *testing.T, dop bool) {
	plan := nativeTestPlan
	if dop {
		plan = dopTestPlan
	}
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(10, 49, 50, 52)
	b, err := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
	if err != nil {
		t.Fatal(err)
	}
	admissions := 0
	AdmitNativeForIntegration(b, func(d format.NativeDSD) (engine.NativePlan, error) { admissions++; return plan(d) }, k.OpenNativeSink)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref, _ := engine.NewReference("a")
	ev := &nativeEvents{done: make(chan struct{})}
	if err = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}, Events: ev}); err != nil {
		t.Fatal(err)
	}
	s := b.known(1)
	s.mu.Lock()
	out := s.cur
	policy := out.policyID
	s.mu.Unlock()
	if err = b.Pause(ctx, 1); err != nil {
		t.Fatal(err)
	}
	k.Grant(31 * time.Second)
	for {
		ch := out.native.Changed()
		if out.native.Position().Released {
			break
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("native pause did not release at 30 seconds")
		}
	}
	if _, err = b.Resume(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if out.policyID != policy || out.policyID != "test-policy" || admissions != 1 || k.View().Opens != 2 {
		t.Fatalf("reopen changed plan: policy=%s admissions=%d opens=%d", out.policyID, admissions, k.View().Opens)
	}
	if out.native.Position().Submitted != 31 {
		t.Fatal("reopen lost retained content")
	}
	if err = b.Release(ctx, 1); err != nil {
		t.Fatal(err)
	}
}

type nativeSuccessorBarrier struct {
	nativeRecordingBackend
	entered, allow chan struct{}
}

func (b nativeSuccessorBarrier) Start(ctx context.Context, r engine.StartRequest) error {
	if r.Session > 1 {
		close(b.entered)
		select {
		case <-b.allow:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return b.nativeRecordingBackend.Start(ctx, r)
}
func TestNativeIntegrationRetiringThroughEngine(t *testing.T) {
	nativeEngineStorage(t)
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	k.Free()
	b, err := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
	if err != nil {
		t.Fatal(err)
	}
	AdmitNativeForIntegration(b, nativeTestPlan, k.OpenNativeSink)
	events := &nativeEvents{done: make(chan struct{})}
	barrier := nativeSuccessorBarrier{nativeRecordingBackend{b, events}, make(chan struct{}), make(chan struct{})}
	dir := t.TempDir()
	eng, err := engine.New(engine.Config{Backend: barrier, PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e", DataDir: dir, QueuePath: filepath.Join(dir, "queue.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	var once sync.Once
	allow := func() { once.Do(func() { close(barrier.allow) }) }
	t.Cleanup(func() {
		allow()
		cancel()
		<-done
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := b.Wait(c); err != nil {
			t.Error(err)
		}
	})
	ref, _ := engine.NewReference("a")
	var ids []uint32
	var prev uint32
	for range 3 {
		id, e := eng.InsertAfter(ctx, prev, ref, nil)
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
		prev = id
	}
	if err = eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true}); err != nil {
		t.Fatal(err)
	}
	parked, unpark := k.ParkNextWrite()
	defer unpark()
	if err = eng.Play(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parked:
	case <-ctx.Done():
		t.Fatal("two-occurrence hardware prefix did not park")
	}
	s := b.known(1)
	nav := make(chan error, 1)
	go func() { nav <- eng.PlayID(ctx, ids[2]) }()
	select {
	case <-s.ctx.Done():
	case <-ctx.Done():
		t.Fatal("navigation did not establish release")
	}
	unpark()
	select {
	case <-barrier.entered:
	case <-ctx.Done():
		t.Fatal("release did not authorize successor")
	}
	sn := eng.Snapshot()
	if sn.Selected() != ids[2] || sn.TrackCount() != 2 {
		t.Fatalf("retiring accounting changed destination or lost a boundary: selected=%d count=%d", sn.Selected(), sn.TrackCount())
	}
	// The barrier is entered only after final release events and acknowledgement
	// were consumed; the successor has not opened or submitted any content yet.
	if k.View().Opens != 1 {
		t.Fatal("successor opened before release barrier")
	}
	allow()
	select {
	case err = <-nav:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("navigation stalled")
	}
	if err = eng.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNativeIntegrationDeleteDuringActivation(t *testing.T) {
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	k.Free()
	b, err := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
	if err != nil {
		t.Fatal(err)
	}
	entered, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(gate) }) }
	defer unblock()
	AdmitNativeForIntegration(b, func(d format.NativeDSD) (engine.NativePlan, error) {
		p, _ := nativeTestPlan(d)
		p.Prefill = time.Millisecond
		return p, nil
	}, func(ctx context.Context, c alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
		before := c.Check
		calls := 0
		c.Check = func(n alsapcm.NodeInfo) error {
			calls++
			if calls == 2 {
				close(entered)
				<-gate
			}
			return before(n)
		}
		return k.OpenNativeSink(ctx, c)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref, _ := engine.NewReference("a")
	ev := &nativeEvents{done: make(chan struct{})}
	answer := make(chan error, 1)
	go func() {
		answer <- b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}, Events: ev})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("activation did not park")
	}
	if ok, e := b.Cut(ctx, 1, 1); e != nil || !ok {
		t.Fatalf("delete inert current: %t %v", ok, e)
	}
	unblock()
	select {
	case <-answer:
	case <-ctx.Done():
		t.Fatal("start did not retire")
	}
	_ = b.Release(ctx, 1)
	if err = b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if k.View().Writes != 0 {
		t.Fatalf("deleted occurrence wrote prefill/music: %+v", k.View())
	}
}

func TestNativeIntegrationPlaybackErrorReleases(t *testing.T) {
	testNativePlaybackErrorReleases(t, false)
}
func testNativePlaybackErrorReleases(t *testing.T, dop bool) {
	plan := nativeTestPlan
	if dop {
		plan = dopTestPlan
	}
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(10, 49, 50, 52)
	b, _ := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
	AdmitNativeForIntegration(b, plan, func(ctx context.Context, c alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
		checks := 0
		c.Check = func(alsapcm.NodeInfo) error {
			checks++
			if checks == 2 {
				return errors.New("activation refusal")
			}
			return nil
		}
		return k.OpenNativeSink(ctx, c)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref, _ := engine.NewReference("a")
	ev := &nativeEvents{done: make(chan struct{})}
	if err := b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}, Events: ev}); err == nil {
		t.Fatal("activation succeeded")
	}
	if err := b.Release(ctx, 1); err != nil {
		t.Fatalf("playback error poisoned successful cleanup: %v", err)
	}
	if err := b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.hold(newSession(b, 2)); err != nil {
		t.Fatalf("successful cleanup refused explicit next play: %v", err)
	}
	k.CheckClosedEach(t)
}

type nativePauseBarrier struct {
	streamSink
	entered, allow chan struct{}
}

func (p nativePauseBarrier) Pause() error {
	err := p.streamSink.Pause()
	close(p.entered)
	<-p.allow
	return err
}
func TestNativeIntegrationInstallationResume(t *testing.T) {
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	b, _ := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
	AdmitNativeForIntegration(b, nativeTestPlan, k.OpenNativeSink)
	s := newSession(b, 1)
	defer s.cancel()
	s.ev = &nativeEvents{done: make(chan struct{})}
	ref, _ := engine.NewReference("a")
	o := &occ{id: 1, ref: ref, done: make(chan struct{})}
	b.open(s.ctx, o)
	close(o.done)
	out, err := s.openOutput(s.ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	defer s.closeOutput(out)
	gate := nativePauseBarrier{out.sink, make(chan struct{}), make(chan struct{})}
	out.sink = gate
	s.paused = true
	s.handed = out
	s.chain = []*occ{o}
	answer := make(chan bool, 1)
	go func() { answer <- s.transition(o) }()
	<-gate.entered
	if _, err := s.resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(gate.allow)
	if !<-answer {
		t.Fatal("resume during installation activated twice")
	}
	s.mu.Lock()
	fault := s.fault
	s.mu.Unlock()
	if fault != nil {
		t.Fatalf("installation fault: %+v", fault)
	}
}

func TestNativeIntegrationBetweenTransportPauseDeadline(t *testing.T) {
	nativeEngineStorage(t)
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	k.Free()
	second := nativeDSF(t, 31)
	binary.LittleEndian.PutUint32(second[56:60], 5644800)
	entered, allow := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(allow) }) }
	defer unblock()
	type timerCall struct {
		delay time.Duration
		fire  func()
	}
	timer := make(chan timerCall, 8)
	b, _ := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31), "b": second}, Locator: nativeLocator{}, Seams: &Seams{TransportDrained: func() { close(entered); <-allow }}, AfterFunc: func(d time.Duration, f func()) func() bool {
		timer <- timerCall{d, f}
		return func() bool { return true }
	}})
	AdmitNativeForIntegration(b, nativeTestPlan, k.OpenNativeSink)
	dir := t.TempDir()
	eng, err := engine.New(engine.Config{Backend: b, PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e", DataDir: dir, QueuePath: filepath.Join(dir, "q.json"), PauseRelease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	t.Cleanup(func() {
		unblock()
		cancel()
		<-done
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := b.Wait(c); err != nil {
			t.Error(err)
		}
	})
	a, _ := engine.NewReference("a")
	bb, _ := engine.NewReference("b")
	id, _ := eng.InsertAfter(ctx, 0, a, nil)
	_, _ = eng.InsertAfter(ctx, id, bb, nil)
	_ = eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true})
	if err := eng.Play(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("transition did not drain")
	}
	if err := eng.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	var call timerCall
	select {
	case call = <-timer:
	case <-ctx.Done():
		t.Fatal("no transition timer")
	}
	if call.delay != 30*time.Second {
		t.Errorf("native no-device pause armed %v, engine expects 30s", call.delay)
	}
	unblock()
	call.fire()
	for {
		ch := eng.Changed()
		if eng.Snapshot().DeviceReleased() {
			break
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("no-device release not reported")
		}
	}
	if _, bad := eng.Snapshot().ReleaseUnconfirmed(); bad {
		t.Fatal("confirmed empty transition marked unconfirmed")
	}
}

func TestNativeIntegrationSuccessfulCutReplacement(t *testing.T) {
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	b, _ := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31), "b": nativeDSF(t, 131071)}, Locator: nativeLocator{}})
	AdmitNativeForIntegration(b, nativeTestPlan, func(ctx context.Context, c alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
		c.RingBytes = 32000
		c.BufferTime = time.Millisecond
		return k.OpenNativeSink(ctx, c)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, _ := engine.NewReference("a")
	bb, _ := engine.NewReference("b")
	ev := &nativeEvents{done: make(chan struct{})}
	if err := b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: a}, Events: ev}); err != nil {
		t.Fatal(err)
	}
	defer b.Release(context.Background(), 1)
	if err := b.Pause(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := b.Prepare(ctx, 1, 1, &engine.Occurrence{ID: 2, Ref: bb}); err != nil {
		t.Fatal(err)
	}
	out := b.known(1).cur
	for {
		ch := out.native.Changed()
		if out.native.Position().Submitted == 128000 {
			break
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("successor did not fill software ring")
		}
	}
	if ok, err := b.Cut(ctx, 1, 2); err != nil || !ok {
		t.Fatalf("software cut: %t %v", ok, err)
	}
	if err := b.Prepare(ctx, 1, 1, &engine.Occurrence{ID: 3, Ref: a}); err != nil {
		t.Fatal(err)
	}
	if err := b.Prepare(ctx, 1, 3, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Resume(ctx, 1); err != nil {
		t.Fatal(err)
	}
	k.Free()
	select {
	case <-ev.done:
	case <-ctx.Done():
		t.Fatal("replacement never completed after successful cut")
	}
	if err := b.Release(ctx, 1); err != nil {
		t.Fatal(err)
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	if !slices.Contains(ev.records, "position:3:31:2822400") {
		t.Fatalf("replacement not played: %v", ev.records)
	}
}

func TestNativeIntegrationFaultThenExplicitPCM(t *testing.T) {
	nativeEngineStorage(t)
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(2, 6, 10, 49, 50, 52)
	k.Free()
	pcm, err := os.ReadFile(filepath.Join("..", "decode", "testdata", "goldens", "decorrelated_16bit_44100.wav"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := New(Config{Resolver: nativeFiles{"dsd": nativeDSF(t, 31), "pcm": pcm}, Locator: nativeLocator{}, OpenSink: k.OpenSink})
	AdmitNativeForIntegration(b, nativeTestPlan, func(ctx context.Context, c alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
		checks := 0
		c.Check = func(alsapcm.NodeInfo) error {
			checks++
			if checks == 2 {
				return errors.New("native activation refused")
			}
			return nil
		}
		return k.OpenNativeSink(ctx, c)
	})
	dir := t.TempDir()
	eng, err := engine.New(engine.Config{Backend: b, PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e", DataDir: dir, QueuePath: filepath.Join(dir, "q.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := b.Wait(c); err != nil {
			t.Error(err)
		}
		k.CheckClosedEach(t)
	})
	d, _ := engine.NewReference("dsd")
	p, _ := engine.NewReference("pcm")
	id, _ := eng.InsertAfter(ctx, 0, d, nil)
	pcmID, _ := eng.InsertAfter(ctx, id, p, nil)
	_ = eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true})
	if err := eng.Play(ctx); err == nil {
		t.Fatal("native activation succeeded")
	}
	for {
		ch := eng.Changed()
		if eng.Snapshot().State() == engine.Stopped {
			break
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("native fault cleanup stalled")
		}
	}
	sn := eng.Snapshot()
	if _, bad := sn.ReleaseUnconfirmed(); bad {
		t.Fatal("playback error classified as failed cleanup")
	}
	if len(sn.Faults()) != 1 {
		t.Fatalf("fault count %d", len(sn.Faults()))
	}
	if err := eng.PlayID(ctx, pcmID); err != nil {
		t.Fatalf("explicit PCM after confirmed cleanup refused: %v", err)
	}
}

func TestNativeIntegrationApprovedConstructor(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "version", "identity", "tampered", "candidate", "replaced_before_activation", "revoke_before_publication"} {
		t.Run(mode, func(t *testing.T) {
			cfg, record := approvedBackendFixture(t)
			cfg.Resolver = nativeFiles{"a": nativeDSF(t, 31)}
			switch mode {
			case "missing":
				os.Remove(cfg.Authorization.path())
			case "version":
				record.Body.QualificationVersion--
				saveApprovedFixture(t, cfg.Authorization, record)
			case "identity":
				record.Body.Identity.BCDDevice = "ffff"
				saveApprovedFixture(t, cfg.Authorization, record)
			case "tampered":
				os.WriteFile(cfg.Authorization.path(), []byte(`{"Body":{},"SHA256":"bad"}`), 0600)
			case "candidate":
				record.Body.Confirmation = "CLI candidate; not approved"
				saveApprovedFixture(t, cfg.Authorization, record)
			}
			b, e := New(cfg)
			if e != nil {
				t.Fatal(e)
			}
			k := alsapcm.NewFakeKernel(t)
			k.HardwareFormats(49, 50, 52)
			k.Free()
			b.nativeOpen = func(ctx context.Context, c alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
				sink, e := k.OpenNativeSink(ctx, c)
				if mode == "replaced_before_activation" {
					os.Remove(cfg.Authorization.path())
				}
				if mode == "revoke_before_publication" {
					b.Revoke(engine.RevokeInput{Session: 1, Reason: "before publication"})
				}
				return sink, e
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ref, _ := engine.NewReference("a")
			ev := &nativeEvents{done: make(chan struct{})}
			e = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}, Events: ev})
			if (e == nil) != (mode == "valid") {
				t.Fatalf("start=%v", e)
			}
			if mode != "valid" && k.View().Writes != 0 {
				t.Fatal("invalid approval wrote hardware")
			}
			if mode != "valid" && mode != "replaced_before_activation" && mode != "revoke_before_publication" && k.View().Opens != 0 {
				t.Fatal("invalid approval opened hardware")
			}
			if err := b.Release(ctx, 1); err != nil {
				t.Fatal(err)
			}
			if err := b.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			k.CheckClosedEach(t)
		})
	}
}

func TestNativeIntegrationRevokeRetiringTail(t *testing.T) {
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	k.Free()
	b, err := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
	if err != nil {
		t.Fatal(err)
	}
	AdmitNativeForIntegration(b, nativeTestPlan, k.OpenNativeSink)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref, _ := engine.NewReference("a")
	if err = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}, Events: &nativeEvents{done: make(chan struct{})}}); err != nil {
		t.Fatal(err)
	}
	parked, unpark := k.ParkNextWrite()
	defer unpark()
	result := make(chan error, 1)
	go func() {
		result <- b.ReleaseNative(ctx, engine.ReleaseRequest{Token: engine.ReleaseToken{Session: 1, Generation: 1}, Mode: engine.ReleaseTrackChange})
	}()
	select {
	case <-parked:
	case <-ctx.Done():
		t.Fatal("retiring tail did not reach write")
	}
	s := b.known(1)
	if s == nil {
		t.Fatal("retired before tail completion")
	}
	b.Revoke(engine.RevokeInput{Session: 1, Reason: "record revoked during tail"})
	unpark()
	select {
	case err = <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("revoked tail did not finish")
	}
	if err = b.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if k.View().Drains != 0 {
		t.Fatal("revoked retiring tail drained instead of Immediate")
	}
	b.Revoke(engine.RevokeInput{Session: 1, Reason: "late monitor callback"})
	if b.Stats().Sessions != 0 {
		t.Fatal("late monitor recreated session")
	}
	k.CheckClosedEach(t)
}

func TestNativeIntegrationApprovedPCMGapless(t *testing.T) {
	cfg, _ := approvedBackendFixture(t)
	pcm, e := os.ReadFile(filepath.Join("..", "decode", "testdata", "goldens", "decorrelated_16bit_44100.wav"))
	if e != nil {
		t.Fatal(e)
	}
	cfg.Resolver = nativeFiles{"a": pcm}
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(2, 6, 10)
	k.Free()
	cfg.OpenSink = k.OpenSink
	b, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = b.Release(context.Background(), 1); _ = b.Wait(context.Background()); k.CheckClosedEach(t) })
	b.RefreshAuthorization(ctx)
	generation := b.Policy().Generation
	ev := &nativeEvents{done: make(chan struct{})}
	ref, _ := engine.NewReference("a")
	if e = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}, Events: ev}); e != nil {
		t.Fatal(e)
	}
	if e = b.Prepare(ctx, 1, 1, &engine.Occurrence{ID: 2, Ref: ref}); e != nil {
		t.Fatal(e)
	}
	if e = b.Prepare(ctx, 1, 2, nil); e != nil {
		t.Fatal(e)
	}
	select {
	case <-ev.done:
	case <-ctx.Done():
		t.Fatal("approved PCM sequence stalled")
	}
	ev.mu.Lock()
	records := slices.Clone(ev.records)
	ev.mu.Unlock()
	if !slices.Contains(records, "audible:2") || k.View().Opens != 1 {
		t.Fatalf("PCM gapless refused: %v opens=%d", records, k.View().Opens)
	}
	if b.Policy().Generation != generation {
		t.Fatal("PCM changed authorization epoch")
	}
}

func TestNativeIntegrationPausedAdmissionEdge(t *testing.T) {
	cfg, record := approvedBackendFixture(t)
	record.Body.Edges = nil
	saveApprovedFixture(t, cfg.Authorization, record)
	cfg.Resolver = nativeFiles{"a": nativeDSF(t, 31)}
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	k.Free()
	b, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	b.nativeOpen = k.OpenNativeSink
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = b.Release(context.Background(), 1); _ = b.Wait(context.Background()); k.CheckClosedEach(t) })
	s := b.session(1)
	s.paused = true
	ref, _ := engine.NewReference("a")
	ev := &nativeEvents{done: make(chan struct{})}
	if e = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}, Events: ev}); e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	first := s.chain[0]
	s.mu.Unlock()
	if e = b.checkEdge(first.transport, first.plan, first.permission); e == nil {
		t.Fatal("paused native installation omitted its outgoing edge")
	}
	if k.View().Writes != 0 {
		t.Fatal("paused installation wrote audio")
	}
}

func TestNativeIntegrationPausedAdmissionConstraints(t *testing.T) {
	cfg, _ := approvedBackendFixture(t)
	cfg.Resolver = nativeFiles{"a": nativeDSF(t, 31)}
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	k.Free()
	b, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	b.nativeOpen = func(ctx context.Context, c alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
		sink, e := k.OpenNativeSink(ctx, c)
		if e == nil {
			if e = os.Remove(cfg.Authorization.path()); e != nil {
				t.Fatal(e)
			}
		}
		return sink, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Cleanup(func() { _ = b.Release(context.Background(), 1); _ = b.Wait(context.Background()); k.CheckClosedEach(t) })
	s := b.session(1)
	s.paused = true
	ref, _ := engine.NewReference("a")
	e = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Ref: ref}, Events: &nativeEvents{done: make(chan struct{})}})
	if e == nil || k.View().Writes != 0 {
		t.Fatal("paused installation bypassed changed permission", e)
	}
}

func dopTestPlan(d format.NativeDSD) (engine.NativePlan, error) {
	p, e := nativeTestPlan(d)
	p.Tuple.Transport = "dop"
	p.Tuple.Format = "S32_LE"
	return p, e
}
func TestNativeIntegrationDoPThroughEngine(t *testing.T) {
	t.Run("StalledReservations", func(t *testing.T) { testNativeStalledReservations(t, true) })
	t.Run("PauseKeepsPlan", func(t *testing.T) { testNativePauseKeepsPlan(t, true) })
	t.Run("ReleaseCleanup", func(t *testing.T) { testNativeReleaseCleanup(t, true) })
	t.Run("PlaybackErrorReleases", func(t *testing.T) { testNativePlaybackErrorReleases(t, true) })

	nativeEngineStorage(t)
	for _, bits := range []int{1, 7, 15, 31} {
		t.Run(fmt.Sprint(bits), func(t *testing.T) {
			k := alsapcm.NewFakeKernel(t)
			k.HardwareFormats(2, 10, 32, 49, 50, 52)
			k.Free()
			b, e := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, bits)}, Locator: nativeLocator{}})
			if e != nil {
				t.Fatal(e)
			}
			AdmitNativeForIntegration(b, dopTestPlan, k.OpenNativeSink)
			events := &nativeEvents{done: make(chan struct{})}
			dir := t.TempDir()
			eng, e := engine.New(engine.Config{Backend: nativeRecordingBackend{b, events}, PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e", DataDir: dir, QueuePath: filepath.Join(dir, "queue.json")})
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); _ = eng.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				<-done
				c, stop := context.WithTimeout(context.Background(), 5*time.Second)
				defer stop()
				if e := b.Wait(c); e != nil {
					t.Error(e)
				}
				k.CheckClosedEach(t)
			})
			ref, _ := engine.NewReference("a")
			a, e := eng.InsertAfter(ctx, 0, ref, nil)
			if e != nil {
				t.Fatal(e)
			}
			_, e = eng.InsertAfter(ctx, a, ref, nil)
			if e != nil {
				t.Fatal(e)
			}
			if e = eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true}); e != nil {
				t.Fatal(e)
			}
			if e = eng.Play(ctx); e != nil {
				t.Fatal(e)
			}
			for {
				ch := eng.Changed()
				if eng.Snapshot().State() == engine.Stopped {
					break
				}
				select {
				case <-ch:
				case <-time.After(5 * time.Second):
					t.Fatal("tiny engine stalled")
				}
			}
			if k.View().Opens != 1 {
				t.Errorf("natural compatible advance reopened: %d", k.View().Opens)
			}
			if n := eng.Snapshot().TrackCount(); n != 2 {
				t.Fatalf("heard %d occurrences, want two", n)
			}
			events.mu.Lock()
			defer events.mu.Unlock()
			for _, id := range []int{1, 2} {
				for _, want := range []string{fmt.Sprintf("details:%d:1:2822400:DSD:true", id), fmt.Sprintf("audible:%d", id), fmt.Sprintf("position:%d:%d:2822400", id, bits)} {
					if !slices.Contains(events.records, want) {
						t.Errorf("missing %s: %v", want, events.records)
					}
				}
			}
		})
	}
}

func TestNativeIntegrationDoPRetiring(t *testing.T) {
	for _, selector := range []string{"Next", "Previous", "SeekId", "SeekIndex"} {
		t.Run(selector, func(t *testing.T) { testDoPRetiring(t, selector) })
	}
}
func testDoPRetiring(t *testing.T, selector string) {
	nativeEngineStorage(t)
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(2, 10, 32, 49, 50, 52)
	k.Free()
	b, err := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 15)}, Locator: nativeLocator{}})
	if err != nil {
		t.Fatal(err)
	}
	AdmitNativeForIntegration(b, dopTestPlan, k.OpenNativeSink)
	events := &nativeEvents{done: make(chan struct{})}
	barrier := nativeSuccessorBarrier{nativeRecordingBackend{b, events}, make(chan struct{}), make(chan struct{})}
	dir := t.TempDir()
	eng, err := engine.New(engine.Config{Backend: barrier, PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e", DataDir: dir, QueuePath: filepath.Join(dir, "queue.json")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	var once sync.Once
	allow := func() { once.Do(func() { close(barrier.allow) }) }
	t.Cleanup(func() {
		allow()
		cancel()
		<-done
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := b.Wait(c); err != nil {
			t.Error(err)
		}
	})
	ref, _ := engine.NewReference("a")
	var ids []uint32
	var prev uint32
	for range 3 {
		id, e := eng.InsertAfter(ctx, prev, ref, nil)
		if e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
		prev = id
	}
	if err = eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true}); err != nil {
		t.Fatal(err)
	}
	parked, unpark := k.ParkNextWrite()
	defer unpark()
	if err = eng.Play(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parked:
	case <-ctx.Done():
		t.Fatal("two-occurrence hardware prefix did not park")
	}
	s := b.known(1)
	nav := make(chan error, 1)
	target := ids[2]
	navigate := func() error { return eng.PlayID(ctx, ids[2]) }
	switch selector {
	case "Next":
		target = ids[1]
		navigate = func() error { return eng.Next(ctx) }
	case "Previous":
		target = ids[0]
		navigate = func() error { return eng.Previous(ctx) }
	case "SeekIndex":
		navigate = func() error { return eng.PlayIndex(ctx, 2) }
	}
	go func() { nav <- navigate() }()
	select {
	case <-s.ctx.Done():
	case <-ctx.Done():
		t.Fatal("navigation did not establish release")
	}
	unpark()
	select {
	case <-barrier.entered:
	case <-ctx.Done():
		t.Fatal("release did not authorize successor")
	}
	sn := eng.Snapshot()
	if sn.Selected() != target || sn.TrackCount() != 2 {
		t.Fatalf("retiring accounting changed destination or lost a boundary: selected=%d count=%d", sn.Selected(), sn.TrackCount())
	}
	// The barrier is entered only after final release events and acknowledgement
	// were consumed; the successor has not opened or submitted any content yet.
	if k.View().Opens != 1 {
		t.Fatal("successor opened before release barrier")
	}
	allow()
	select {
	case err = <-nav:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("navigation stalled")
	}
	if err = eng.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestNativeIntegrationDoPTransitions(t *testing.T) {
	for _, mode := range []string{"same_carrier", "cross_rate", "policy"} {
		t.Run(mode, func(t *testing.T) { doPTransitionEngine(t, mode) })
	}
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(2, 10, 49, 50, 52)
	k.Free()
	second := nativeDSF(t, 31)
	binary.LittleEndian.PutUint32(second[56:60], 5644800)
	b, e := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31), "b": second}, Locator: nativeLocator{}})
	if e != nil {
		t.Fatal(e)
	}
	AdmitNativeForIntegration(b, func(d format.NativeDSD) (engine.NativePlan, error) {
		p, _ := dopTestPlan(d)
		p.PolicyID = fmt.Sprint(d.BitRateHz)
		return p, nil
	}, k.OpenNativeSink)
	ev := &nativeEvents{done: make(chan struct{})}
	r, _ := engine.NewReference("a")
	r2, _ := engine.NewReference("b")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e = b.Start(ctx, engine.StartRequest{Session: 1, First: engine.Occurrence{ID: 1, Entry: 1, Ref: r}, Events: ev}); e != nil {
		t.Fatal(e)
	}
	if e = b.Prepare(ctx, 1, 1, &engine.Occurrence{ID: 2, Entry: 2, Ref: r2}); e != nil {
		t.Fatal(e)
	}
	s := b.known(1)
	for {
		s.mu.Lock()
		o := s.find(2)
		done := o != nil && o.opened()
		var ch <-chan struct{}
		if o != nil {
			ch = o.done
		}
		s.mu.Unlock()
		if done {
			break
		}
		select {
		case <-ch:
		case <-ctx.Done():
			t.Fatal("successor open stalled")
		}
	}
	if e = b.Prepare(ctx, 1, 2, nil); e != nil {
		t.Fatal(e)
	}
	select {
	case <-ev.done:
	case <-ctx.Done():
		t.Fatal("mixed transition stalled")
	}
	s.mu.Lock()
	cur := s.cur
	id := cur.policyID
	s.mu.Unlock()
	if id != "5644800" {
		t.Fatalf("mixed boundary changed policy ID: %q", id)
	}
	if e = b.Release(ctx, 1); e != nil {
		t.Fatal(e)
	}
	ev.mu.Lock()
	defer ev.mu.Unlock()
	found := false
	for _, r := range ev.records {
		if r == "position:2:31:5644800" {
			found = true
		}
	}
	if !found {
		t.Fatalf("mixed boundary lost logical position: %v", ev.records)
	}
}

func doPTransitionEngine(t *testing.T, mode string) {
	nativeEngineStorage(t)
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(2, 10, 49, 50, 52)
	k.Free()
	rate := 176400
	if mode == "cross_rate" {
		rate = 44100
	}
	pcm := make([]byte, 44+32*8)
	copy(pcm, "RIFF")
	binary.LittleEndian.PutUint32(pcm[4:], uint32(len(pcm)-8))
	copy(pcm[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(pcm[16:], 16)
	binary.LittleEndian.PutUint16(pcm[20:], 1)
	binary.LittleEndian.PutUint16(pcm[22:], 2)
	binary.LittleEndian.PutUint32(pcm[24:], uint32(rate))
	binary.LittleEndian.PutUint32(pcm[28:], uint32(rate*8))
	binary.LittleEndian.PutUint16(pcm[32:], 8)
	binary.LittleEndian.PutUint16(pcm[34:], 32)
	copy(pcm[36:], "data")
	binary.LittleEndian.PutUint32(pcm[40:], 256)
	for i := 44; i < len(pcm); i++ {
		pcm[i] = byte(i)
	}
	first := pcm
	if mode == "policy" {
		first = nativeDSF(t, 31)
	}
	b, e := New(Config{Resolver: nativeFiles{"a": first, "b": nativeDSF(t, 15)}, Locator: nativeLocator{}, OpenSink: k.OpenSink})
	if e != nil {
		t.Fatal(e)
	}
	AdmitNativeForIntegration(b, func(d format.NativeDSD) (engine.NativePlan, error) {
		p, e := dopTestPlan(d)
		p.PolicyID = fmt.Sprint(d.Frames)
		return p, e
	}, k.OpenNativeSink)
	dir := t.TempDir()
	eng, e := engine.New(engine.Config{Backend: b, PlayerUUID: "0f8fad5b-d9cb-469f-a165-70867728950e", DataDir: dir, QueuePath: filepath.Join(dir, "q.json")})
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = eng.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		c, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if e := b.Wait(c); e != nil {
			t.Error(e)
		}
		k.CheckClosedEach(t)
	})
	var prev uint32
	for _, name := range []string{"a", "b", "a"} {
		r, _ := engine.NewReference(name)
		prev, e = eng.InsertAfter(ctx, prev, r, nil)
		if e != nil {
			t.Fatal(e)
		}
	}
	if e = eng.ReportOutput(ctx, engine.OutputReport{Generation: 1, Available: true}); e != nil {
		t.Fatal(e)
	}
	if e = eng.Play(ctx); e != nil {
		t.Fatal(e)
	}
	for {
		ch := eng.Changed()
		if eng.Snapshot().State() == engine.Stopped {
			break
		}
		select {
		case <-ch:
		case <-time.After(5 * time.Second):
			t.Fatal("transition stalled")
		}
	}
	if skips := eng.Snapshot().Skips(); len(skips) != 0 {
		t.Fatal("transition refused", skips)
	}
	if k.View().Opens != 3 {
		t.Fatalf("different kind/policy reused sink: %+v", k.View())
	}
	if mode != "policy" {
		got := k.Given()
		at := k.View().OpenAt
		if len(at) != 3 || !bytes.Equal(got[:at[1]], pcm[44:]) || !bytes.Equal(got[at[2]:], pcm[44:]) {
			t.Fatal("PCM edge changed bytes or added PCM silence")
		}
	}
}
