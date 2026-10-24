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
// as its canonical form, which is the URI itself but for a name in capitals,
// written lower-case (B470 PR 5a4, round 2: the form an Insert stores); every
// other spelling of the same track is refused as not canonical, a host with a
// byte that is not ASCII among them (an internationalized name is taken in
// its punycode form alone), and one past MaxLength bytes as too long,
// whatever its shape.
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
		// canon is what the URI reads back as, "" for the URI itself.
		canon string
	}{
		{"http://192.168.1.20:7790/dlna/file/" + id, "192.168.1.20", "", 7790, ""},
		{"http://10.0.0.2:7800/dlna/file/" + id, "10.0.0.2", "", 7800, ""},
		{"http://[fd00::1]:7790/dlna/file/" + id, "fd00::1", "", 7790, ""},
		{"http://[fe80::1]:7790/dlna/file/" + id, "fe80::1", "", 7790, ""},
		{"http://bridge.local:7790/dlna/file/" + id, "", "bridge.local", 7790, ""},
		{"http://Bridge-1.LOCAL:7790/dlna/file/" + id, "", "bridge-1.local", 7790, "http://bridge-1.local:7790/dlna/file/" + id},
		{"http://KITCHEN:7790/dlna/file/" + id, "", "kitchen", 7790, "http://kitchen:7790/dlna/file/" + id},
		{"http://xn--bridge-ryd.local:7790/dlna/file/" + id, "", "xn--bridge-ryd.local", 7790, ""},
		{"http://nas:1/dlna/file/" + id, "", "nas", 1, ""},
		{"http://192.168.1.20:65535/dlna/file/" + id, "192.168.1.20", "", 65535, ""},
		{long, "", strings.ToLower(long[7 : len(long)-len(":7790/dlna/file/"+id)]), 7790, ""},
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
		canon := tc.canon
		if canon == "" {
			canon = tc.uri
		}
		if u.Addr != want || u.Name != tc.name || u.Port != tc.port || u.ID != id || u.String() != canon {
			t.Errorf("%s: %+v, reads back %q, want %q", tc.uri, u, u.String(), canon)
		}
		if again, err := Parse(u.String()); err != nil || again != u || again.String() != canon {
			t.Errorf("%s: its canonical form %q parses to %+v, %v", tc.uri, u.String(), again, err)
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
		// Round 2: a host is ASCII, looked at before anything is folded.
		// Unicode's lower-case of U+0130 (İ) is i and of U+212A (the Kelvin
		// sign) k, so a name folded first read as the ASCII name it is not;
		// U+017F (long s) folds to s under Unicode's case folding; a
		// fullwidth letter and a byte that is not ASCII in the port are
		// refused by the grammar too.
		{"a dotted capital I, which lower-cases to i", "http://brİdge.local:7790/dlna/file/" + id},
		{"the Kelvin sign, which lower-cases to k", "http://Kitchen.local:7790/dlna/file/" + id},
		{"a long s, which folds to s", "http://ſpeaker.local:7790/dlna/file/" + id},
		{"a fullwidth letter", "http://ｂridge.local:7790/dlna/file/" + id},
		{"a name in its Unicode form", "http://bücher.local:7790/dlna/file/" + id},
		{"fullwidth digits in the port", "http://192.168.1.20:７７９０/dlna/file/" + id},
		{"a superscript digit in the port", "http://192.168.1.20:779¹/dlna/file/" + id},
		{"a byte that is not UTF-8 in the port", "http://192.168.1.20:7790\xff/dlna/file/" + id},
		{"fullwidth digits in an address", "http://１９２.168.1.20:7790/dlna/file/" + id},
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

// TestACanonicalNameIsASCIIBeforeItIsFolded pins CanonicalName, through which
// a URI's name and every name the bridge advertises are compared (round 2):
// ASCII, asked of every byte before any is folded, then lower-cased byte by
// byte; a host Unicode's rules would fold to ASCII (U+0130 to i, the Kelvin
// sign U+212A to k, a long s U+017F to s), a fullwidth letter and a name in
// its Unicode form are no name, its punycode form is one, and so is no
// spelling of an address or a host the grammar refuses.
func TestACanonicalNameIsASCIIBeforeItIsFolded(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"bridge.local", "bridge.local"},
		{"Bridge.LOCAL", "bridge.local"},
		{"KITCHEN", "kitchen"},
		{"xn--bcher-kva.local", "xn--bcher-kva.local"},
	} {
		if got, ok := CanonicalName(tc.host); !ok || got != tc.want {
			t.Errorf("%q: %q, %v; want %q", tc.host, got, ok, tc.want)
		}
	}
	for _, host := range []string{"brİdge.local", "Kitchen.local", "ſpeaker.local", "ｂridge.local", "bücher.local",
		"bridge.local\xff", "192.168.1.20", "127.1", "bridge.local.", "", "my_bridge"} {
		if got, ok := CanonicalName(host); ok {
			t.Errorf("%q: taken as %q, want no name", host, got)
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
// a URI it takes is pure ASCII and at most MaxLength bytes; its serialization
// is canonical, the input with its name's capitals lower-cased and nothing
// else changed (so an address, the port and the path are the input's own),
// and idempotent: it parses again to the same URI, which serializes to
// itself; its host is an address in its canonical form (never IPv4-mapped or
// zoned) or a lower-case ASCII name, never both; its port is not 0 and its
// id a track id; and ParseStored names the same id from the input and from
// its serialization. Round 2 replaced the oracle that a taken URI reads back
// as its input byte for byte: an Insert now stores the canonical form.
func FuzzParse(f *testing.F) {
	for _, s := range []string{"http://192.168.1.20:7790/dlna/file/" + id, "http://[fd00::1]:7790/dlna/file/" + id,
		"http://Bridge.Local:7790/dlna/file/" + id, "http://192.168.001.020:7790/dlna/file/" + id, id, "",
		"http://[::ffff:1.2.3.4]:1/dlna/file/" + id, "http://a:07/dlna/file/" + id,
		// Round 2: what Unicode folds to ASCII, and what it does not.
		"http://brİdge.local:7790/dlna/file/" + id, "http://Kitchen.local:7790/dlna/file/" + id,
		"http://ſpeaker.local:7790/dlna/file/" + id, "http://ｂridge.local:7790/dlna/file/" + id,
		"http://192.168.1.20:７７９０/dlna/file/" + id, "http://192.168.1.20:7790\xff/dlna/file/" + id} {
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
		for i := 0; i < len(s); i++ {
			if s[i] >= 0x80 {
				t.Fatalf("%q taken as %+v, with a byte that is not ASCII at %d", s, u, i)
			}
		}
		c := u.String()
		if len(s) > MaxLength || c != lowerASCII(s) || !IsID(u.ID) || u.Port == 0 {
			t.Fatalf("%q taken as %+v, serialized %q: not the input with its capitals lower-cased", s, u, c)
		}
		switch {
		case u.Addr.IsValid() == (u.Name != ""):
			t.Fatalf("%q taken as %+v: an address and a name, or neither", s, u)
		case u.Addr.IsValid() && (u.Addr.Is4In6() || u.Addr.Zone() != "" || !strings.Contains(c, "//"+hostOf(u.Addr)+":")):
			t.Fatalf("%q taken as %+v: an address not in its canonical form", s, u)
		case u.Name != "" && (u.Name != lowerASCII(u.Name) || !strings.HasPrefix(c, "http://"+u.Name+":")):
			t.Fatalf("%q taken as %+v: a name that is not lower-case ASCII as it is written", s, u)
		}
		again, err := Parse(c)
		if err != nil || again != u || again.String() != c {
			t.Fatalf("%q's serialization %q does not parse to itself: %+v, %v", s, c, again, err)
		}
		for _, stored := range []string{s, c} {
			if got, err := ParseStored(stored); err != nil || got != u.ID {
				t.Fatalf("%q as a stored reference: %q, %v", stored, got, err)
			}
		}
	})
}

// lowerASCII is s with each ASCII capital lower-cased and every other byte
// as it is: the oracle's own, so the fuzz does not ask the parser what the
// canonical form is.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// hostOf is a's host as the canonical form writes it: netip's form, an IPv6
// address in brackets.
func hostOf(a netip.Addr) string {
	if a.Is6() {
		return "[" + a.String() + "]"
	}
	return a.String()
}
