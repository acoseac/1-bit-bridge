package trackref

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/dlna"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// id is the track id of Artist/Album (2020)/01 Track One.flac: the first 8
// bytes of SHA-256("\x00" + that path), computed outside Go.
const id = "b5b4c8dc9764772e"

// TestMaxLengthIsTheEngines and TestTheFilePrefixIsDLNAs hold the package's
// constants to what they stand for.
func TestMaxLengthIsTheEngines(t *testing.T) {
	if MaxLength != engine.MaxReferenceBytes {
		t.Fatalf("MaxLength %d, the engine's longest reference %d", MaxLength, engine.MaxReferenceBytes)
	}
}

func TestTheFilePrefixIsDLNAs(t *testing.T) {
	if filePrefix != dlna.FilePathPrefix {
		t.Fatalf("the file prefix %q, internal/dlna's %q", filePrefix, dlna.FilePathPrefix)
	}
}

// TestOnlyTheCanonicalFormIsAURI pins the canonical form, spelling by
// spelling: each accepted URI parses to its host, port and id and reads back
// byte for byte; every other spelling of the same track is refused as not
// canonical, and one past MaxLength bytes as too long, whatever its shape.
func TestOnlyTheCanonicalFormIsAURI(t *testing.T) {
	long := "http://" + strings.Repeat("a", 63) + "." + strings.Repeat("b", 19) + ".local:7790/dlna/file/" + id
	if len(long) != MaxLength {
		t.Fatalf("the longest name's URI is %d bytes, want %d", len(long), MaxLength)
	}
	for _, tc := range []struct {
		uri  string
		addr string
		name string
		port uint16
	}{
		{"http://192.168.1.20:7790/dlna/file/" + id, "192.168.1.20", "", 7790},
		{"http://10.0.0.2:7800/dlna/file/" + id, "10.0.0.2", "", 7800},
		{"http://[fd00::1]:7790/dlna/file/" + id, "fd00::1", "", 7790},
		{"http://[fe80::1]:7790/dlna/file/" + id, "fe80::1", "", 7790},
		{"http://bridge.local:7790/dlna/file/" + id, "", "bridge.local", 7790},
		{"http://Bridge-1.LOCAL:7790/dlna/file/" + id, "", "bridge-1.local", 7790},
		{"http://nas:1/dlna/file/" + id, "", "nas", 1},
		{"http://192.168.1.20:65535/dlna/file/" + id, "192.168.1.20", "", 65535},
		{long, "", strings.ToLower(long[7 : len(long)-len(":7790/dlna/file/"+id)]), 7790},
	} {
		u, err := Parse(tc.uri)
		if err != nil {
			t.Errorf("%s: %v, want it taken", tc.uri, err)
			continue
		}
		var want netip.Addr
		if tc.addr != "" {
			want = netip.MustParseAddr(tc.addr)
		}
		if u.Addr != want || u.Name != tc.name || u.Port != tc.port || u.ID != id || u.String() != tc.uri {
			t.Errorf("%s: %+v, reads back %q", tc.uri, u, u.String())
		}
		if got, err := ParseStored(tc.uri); err != nil || got != id {
			t.Errorf("%s as a stored reference: %q, %v", tc.uri, got, err)
		}
	}
	for _, tc := range []struct{ why, uri string }{
		{"the scheme in capitals", "HTTP://192.168.1.20:7790/dlna/file/" + id},
		{"https", "https://192.168.1.20:7790/dlna/file/" + id},
		{"no scheme", "192.168.1.20:7790/dlna/file/" + id},
		{"a zero-padded port", "http://192.168.1.20:07790/dlna/file/" + id},
		{"an empty port", "http://192.168.1.20:/dlna/file/" + id},
		{"no port", "http://192.168.1.20/dlna/file/" + id},
		{"port 0", "http://192.168.1.20:0/dlna/file/" + id},
		{"a port past 65535", "http://192.168.1.20:65536/dlna/file/" + id},
		{"a signed port", "http://192.168.1.20:+7790/dlna/file/" + id},
		{"a zero-padded address", "http://192.168.001.020:7790/dlna/file/" + id},
		{"an abbreviated address", "http://127.1:7790/dlna/file/" + id},
		{"an address as one number", "http://3232235796:7790/dlna/file/" + id},
		{"a trailing dot", "http://bridge.local.:7790/dlna/file/" + id},
		{"an empty label", "http://bridge..local:7790/dlna/file/" + id},
		{"an underscore", "http://my_bridge.local:7790/dlna/file/" + id},
		{"a label ending in a hyphen", "http://bridge-.local:7790/dlna/file/" + id},
		{"user information", "http://user@192.168.1.20:7790/dlna/file/" + id},
		{"user and password", "http://user:pw@192.168.1.20:7790/dlna/file/" + id},
		{"a query", "http://192.168.1.20:7790/dlna/file/" + id + "?x=1"},
		{"an empty query", "http://192.168.1.20:7790/dlna/file/" + id + "?"},
		{"a fragment", "http://192.168.1.20:7790/dlna/file/" + id + "#x"},
		{"percent-encoding in the path", "http://192.168.1.20:7790/dlna/file/%62" + id[1:]},
		{"percent-encoding in the host", "http://192.168.1.2%30:7790/dlna/file/" + id},
		{"a trailing slash", "http://192.168.1.20:7790/dlna/file/" + id + "/"},
		{"a rendition's path", "http://192.168.1.20:7790/dlna/file/" + id + "/variant-abc.flac"},
		{"the path in capitals", "http://192.168.1.20:7790/DLNA/file/" + id},
		{"a doubled slash", "http://192.168.1.20:7790//dlna/file/" + id},
		{"an id in capitals", "http://192.168.1.20:7790/dlna/file/" + strings.ToUpper(id)},
		{"a short id", "http://192.168.1.20:7790/dlna/file/" + id[:15]},
		{"a long id", "http://192.168.1.20:7790/dlna/file/" + id + "0"},
		{"no id", "http://192.168.1.20:7790/dlna/file/"},
		{"an IPv4-mapped IPv6 address", "http://[::ffff:192.168.1.20]:7790/dlna/file/" + id},
		{"an IPv6 address in capitals", "http://[FD00::1]:7790/dlna/file/" + id},
		{"an IPv6 address not compressed", "http://[fd00:0:0:0:0:0:0:1]:7790/dlna/file/" + id},
		{"a zone", "http://[fe80::1%25eth0]:7790/dlna/file/" + id},
		{"an IPv6 address unbracketed", "http://fd00::1:7790/dlna/file/" + id},
		{"an IPv4 address bracketed", "http://[192.168.1.20]:7790/dlna/file/" + id},
		{"no closing bracket", "http://[fd00::1:7790/dlna/file/" + id},
		{"no host", "http://:7790/dlna/file/" + id},
		{"a bare id", id},
		{"nothing", ""},
		{"white space", " http://192.168.1.20:7790/dlna/file/" + id},
	} {
		if _, err := Parse(tc.uri); !errors.Is(err, ErrNotCanonical) || errors.Is(err, ErrTooLong) {
			t.Errorf("%s (%q): %v, want it refused as not canonical", tc.why, tc.uri, err)
		}
		if tc.uri == id {
			continue
		}
		if _, err := ParseStored(tc.uri); !errors.Is(err, ErrNotCanonical) {
			t.Errorf("%s (%q) as a stored reference: %v, want it refused", tc.why, tc.uri, err)
		}
	}
	for _, s := range []string{long[:7] + "x" + long[7:], strings.Repeat("x", MaxLength+1), "http://" + strings.Repeat("%", MaxLength)} {
		if _, err := Parse(s); !errors.Is(err, ErrTooLong) {
			t.Errorf("%d bytes: %v, want ErrTooLong", len(s), err)
		}
		if _, err := ParseStored(s); !errors.Is(err, ErrTooLong) {
			t.Errorf("%d bytes as a stored reference: %v, want ErrTooLong", len(s), err)
		}
	}
}

