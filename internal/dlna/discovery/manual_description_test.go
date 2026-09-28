package discovery

// A manual upstream's own description fetch (internal/upnp's ManualPoller)
// was the one request to a device no rule here covered (backlog B54). #1069
// left it without a dial check, since the operator typed its host, path and
// port and what it returns is parsed, never relayed, so a URL on this
// machine is legitimate there; and #1074's cloud metadata rule ("no approval
// reaches a metadata address") reached every later dial of such a server
// but not this one. These pin the approval that fetch runs under and the
// string check its poller asks first.

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

// TestManualDescriptionFetchApprovesEveryAddressButAMetadataOne pins the
// approval of a manual upstream's description fetch on the address a
// connect targets: this machine, the link, the LAN and elsewhere, reached by
// a name or a literal alike, since the operator chose the URL; and never a
// cloud metadata address, which the refusal names as such (the poller warns
// on exactly that error).
func TestManualDescriptionFetchApprovesEveryAddressButAMetadataOne(t *testing.T) {
	ctx := WithDialApproval(context.Background(), ManualDescriptionFetch())
	for _, addr := range []string{
		"127.0.0.1:8200",
		"127.0.1.1:8200", // Debian's address for the host's own name
		"[::1]:8200",
		"0.0.0.0:8200",
		"169.254.7.7:8200", // a direct-cable device
		"[fe80::2%en1]:8200",
		"192.168.1.42:8200",
		"100.100.100.100:53", // Tailscale's MagicDNS, beside Alibaba's metadata address
		"203.0.113.9:80",
	} {
		if err := refuseUnapprovedHostLocal(ctx, "tcp", addr, nil); err != nil {
			t.Errorf("a connect to %s was refused: %v", addr, err)
		}
	}
	for _, a := range metadataAddrsInOrder() {
		addr := netip.AddrPortFrom(a, 80).String()
		if err := refuseUnapprovedHostLocal(ctx, "tcp", addr, nil); !errors.Is(err, ErrCloudMetadataAddr) {
			t.Errorf("a connect to %s = %v, want the cloud metadata refusal", addr, err)
		}
	}
}

// TestNamesCloudMetadataAddrReadsTheHostStringAlone pins the string check
// the poller makes before any fetch: true for a URL whose host is a cloud
// metadata address as an IP literal, in any spelling a URL can carry one
// (bracketed, zoned, mapped, a trailing dot, beside a user name and a
// query), and false for anything else, a NAME included: what a name
// resolves to is the dial check's question (ManualDescriptionFetch).
func TestNamesCloudMetadataAddrReadsTheHostStringAlone(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"http://169.254.169.254/latest/meta-data/", true},
		{"http://169.254.169.254:8200/rootDesc.xml", true},
		{"http://[fd00:ec2::254]/latest/", true},
		{"http://[::ffff:169.254.169.254]/", true},
		{"http://[fe80::a9fe:a9fe%25en0]:80/", true},
		{"http://168.63.129.16./machine/", true},
		{"https://user:pw@100.100.100.200:8443/x?y=z", true},
		{"http://169.254.7.7:8200/rootDesc.xml", false},
		{"http://100.100.100.100/", false},
		{"http://192.168.1.42:8200/rootDesc.xml", false},
		{"http://127.0.0.1:8200/rootDesc.xml", false},
		{"http://metadata.google.internal/computeMetadata/v1/", false},
		{"http://nas.local:8200/rootDesc.xml", false},
		{"http://[::1", false},
		{"", false},
	} {
		if got := NamesCloudMetadataAddr(tc.url); got != tc.want {
			t.Errorf("NamesCloudMetadataAddr(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}
