package dlna

import (
	"context"
	"log/slog"
	"net/netip"
	"time"
)

// ssdpTable is what the readers dispatch by: every registration with its
// active endpoints, in registration order, rebuilt whole on every change.
type ssdpTable struct {
	entries []ssdpTableEntry
	// owners lists, per interface index, the registrations with an active
	// endpoint on an interface of that index.
	owners map[int][]*SSDPHandle
}

// ssdpTableEntry is one registration in a table.
type ssdpTableEntry struct {
	h   *SSDPHandle
	eps []*ssdpEndpoint
}

// publishLocked rebuilds the dispatch table from the registrations, and
// starts the reader of a listener it lists for the first time. The
// subsystem's mu is held.
func (s *SSDPSubsystem) publishLocked() {
	t := buildTable(s.handles)
	s.table.Store(t)
	if !s.closed {
		s.startReadersLocked(t)
	}
}

// buildTable is the dispatch table of handles, each with its published
// endpoints (SSDPHandle.active).
func buildTable(handles []*SSDPHandle) *ssdpTable {
	t := &ssdpTable{owners: map[int][]*SSDPHandle{}}
	for _, h := range handles {
		t.entries = append(t.entries, ssdpTableEntry{h: h, eps: h.active})
		for _, ep := range h.active {
			if idx := ep.key.index; idx > 0 && !containsHandle(t.owners[idx], h) {
				t.owners[idx] = append(t.owners[idx], h)
			}
		}
	}
	return t
}

func containsHandle(hs []*SSDPHandle, h *SSDPHandle) bool {
	for _, x := range hs {
		if x == h {
			return true
		}
	}
	return false
}

// ssdpSearchKind is what a datagram's reported destination makes of it.
type ssdpSearchKind int

const (
	// searchToGroup was sent to the SSDP group.
	searchToGroup ssdpSearchKind = iota
	// searchUnicast was sent to an address of this host.
	searchUnicast
	// searchUnarmed was read by a listener that could not be asked for
	// destinations: group or unicast, nobody can tell.
	searchUnarmed
	// searchUnreported was read by a listener that asked, and the kernel
	// reported no destination beside it.
	searchUnreported
)

// kindOf classes a datagram read by a listener with caps.
func kindOf(caps SSDPListenerCaps, d SSDPDatagram) ssdpSearchKind {
	switch {
	case !caps.DestinationReporting:
		return searchUnarmed
	case !d.DestinationValid:
		return searchUnreported
	case d.Destination == ssdpGroupAddr:
		return searchToGroup
	}
	return searchUnicast
}

// dispatch answers one datagram l read: it is parsed once, then each
// registration matches and admits it on its own, picks the one endpoint it
// would answer from, and only then takes one of that endpoint's responder
// slots, so a datagram that matches nothing, or a registration's full pool,
// costs no other registration anything. It runs on l's read loop and never
// blocks: admission is a lookup, a strict source hook is pure, a slot is
// taken or the answer dropped, and the answer itself runs on a goroutine of
// its own.
func (s *SSDPSubsystem) dispatch(l *ssdpListener, pkt []byte, src netip.AddrPort, m datagramMeta) {
	t := s.table.Load()
	if t == nil {
		return
	}
	mx, plan := t.plan(l, pkt, src, m)
	for _, p := range plan {
		p.h.answerFrom(p.ep, p.targets, mx, src)
	}
}

// ssdpPlanned is one answer a dispatch decided on: the registration, the one
// endpoint it answers from, and the targets the search matched.
type ssdpPlanned struct {
	h       *SSDPHandle
	ep      *ssdpEndpoint
	targets []NotifyTarget
}

