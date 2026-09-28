package upnp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dlna/discovery"
	"github.com/acoseac/1-bit-bridge/internal/dnstest"
)

// TestManualPollerNeverFetchesACloudMetadataDescription: a manual upstream's
// own description fetch was the one request to a device #1074's rule ("no
// approval reaches a cloud metadata address") did not cover (backlog B54).
// It is refused now, where the URL names one as a literal before any
// request, and where a name resolves to one at the connect, and the refusal
// is said once per server: a fetch that fails is a Debug line, which the
// bridge never prints, and a server refused every poll would otherwise
// just never appear. Its later dials were refused already, so such a
// server could never walk; what this adds is that the bridge no longer
// sends the metadata service a GET every poll, and says why. The controls
// keep the rest of #1069's decision: a direct-cable device on its own
// link-local address, and a name the operator chose that answers this
// machine, are still fetched.
func TestManualPollerNeverFetchesACloudMetadataDescription(t *testing.T) {
	const warning = "UPnP manual server: not fetching its description, which is on a cloud metadata address"

	t.Run("a literal, refused before any request", func(t *testing.T) {
		disp := &stubDispatcher{queue: []stubResp{{body: descXML("uuid:cable", "Cable", "/ctl")}}}
		cache := NewServerCache()
		var buf bytes.Buffer
		servers := []ManualServer{
			{Key: "manual:imds", DescriptionURL: "http://169.254.169.254:1/latest/meta-data/", Name: "IMDS"},
			{Key: "manual:imds6", DescriptionURL: "http://[fd00:ec2::254]:1/latest/meta-data/", Name: "IMDSv6"},
			{Key: "manual:alibaba", DescriptionURL: "http://100.100.100.200:1/latest/meta-data/", Name: "Alibaba"},
			{Key: "manual:cable", DescriptionURL: "http://169.254.7.7:8200/rootDesc.xml", Name: "Direct cable"},
		}
		p := NewManualPoller(ManualPollerConfig{
			Cache:      cache,
			Dispatcher: disp,
			Servers:    func() []ManualServer { return servers },
			Logger:     slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		})
		for i := 0; i < 3; i++ {
			p.PollOnce(context.Background())
		}
		for _, req := range disp.reqs {
			if req.URL.Hostname() != "169.254.7.7" {
				t.Errorf("the poller requested %s, on a cloud metadata address", req.URL)
			}
		}
		if len(disp.reqs) != 3 {
			t.Errorf("%d requests, want the direct-cable device's description once a poll (3)", len(disp.reqs))
		}
		for _, srv := range servers[:3] {
			if _, cached := cache.Get(srv.Key); cached {
				t.Errorf("%s was cached", srv.Name)
			}
			lines := linesContaining(buf.String(), warning)
			if n := countContaining(lines, "server="+srv.Name+" "); n != 1 {
				t.Errorf("%s: %d warnings over three polls, want exactly one:\n%s", srv.Name, n, buf.String())
			}
		}
		if _, cached := cache.Get("manual:cable"); !cached {
			t.Error("the direct-cable device on its own link-local address was not cached")
		}
		for _, l := range linesContaining(buf.String(), warning) {
			if strings.Contains(l, "/latest/meta-data/") {
				t.Errorf("the warning names the URL's path, where it should name the host alone: %s", l)
			}
		}
	})

	t.Run("a name, refused at the connect", func(t *testing.T) {
		const name = "upstream.rebind.test"
		dns := dnstest.Start(t, name, netip.MustParseAddr("169.254.169.254"))
		t.Cleanup(discovery.UseResolverForTest(dns.Resolver()))
		cache := NewServerCache()
		var buf bytes.Buffer
		p := manualTestPoller(t, cache, []ManualServer{{
			Key: "manual:named", DescriptionURL: "http://" + name + ":1/latest/meta-data/", Name: "Named",
		}}, nil, &buf)
		p.PollOnce(context.Background())
		p.PollOnce(context.Background())
		if dns.Queries() == 0 {
			t.Fatal("the name was never resolved through the test's DNS, so no connect was judged; " +
				"the manual fetch does not dial through discovery.NewDeviceTransport")
		}
		if _, cached := cache.Get("manual:named"); cached {
			t.Error("a description on a name answering 169.254.169.254 was cached")
		}
		lines := linesContaining(buf.String(), warning)
		if len(lines) != 1 || !strings.Contains(lines[0], "server=Named") || !strings.Contains(lines[0], "host="+name+":1") {
			t.Errorf("warnings %q over two polls, want exactly one naming the server and its host:\n%s", lines, buf.String())
		}

		// The control: the same name answering this machine, where a
		// server does serve a description. The operator's URL still
		// reaches this machine by a name, as #1069 left it; only its later
		// dials need the URL to say localhost (OperatorChose).
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, descXML("uuid:local", "Local", "/ctl"))
		}))
		t.Cleanup(srv.Close)
		u, err := url.Parse(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		_, port, err := net.SplitHostPort(u.Host)
		if err != nil {
			t.Fatal(err)
		}
		dns.Answer(netip.MustParseAddr("127.0.0.1"))
		var buf2 bytes.Buffer
		local := manualTestPoller(t, cache, []ManualServer{{
			Key: "manual:local", DescriptionURL: "http://" + name + ":" + port + "/rootDesc.xml", Name: "Local",
		}}, nil, &buf2)
		local.PollOnce(context.Background())
		if _, cached := cache.Get("manual:local"); !cached {
			t.Errorf("a manual URL whose name answers this machine was not fetched:\n%s", buf2.String())
		}
	})
}

// TestManualPollerWarningsNameTheHostAlone: both warnings the poller logs
// about a manual URL, the metadata refusal and the duplicate-configuration
// one, name the server and the URL's host, never the URL, which is the
// operator's and can carry a user name and password or a token in the
// query (backlog B54). The duplicate warning logged the URL whole until
// B54.
func TestManualPollerWarningsNameTheHostAlone(t *testing.T) {
	const secret = "s3cret-Pw"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, descXML("uuid:configured-twice", "Twice", "/ctl"))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewServerCache()
	var buf bytes.Buffer
	p := manualTestPoller(t, cache, []ManualServer{
		{Key: "manual:twice", Name: "Twice",
			DescriptionURL: "http://user:" + secret + "@" + u.Host + "/rootDesc.xml?token=" + secret},
		{Key: "manual:imds", Name: "IMDS",
			DescriptionURL: "http://" + secret + "@169.254.169.254/latest/?token=" + secret},
	}, map[string]struct{}{"uuid:configured-twice": {}}, &buf)
	p.PollOnce(context.Background())

	for _, want := range []struct{ msg, server, host string }{
		{"already configured by UDN", "Twice", u.Host},
		{"on a cloud metadata address", "IMDS", "169.254.169.254"},
	} {
		lines := linesContaining(buf.String(), want.msg)
		if len(lines) != 1 || !strings.Contains(lines[0], "server="+want.server) ||
			!strings.Contains(lines[0], "host="+want.host) {
			t.Errorf("warnings %q, want one naming server %s and host %s:\n%s", lines, want.server, want.host, buf.String())
		}
		for _, l := range lines {
			if strings.Contains(strings.ToLower(l), strings.ToLower(secret)) {
				t.Errorf("a warning carries the URL's secret: %s", l)
			}
		}
	}
}

// linesContaining returns the lines of out that contain s.
func linesContaining(out, s string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, s) {
			lines = append(lines, l)
		}
	}
	return lines
}

// countContaining counts the lines that contain s.
func countContaining(lines []string, s string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, s) {
			n++
		}
	}
	return n
}
