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
// certified nor published.
func TestTheCertifiedNameAndTheLocalEndpointAreOneForm(t *testing.T) {
	withIfaces(t, nil)
	for _, tc := range []struct {
		name, host string
		cert       string
		local      []string
	}{
		{"a Cyrillic name", "Жук", "xn--f1ai0a", []string{"https://xn--f1ai0a.local:7788"}},
		{"the test Mac's bytes", "Ð\u0091Ä¯U", "", nil},
		{"mixed case and an underscore", "MacBook-Pro_2", "MacBook-Pro_2", []string{"https://MacBook-Pro_2.local:7788"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(SetHostProbeForTest(
				func() (string, error) { return tc.host, nil },
				func() bool { return false },
			))
			if got := CertHostname(); got != tc.cert {
				t.Errorf("CertHostname = %q, want %q", got, tc.cert)
			}
			var local []string
			for _, e := range Endpoints(Params{Port: 7788}) {
				if e.Class == ClassMDNSHost {
					local = append(local, e.URL)
				}
			}
			if !slices.Equal(local, tc.local) {
				t.Errorf("the .local endpoints are %q, want %q", local, tc.local)
			}
		})
	}
}