// plan decides, without sending or spawning anything, who answers one
// datagram l read: at most one entry per registration, in registration
// order, and the search's MX. A refusal the MediaServer's admission logs is
// logged here.
func (t *ssdpTable) plan(l *ssdpListener, pkt []byte, src netip.AddrPort, m datagramMeta) (int, []ssdpPlanned) {
	st := ParseMSearchST(pkt)
	if st == "" {
		return 0, nil // not an M-SEARCH or missing ST — silently drop
	}
	mx := ParseMSearchMX(pkt)
	d := SSDPDatagram{
		Source:           src,
		Destination:      m.dst,
		DestinationValid: m.dstOK,
		ArrivalIndex:     m.ifIndex,
		ArrivalValid:     m.ifOK,
		Listener:         l.key.public(),
		ListenerCaps:     l.setup.caps,
	}
	kind := kindOf(l.setup.caps, d)
	var plan []ssdpPlanned
	for _, e := range t.entries {
		if len(e.eps) == 0 {
			continue // parked by an Update that left it nothing
		}
		var ep *ssdpEndpoint
		var targets []NotifyTarget
		switch a := e.h.admission.(type) {
		case ssdpServedAdmission:
			// The MediaServer admits first, as its read loop always did,
			// so its refusal log sees what it saw.
			if ep = e.legacySelect(t, l, kind, d, a); ep == nil {
				continue
			}
			if targets = MSearchTargets(st, e.h.targets); len(targets) == 0 {
				continue // ST didn't match anything it advertises
			}
		case SSDPStrictAdmission:
			if targets = MSearchTargets(st, e.h.targets); len(targets) == 0 {
				continue
			}
			if ep = e.strictSelect(l, kind, d); ep == nil {
				continue
			}
			if !a.Source(SSDPSearch{Endpoint: ep.snapshot(), Datagram: d}) {
				continue
			}
		default:
			continue
		}
		plan = append(plan, ssdpPlanned{h: e.h, ep: ep, targets: targets})
	}
	return mx, plan
}

// firstOn is e's first active endpoint on l, in the caller's order, or nil.
func (e ssdpTableEntry) firstOn(l *ssdpListener) *ssdpEndpoint {
	for _, ep := range e.eps {
		if ep.listener == l {
			return ep
		}
	}
	return nil
}

// legacySelect is the MediaServer's admission and choice of endpoint
// (ssdpServedAdmission), unchanged by B470 PR 5a1 but for one refusal no
// MediaServer alone can meet. A group search is answered from its endpoint
// on the listener that read it, and from no other: a listener that could
// not be confined (Linux's IP_MULTICAST_ALL refused) also hears the group
// datagrams of other interfaces where something on the host joined, and
// answers them with its own LOCATION, as it always has; but not one whose
// arrival index names an interface where only another registration of this
// subsystem has an endpoint, a membership that exists only because of that
// registration. A unicast is admitted by its destination (servedAddrs) and
// answered from its endpoint on the listener, or from its first endpoint if
// the listener is another registration's, never chosen by the destination
// (backlog B119). An unarmed listener's datagram is answered from the
// endpoint on it unchecked, as before B48, and an armed one's with no
// destination reported is refused.
func (e ssdpTableEntry) legacySelect(t *ssdpTable, l *ssdpListener, kind ssdpSearchKind, d SSDPDatagram, a ssdpServedAdmission) *ssdpEndpoint {
	onL := e.firstOn(l)
	switch kind {
	case searchToGroup:
		if onL == nil {
			return nil
		}
		if d.ArrivalValid && l.key.index > 0 && d.ArrivalIndex != l.key.index &&
			!containsHandle(t.owners[d.ArrivalIndex], e.h) && len(t.owners[d.ArrivalIndex]) > 0 {
			return nil
		}
		return onL
	case searchUnicast:
		if !a.admits(e.h.log, kind, d) {
			return nil
		}
		if onL != nil {
			return onL
		}
		return e.eps[0]
	case searchUnarmed:
		return onL
	default: // searchUnreported: refused, and logged by the listener's own MediaServer
		if onL != nil {
			_ = a.admits(e.h.log, kind, d)
		}
		return nil
	}
}

// admits is the MediaServer's admission of a datagram of kind, and logs a
// refusal (servedAddrs.admit). One sent to the SSDP group is admitted: a
// group datagram reaches a listener only on the interface it joined (B71).
// Anything else is a unicast to an address of this host, which the
// listener's wildcard bind takes on every address, and is admitted only when
// the server serves that address (backlog B48). One whose destination was
// asked for and not reported is refused: the listener cannot tell it from a
// unicast to the tailnet. One read by a listener that could not ask is
// admitted, as every search was before B48. It was the advertiser's
// searchedFor until B470 PR 5a1, unchanged.
func (a ssdpServedAdmission) admits(log *slog.Logger, kind ssdpSearchKind, d SSDPDatagram) bool {
	from := ""
	if d.Source.IsValid() {
		from = d.Source.Addr().String()
	}
	switch kind {
	case searchToGroup, searchUnarmed:
		return true
	case searchUnicast:
		return a.served.admit(log, "an M-SEARCH", d.Destination, from)
	}
	return a.served.admit(log, "an M-SEARCH", netip.Addr{}, from)
}

