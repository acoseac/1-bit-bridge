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
// runner's own adapters may forward. Interfaces that do not read and a table
// with no row fail it; a table that does not read fails it where CI is set
// and skips it elsewhere (readingErrorSkips); each ends the reconciliation at
// its first reading. Where CI is set a reading that is not whole fails it
// too: an interface whose addresses do not read, or one holding an address
// of the family, a *net.IPNet or an anycast *net.IPAddr, with no row in the
// table, each asked of the rows' indexes directly. The interfaces, the table,
// each row's entry and the addresses are separate observations, read again
// together up to hostModelAttempts times before a mismatch between them fails
// (reconcileHostModel, backlog B997).
func TestTheStrongHostModelIsReadFromTheStack(t *testing.T) {
	ci := os.Getenv("CI") != ""
	for _, ipv6 := range []bool{false, true} {
		var took time.Duration
		out, err := reconcileHostModel(ipv6, func() hostModelReading {
			start := time.Now()
			r := readHostModel(ipv6)
			took = time.Since(start)
			return r
		})
		if err != nil {
			if readingErrorSkips(out, ci) {
				t.Skip(err)
			}
			if ci {
				t.Fatalf("CI: %v", err)
			}
			t.Fatal(err)
		}
		t.Logf("ipv6=%v: %d rows, %d reading(s), the last in %v (the interfaces, the table, each row's entry and the addresses)",
			ipv6, len(out.reading.table), out.attempts, took)
		for _, row := range out.reading.table {
			t.Logf("  row %d (%s): weakHostSend=%v weakHostReceive=%v forwarding=%v",
				row.Index, out.names[row.Index], row.WeakHostSend, row.WeakHostReceive, row.Forwarding)
		}
		for _, m := range out.mismatches {
			switch {
			case !m.ciOnly:
				t.Errorf("after %d readings: %s", out.attempts, m.text)
			case ci:
				t.Errorf("CI: after %d readings: %s", out.attempts, m.text)
			default:
				t.Logf("  %s", m.text)
			}
		}
		// The decision the console would make of these rows, for a reading
		// whose every interface with an address of the family has a row.
		if len(out.addressed) > 0 {
			t.Logf("  the console's decision for interface %d: %v", out.addressed[0],
				JudgeStrongHost(out.reading.table, out.addressed, out.addressed[0], out.names))
		}
	}
}

// readHostModel is one reading of a family's four observations from the
// stack, in turn: the interfaces and their names, the interface table, each
// row's own entry, and each interface's addresses.
func readHostModel(ipv6 bool) hostModelReading {
	var r hostModelReading
	ifs, err := net.Interfaces()
	if err != nil {
		r.interfacesErr = err
		return r
	}
	if r.table, r.tableErr = HostModel(ipv6); r.tableErr != nil {
		return r
	}
	r.entries = make(map[int]readEntry, len(r.table))
	for _, row := range r.table {
		entry, err := hostModelEntry(row.Index, ipv6)
		r.entries[row.Index] = readEntry{row: entry, err: err}
	}
	for _, in := range ifs {
		addrs, err := in.Addrs()
		r.interfaces = append(r.interfaces, readInterface{index: in.Index, name: in.Name, addrs: addrs, addrErr: err})
	}
	return r
}
