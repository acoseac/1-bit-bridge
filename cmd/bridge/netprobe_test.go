package main

import "testing"

func TestProbeLoopbackAddr(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ipv4 wildcard", "0.0.0.0:7789", "127.0.0.1:7789"},
		{"empty host", ":7789", "127.0.0.1:7789"},
		{"ipv6 wildcard", "[::]:7789", "[::1]:7789"},
		{"ipv4 loopback unchanged", "127.0.0.1:7789", "127.0.0.1:7789"},
		{"ipv6 loopback unchanged", "[::1]:7789", "[::1]:7789"},
		{"concrete lan ip unchanged", "192.168.0.208:7789", "192.168.0.208:7789"},
		{"hostname unchanged", "example.com:7789", "example.com:7789"},
		{"localhost unchanged", "localhost:7789", "localhost:7789"},
		{"no port returned verbatim", "not-a-hostport", "not-a-hostport"},
		{"empty string verbatim", "", ""},
		{"whitespace verbatim", "   ", "   "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := probeLoopbackAddr(c.in); got != c.want {
				t.Errorf("probeLoopbackAddr(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
