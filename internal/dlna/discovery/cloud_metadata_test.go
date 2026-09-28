package discovery

// The cloud metadata addresses no device's say-so or approval may lead the
// bridge to (cloudMetadataAddrs; CodeRabbit on #1074). An SSDP source is not
// authenticated: a peer on the same link can send a packet FROM
// 169.254.169.254, and #1069's same-address exception approved exactly that
// address, for the description fetch and, since backlog B36, for every
// later dial of the URLs it led to. On a cloud VM that address serves the
// instance's credentials, and the upstream proxy relays what a byte fetch
// answers to the unauthenticated DLNA listener.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dnstest"
)

// TestCloudMetadataAddrsAreTheDocumentedOnes pins the list against each
// provider's documented addresses, in every spelling a URL or a connect can
// carry one, and the neighbours it must leave alone: a direct-cable device
// self-assigns anywhere in 169.254/16 and fe80::/10, and Tailscale serves
// MagicDNS on 100.100.100.100. Every address the list holds must be a row
// here, so one added without a documented source fails.
func TestCloudMetadataAddrsAreTheDocumentedOnes(t *testing.T) {
	documented := []struct{ addr, source string }{
		{"169.254.169.254", "AWS, Azure, Google Cloud, Oracle Cloud, OpenStack, DigitalOcean and others"},
		{"fd00:ec2::254", "AWS instance metadata, IPv6"},
		{"169.254.169.253", "AWS Route 53 Resolver"},
		{"fd00:ec2::253", "AWS Route 53 Resolver, IPv6"},
		{"169.254.169.123", "AWS Time Sync Service"},
		{"fd00:ec2::123", "AWS Time Sync Service, IPv6"},
		{"169.254.170.2", "AWS ECS task metadata and credentials"},
		{"169.254.170.23", "AWS EKS Pod Identity Agent"},
		{"fd00:ec2::23", "AWS EKS Pod Identity Agent, IPv6"},
		{"fd20:ce::254", "Google Cloud metadata, IPv6-only instances"},
		{"fd00:c1::a9fe:a9fe", "Oracle Cloud instance metadata, IPv6"},
		{"fe80::a9fe:a9fe", "OpenStack and Linode metadata, IPv6"},
		{"fd00:a9fe:a9fe::1", "Linode metadata, IPv6"},
		{"169.254.42.42", "Scaleway metadata"},
		{"fd00:42::42", "Scaleway metadata, IPv6"},
		{"169.254.0.23", "Tencent Cloud metadata"},
		{"169.254.10.10", "Tencent Cloud metadata"},
		{"100.100.100.200", "Alibaba Cloud metadata"},
		{"168.63.129.16", "Azure WireServer"},
	}
	for _, d := range documented {
		if !isCloudMetadataAddr(netip.MustParseAddr(d.addr)) {
			t.Errorf("%s (%s) is not in the list", d.addr, d.source)
		}
	}
	for a := range cloudMetadataAddrs {
		if !slices.ContainsFunc(documented, func(d struct{ addr, source string }) bool {
			return netip.MustParseAddr(d.addr) == a
		}) {
			t.Errorf("the list holds %s, which no documented row names", a)
		}
	}
	for _, tc := range []struct {
		addr     string
		metadata bool
	}{
		{"::ffff:169.254.169.254", true}, // mapped, as a dual-stack socket reports it
		{"fe80::a9fe:a9fe%en0", true},    // zoned, as a URL or a connect carries it
		{"169.254.169.255", false},
		{"169.254.170.3", false},
		{"169.254.7.7", false},
		{"169.254.10.20", false},
		{"fe80::1", false},
		{"fe80::a9fe:a9ff", false},
		{"fd00:ec2::255", false},
		{"100.100.100.100", false}, // Tailscale's MagicDNS
		{"100.100.100.201", false},
		{"168.63.129.17", false},
		{"192.168.1.42", false},
		{"127.0.0.1", false},
	} {
		if got := isCloudMetadataAddr(netip.MustParseAddr(tc.addr)); got != tc.metadata {
			t.Errorf("isCloudMetadataAddr(%s) = %v, want %v", tc.addr, got, tc.metadata)
		}
	}
}

// metadataAddrsInOrder is the list, sorted, so a test reports its rows in
// the same order every run.
func metadataAddrsInOrder() []netip.Addr {
	addrs := make([]netip.Addr, 0, len(cloudMetadataAddrs))
	for a := range cloudMetadataAddrs {
		addrs = append(addrs, a)
	}
	slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
	return addrs
}

// TestHandlePacket_NeverFetchesACloudMetadataLocation drives the real
// announcement path with a LOCATION on each metadata address, from a LAN
// address and from that very address, as a peer on the link can spoof it.
// Nothing is requested and nothing is cached. Before the rule the packet's
// own address was fetched (the same-address exception), and so was a
// metadata address that is not link-local from any source.
func TestHandlePacket_NeverFetchesACloudMetadataLocation(t *testing.T) {
	disp := &requestLog{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(chordDeviceXML))
	}}
	c := newTestClient(t, disp)
	for i, a := range metadataAddrsInOrder() {
		location := "http://" + net.JoinHostPort(a.String(), "80") + "/latest/meta-data/"
		for j, source := range []string{a.String(), "192.0.2.7"} {
			udn := fmt.Sprintf("uuid:metadata-%d-%d", i, j)
			c.handlePacket(context.Background(), rendererAnnouncement(udn, location), udpFrom(source))
			c.wg.Wait()
			if _, cached := c.cache.Get(udn); cached {
				t.Errorf("LOCATION %q from %s produced a cache entry", location, source)
			}
		}
	}
	if reqs := disp.requests(); len(reqs) != 0 {
		t.Errorf("requests = %q, want none: each LOCATION named a cloud metadata address", reqs)
	}
}