// strictSelect is a strict registration's choice of endpoint
// (SSDPStrictAdmission): a group search arrived, by its reported index, on
// the interface of the listener that read it, which must report both and be
// confined, and is answered from the first endpoint on that listener; a
// unicast, read by a listener that reports both (confinement is a matter of
// group datagrams), is answered from the endpoint whose Local address it was
// sent to on the interface it arrived on; nothing else is answered.
func (e ssdpTableEntry) strictSelect(l *ssdpListener, kind ssdpSearchKind, d SSDPDatagram) *ssdpEndpoint {
	caps := l.setup.caps
	switch kind {
	case searchToGroup:
		if !caps.DestinationReporting || !caps.ArrivalReporting || !caps.ReceiveConfined {
			return nil
		}
		if !d.ArrivalValid || d.ArrivalIndex != l.key.index {
			return nil
		}
		return e.firstOn(l)
	case searchUnicast:
		if !caps.DestinationReporting || !caps.ArrivalReporting || !d.ArrivalValid {
			return nil
		}
		for _, ep := range e.eps {
			if ep.spec.Local.IsValid() && ep.spec.Local.Addr().Unmap() == d.Destination && ep.key.index == d.ArrivalIndex {
				return ep
			}
		}
	}
	return nil
}

// answerFrom answers one search from ep: it takes one of ep's responder
// slots, or drops the answer when they are all taken, and joins ep's work
// unless ep has started to go, and runs the answer on a goroutine of its
// own. The slot is held across the MX delay and every write.
func (h *SSDPHandle) answerFrom(ep *ssdpEndpoint, targets []NotifyTarget, mx int, to netip.AddrPort) {
	select {
	case ep.pool <- struct{}{}:
	default:
		// Responder pool full — drop. The control point re-searches;
		// SSDP discovery is best-effort by design.
		h.log.Debug("M-SEARCH responder pool full; dropping",
			slog.String("src", to.String()))
		return
	}
	if hook := h.s.slotTaken; hook != nil {
		hook(ep)
	}
	h.gate.Lock()
	if ep.closing {
		h.gate.Unlock()
		<-ep.pool
		return
	}
	ep.work.Add(1)
	h.gate.Unlock()
	go func() {
		defer ep.work.Done()
		defer func() { <-ep.pool }()
		h.answer(ep, targets, mx, to)
	}()
}

// answer waits the spec-mandated random [0, MX] delay (a missing or zero MX
// waits up to defaultMSearchJitter, so simultaneous searches still don't
// reply in a synchronized burst), then sends one unicast response per
// matched target to the searcher over a SINGLE transient socket. Its
// endpoint's cancellation ends the delay, closes the socket (one whose dial
// completes after it, too), and stops the burst before its next write; the
// burst's writes are bounded together, from the first one
// (ssdpBurstDeadline).
func (h *SSDPHandle) answer(ep *ssdpEndpoint, targets []NotifyTarget, mx int, to netip.AddrPort) {
	window := defaultMSearchJitter
	if mx > 0 {
		window = time.Duration(mx) * time.Second
	}
	timer := time.NewTimer(h.s.delay(window))
	defer timer.Stop()
	select {
	case <-ep.ctx.Done():
		return
	case <-timer.C:
	}

	// Dial the transient unicast socket ONCE (not per target). An
	// `ssdp:all` search matches every target; re-dialing the same source
	// per match is pure socket churn. One datagram per target/USN is still
	// correct UPnP — only the socket is shared.
	conn, err := h.s.transport.dialReply(to)
	if err != nil {
		h.log.Debug("M-SEARCH response dial failed",
			slog.String("src", to.String()),
			slog.String("err", err.Error()))
		return
	}
	stop := context.AfterFunc(ep.ctx, func() { _ = conn.Close() })
	defer func() {
		stop()
		_ = conn.Close()
	}()

	_ = conn.SetWriteDeadline(time.Now().Add(ssdpBurstDeadline))
	now := time.Now()
	for _, target := range targets {
		if ep.ctx.Err() != nil {
			return
		}
		response := BuildMSearchResponse(ep.spec.Location, h.serverToken, target.NT, target.USN, now)
		if _, err := conn.Write(response); err != nil {
			h.log.Debug("M-SEARCH response write failed",
				slog.String("src", to.String()),
				slog.String("nt", target.NT),
				slog.String("err", err.Error()))
		}
	}
}
