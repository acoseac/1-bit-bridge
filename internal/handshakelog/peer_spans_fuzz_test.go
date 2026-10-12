package handshakelog

import "testing"

// FuzzPeerAddressSpansMatchThePattern holds the hand-written walk
// (peerAddressSpans) to the regular expression it replaced (peerAddr) on
// any line, not only the fixtures TestPeerAddressSpansMatchThePattern
// lists. A span the walk misses is a peer address left in a log line, so
// the two must agree byte for byte. The nightly fuzz job finds this target
// by its name.
func FuzzPeerAddressSpansMatchThePattern(f *testing.F) {
	for _, seed := range []string{
		"http: TLS handshake error from 10.0.0.2:5000: EOF",
		"read tcp 127.0.0.1:7788->10.0.0.2:5000: i/o timeout",
		"read tcp 10.0.0.2:5000: connection reset by peer",
		"from [fe80::7%eth0]:50000 serving [fe80::1%25]:9",
		"client [fe80::7%Wi-Fi 4]:50000",
		"from 10.0.0.2:5000 and 10.0.0.2:50001 and 110.0.0.2:5000",
		"from 10.1.2.3:4\nclient 10.1.2.3:5",
		"[::1]:0 [:]:1 1.2.3.4: 1.2.3.4:x 999.1.1.1:80",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		got := peerAddressSpans(line)
		want := peerAddr.FindAllStringSubmatchIndex(line, -1)
		if len(got) != len(want) {
			t.Fatalf("walk found %d spans, the pattern %d, in %q", len(got), len(want), line)
		}
		for i, m := range want {
			if len(m) < 6 || got[i][0] != m[4] || got[i][1] != m[5] {
				t.Fatalf("span %d = %v, the pattern's group is %v, in %q", i, got[i], m, line)
			}
		}
	})
}
