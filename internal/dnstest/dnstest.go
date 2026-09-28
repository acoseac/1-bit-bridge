// Package dnstest serves the DNS answers a test chooses, through a
// net.Resolver it hands out, so a test can make a NAME resolve to one address
// and then to another (DNS rebinding) without touching the host's resolver or
// the network. Like net/http/httptest it is imported only by tests, so none of
// it reaches the binary.
//
// It exists because the threat it reproduces lives in the resolution: a URL
// that names a host by a name the attacker's DNS answers is checked at one
// moment and dialled at another, and only the address the name resolved to at
// the dial says where the connection went. Go's resolver keeps no cache, so
// every dial asks again, and so does this server.
package dnstest

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// Server answers DNS queries over UDP on 127.0.0.1. A query for its one name
// gets the address last given to Start or Answer (an A record for an IPv4
// address, AAAA for IPv6, and no record of the other type); any other name
// gets NXDOMAIN. Every answer has a TTL of zero.
type Server struct {
	conn net.PacketConn
	name string // lower case, with the root's trailing dot

	mu      sync.Mutex
	addr    netip.Addr
	queries int
}

// Start serves name, matched without regard to case or a trailing dot, with
// addr until the test ends.
func Start(t testing.TB, name string, addr netip.Addr) *Server {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("dnstest: listen: %v", err)
	}
	s := &Server{conn: conn, name: strings.ToLower(strings.TrimSuffix(name, ".")) + ".", addr: addr}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.serve()
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	return s
}

// Answer makes every later query for the name get addr.
func (s *Server) Answer(addr netip.Addr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addr = addr
}

// Queries is how many queries the server has answered, for any name.
func (s *Server) Queries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queries
}

// Resolver returns a resolver that asks this server and nothing else. It
// sets PreferGo so the Go resolver, which dials through Dial, is used on
// every platform (macOS and Windows otherwise ask the system's).
func (s *Server) Resolver() *net.Resolver {
	addr := s.conn.LocalAddr().String()
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp4", addr)
		},
	}
}

func (s *Server) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := s.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		if resp, ok := s.respond(buf[:n]); ok {
			_, _ = s.conn.WriteTo(resp, from)
		}
	}
}

// respond builds the answer to one query, or reports false for a packet that
// is not a query it can read. The header says authoritative and
// recursion-available, because Go's resolver reads a no-answer response
// without either as a lame referral and asks the next server.
func (s *Server) respond(query []byte) ([]byte, bool) {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil, false
	}
	q, err := p.Question()
	if err != nil {
		return nil, false
	}
	s.mu.Lock()
	addr := s.addr
	s.queries++
	s.mu.Unlock()

	rh := dnsmessage.Header{
		ID: h.ID, Response: true, Authoritative: true,
		RecursionDesired: h.RecursionDesired, RecursionAvailable: true,
	}
	ours := strings.EqualFold(q.Name.String(), s.name)
	if !ours {
		rh.RCode = dnsmessage.RCodeNameError
	}
	b := dnsmessage.NewBuilder(nil, rh)
	if b.StartQuestions() != nil || b.Question(q) != nil || b.StartAnswers() != nil {
		return nil, false
	}
	if ours {
		rr := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET}
		switch {
		case q.Type == dnsmessage.TypeA && addr.Is4():
			err = b.AResource(rr, dnsmessage.AResource{A: addr.As4()})
		case q.Type == dnsmessage.TypeAAAA && addr.Is6() && !addr.Is4In6():
			err = b.AAAAResource(rr, dnsmessage.AAAAResource{AAAA: addr.As16()})
		}
		if err != nil {
			return nil, false
		}
	}
	msg, err := b.Finish()
	return msg, err == nil
}
