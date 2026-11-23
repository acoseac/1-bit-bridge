package consolelan

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
)

// hostModelAttempts bounds the readings the live Windows test reconciles
// (TestTheStrongHostModelIsReadFromTheStack, backlog B997). Its four
// observations of a family, the interfaces and their names, the interface
// table, each row's own entry and each interface's addresses, are separate
// calls the stack answers at separate moments, so an interface that gains an
// address, or a row that changes, between two of them reads as a mismatch of
// a correct stack. Each attempt reads all four again, and a mismatch is the
// reading's only once the attempts are spent: a bounded reconciliation of
// separate observations, not an atomic snapshot, which the stack does not
// give.
const hostModelAttempts = 3

// hostModelReading is one attempt's four observations of one family, in the
// order the live test reads them.
type hostModelReading struct {
	interfaces    []readInterface
	interfacesErr error
	table         Table
	tableErr      error
	entries       map[int]readEntry
}

// readInterface is one interface of net.Interfaces' answer, with what its
// Addrs gave.
type readInterface struct {
	index   int
	name    string
	addrs   []net.Addr
	addrErr error
}

// readEntry is what GetIpInterfaceEntry gave for one row's interface.
type readEntry struct {
	row Row
	err error
}

// hostModelMismatch is one disagreement between a reading's observations, or
// one that did not read. ciOnly is one the live test fails on where CI is set
// and only logs elsewhere: an interface whose addresses did not read, and one
// holding an address of the family with no row.
type hostModelMismatch struct {
	text   string
	ciOnly bool
}

// hostModelOutcome is the reading reconcileHostModel ended on, the attempts it
// took, the interfaces that reading shows holding an address of the family
// (the set JudgeStrongHost asks to have rows) with every interface's name, and
// the reading's mismatches, none where it agreed.
type hostModelOutcome struct {
	reading    hostModelReading
	attempts   int
	addressed  []int
	names      map[int]string
	mismatches []hostModelMismatch
}

// reconcileHostModel asks read for a fresh reading of the family up to
// hostModelAttempts times and ends on the first whose observations agree, or
// on the last, with its mismatches, once the attempts are spent. A reading
// whose interfaces or table did not read, or whose table has no row, ends it at
// once with that error, as the live test failed on them before.
func reconcileHostModel(ipv6 bool, read func() hostModelReading) (hostModelOutcome, error) {
	var out hostModelOutcome
	for attempt := 1; attempt <= hostModelAttempts; attempt++ {
		r := read()
		out = hostModelOutcome{reading: r, attempts: attempt}
		switch {
		case r.interfacesErr != nil:
			return out, fmt.Errorf("the interfaces did not read: %w", r.interfacesErr)
		case r.tableErr != nil:
			return out, fmt.Errorf("the interface table (ipv6=%v) did not read: %w", ipv6, r.tableErr)
		case len(r.table) == 0:
			return out, fmt.Errorf("the interface table (ipv6=%v) has no row", ipv6)
		}
		out.addressed, out.names, out.mismatches = judgeHostModelReading(ipv6, r)
		if len(out.mismatches) == 0 {
			return out, nil
		}
	}
	return out, nil
}

// judgeHostModelReading compares one reading's observations: each row with its
// own entry, and each interface holding an address of the family with the
// table's rows.
func judgeHostModelReading(ipv6 bool, r hostModelReading) (addressed []int, names map[int]string, mismatches []hostModelMismatch) {
	names = map[int]string{}
	for _, in := range r.interfaces {
		names[in.index] = in.name
	}
	rowed := map[int]bool{}
	for _, row := range r.table {
		rowed[row.Index] = true
		e, ok := r.entries[row.Index]
		switch {
		case !ok:
			mismatches = append(mismatches, hostModelMismatch{text: fmt.Sprintf("row %d (ipv6=%v) has no entry read for it", row.Index, ipv6)})
		case e.err != nil:
			mismatches = append(mismatches, hostModelMismatch{text: fmt.Sprintf("GetIpInterfaceEntry(%d, ipv6=%v): %v", row.Index, ipv6, e.err)})
		case e.row != row:
			mismatches = append(mismatches, hostModelMismatch{text: fmt.Sprintf("row %d reads %+v from the table and %+v from its entry", row.Index, row, e.row)})
		}
	}
	for _, in := range r.interfaces {
		if in.addrErr != nil {
			mismatches = append(mismatches, hostModelMismatch{ciOnly: true,
				text: fmt.Sprintf("the addresses of interface %d (%s) did not read: %v", in.index, in.name, in.addrErr)})
			continue
		}
		if slices.ContainsFunc(in.addrs, func(a net.Addr) bool { return addressOfFamily(a, ipv6) }) {
			addressed = append(addressed, in.index)
			if !rowed[in.index] {
				mismatches = append(mismatches, hostModelMismatch{ciOnly: true,
					text: fmt.Sprintf("interface %d (%s) holds an address of the family (ipv6=%v) and has no row in the table", in.index, in.name, ipv6)})
			}
		}
	}
	return addressed, names, mismatches
}

