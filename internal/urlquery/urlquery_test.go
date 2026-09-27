package urlquery

import (
	"fmt"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// escapedForm is everything Escape may write: RFC 3986's unreserved
// characters and %XX escapes. No "+", so no reader has a choice to make.
var escapedForm = regexp.MustCompile(`^(?:[A-Za-z0-9._~-]|%[0-9A-F]{2})*$`)

// samples covers a space next to a literal plus and a literal percent, the
// characters that end a key or a value, whitespace, non-ASCII, a NUL and a
// byte that is not UTF-8: every one of them has to come back as it went in.
var samples = []string{
	"", "plain", "My Library", "1-bit Bridge", "A+B", " + ", "C++ & Friends",
	"100%", "%20", "%2B", "a&b=c", "?#/[]@!$'()*,;", "\n\t\r",
	"  padded  ", "Ünïcödé Álbum", "東京の音楽 ライブラリ", "Listening 🎧 Room",
	"\x00", "~-._", "\xff\xfe",
}

// TestNetURLWritesASpaceAsAPlusAndAPlusEscaped pins the two facts the
// rewrite rests on. The day url.QueryEscape writes %20 itself, the rewrite
// does nothing and can go. The day it stops escaping a literal "+", the
// rewrite would turn that plus into a space, and the round trips below
// fail too.
func TestNetURLWritesASpaceAsAPlusAndAPlusEscaped(t *testing.T) {
	if got := url.QueryEscape(" "); got != "+" {
		t.Errorf("url.QueryEscape(%q) = %q, want %q", " ", got, "+")
	}
	if got := url.QueryEscape("+"); got != "%2B" {
		t.Errorf("url.QueryEscape(%q) = %q, want %q", "+", got, "%2B")
	}
}

// TestEscapeReadsBackTheSameUnderBothReaders: an RFC 3986 reader
// (url.PathUnescape, which keeps "+" as Foundation's queryItems and the
// admin console's safeQuery do) and a form reader (url.QueryUnescape, which
// reads "+" as a space as url.ParseQuery and URLSearchParams do) both get
// back exactly what was escaped.
func TestEscapeReadsBackTheSameUnderBothReaders(t *testing.T) {
	for _, s := range samples {
		e := Escape(s)
		if !escapedForm.MatchString(e) {
			t.Errorf("Escape(%q) = %q, which holds more than unreserved characters and escapes", s, e)
		}
		if got, err := url.PathUnescape(e); err != nil || got != s {
			t.Errorf("Escape(%q) = %q, which an RFC 3986 reader reads as %q (err %v)", s, e, got, err)
		}
		if got, err := url.QueryUnescape(e); err != nil || got != s {
			t.Errorf("Escape(%q) = %q, which a form reader reads as %q (err %v)", s, e, got, err)
		}
	}
}

// TestEncodeReadsBackTheSameUnderBothReaders is the same for a whole query:
// keys with spaces, a key with two values, and every sample as a value.
func TestEncodeReadsBackTheSameUnderBothReaders(t *testing.T) {
	v := url.Values{}
	for i, s := range samples {
		v.Set(fmt.Sprintf("key %d", i), s)
	}
	v.Add("two values", "a b")
	v.Add("two values", "c+d")
	enc := Encode(v)

	if got, err := url.ParseQuery(enc); err != nil || !reflect.DeepEqual(got, v) {
		t.Errorf("a form reader reads Encode's %q as %q (err %v), want %q", enc, got, err, v)
	}
	rfc3986 := url.Values{}
	for _, pair := range strings.Split(enc, "&") {
		if !strings.Contains(pair, "=") {
			t.Fatalf("Encode wrote %q, a pair with no \"=\"", pair)
		}
		rawKey, rawValue, _ := strings.Cut(pair, "=")
		if !escapedForm.MatchString(rawKey) || !escapedForm.MatchString(rawValue) {
			t.Errorf("Encode wrote %q, which holds more than unreserved characters and escapes", pair)
		}
		key, kerr := url.PathUnescape(rawKey)
		value, verr := url.PathUnescape(rawValue)
		if kerr != nil || verr != nil {
			t.Fatalf("Encode wrote %q, which an RFC 3986 reader cannot decode: %v %v", pair, kerr, verr)
		}
		rfc3986.Add(key, value)
	}
	if !reflect.DeepEqual(rfc3986, v) {
		t.Errorf("an RFC 3986 reader reads Encode's %q as %q, want %q", enc, rfc3986, v)
	}
}