// TestAStoredReferenceIsAURIOrABareID pins ParseStored: a bare track id (the
// form before PR 5a4) and a canonical URI, whatever host and port it names,
// name the id; anything else is refused, the near misses of an id included.
func TestAStoredReferenceIsAURIOrABareID(t *testing.T) {
	for _, s := range []string{id, "http://203.0.113.9:1/dlna/file/" + id, "http://[fe80::1]:7790/dlna/file/" + id} {
		if got, err := ParseStored(s); err != nil || got != id {
			t.Errorf("%q: %q, %v; want %s", s, got, err, id)
		}
	}
	for _, s := range []string{strings.ToUpper(id), id + " ", " " + id, id[:15], "/dlna/file/" + id, "a", "x" + id[1:]} {
		if _, err := ParseStored(s); !errors.Is(err, ErrNotCanonical) {
			t.Errorf("%q: %v, want it refused", s, err)
		}
	}
}

// FuzzParse holds Parse to the canonical form on any input: never a panic;
// a URI it takes is at most MaxLength bytes, reads back as the input byte for
// byte, and parses again to itself; and ParseStored names the same id.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"http://192.168.1.20:7790/dlna/file/" + id, "http://[fd00::1]:7790/dlna/file/" + id,
		"http://Bridge.Local:7790/dlna/file/" + id, "http://192.168.001.020:7790/dlna/file/" + id, id, "",
		"http://[::ffff:1.2.3.4]:1/dlna/file/" + id, "http://a:07/dlna/file/" + id} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, err := Parse(s)
		if err != nil {
			if !errors.Is(err, ErrNotCanonical) && !errors.Is(err, ErrTooLong) {
				t.Fatalf("%q: %v, neither refusal", s, err)
			}
			return
		}
		if len(s) > MaxLength || u.String() != s || !IsID(u.ID) || u.Port == 0 {
			t.Fatalf("%q taken as %+v, reading back %q", s, u, u.String())
		}
		again, err := Parse(u.String())
		if err != nil || again != u {
			t.Fatalf("%q read back does not parse to itself: %+v, %v", s, again, err)
		}
		if got, err := ParseStored(s); err != nil || got != u.ID {
			t.Fatalf("%q as a stored reference: %q, %v", s, got, err)
		}
	})
}
