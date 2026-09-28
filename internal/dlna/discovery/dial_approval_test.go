package discovery

// What a DialApproval lets a request to a device reach (backlog B36). The
// approval of an SSDP packet is pinned row by row in TestDefaultClientDialCheck;
// these are the operator's approval (OperatorChose) and the transport every
// request to a device goes through.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestOperatorChoseApprovesTheKindOfHostItsURLNames pins the operator's
// approval on the address a connect targets. A URL on this machine approves
// every address that reaches this machine, and a link-local URL every
// link-local address but the cloud metadata ones, as resolveServiceURL keeps
// service URLs of that kind from such a description. A NAME approves no local
// address whatever it resolves to, a numeric spelling, an unparseable URL or
// a URL on a metadata address approves nothing, and no approval at all still
// reaches other hosts, the metadata addresses among them excepted.
func TestOperatorChoseApprovesTheKindOfHostItsURLNames(t *testing.T) {
	onLocalhost := OperatorChose("http://localhost:8200/rootDesc.xml")
	onLoopback := OperatorChose("http://127.0.0.1:8200/rootDesc.xml")
	onTheLink := OperatorChose("http://[fe80::1%25en0]:8200/rootDesc.xml")
	byName := OperatorChose("http://nas.local:8200/rootDesc.xml")
	onTheLAN := OperatorChose("http://192.168.0.62:8200/rootDesc.xml")
	onTheMetadataAddress := OperatorChose("http://169.254.169.254/latest/meta-data/")
	for _, tc := range []struct {
		name     string
		approval DialApproval
		address  string
		allowed  bool
	}{
		{"localhost URL, 127.0.0.1", onLocalhost, "127.0.0.1:8200", true},
		{"localhost URL, another loopback address", onLocalhost, "127.0.1.1:8200", true},
		{"localhost URL, IPv6 loopback", onLocalhost, "[::1]:8200", true},
		{"localhost URL, the unspecified address", onLocalhost, "0.0.0.0:8200", true},
		{"localhost URL, a link-local address", onLocalhost, "169.254.169.254:80", false},
		{"localhost URL, a LAN address", onLocalhost, "192.168.1.42:8200", true},
		{"loopback URL, IPv6 loopback", onLoopback, "[::1]:8200", true},
		{"link-local URL, another link-local address", onTheLink, "169.254.7.7:8200", true},
		{"link-local URL, an IPv6 link-local address", onTheLink, "[fe80::2%en1]:8200", true},
		{"link-local URL, this machine", onTheLink, "127.0.0.1:7789", false},
		{"link-local URL, the cloud metadata address", onTheLink, "169.254.169.254:80", false},
		{"link-local URL, the IPv6 cloud metadata address", onTheLink, "[fe80::a9fe:a9fe%en0]:80", false},
		{"localhost URL, Alibaba's metadata address", onLocalhost, "100.100.100.200:80", false},
		{"a URL on the metadata address, itself", onTheMetadataAddress, "169.254.169.254:80", false},
		{"a URL on the metadata address, another link-local address", onTheMetadataAddress, "169.254.7.7:8200", false},
		{"a name, answered with this machine", byName, "127.0.0.1:7789", false},
		{"a name, answered with the host name's own 127.0.1.1", byName, "127.0.1.1:8200", false},
		{"a name, answered with a link-local address", byName, "169.254.169.254:80", false},
		{"a name, answered with a LAN address", byName, "192.168.1.42:8200", true},
		{"a LAN URL, this machine", onTheLAN, "127.0.0.1:7789", false},
		{"a numeric spelling approves nothing", OperatorChose("http://127.1:8200/rootDesc.xml"), "127.0.0.1:8200", false},
		{"an unparseable URL approves nothing", OperatorChose("http://[::1"), "[::1]:8200", false},
		{"no approval, this machine", DialApproval{}, "127.0.0.1:7789", false},
		{"no approval, a LAN address", DialApproval{}, "192.168.1.42:8200", true},
		{"no approval, AWS's IPv6 metadata address", DialApproval{}, "[fd00:ec2::254]:80", false},
		{"no approval, Azure's WireServer", DialApproval{}, "168.63.129.16:80", false},
		{"no packet, this machine", AnnouncedFrom(nil), "127.0.0.1:7789", false},
	} {
		err := refuseUnapprovedHostLocal(WithDialApproval(context.Background(), tc.approval), "tcp", tc.address, nil)
		if got := err == nil; got != tc.allowed {
			t.Errorf("%s: connect to %s allowed = %v (err %v), want %v", tc.name, tc.address, got, err, tc.allowed)
		}
	}
}

// TestNewDeviceTransportChecksEveryConnectWhateverTheDialerSays pins the
// constructor every client that sends a device a request is built on
// (NewDeviceFetchClient, the ingest's SOAP client, upnpproxy). The dial check
// replaces a ControlContext the template carries, so a caller cannot pass
// one that lets everything through, and the three settings the check needs
// hold.
func TestNewDeviceTransportChecksEveryConnectWhateverTheDialerSays(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	allowAll := func(context.Context, string, string, syscall.RawConn) error { return nil }
	tr := NewDeviceTransport(net.Dialer{Timeout: 3 * time.Second, ControlContext: allowAll})
	if tr.Proxy != nil || !tr.DisableKeepAlives || tr.DialTLSContext != nil {
		t.Errorf("transport = {Proxy set: %v, DisableKeepAlives: %v, DialTLSContext set: %v}, "+
			"want no proxy, no kept-alive connections and no TLS dialer of its own",
			tr.Proxy != nil, tr.DisableKeepAlives, tr.DialTLSContext != nil)
	}
	client := &http.Client{Transport: tr}
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		wantN int32
	}{
		{"no approval", context.Background(), 0},
		{"announced from a LAN address", WithAnnouncementSource(context.Background(), udpFrom("192.0.2.7")), 0},
		{"announced from 127.0.0.1", WithAnnouncementSource(context.Background(), udpFrom("127.0.0.1")), 1},
	} {
		req, err := http.NewRequestWithContext(tc.ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
		}
		if got := hits.Swap(0); got != tc.wantN {
			t.Errorf("%s: the listener on 127.0.0.1 saw %d requests, want %d", tc.name, got, tc.wantN)
		}
	}
}
