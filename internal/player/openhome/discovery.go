package openhome

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/player/lan"
)

// DiscoveryStopBound bounds SSDP retirement separately from HTTP shutdown.
const DiscoveryStopBound = 5 * time.Second

// sourceTargets derives the ordered announcement from the description's table.
func sourceTargets(id string) []dlna.NotifyTarget {
	udn := UDN(id)
	nts := []string{"upnp:rootdevice", udn, DeviceType}
	for _, service := range services {
		nts = append(nts, service.serviceType())
	}
	out := make([]dlna.NotifyTarget, 0, len(nts))
	for _, nt := range nts {
		usn := udn + "::" + nt
		if nt == udn {
			usn = udn
		}
		out = append(out, dlna.NotifyTarget{NT: nt, USN: usn})
	}
	return out
}

// sourceServerToken deliberately carries no DLNA profile claim.
func sourceServerToken(version string) string {
	if version == "" {
		version = "dev"
	}
	return "Darwin/UPnP/1.0 1-bit-bridge/" + version
}

type discoveryView struct {
	endpoints []dlna.SSDPEndpointSnapshot
	own       []netip.Addr
}

type discoveryChange struct {
	listening []Listening
	inventory lan.Inventory
	done      chan struct{}
}

// sourceDiscovery owns its handle on one goroutine. Surface transactions wait
// for its acknowledgement, without holding the surface or admission locks.
// Cancellation wins over an in-flight registration and its late result.
type sourceDiscovery struct {
	surface   *Surface
	subsystem *dlna.SSDPSubsystem
	ctx       context.Context
	cancel    context.CancelFunc
	changes   chan discoveryChange
	done      chan struct{}
	view      atomic.Pointer[discoveryView]
	mu        sync.Mutex
	deadline  time.Time
	// The handle's lifetime can end while the transaction worker is in
	// Register/Update. HTTP exit cancels it without waiting on that worker.
	registrationCancel  context.CancelFunc
	registrationBarrier *dlna.SSDPSendBarrier
}

func newSourceDiscovery(s *Surface, subsystem *dlna.SSDPSubsystem) *sourceDiscovery {
	ctx, cancel := context.WithCancel(context.Background())
	d := &sourceDiscovery{surface: s, subsystem: subsystem, ctx: ctx, cancel: cancel,
		changes: make(chan discoveryChange), done: make(chan struct{})}
	d.view.Store(&discoveryView{})
	go d.run()
	return d
}

func (d *sourceDiscovery) admits(search dlna.SSDPSearch) bool {
	v := d.view.Load()
	if d.ctx.Err() != nil || !slices.Contains(v.endpoints, search.Endpoint) {
		return false
	}
	src := search.Datagram.Source.Addr()
	return sourceRefusal(search.Endpoint.Local, src, src.IsValid(), slices.Contains(v.own, src)) == ""
}

func (d *sourceDiscovery) change(ctx context.Context, listening []Listening, inv lan.Inventory) {
	c := discoveryChange{listening: listening, inventory: inv, done: make(chan struct{})}
	select {
	case d.changes <- c:
	case <-ctx.Done():
		return
	case <-d.done:
		return
	}
	select {
	case <-c.done:
	case <-ctx.Done():
	case <-d.done:
	}
}

// invalidate cancels and joins normal sends after an unexpected HTTP exit.
// The whole registration pauses: the next transaction withdraws it and
// registers the surviving listeners under a fresh lifetime.
func (d *sourceDiscovery) invalidate() {
	d.mu.Lock()
	d.view.Store(&discoveryView{})
	if d.registrationCancel != nil {
		d.registrationCancel()
	}
	barrier := d.registrationBarrier
	d.mu.Unlock()
	// No surface/publication lock is held while a bounded write finishes.
	if barrier != nil {
		barrier.Invalidate()
	}
}

func (d *sourceDiscovery) refuse(ctx context.Context) {
	deadline := time.Now().Add(DiscoveryStopBound)
	if t, ok := ctx.Deadline(); ok && t.Before(deadline) {
		deadline = t
	}
	d.mu.Lock()
	if d.deadline.IsZero() || deadline.Before(d.deadline) {
		d.deadline = deadline
	}
	d.mu.Unlock()
	d.cancel()
}

func (d *sourceDiscovery) withdrawalContext() (context.Context, context.CancelFunc) {
	d.mu.Lock()
	deadline := d.deadline
	d.mu.Unlock()
	if deadline.IsZero() {
		deadline = time.Now().Add(DiscoveryStopBound)
	}
	return context.WithDeadline(context.Background(), deadline)
}

