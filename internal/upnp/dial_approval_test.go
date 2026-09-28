package upnp

// The approval a cached control URL was found under travels with it (backlog
// B36). The ingest's SOAP Browse and every byte fetch of a routed track dial
// the URL's host long after discovery, and a name in it resolves again at
// each dial, so what let discovery connect to this machine or a link-local
// address is recorded beside the URL and judged at every later connect
// (cmd/bridge's TestARebindingNameCannotTakeTheIngestOrAByteFetchToThisMachine
// drives the dials).

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
)

// TestSSDPDiscoveryRecordsTheAnnouncingAddressWithTheControlURL drives the
// real SSDP path. The approval cached with a control URL is that of the
// packet whose LOCATION led to it; a move replaces the two together; and an
// announcement that fetches nothing, or a move whose description names no
// ContentDirectory, changes neither.
func TestSSDPDiscoveryRecordsTheAnnouncingAddressWithTheControlURL(t *testing.T) {
	disp := &controlURLByHost{ctrl: map[string]string{
		"192.0.2.7:8200": "/ctl/ContentDir",
		"192.0.2.8:8200": "/ctl/ContentDir",
		"192.0.2.9:8200": "", // a description with no control URL
	}}
	cache := NewServerCache()
	c := newServerDiscoveryTestClient(t, disp, cache)
	for _, step := range []struct {
		what, location, from, wantControl, wantFrom string
	}{
		{"first seen", "http://192.0.2.7:8200/desc.xml", "192.0.2.7", "http://192.0.2.7:8200/ctl/ContentDir", "192.0.2.7"},
		{"moved", "http://192.0.2.8:8200/desc.xml", "192.0.2.8", "http://192.0.2.8:8200/ctl/ContentDir", "192.0.2.8"},
		{"announced again from elsewhere", "http://192.0.2.8:8200/desc.xml", "192.0.2.99", "http://192.0.2.8:8200/ctl/ContentDir", "192.0.2.8"},
		{"moved to a description with no control URL", "http://192.0.2.9:8200/desc.xml", "192.0.2.9", "http://192.0.2.8:8200/ctl/ContentDir", "192.0.2.8"},
	} {
		c.handlePacket(context.Background(), alivePacket("uuid:ms", step.location), udpFrom(step.from))
		c.wg.Wait()
		info, ok := cache.Get("uuid:ms")
		if !ok {
			t.Fatalf("%s: nothing cached", step.what)
		}
		want := discovery.AnnouncedFrom(udpFrom(step.wantFrom))
		if info.ContentDirectoryControlURL != step.wantControl || info.DialApproval != want {
			t.Errorf("%s: cached %q approved %+v, want %q approved as announced from %s (%+v)",
				step.what, info.ContentDirectoryControlURL, info.DialApproval, step.wantControl, step.wantFrom, want)
		}
	}
}

// TestServerCacheUpsertKeepsTheDialApprovalWithItsControlURL pins the merge.
// A refresh that carries no control URL keeps the cached URL's approval; a
// write that carries a control URL carries that URL's approval, a zero one
// included, so an old approval cannot outlive the URL it came with; and an
// approval that comes without a URL approves nothing new.
func TestServerCacheUpsertKeepsTheDialApprovalWithItsControlURL(t *testing.T) {
	c := NewServerCache()
	first := discovery.AnnouncedFrom(udpFrom("127.0.0.1"))
	c.Upsert(ServerInfo{UDN: "uuid:one", ContentDirectoryControlURL: "http://127.0.0.1:8200/ctl", DialApproval: first})
	for _, step := range []struct {
		what   string
		update ServerInfo
		want   discovery.DialApproval
	}{
		{"a refresh", ServerInfo{UDN: "uuid:one", LastSeenAt: time.Unix(200, 0)}, first},
		{"a new control URL with no approval", ServerInfo{UDN: "uuid:one", ContentDirectoryControlURL: "http://nas.example:8200/ctl"}, discovery.DialApproval{}},
		{"an approval with no control URL", ServerInfo{UDN: "uuid:one", DialApproval: first}, discovery.DialApproval{}},
	} {
		c.Upsert(step.update)
		if got, _ := c.Get("uuid:one"); got.DialApproval != step.want {
			t.Errorf("after %s: approval = %+v, want %+v", step.what, got.DialApproval, step.want)
		}
	}
}

// TestManualPollerRecordsTheOperatorsURLAsTheApproval: a manual server's
// control URL is cached with the approval of the operator's URL, which
// covers this machine when the URL names localhost and nothing local when it
// names a host by a name.
func TestManualPollerRecordsTheOperatorsURLAsTheApproval(t *testing.T) {
	for _, descURL := range []string{"http://localhost:8200/rootDesc.xml", "http://nas.local:8200/rootDesc.xml"} {
		cache := NewServerCache()
		var buf bytes.Buffer
		p := manualTestPoller(t, cache, []ManualServer{{Key: "manual:k", DescriptionURL: descURL, Name: "K"}}, nil, &buf)
		p.dispatcher = manualDescriptionAt{ctrl: "/ctl/ContentDir"}
		p.PollOnce(context.Background())
		info, ok := cache.Get("manual:k")
		if want := discovery.OperatorChose(descURL); !ok || info.DialApproval != want {
			t.Errorf("manual URL %s: cached %+v (present %v), want the approval %+v; log:\n%s",
				descURL, info, ok, want, buf.String())
		}
	}
	if discovery.OperatorChose("http://localhost:8200/") == (discovery.DialApproval{}) {
		t.Fatal("fixture: a localhost URL's approval is the zero one, so the check above cannot tell recorded from absent")
	}
}
