package dlna

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"
)

// testAdvertiserConfig is what the tests gave NewSSDPAdvertiser until B470 PR
// 5a1 moved SSDP onto shared registrations: one MediaServer-style device
// (ssdpServedAdmission) with one endpoint.
type testAdvertiserConfig struct {
	UDN, Location, ServerToken string
	Interface                  *net.Interface
	// AdvertiseInterval is honoured as given, a test's millisecond
	// included (the subsystem's floor is lowered for it).
	AdvertiseInterval time.Duration
	Logger            *slog.Logger
	// served is the admission's served addresses; nil is this host's.
	served *servedAddrs
}

// testAdvertiser is one such device on a subsystem of its own, over the
// host's sockets: what an SSDPAdvertiser was.
type testAdvertiser struct {
	cfg testAdvertiserConfig
	sub *SSDPSubsystem
	h   *SSDPHandle
}

func newTestAdvertiser(cfg testAdvertiserConfig) *testAdvertiser {
	return &testAdvertiser{cfg: cfg}
}

// registration is the device's registration.
func (c testAdvertiserConfig) registration() SSDPRegistration {
	served := c.served
	if served == nil {
		served = newServedAddrs(nil)
	}
	return SSDPRegistration{
		Name:              "DLNA",
		UDN:               c.UDN,
		Targets:           NotifyTargetsFor(c.UDN),
		ServerToken:       c.ServerToken,
		Endpoints:         []SSDPEndpoint{{Interface: c.Interface, Location: c.Location}},
		Admission:         ssdpServedAdmission{served: served},
		ResponderSlots:    maxConcurrentMSearchResponders,
		AdvertiseInterval: c.AdvertiseInterval,
		Logger:            c.Logger,
	}
}

// Start registers the device on a subsystem of its own, as an advertiser's
// Start bound its sockets and sent its first burst. Its error is the one
// endpoint's.
func (a *testAdvertiser) Start(ctx context.Context) error {
	a.sub = NewSSDPSubsystem(ctx, SSDPSubsystemOptions{Logger: a.cfg.Logger})
	if a.cfg.AdvertiseInterval > 0 {
		a.sub.intervalFloor = 0
	}
	h, res, err := a.sub.Register(ctx, a.cfg.registration())
	if err != nil {
		_ = a.sub.Close(context.Background())
		if len(res.Failed) > 0 {
			return res.Failed[0].Err
		}
		return err
	}
	a.h = h
	return nil
}

// Stop withdraws the device and closes its subsystem, as an advertiser's
// Stop sent its byebye and closed its sockets. It is safe before Start and
// twice.
func (a *testAdvertiser) Stop() {
	if a.h != nil {
		_ = a.h.Withdraw(context.Background())
	}
	if a.sub != nil {
		_ = a.sub.Close(context.Background())
	}
}

// endpoint is the device's one endpoint.
func (a *testAdvertiser) endpoint() *ssdpEndpoint {
	a.h.opMu.Lock()
	defer a.h.opMu.Unlock()
	return a.h.endpoints[0]
}

// loopbackDiscard is a UDP socket the test holds open on loopback, and its
// address, for a subsystem's NOTIFYs to go to in place of the SSDP group: a
// test whose endpoint is not pinned to the loopback interface must not
// multicast onto the LAN (B38).
func loopbackDiscard(t *testing.T) *net.UDPAddr {
	t.Helper()
	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("cannot bind a loopback UDP socket in this environment: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	return sink.LocalAddr().(*net.UDPAddr)
}

// socketTransport is the host's transport but for its listeners: listen
// hands the subsystem the socket the test opened for that interface (by
// name, "" for the OS default), armed by the host transport's own arm (whose
// seams can refuse a report), so the subsystem's real read loop runs on a
// socket of the test's choosing, a plain loopback one with no multicast at
// all. Its senders are the host's.
type socketTransport struct {
	hostSSDPTransport
	mu    sync.Mutex
	socks map[string]*net.UDPConn
}

func (tr *socketTransport) listen(_ context.Context, iface *net.Interface, log *slog.Logger) (ssdpReadConn, ssdpListenerSetup, error) {
	name := ""
	if iface != nil {
		name = iface.Name
	}
	tr.mu.Lock()
	c := tr.socks[name]
	delete(tr.socks, name)
	tr.mu.Unlock()
	if c == nil {
		return nil, ssdpListenerSetup{}, errors.New("the test opened no socket for this interface")
	}
	setup := tr.hostSSDPTransport.arm(c, iface, log)
	setup.caps.ReceiveConfined = true
	return hostReadConn{c}, setup, nil
}

// registerOnSocket registers reg on a subsystem of its own whose listener
// for reg's one endpoint is the socket l (socketTransport), with every NOTIFY
// going to a loopback socket the test holds (loopbackDiscard). It fails the
// test unless l could be armed for destinations, as Start arms a listener,
// and withdraws and closes everything at the test's end.
func registerOnSocket(t *testing.T, l *net.UDPConn, reg SSDPRegistration) (*SSDPSubsystem, *SSDPHandle) {
	t.Helper()
	sub, h, res := registerOnTransport(t, &socketTransport{}, l, reg)
	if !res.Active[0].Listener.DestinationReporting {
		t.Fatal("the listener could not be armed")
	}
	return sub, h
}

// registerOnTransport is registerOnSocket over a socketTransport the test
// set up (one that refuses a report, say), returning the result.
func registerOnTransport(t *testing.T, tr *socketTransport, l *net.UDPConn, reg SSDPRegistration) (*SSDPSubsystem, *SSDPHandle, SSDPResult) {
	t.Helper()
	name := ""
	if iface := reg.Endpoints[0].Interface; iface != nil {
		name = iface.Name
	}
	tr.socks = map[string]*net.UDPConn{name: l}
	sub := NewSSDPSubsystem(context.Background(), SSDPSubsystemOptions{Logger: reg.Logger})
	sub.transport = tr
	sub.notifyTo = loopbackDiscard(t)
	h, res, err := sub.Register(context.Background(), reg)
	if err != nil {
		_ = l.Close()
		t.Fatalf("Register on the test's socket: %v (%+v)", err, res)
	}
	t.Cleanup(func() {
		_ = h.Withdraw(context.Background())
		_ = sub.Close(context.Background())
	})
	return sub, h, res
}