// addressOfFamily reports whether a, as net.Interface's Addrs gives it, is an
// address of the family: a *net.IPNet, or a *net.IPAddr, the type Go's Windows
// reading gives an anycast address (src/net/interface_windows.go, the walk of
// FirstAnycastAddress), so an interface holding only an anycast address has
// to have a row too.
func addressOfFamily(a net.Addr, ipv6 bool) bool {
	var ip net.IP
	switch a := a.(type) {
	case *net.IPNet:
		ip = a.IP
	case *net.IPAddr:
		ip = a.IP
	}
	return len(ip) > 0 && (ip.To4() == nil) == ipv6
}

// readingOf is a reading of ifs, the table holding a row of each index in
// rows (every flag off) and each row's entry the same row.
func readingOf(ifs []readInterface, rows ...int) hostModelReading {
	r := hostModelReading{interfaces: ifs, entries: map[int]readEntry{}}
	for _, i := range rows {
		r.table = append(r.table, Row{Index: i})
		r.entries[i] = readEntry{row: Row{Index: i}}
	}
	return r
}

// mixedInterfaces hold, per family, an address as a *net.IPNet and one as a
// *net.IPAddr alone, and one interface holds none.
func mixedInterfaces() []readInterface {
	return []readInterface{
		{index: 1, name: "v4-unicast", addrs: []net.Addr{&net.IPNet{IP: net.ParseIP("192.0.2.2"), Mask: net.CIDRMask(24, 32)}}},
		{index: 2, name: "v4-anycast", addrs: []net.Addr{&net.IPAddr{IP: net.ParseIP("192.0.2.3")}}},
		{index: 3, name: "v6-unicast", addrs: []net.Addr{&net.IPNet{IP: net.ParseIP("2001:db8::2"), Mask: net.CIDRMask(64, 128)}}},
		{index: 4, name: "v6-anycast", addrs: []net.Addr{&net.IPAddr{IP: net.ParseIP("2001:db8::3")}}},
		{index: 5, name: "unnumbered"},
	}
}

// TestTheExpectedSetTakesBothAddressTypesInBothFamilies: an interface holding
// an address of the family as a *net.IPNet and one holding it only as a
// *net.IPAddr (an anycast address on Windows) are both in the set that must
// have rows, in IPv4 and in IPv6, and the anycast-only interface's missing row
// is a mismatch.
func TestTheExpectedSetTakesBothAddressTypesInBothFamilies(t *testing.T) {
	for _, ipv6 := range []bool{false, true} {
		unicast, anycast := 1, 2
		if ipv6 {
			unicast, anycast = 3, 4
		}
		out, err := reconcileHostModel(ipv6, func() hostModelReading {
			return readingOf(mixedInterfaces(), unicast, anycast)
		})
		if err != nil || len(out.mismatches) != 0 {
			t.Fatalf("ipv6=%v: a reading with a row for each addressed interface gave %v and mismatches %v", ipv6, err, out.mismatches)
		}
		if want := []int{unicast, anycast}; !slices.Equal(out.addressed, want) {
			t.Errorf("ipv6=%v: the interfaces holding an address of the family read %v, want %v", ipv6, out.addressed, want)
		}
		out, err = reconcileHostModel(ipv6, func() hostModelReading {
			return readingOf(mixedInterfaces(), unicast)
		})
		if err != nil {
			t.Fatal(err)
		}
		missing := fmt.Sprintf("interface %d (%s) holds an address of the family", anycast, mixedInterfaces()[anycast-1].name)
		if len(out.mismatches) != 1 || !strings.Contains(out.mismatches[0].text, missing) || !out.mismatches[0].ciOnly {
			t.Errorf("ipv6=%v: a table missing the anycast-only interface's row gave the mismatches %v, want one naming it", ipv6, out.mismatches)
		}
	}
}

