package dlna

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

// platformControlData is Linux's IPPROTO_IP messages of type 0, on which
// x/net's parse panics (B610): its Linux table sets only the TTL and
// packet-info entries, and type 0 matches the unset destination entry. Each
// reports neither, alone, before an IP_PKTINFO, after one or between two: a
// message x/net's parse cannot read without panicking leaves nothing of the
// control data around it.
func platformControlData() []controlDataCase {
	pkt := pktinfoAt(7, ssdpGroup4)
	zero := unixCmsg(unix.IPPROTO_IP, 0, []byte{0, 0, 0, 0}, -1)
	return []controlDataCase{
		{"type_0_alone", zero, datagramMeta{}, true},
		{"type_0_before_a_pktinfo", slices.Concat(zero, pkt), datagramMeta{}, true},
		{"type_0_after_a_pktinfo", slices.Concat(pkt, zero), datagramMeta{}, true},
		{"type_0_between_two_pktinfos", slices.Concat(pkt, zero, pktinfoAt(9, peer4)), datagramMeta{}, true},
	}
}

// datagramMetaSeeds are the seeds committed in
// testdata/fuzz/FuzzParseDatagramMeta, each the control data amd64 and arm64
// Linux lay out, with the metadata it must decode to. The first is the
// nightly's crash (B610): the target's own Windows-layout seed with its type
// zeroed, which here reads as one IPPROTO_IP message of type 0 with 8 bytes
// of data, the unit case's message byte for byte. Then an IP_PKTINFO alone,
// the messages a socket asking for a timestamp, the packet info and the TTL
// is handed (SCM_TIMESTAMP, IP_PKTINFO, IP_TTL, in the order the kernel writes
// them), and an IP_PKTINFO followed by a message of type 0.
func datagramMetaSeeds() []controlDataCase {
	pkt := pktinfoAt(7, ssdpGroup4)
	return []controlDataCase{
		{"the_nightlys_crash", unixCmsg(unix.IPPROTO_IP, 0, []byte{192, 168, 1, 20, 7, 0, 0, 0}, -1), datagramMeta{}, true},
		{"a_pktinfo", pkt, pktinfoMeta(ssdpGroup4, 7), false},
		{"a_timestamp_a_pktinfo_and_a_ttl", slices.Concat(
			unixCmsg(unix.SOL_SOCKET, unix.SCM_TIMESTAMP, make([]byte, 16), -1),
			pkt,
			unixCmsg(unix.IPPROTO_IP, unix.IP_TTL, []byte{64, 0, 0, 0}, -1)), pktinfoMeta(ssdpGroup4, 7), false},
		{"a_pktinfo_then_type_0", slices.Concat(pkt, unixCmsg(unix.IPPROTO_IP, 0, []byte{0, 0, 0, 0}, -1)), datagramMeta{}, true},
	}
}

// TestTheDatagramMetaSeedsAreLinuxsControlData pins the seeds of
// FuzzParseDatagramMeta, whose invariant alone would accept metadata that
// reports nothing for every input: each file holds exactly its control data
// in the fuzz engine's format, under the name the engine gives such a file,
// and decodes to its metadata, x/net's own answer or neither where x/net's
// parse panics.
func TestTheDatagramMetaSeedsAreLinuxsControlData(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("the seeds are the control data amd64 and arm64 lay out, not %s's", runtime.GOARCH)
	}
	for _, s := range datagramMetaSeeds() {
		content := fmt.Sprintf("go test fuzz v1\n[]byte(%q)\n", s.oob)
		name := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))[:16]
		b, err := os.ReadFile(filepath.Join("testdata", "fuzz", "FuzzParseDatagramMeta", name))
		if err != nil {
			t.Errorf("%s: no seed %s: %v", s.name, name, err)
		} else if string(b) != content {
			t.Errorf("%s: seed %s holds %q, want %q", s.name, name, b, content)
		}
		if m := parseDatagramMeta(s.oob); m != s.want {
			t.Errorf("%s: reported %+v, want %+v", s.name, m, s.want)
		}
		if ref, panicked := referenceDatagramMeta(s.oob); panicked != s.panics || ref != s.want {
			t.Errorf("%s: x/net's parse reports %+v (panicked %v), want %+v (panicked %v)", s.name, ref, panicked, s.want, s.panics)
		}
	}
}
