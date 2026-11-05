package advertise

import (
	"slices"
	"testing"
)

// TestTheCertifiedNameAndTheLocalEndpointAreOneForm is backlog B739. The
// host's name went into the certificate's want-set (CertHostname) and into
// the `.local` endpoint /v1/health publishes as the system reported it. A
// client checks the certificate against the name it dials, so the two must be
// one name: an ASCII name as it is, byte for byte, its case and its
// underscore kept; a name with non-ASCII characters in its IDNA ASCII form;
// and a name with no valid form, as a test Mac's kern.hostname, neither
// certified nor published. The converted form is one no responder on the LAN
// but the bridge's own answers (PairingHost's Converted), so with that
// responder off (Params.MDNS) its `.local` endpoint is not published, while
// the want-set keeps it (B739's second round): turning mDNS off or on makes
// no certificate stale. An ASCII name's is published either way.
func TestTheCertifiedNameAndTheLocalEndpointAreOneForm(t *testing.T) {
	withIfaces(t, nil)
	for _, tc := range []struct {
		name, host string
		cert       string
		// on and off are the `.local` endpoints with the responder on and
		// off, and converted PairingHost's answer.
		on, off   []string
		converted bool
	}{
		{"a Cyrillic name", "Жук", "xn--f1ai0a", []string{"https://xn--f1ai0a.local:7788"}, nil, true},
		{"the test Mac's bytes", "Ð\u0091Ä¯U", "", nil, nil, false},
		{"mixed case and an underscore", "MacBook-Pro_2", "MacBook-Pro_2",
			[]string{"https://MacBook-Pro_2.local:7788"}, []string{"https://MacBook-Pro_2.local:7788"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(SetHostProbeForTest(
				func() (string, error) { return tc.host, nil },
				func() bool { return false },
			))
			if got := CertHostname(); got != tc.cert {
				t.Errorf("CertHostname = %q, want %q", got, tc.cert)
			}
			if got := PairingHost(); got.Name != tc.cert || got.Converted != tc.converted {
				t.Errorf("PairingHost = %+v, want the name %q, converted %t", got, tc.cert, tc.converted)
			}
			for _, mdns := range []bool{true, false} {
				var local []string
				for _, e := range Endpoints(Params{Port: 7788, MDNS: mdns}) {
					if e.Class == ClassMDNSHost {
						local = append(local, e.URL)
					}
				}
				want := tc.on
				if !mdns {
					want = tc.off
				}
				if !slices.Equal(local, want) {
					t.Errorf("with MDNS %t the .local endpoints are %q, want %q", mdns, local, want)
				}
			}
		})
	}
}
