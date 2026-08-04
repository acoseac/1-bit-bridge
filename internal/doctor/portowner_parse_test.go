package doctor

import (
	"reflect"
	"strings"
	"testing"
)

// Real /proc/net/tcp shape. Header plus, in order:
//   - 127.0.0.1:7789 LISTEN, uid 1000   (the admin console)
//   - 0.0.0.0:22     LISTEN, uid 0      (sshd — a foreign uid on another port)
//   - 127.0.0.1:1E6D as a NON-listen row (st 01 = ESTABLISHED): an outbound
//     connection whose LOCAL port happens to be 7789. Matching on port alone
//     without the state filter would credit uid 65534 to port 7789.
//   - the last row is COPIED VERBATIM from bridge.ars.md, the host this fix
//     exists for: an ESTABLISHED outbound connection TO someone else's :443,
//     so 01BB appears in the REMOTE column. Its uid is 1000 — the bridge's
//     OWN uid — so reading field 2 instead of field 1 would report "port 443
//     is owned by this user" on the strength of an outgoing HTTPS request.
//     Two independent guards reject it (the column index and the state
//     filter) and the real host really does produce it.
const procNetTCPFixture = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1E6D 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 24680 1 0000000000000000 100 0 0 10 0
   1: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 13579 1 0000000000000000 100 0 0 10 0
   2: 0100007F:1E6D 0100007F:C001 01 00000000:00000000 00:00000000 00000000 65534        0 11111 1 0000000000000000 100 0 0 10 0
   4: 0400000A:897E 06CD7FB9:01BB 01 00000000:00000000 02:000000FE 00000000  1000        0 6213105 2 0000000000000000 21 4 31 10 -1
`

// Real /proc/net/tcp6 shape, also copied from bridge.ars.md: the IPv6
// wildcard listener the bridge's [::]:443 bind produces, owned by uid 1000.
// Dual-stack, so it appears here and NOT in /proc/net/tcp — which is why
// both files have to be read.
const procNetTCP6Fixture = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:01BB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 6213098 2 0000000000000000 100 0 0 10 0
`

func TestScanListenerUIDs(t *testing.T) {
	tests := []struct {
		name  string
		table string
		port  int
		want  []int
	}{
		{
			name:  "ipv4 listener matches on big-endian hex port",
			table: procNetTCPFixture,
			port:  7789, // 0x1E6D
			want:  []int{1000},
		},
		{
			name:  "ipv6 wildcard listener is found in tcp6",
			table: procNetTCP6Fixture,
			port:  443, // 0x01BB
			want:  []int{1000},
		},
		{
			name:  "foreign uid on another port is not attributed",
			table: procNetTCPFixture,
			port:  22, // 0x0016
			want:  []int{0},
		},
		{
			name:  "a port nothing listens on yields nothing",
			table: procNetTCPFixture,
			port:  8080,
			want:  nil,
		},
		{
			// Also the column-index guard: the v4 table's last row is a
			// real outbound connection TO :443 carrying our own uid, so a
			// parser reading rem_address would answer [1000] here.
			name:  "ipv6 listener absent from v4 table, and a remote :443 is not a local one",
			table: procNetTCPFixture,
			port:  443,
			want:  nil,
		},
		{
			name:  "header-only input is not a row",
			table: strings.SplitAfter(procNetTCPFixture, "\n")[0],
			port:  7789,
			want:  nil,
		},
		{
			name:  "empty input",
			table: "",
			port:  7789,
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scanListenerUIDs(strings.NewReader(tc.table), tc.port)
			if err != nil {
				t.Fatalf("scanListenerUIDs: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("port %d: got %v, want %v", tc.port, got, tc.want)
			}
		})
	}
}

// TestScanListenerUIDs_IgnoresNonListenRows is the state filter's own test,
// separated out because it is the one clause whose absence still passes
// every case above: fixture row 2 is an ESTABLISHED socket whose LOCAL port
// is 7789 and whose uid (65534) is not the listener's. Without the `st`
// check that uid joins the result and a foreign process can be reported as
// owning our port.
func TestScanListenerUIDs_IgnoresNonListenRows(t *testing.T) {
	got, err := scanListenerUIDs(strings.NewReader(procNetTCPFixture), 7789)
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range got {
		if uid == 65534 {
			t.Fatalf("uid 65534 came from the ESTABLISHED row — the st==%s filter is not applied; "+
				"an outbound connection from this port must never be read as owning it", tcpStateListen)
		}
	}
	if len(got) != 1 {
		t.Errorf("got %v, want exactly the one LISTEN row", got)
	}
}

// TestScanListenerUIDs_PortIsBigEndian pins the byte order. The address half
// of local_address is little-endian for IPv4 while the port half is not, so
// reading the port the same way as the address is an easy and silent error:
// 7789 (0x1E6D) byte-swapped is 0x6D1E = 27934, which would match nothing on
// a real host and make the whole check quietly useless.
func TestScanListenerUIDs_PortIsBigEndian(t *testing.T) {
	if got, _ := scanListenerUIDs(strings.NewReader(procNetTCPFixture), 7789); len(got) != 1 {
		t.Errorf("port 7789 (0x1E6D) should match the fixture's listener, got %v", got)
	}
	if got, _ := scanListenerUIDs(strings.NewReader(procNetTCPFixture), 27934); len(got) != 0 {
		t.Errorf("port 27934 (0x6D1E, the byte-swapped form) must NOT match, got %v", got)
	}
}
