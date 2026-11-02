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

// platformControlData is Linux's shapes of its own, none since x/net v0.62.0
// (backlog B615). Its Linux table sets only the TTL and packet-info entries,
// and up to v0.61.0 an IPPROTO_IP message of type 0 matched the unset
// destination entry and x/net's parse panicked on it (B610); since v0.62.0
// such a message is passed over as on macOS, and its rows are
// portableControlData's.
func platformControlData() []controlDataCase { return nil }

// datagramMetaSeeds are the seeds committed in
// testdata/fuzz/FuzzParseDatagramMeta, each the control data amd64 and arm64
// Linux lay out, with the metadata it must decode to. The first reproduced
// the nightly's crash at x/net v0.61.0 (B610), reconstructed rather than
// taken from the run's artifact, so the stack was the nightly's but the bytes
// need not be: the target's own Windows-layout seed with its type zeroed,
// which here reads as one IPPROTO_IP message of type 0 with 8 bytes of data,
// the unit case's message byte for byte. Then an IP_PKTINFO alone,
// the messages a socket asking for a timestamp, the packet info and the TTL
// is handed (SCM_TIMESTAMP, IP_PKTINFO, IP_TTL, in the order the kernel writes
// them), and an IP_PKTINFO followed by a message of type 0, which x/net
// passes over since v0.62.0 (backlog B615), so the IP_PKTINFO stands.
func datagramMetaSeeds() []controlDataCase {
	pkt := pktinfoAt(7, ssdpGroup4)
	return []controlDataCase{
		{"the_nightlys_crash", unixCmsg(unix.IPPROTO_IP, 0, []byte{192, 168, 1, 20, 7, 0, 0, 0}, -1), datagramMeta{}},
		{"a_pktinfo", pkt, pktinfoMeta(ssdpGroup4, 7)},
		{"a_timestamp_a_pktinfo_and_a_ttl", slices.Concat(
			unixCmsg(unix.SOL_SOCKET, unix.SCM_TIMESTAMP, make([]byte, 16), -1),
			pkt,
			unixCmsg(unix.IPPROTO_IP, unix.IP_TTL, []byte{64, 0, 0, 0}, -1)), pktinfoMeta(ssdpGroup4, 7)},
		{"a_pktinfo_then_type_0", slices.Concat(pkt, unixCmsg(unix.IPPROTO_IP, 0, []byte{0, 0, 0, 0}, -1)), pktinfoMeta(ssdpGroup4, 7)},
	}
}

// TestTheDatagramMetaSeedsAreLinuxsControlData pins the seeds of
// FuzzParseDatagramMeta, whose invariant alone would accept metadata that
// reports nothing for every input: each file holds exactly its control data
// in the fuzz engine's format, under the name the engine gives such a file,
// and decodes to its metadata, x/net's own answer, x/net's parse panicking on
// none of them since v0.62.0 (backlog B615).
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
		if ref, panicked := referenceDatagramMeta(s.oob); panicked || ref != s.want {
			t.Errorf("%s: x/net's parse reports %+v (panicked %v), want %+v", s.name, ref, panicked, s.want)
		}
	}
}
