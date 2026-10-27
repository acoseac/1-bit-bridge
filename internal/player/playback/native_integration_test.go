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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
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

func TestNativeIntegrationTinyThroughEngine(t *testing.T) {
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
			if len(events.records) == 0 {
				t.Fatal("no measured events")
			}
			t.Log(events.records)
		})
	}
}

func TestNativeIntegrationReleaseCleanup(t *testing.T) {
	for _, where := range []string{"drop", "hw_free", "close"} {
		t.Run(where, func(t *testing.T) {
			k := alsapcm.NewFakeKernel(t)
			k.HardwareFormats(49, 50, 52)
			b, e := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
			if e != nil {
				t.Fatal(e)
			}
			AdmitNativeForIntegration(b, nativeTestPlan, func(ctx context.Context, c alsapcm.DSDConfig) (*alsapcm.DSDSink, error) {
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
func TestNativeIntegrationStalledReservations(t *testing.T) {
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
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
	AdmitNativeForIntegration(b, nativeTestPlan, k.OpenNativeSink)
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

func TestNativeIntegrationPauseKeepsPlan(t *testing.T) {
	k := alsapcm.NewFakeKernel(t)
	k.HardwareFormats(49, 50, 52)
	b, err := New(Config{Resolver: nativeFiles{"a": nativeDSF(t, 31)}, Locator: nativeLocator{}})
	if err != nil {
		t.Fatal(err)
	}
	admissions := 0
	AdmitNativeForIntegration(b, func(d format.NativeDSD) (engine.NativePlan, error) { admissions++; return nativeTestPlan(d) }, k.OpenNativeSink)
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
