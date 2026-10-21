package engine

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestAStaleGenerationEnterIsRefused pins the check maintenance makes in
// the same step as it closes the engine.
func TestAStaleGenerationEnterIsRefused(t *testing.T) {
	h := startHarness(t)
	g := h.snap().Generation()
	h.insert("a")
	_, err := h.eng.EnterMaintenance(bg, g, "stale")
	h.refused("enter at an old generation", err, ReasonConflict)
	if _, ok := h.snap().Maintenance(); ok {
		t.Fatal("a refused enter holds maintenance")
	}
}

// TestOnlyTheLeaseEndsAHold pins the lease: a wrong or zero one is
// refused, and no field of any snapshot holds it, so no reader can exit or
// renew a hold.
func TestOnlyTheLeaseEndsAHold(t *testing.T) {
	h := startHarness(t)
	h.insert("a")
	hold, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "selecting a DAC")
	h.must("enter", err)
	if hold.Lease.IsZero() || !hold.ReleaseConfirmed {
		t.Fatalf("hold %+v", hold)
	}
	wrong := hold.Lease
	wrong.b[0] ^= 1
	h.refused("exit with a wrong lease", h.eng.ExitMaintenance(bg, wrong), ReasonLease)
	h.refused("exit with the zero lease", h.eng.ExitMaintenance(bg, Lease{}), ReasonLease)
	if _, err := h.eng.RenewMaintenance(bg, wrong); ReasonOf(err) != ReasonLease {
		t.Fatalf("renew with a wrong lease: %v", err)
	}
	sn := h.snap()
	info, ok := sn.Maintenance()
	if !ok || info.Reason != "selecting a DAC" || !info.Expires.Equal(hold.Expires) {
		t.Fatalf("the snapshot's hold %+v %v", info, ok)
	}
	if path := findLease(reflect.ValueOf(sn), hold.Lease, "snapshot"); path != "" {
		t.Fatalf("the snapshot holds the lease at %s", path)
	}
	back, err := ParseLease(hold.Lease.String())
	h.must("parse lease", err)
	h.must("exit with the lease", h.eng.ExitMaintenance(bg, back))
	h.refused("exit again with the spent lease", h.eng.ExitMaintenance(bg, back), ReasonLease)
}

// findLease walks v for the lease, as a Lease, its bytes or its string,
// and returns where it is.
func findLease(v reflect.Value, l Lease, path string) string {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return ""
		}
		return findLease(v.Elem(), l, path)
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(Lease{}) {
			return path
		}
		for i := 0; i < v.NumField(); i++ {
			if p := findLease(v.Field(i), l, path+"."+v.Type().Field(i).Name); p != "" {
				return p
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b := make([]byte, v.Len())
			for i := range b {
				b[i] = byte(v.Index(i).Uint())
			}
			if bytes.Contains(b, l.b[:]) || strings.Contains(string(b), l.String()) {
				return path
			}
			return ""
		}
		for i := 0; i < v.Len(); i++ {
			if p := findLease(v.Index(i), l, path); p != "" {
				return p
			}
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			if p := findLease(v.MapIndex(k), l, path); p != "" {
				return p
			}
		}
	case reflect.String:
		if strings.Contains(v.String(), l.String()) {
			return path
		}
	}
	return ""
}

// TestMaintenanceRefusesMutationsAndServesReads pins the closed engine:
// every mutation refused, typed, Stop a no-op, reads served, a second enter
// refused.
func TestMaintenanceRefusesMutationsAndServesReads(t *testing.T) {
	h := startHarness(t)
	ids, s := h.playing(0, "a", "b")
	hold, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "a test tone")
	h.must("enter", err)
	if !hold.ReleaseConfirmed || !s.isAcked() || h.snap().State() != Maintenance {
		t.Fatalf("enter while playing: %+v, released %v, %s", hold, s.isAcked(), h.snap().State())
	}
	_, err = h.eng.InsertAfter(bg, 0, ref("x"), nil)
	h.refused("insert", err, ReasonMaintenance)
	h.refused("delete", h.eng.Delete(bg, ids[0]), ReasonMaintenance)
	h.refused("delete all", h.eng.DeleteAll(bg), ReasonMaintenance)
	h.refused("repeat", h.eng.SetRepeat(bg, true), ReasonMaintenance)
	h.refused("play", h.eng.Play(bg), ReasonMaintenance)
	h.refused("next", h.eng.Next(bg), ReasonMaintenance)
	h.must("stop is a no-op", h.eng.Stop(bg))
	_, err = h.eng.EnterMaintenance(bg, h.snap().Generation(), "again")
	h.refused("a second enter", err, ReasonMaintenance)
	if tok, got := h.eng.IDArray(); tok == 0 || len(got) != 2 {
		t.Fatalf("IDArray during maintenance: %d %v", tok, got)
	}
	if es, err := h.eng.ReadList(ids); err != nil || len(es) != 2 {
		t.Fatalf("ReadList during maintenance: %v %v", es, err)
	}
	h.must("exit", h.eng.ExitMaintenance(bg, hold.Lease))
	if _, err := h.eng.InsertAfter(bg, 0, ref("x"), nil); err != nil {
		t.Fatalf("insert after the exit: %v", err)
	}
}