// TestAMismatchThatSettlesOnTheSecondAttemptIsNoMismatch: the first reading
// finds an interface holding an address with no row (it gained the address
// between the table and the addresses), the second agrees: the outcome is the
// second reading's, with no mismatch, after exactly two readings.
func TestAMismatchThatSettlesOnTheSecondAttemptIsNoMismatch(t *testing.T) {
	reads := 0
	out, err := reconcileHostModel(false, func() hostModelReading {
		reads++
		if reads == 1 {
			return readingOf(mixedInterfaces(), 1)
		}
		return readingOf(mixedInterfaces(), 1, 2)
	})
	if err != nil {
		t.Fatal(err)
	}
	if reads != 2 || out.attempts != 2 || len(out.mismatches) != 0 {
		t.Fatalf("read %d times, outcome after %d attempts with the mismatches %v; want two readings and none", reads, out.attempts, out.mismatches)
	}
}

// TestAPersistentMismatchIsTheReadingsOnceTheAttemptsAreSpent: every reading
// finds a row whose entry reads other flags; the outcome is the last reading's
// mismatch, after exactly hostModelAttempts readings.
func TestAPersistentMismatchIsTheReadingsOnceTheAttemptsAreSpent(t *testing.T) {
	reads := 0
	out, err := reconcileHostModel(false, func() hostModelReading {
		reads++
		r := readingOf(mixedInterfaces(), 1, 2)
		r.entries[2] = readEntry{row: Row{Index: 2, WeakHostSend: true}}
		return r
	})
	if err != nil {
		t.Fatal(err)
	}
	if reads != hostModelAttempts || out.attempts != hostModelAttempts {
		t.Fatalf("read %d times, outcome after %d attempts; want exactly %d", reads, out.attempts, hostModelAttempts)
	}
	if len(out.mismatches) != 1 || !strings.Contains(out.mismatches[0].text, "row 2 reads") || out.mismatches[0].ciOnly {
		t.Fatalf("the mismatches %v, want the row whose entry differs, failed on everywhere", out.mismatches)
	}
}

// readingErrorSkips is how the live Windows test ends on a reading that did
// not read (reconcileHostModel's error): where CI is set every such error
// fails it, interfaces that did not read, a table that did not read and a
// table with no row alike; outside CI a table that did not read skips it, as
// before backlog B997, and the two others fail it.
func readingErrorSkips(out hostModelOutcome, ci bool) bool {
	return !ci && out.reading.tableErr != nil
}

// readingErrors are the three readings that do not read.
var readingErrors = []struct {
	name    string
	reading hostModelReading
	table   bool
}{
	{"the interfaces", hostModelReading{interfacesErr: errors.New("access denied")}, false},
	{"the table", hostModelReading{interfaces: mixedInterfaces(), tableErr: errors.New("access denied")}, true},
	{"a table with no row", hostModelReading{interfaces: mixedInterfaces(), entries: map[int]readEntry{}}, false},
}

// TestAReadingThatDoesNotReadEndsTheReconciliationAtOnce: interfaces that did
// not read, a table that did not read and a table with no row are no mismatch
// to retry: each ends the reconciliation on its first reading, with its error,
// the bound's other attempts left unread.
func TestAReadingThatDoesNotReadEndsTheReconciliationAtOnce(t *testing.T) {
	for _, re := range readingErrors {
		reads := 0
		out, err := reconcileHostModel(true, func() hostModelReading {
			reads++
			return re.reading
		})
		if err == nil || reads != 1 || out.attempts != 1 {
			t.Errorf("%s: the reconciliation ended with %v after %d reading(s), want its error after one", re.name, err, reads)
		}
	}
}

// TestUnderCIEveryReadingThatDoesNotReadFailsTheLiveTest: where CI is set the
// live Windows test fails on each of the three readings that do not read, after
// the reconciliation has ended on it; outside CI it skips on a table that did
// not read alone.
func TestUnderCIEveryReadingThatDoesNotReadFailsTheLiveTest(t *testing.T) {
	for _, re := range readingErrors {
		out, err := reconcileHostModel(false, func() hostModelReading { return re.reading })
		if err == nil {
			t.Fatalf("%s: the reconciliation ended with no error", re.name)
		}
		if readingErrorSkips(out, true) {
			t.Errorf("%s: under CI the live test skips, want it to fail", re.name)
		}
		if got := readingErrorSkips(out, false); got != re.table {
			t.Errorf("%s: outside CI the live test skips: %v, want %v", re.name, got, re.table)
		}
	}
}
