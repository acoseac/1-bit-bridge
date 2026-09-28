package dnstest

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"
)

// TestAServerAnswersItsNameWithTheAddressLastGiven is the one behaviour the
// rebinding tests rely on: every lookup asks again, so Answer takes effect at
// the next lookup, in either address family; a name the server does not own
// does not resolve; and the lookups reach this server and no other.
func TestAServerAnswersItsNameWithTheAddressLastGiven(t *testing.T) {
	s := Start(t, "Upstream.Rebind.Test.", netip.MustParseAddr("192.0.2.66"))
	r := s.Resolver()
	lookup := func(name string) ([]netip.Addr, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return r.LookupNetIP(ctx, "ip", name)
	}
	for _, step := range []struct {
		answer string
		want   []netip.Addr
	}{
		{"", []netip.Addr{netip.MustParseAddr("192.0.2.66")}},
		{"127.0.0.1", []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		{"fe80::1", []netip.Addr{netip.MustParseAddr("fe80::1")}},
	} {
		if step.answer != "" {
			s.Answer(netip.MustParseAddr(step.answer))
		}
		got, err := lookup("upstream.rebind.test")
		if err != nil || !slices.Equal(got, step.want) {
			t.Errorf("after Answer(%q): lookup = %v, %v; want %v", step.answer, got, err, step.want)
		}
	}
	_, err := lookup("elsewhere.rebind.test")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Errorf("a name the server does not own: err = %v, want not found", err)
	}
	if s.Queries() == 0 {
		t.Error("the server answered no query: the lookups went somewhere else")
	}
}