// TestMaintenanceDiscardsEarlierSessionsEvents pins the session check
// inside a hold.
func TestMaintenanceDiscardsEarlierSessionsEvents(t *testing.T) {
	h := startHarness(t)
	ids, s := h.playing(0, "a", "b")
	next := s.prepared(s.first.ID, 1)
	_, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "the test")
	h.must("enter", err)
	mark := h.appliedMark()
	s.submitted(next.ID)
	s.audible(next.ID)
	s.fault(Fault{Reason: ReasonXrun})
	h.waitApplied("handled fault", mark)
	sn := h.snap()
	if sn.Selected() != ids[0] || len(sn.Faults()) != 0 || sn.State() != Maintenance {
		t.Fatalf("an earlier session's events took effect in maintenance: %s at %d, faults %v", sn.State(), sn.Selected(), sn.Faults())
	}
}

// TestExitLeavesTheTransportStopped pins never resumed.
func TestExitLeavesTheTransportStopped(t *testing.T) {
	h := startHarness(t)
	h.playing(0, "a")
	h.must("pause", h.eng.Pause(bg))
	hold, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "the test")
	h.must("enter", err)
	h.must("exit", h.eng.ExitMaintenance(bg, hold.Lease))
	if sn := h.snap(); sn.State() != Stopped || h.be.starts() != 1 || h.be.count("resume") != 0 {
		t.Fatalf("after the exit %s with %d sessions", sn.State(), h.be.starts())
	}
}

// TestRenewalKeepsAHold pins a holder that keeps renewing: never cut off.
func TestRenewalKeepsAHold(t *testing.T) {
	h := startHarness(t)
	hold, err := h.eng.EnterMaintenance(bg, h.snap().Generation(), "a long apply")
	h.must("enter", err)
	for i := 0; i < 5; i++ {
		h.clk.Advance(DefaultMaintenanceTTL - time.Second)
		exp, err := h.eng.RenewMaintenance(bg, hold.Lease)
		h.must("renew", err)
		if want := h.clk.Now().Add(DefaultMaintenanceTTL); !exp.Equal(want) {
			t.Fatalf("renewed to %v, want %v", exp, want)
		}
	}
	if _, ok := h.snap().Maintenance(); !ok {
		t.Fatal("a renewed hold ended")
	}
}

// TestAnExpiredHoldEndsByItself pins the expiry, which also recovers an
// enter whose caller left before the reply.
func TestAnExpiredHoldEndsByItself(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a")
	g := holdReleases(h)
	ctx, cancel := context.WithCancel(bg)
	enter := async(func() error {
		_, err := h.eng.EnterMaintenance(ctx, h.snap().Generation(), "a caller that leaves")
		return err
	})
	h.waitSnap("maintenance", func(sn *Snapshot) bool { return sn.State() == Maintenance })
	cancel()
	h.refused("the enter whose caller left", h.result("enter", enter), ReasonCancelled)
	g.open()
	h.waitSnap("the release confirmed", func(sn *Snapshot) bool { m, ok := sn.Maintenance(); return ok && m.ReleaseConfirmed })
	if !s.isAcked() {
		t.Fatal("the hold calls a release confirmed that the backend did not acknowledge")
	}
	h.clk.Advance(DefaultMaintenanceTTL)
	sn := h.waitSnap("the hold's end", func(sn *Snapshot) bool { _, ok := sn.Maintenance(); return !ok })
	if sn.State() != Stopped || h.logs.count("maintenance hold expired") != 1 {
		t.Fatalf("after the expiry %s; logs %v", sn.State(), h.logs.all())
	}
	h.must("play after the expiry", h.eng.Play(bg))
}

// TestEnteringWhileAReleaseIsUnconfirmedSaysSo pins entered, release
// unconfirmed.
func TestEnteringWhileAReleaseIsUnconfirmedSaysSo(t *testing.T) {
	h := startHarness(t)
	_, s := h.playing(0, "a")
	never := newGate()
	h.be.script(func() {
		h.be.onRelease = func(ctx context.Context, fs *fakeSession) error {
			return never.wait(ctx, h.be, true)
		}
	})
	enter := make(chan Hold, 1)
	go func() {
		hold, _ := h.eng.EnterMaintenance(bg, h.snap().Generation(), "the test")
		enter <- hold
	}()
	h.be.waitFor("the release", func() bool { return s.releaseAsked })
	h.clk.Advance(ReleaseTimeout)
	var hold Hold
	select {
	case hold = <-enter:
	case <-time.After(*watchdog):
		t.Fatal("the enter did not answer")
	}
	if hold.Lease.IsZero() || hold.ReleaseConfirmed {
		t.Fatalf("hold %+v, want entered, release unconfirmed", hold)
	}
	if info, _ := h.snap().Maintenance(); info.ReleaseConfirmed {
		t.Fatal("the snapshot calls the release confirmed")
	}
}