// TestDefaultClient_RefusesACloudMetadataAddressWhateverApprovedTheFetch is
// the half the string check cannot see, through the production client and a
// real resolution: a LOCATION naming a host by a name, which the string
// check keeps, whose name answers a metadata address at the connect. The dial
// check refuses it under every approval, the spoofed packet's own address
// included, before any packet leaves.
func TestDefaultClient_RefusesACloudMetadataAddressWhateverApprovedTheFetch(t *testing.T) {
	const name = "metadata.rebind.test"
	dns := dnstest.Start(t, name, netip.MustParseAddr("169.254.169.254"))
	t.Cleanup(UseResolverForTest(dns.Resolver()))
	c := newDefaultDispatcherClient(t)
	for _, tc := range []struct {
		name     string
		answer   string
		approval DialApproval
	}{
		{"a packet from 169.254.169.254", "169.254.169.254", AnnouncedFrom(udpFrom("169.254.169.254"))},
		{"an operator's link-local URL", "169.254.169.254", OperatorChose("http://169.254.7.7:8200/d.xml")},
		{"a packet from AWS's IPv6 metadata address", "fd00:ec2::254", AnnouncedFrom(udpFrom("fd00:ec2::254"))},
		{"no approval, Alibaba's address", "100.100.100.200", DialApproval{}},
	} {
		dns.Answer(netip.MustParseAddr(tc.answer))
		location := "http://" + name + "/latest/meta-data/"
		if LocationFromSource(location, udpFrom(tc.answer)) != location {
			t.Fatalf("%s: the string check refused %s, which it cannot place; the case would test nothing", tc.name, location)
		}
		_, err := FetchDeviceDescription(WithDialApproval(context.Background(), tc.approval), c.dispatcher, location)
		if !errors.Is(err, ErrCloudMetadataAddr) {
			t.Errorf("%s: the fetch of %s (answering %s) = %v, want the dial check's refusal",
				tc.name, location, tc.answer, err)
		}
	}
}

// TestParseDeviceDescription_NeverKeepsACloudMetadataServiceURL pins the
// service-URL half, for every source. A description the operator chose on a
// link-local address keeps link-local services (the host-kind rule), and a
// LAN one keeps services on other hosts (#1050's escape hatch): neither may
// name a metadata address. Nor may a description found AT one, though its
// services are on its own host.
func TestParseDeviceDescription_NeverKeepsACloudMetadataServiceURL(t *testing.T) {
	for _, tc := range []struct {
		base, control string
		source        DescriptionSource
	}{
		{"http://169.254.7.7:8200/d.xml", "http://169.254.169.254/latest/meta-data/", SourceUserChosen},
		{"http://[fe80::7%25en0]:8200/d.xml", "http://[fe80::a9fe:a9fe%25en0]/latest/", SourceUserChosen},
		{"http://192.168.1.42:8200/d.xml", "http://100.100.100.200/latest/meta-data/", SourceUserChosen},
		{"http://192.168.1.42:8200/d.xml", "http://[fd00:ec2::254]/latest/meta-data/", SourceUserChosen},
		{"http://192.168.1.42:8200/d.xml", "http://168.63.129.16/machine/", SourceUserChosen},
		{"http://169.254.169.254/d.xml", "/latest/meta-data/", SourceDiscovered},
		{"http://169.254.169.254/d.xml", "/latest/meta-data/", SourceUserChosen},
		{"http://[fd00:ec2::254]/d.xml", "/latest/meta-data/", SourceDiscovered},
	} {
		desc, _ := ParseDeviceDescriptionWithSource(avtOnlyDescription(tc.control), tc.base, tc.source)
		if svc, kept := desc.Services[ServiceAVTransport]; kept {
			t.Errorf("description %s (source %d), control %q: kept as %q", tc.base, tc.source, tc.control, svc.ControlURL)
		}
		base, err := url.Parse(tc.base)
		if err != nil {
			t.Fatalf("base %q: %v", tc.base, err)
		}
		if _, err := resolveServiceURL(base, tc.control, tc.source); !errors.Is(err, errServiceURLCloudMetadata) {
			t.Errorf("description %s, control %q: refusal = %v, want the cloud metadata one", tc.base, tc.control, err)
		}
	}
	// The direct-cable device beside them keeps its link-local service.
	desc, err := ParseDeviceDescriptionWithSource(avtOnlyDescription("http://169.254.7.8:8200/ctl"),
		"http://169.254.7.7:8200/d.xml", SourceUserChosen)
	if got := desc.Services[ServiceAVTransport].ControlURL; err != nil || got != "http://169.254.7.8:8200/ctl" {
		t.Errorf("a direct-cable device's link-local control URL = %q (err %v), want it kept", got, err)
	}
}