func (d *sourceDiscovery) run() {
	defer close(d.done)
	var handle *dlna.SSDPHandle
	var registrationCtx context.Context
	var registrationBarrier *dlna.SSDPSendBarrier
	defer func() {
		if handle != nil {
			ctx, cancel := d.withdrawalContext()
			defer cancel()
			if err := handle.Withdraw(ctx); err != nil {
				d.surface.log.Warn("player: OpenHome discovery withdrawal failed", "err", err)
			}
		}
	}()
	// A life changes only after an observed absence. Alias and prefix edits
	// keep it; disappearance/recreation between polls remains unobservable.
	lives := map[dlna.SSDPInterface]uint64{}
	present := map[dlna.SSDPInterface]bool{}
	var previous []dlna.SSDPEndpointSnapshot
	retry := false
	for {
		select {
		case <-d.ctx.Done():
			return
		case c := <-d.changes:
			if d.ctx.Err() != nil {
				close(c.done)
				return
			}
			if registrationCtx != nil && registrationCtx.Err() != nil && handle != nil {
				ctx, cancel := d.withdrawalContext()
				if err := handle.Withdraw(ctx); err != nil {
					d.surface.log.Warn("player: OpenHome discovery withdrawal failed", "err", err)
				}
				cancel()
				handle = nil
			}
			d.mu.Lock()
			if handle == nil {
				if d.registrationCancel != nil {
					d.registrationCancel()
				}
				registrationCtx, d.registrationCancel = context.WithCancel(d.ctx)
				registrationBarrier = new(dlna.SSDPSendBarrier)
				d.registrationBarrier = registrationBarrier
			}
			// Recheck liveness under the invalidation lock: a change queued
			// before Serve exited must never republish the dead listener.
			live := d.surface.Status().Listening
			c.listening = slices.DeleteFunc(c.listening, func(l Listening) bool { return !slices.Contains(live, l) })

			now := map[dlna.SSDPInterface]bool{}
			for _, a := range c.inventory.Served {
				now[dlna.SSDPInterface{Index: a.Index, Name: a.Interface}] = true
			}
			for key := range present {
				if !now[key] {
					lives[key]++
				}
			}
			present = now
			var endpoints []dlna.SSDPEndpoint
			v := &discoveryView{own: slices.Clone(c.inventory.Own)}
			for i, a := range v.own {
				v.own[i] = a.Unmap().WithZone("")
			}
			for _, l := range c.listening {
				key := dlna.SSDPInterface{Index: l.Address.Index, Name: l.Address.Interface}
				ep := dlna.SSDPEndpoint{Interface: &net.Interface{Index: key.Index, Name: key.Name}, Incarnation: lives[key], Local: l.Address.Prefix,
					Location: sourceLocation(l.Bound)}
				key.Incarnation = ep.Incarnation
				endpoints = append(endpoints, ep)
				v.endpoints = append(v.endpoints, dlna.SSDPEndpointSnapshot{Interface: key, Local: ep.Local, Location: ep.Location})
			}
			d.view.Store(v)
			d.mu.Unlock()
			ctx, cancel := context.WithTimeout(registrationCtx, DiscoveryStopBound)
			var result dlna.SSDPResult
			var err error
			switch {
			case len(endpoints) == 0:
				if handle != nil {
					err = handle.Withdraw(ctx)
					handle = nil
				}
				retry = false
			case handle == nil:
				// Register retains its context for the handle lifetime. The
				// operation timeout below belongs only to Update/Withdraw.
				interval := time.Duration(0)
				if seams := d.surface.cfg.Seams; seams != nil {
					interval = seams.AdvertiseInterval
				}
				handle, result, err = d.subsystem.Register(registrationCtx, dlna.SSDPRegistration{Name: "OpenHome", UDN: UDN(d.surface.cfg.Device.UUID),
					Targets: sourceTargets(d.surface.cfg.Device.UUID), ServerToken: sourceServerToken(d.surface.cfg.Device.ModelNumber),
					Endpoints: endpoints, Admission: dlna.SSDPStrictAdmission{Source: d.admits}, SendBarrier: registrationBarrier,
					Logger: d.surface.log, AdvertiseInterval: interval})
				retry = err != nil || len(result.Failed) > 0 || len(result.Active) != len(endpoints)
			case retry || !slices.Equal(previous, v.endpoints):
				result, err = handle.Update(ctx, endpoints)
				retry = err != nil || len(result.Failed) > 0 || len(result.Active) != len(endpoints)
			}
			cancel()
			previous = v.endpoints
			if err != nil || len(result.Failed) > 0 {
				d.surface.log.Warn("player: OpenHome discovery is degraded; it retries at the next listing", "active", len(result.Active), "failed", len(result.Failed), "err", err)
			}
			close(c.done)
		}
	}
}

// syncDiscovery is a transaction phase: only bound and serving endpoints,
// including survivors, reach SSDP. Failed listings reuse the last good view.
func (s *Surface) syncDiscovery(listening []Listening, inv lan.Inventory) {
	if s.discovery != nil {
		s.discovery.change(s.ctx, listening, inv)
	}
}

// sourceRefusal is shared by TCP admission and SSDP's immutable source view.
// It performs no I/O, logging, connection counting or slot acquisition.
func sourceRefusal(prefix netip.Prefix, src netip.Addr, valid, own bool) string {
	switch {
	case !valid || !src.Is4():
		return refusedNotIPv4
	case src.IsLoopback():
		return refusedLoopback
	case own:
		return refusedOwnAddress
	case !prefix.Masked().Contains(src):
		return refusedOutsidePrefix
	case !lan.IsLANAddress(src):
		return refusedNotLAN
	}
	return ""
}
