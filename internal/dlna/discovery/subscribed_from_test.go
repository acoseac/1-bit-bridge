package discovery

import (
	"context"
	"net/netip"
	"testing"
)

// TestSubscribedFromApprovesTheSubscribersOwnAddress pins the approval
// internal/dlna's GENA NOTIFY dials under (backlog B39), on the address a
// connect targets: this machine or a link-local address only at the
// SUBSCRIBE's own address, compared unmapped and without its zone; never
// the unspecified address or a cloud metadata address, not even the
// subscriber's own; and any other address whatever the source, as every
// approval allows.
func TestSubscribedFromApprovesTheSubscribersOwnAddress(t *testing.T) {
	for _, tc := range []struct {
		source  string // "" is the zero Addr: no subscriber
		address string
		allowed bool
	}{
		{"127.0.0.1", "127.0.0.1:7789", true},
		{"::ffff:127.0.0.1", "127.0.0.1:7789", true}, // a mapped source is its IPv4 address
		{"127.0.0.1", "[::ffff:127.0.0.1]:7789", true},
		{"fe80::1%en0", "[fe80::1]:8080", true}, // a zoned source is its address
		{"169.254.10.20", "169.254.10.20:8080", true},
		{"127.0.0.1", "127.0.0.2:7789", false},
		{"127.0.0.1", "[::1]:7789", false},
		{"192.168.1.9", "127.0.0.1:7789", false},
		{"169.254.10.20", "169.254.10.21:8080", false},
		{"0.0.0.0", "0.0.0.0:7789", false},
		{"169.254.169.254", "169.254.169.254:80", false}, // a metadata address, even the subscriber's own
		{"192.168.1.9", "[fd00:ec2::254]:80", false},
		{"", "127.0.0.1:7789", false},
		{"", "192.168.1.42:8080", true},
		{"192.168.1.9", "203.0.113.9:80", true},
	} {
		var from netip.Addr
		if tc.source != "" {
			from = netip.MustParseAddr(tc.source)
		}
		ctx := WithDialApproval(context.Background(), SubscribedFrom(from))
		err := refuseUnapprovedHostLocal(ctx, "tcp", tc.address, nil)
		if got := err == nil; got != tc.allowed {
			t.Errorf("subscribed from %q, connect to %s: allowed = %v (err %v), want %v",
				tc.source, tc.address, got, err, tc.allowed)
		}
	}
}
