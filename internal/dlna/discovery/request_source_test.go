package discovery

import (
	"context"
	"net/netip"
	"testing"
)

// TestWithRequestSourceComparesUnmappedAndWithoutAZone pins the form the
// dial check compares a request's source in when it arrives through
// WithRequestSource, the entry internal/dlna's GENA NOTIFY uses: a mapped
// source is its IPv4 address, a zoned one is its address, and the zero Addr
// matches nothing, as WithAnnouncementSource's sources do.
func TestWithRequestSourceComparesUnmappedAndWithoutAZone(t *testing.T) {
	for _, tc := range []struct {
		source  netip.Addr
		address string
		allowed bool
	}{
		{netip.MustParseAddr("::ffff:127.0.0.1"), "127.0.0.1:7789", true},
		{netip.MustParseAddr("fe80::1%en0"), "[fe80::1]:8080", true},
		{netip.MustParseAddr("127.0.0.1"), "127.0.0.1:7789", true},
		{netip.MustParseAddr("192.168.1.9"), "127.0.0.1:7789", false},
		{netip.Addr{}, "127.0.0.1:7789", false},
		{netip.Addr{}, "192.168.1.42:8080", true},
	} {
		ctx := WithRequestSource(context.Background(), tc.source)
		err := refuseUnannouncedHostLocal(ctx, "tcp", tc.address, nil)
		if got := err == nil; got != tc.allowed {
			t.Errorf("source %v, connect to %s: allowed = %v (err %v), want %v",
				tc.source, tc.address, got, err, tc.allowed)
		}
	}
}
