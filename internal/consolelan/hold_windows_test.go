//go:build windows

package consolelan

import (
	"net"
	"os"
	"testing"
	"time"
)

// TestTheStrongHostModelIsReadFromTheStack qualifies the Windows table on
// the windows-latest leg (the plan's §9.5): HostModel reads the interface
// table of each family from the stack in one call, each row's interface and
// flags those GetIpInterfaceEntry gives for it, and JudgeStrongHost decides
// on the rows read. It records the runner's rows and a reading's cost in its
// log (§3.4, §10.4) and asserts no particular answer of them, since a
// runner's own adapters may forward. Where CI is set, a table that does not
// read fails, and so does a reading that is not whole: an interface whose
// addresses do not read, or one holding an address of the family with no row
// in the table, each asked of the rows' indexes directly.
func TestTheStrongHostModelIsReadFromTheStack(t *testing.T) {
	ifs, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	names := map[int]string{}
	for _, in := range ifs {
		names[in.Index] = in.Name
	}
	for _, ipv6 := range []bool{false, true} {
		start := time.Now()
		table, err := HostModel(ipv6)
		took := time.Since(start)
		if err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("CI: the interface table (ipv6=%v) did not read: %v", ipv6, err)
			}
			t.Skipf("the interface table (ipv6=%v) did not read: %v", ipv6, err)
		}
		if len(table) == 0 {
			t.Fatalf("the interface table (ipv6=%v) has no row", ipv6)
		}
		t.Logf("ipv6=%v: %d rows read in %v", ipv6, len(table), took)
		for _, row := range table {
			entry, err := hostModelEntry(row.Index, ipv6)
			if err != nil {
				t.Errorf("GetIpInterfaceEntry(%d, ipv6=%v): %v", row.Index, ipv6, err)
				continue
			}
			if entry != row {
				t.Errorf("row %d reads %+v from the table and %+v from its entry", row.Index, row, entry)
			}
			t.Logf("  row %d (%s): weakHostSend=%v weakHostReceive=%v forwarding=%v",
				row.Index, names[row.Index], row.WeakHostSend, row.WeakHostReceive, row.Forwarding)
		}
		// The decision the console would make of these rows, for a reading
		// whose every interface with an address of the family has a row.
		var addressed []int
		for _, in := range ifs {
			addrs, err := in.Addrs()
			if err != nil {
				if os.Getenv("CI") != "" {
					t.Fatalf("CI: the addresses of interface %d (%s) did not read: %v", in.Index, in.Name, err)
				}
				continue
			}
			for _, a := range addrs {
				if n, ok := a.(*net.IPNet); ok && (n.IP.To4() == nil) == ipv6 {
					addressed = append(addressed, in.Index)
					break
				}
			}
		}
		rowed := map[int]bool{}
		for _, row := range table {
			rowed[row.Index] = true
		}
		for _, index := range addressed {
			if rowed[index] {
				continue
			}
			if os.Getenv("CI") != "" {
				t.Errorf("CI: interface %d (%s) holds an address of the family (ipv6=%v) and has no row in the table",
					index, names[index], ipv6)
			} else {
				t.Logf("  interface %d (%s) holds an address of the family and has no row in the table", index, names[index])
			}
		}
		if len(addressed) > 0 {
			t.Logf("  the console's decision for interface %d: %v", addressed[0],
				JudgeStrongHost(table, addressed, addressed[0], names))
		}
		start = time.Now()
		for _, in := range ifs {
			_, _ = in.Addrs()
		}
		_, _ = HostModel(ipv6)
		t.Logf("  a reading (the addresses of %d interfaces and the table) took %v", len(ifs), time.Since(start))
	}
}
