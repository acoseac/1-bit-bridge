package dlna

import (
	"strings"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// SSDPServerToken
// -----------------------------------------------------------------------------

func Test_SSDPServerToken(t *testing.T) {
	cases := []struct {
		name           string
		productVersion string
		wantSubstrings []string
	}{
		{
			name:           "real_version",
			productVersion: "v0.1.4",
			wantSubstrings: []string{"UPnP/1.0", "1-bit-bridge/v0.1.4", "DLNADOC/1.50"},
		},
		{
			name:           "empty_version_defaults_to_dev",
			productVersion: "",
			wantSubstrings: []string{"1-bit-bridge/dev", "DLNADOC/1.50"},
		},
		{
			name:           "dev_build_tag",
			productVersion: "v0.1.4-2-gabcdef-dirty",
			wantSubstrings: []string{"1-bit-bridge/v0.1.4-2-gabcdef-dirty"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SSDPServerToken(tc.productVersion)
			for _, sub := range tc.wantSubstrings {
				if !strings.Contains(got, sub) {
					t.Errorf("SSDPServerToken(%q) = %q, missing substring %q",
						tc.productVersion, got, sub)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------------
// NotifyTargetsFor
// -----------------------------------------------------------------------------

func Test_NotifyTargetsFor_GoldenOrder(t *testing.T) {
	const udn = "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c"
	got := NotifyTargetsFor(udn)
	want := []NotifyTarget{
		{NT: "upnp:rootdevice", USN: udn + "::upnp:rootdevice"},
		{NT: udn, USN: udn},
		{NT: "urn:schemas-upnp-org:device:MediaServer:1", USN: udn + "::urn:schemas-upnp-org:device:MediaServer:1"},
		{NT: "urn:schemas-upnp-org:service:ContentDirectory:1", USN: udn + "::urn:schemas-upnp-org:service:ContentDirectory:1"},
		{NT: "urn:schemas-upnp-org:service:ConnectionManager:1", USN: udn + "::urn:schemas-upnp-org:service:ConnectionManager:1"},
	}
	if len(got) != len(want) {
		t.Fatalf("NotifyTargetsFor returned %d targets, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("target %d mismatch:\n  got:  %+v\n  want: %+v", i, got[i], want[i])
		}
	}
}

// Test_NotifyTargetsFor_RootDeviceIsFirst pins the spec-recommended
// order (most-universally-searched ST first). Some renderers populate
// their discovery table in arrival order; emitting rootdevice first
// gets us indexed by the most renderers fastest.
func Test_NotifyTargetsFor_RootDeviceIsFirst(t *testing.T) {
	targets := NotifyTargetsFor("uuid:test")
	if len(targets) == 0 {
		t.Fatal("empty NotifyTargets")
	}
	if targets[0].NT != "upnp:rootdevice" {
		t.Errorf("first NotifyTarget NT = %q, want upnp:rootdevice", targets[0].NT)
	}
}

// -----------------------------------------------------------------------------
// BuildNotifyAlive — golden byte shape
// -----------------------------------------------------------------------------

func Test_BuildNotifyAlive_GoldenBytes(t *testing.T) {
	target := NotifyTarget{
		NT:  "urn:schemas-upnp-org:device:MediaServer:1",
		USN: "uuid:test::urn:schemas-upnp-org:device:MediaServer:1",
	}
	got := BuildNotifyAlive(
		"http://192.168.0.14:7790/dlna/description.xml",
		"Darwin/UPnP/1.0 1-bit-bridge/v0.1.4 DLNADOC/1.50",
		target,
	)
	want := "NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"CACHE-CONTROL: max-age=1800\r\n" +
		"LOCATION: http://192.168.0.14:7790/dlna/description.xml\r\n" +
		"SERVER: Darwin/UPnP/1.0 1-bit-bridge/v0.1.4 DLNADOC/1.50\r\n" +
		"NT: urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"NTS: ssdp:alive\r\n" +
		"USN: uuid:test::urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"\r\n"
	if string(got) != want {
		t.Errorf("NOTIFY alive byte shape mismatch:\ngot:\n%q\n\nwant:\n%q", string(got), want)
	}
}

// Test_BuildNotifyAlive_CRLFLineEndings is the LOAD-BEARING regression
// guard for the CRLF-line-ending invariant. Some renderers parse SSDP
// strictly and silently drop packets with bare-LF line endings.
func Test_BuildNotifyAlive_CRLFLineEndings(t *testing.T) {
	target := NotifyTarget{NT: "upnp:rootdevice", USN: "uuid:test::upnp:rootdevice"}
	got := BuildNotifyAlive("http://x/y", "Server/1.0", target)
	s := string(got)
	// Every \n must be preceded by \r
	for i, b := range got {
		if b == '\n' && (i == 0 || got[i-1] != '\r') {
			t.Errorf("byte %d: bare LF (no preceding CR) — SSDP spec requires CRLF, packet: %q", i, s)
		}
	}
	// MUST end with \r\n\r\n (terminating empty line per HTTP framing)
	if !strings.HasSuffix(s, "\r\n\r\n") {
		t.Errorf("NOTIFY packet must end with \\r\\n\\r\\n terminator, got tail: %q", s[len(s)-4:])
	}
}

// -----------------------------------------------------------------------------
// BuildNotifyByeBye — golden byte shape
// -----------------------------------------------------------------------------

func Test_BuildNotifyByeBye_GoldenBytes(t *testing.T) {
	target := NotifyTarget{
		NT:  "upnp:rootdevice",
		USN: "uuid:test::upnp:rootdevice",
	}
	got := BuildNotifyByeBye(
		"http://192.168.0.14:7790/dlna/description.xml",
		"Server/1.0",
		target,
	)
	want := "NOTIFY * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"LOCATION: http://192.168.0.14:7790/dlna/description.xml\r\n" +
		"SERVER: Server/1.0\r\n" +
		"NT: upnp:rootdevice\r\n" +
		"NTS: ssdp:byebye\r\n" +
		"USN: uuid:test::upnp:rootdevice\r\n" +
		"\r\n"
	if string(got) != want {
		t.Errorf("NOTIFY byebye byte shape mismatch:\ngot:\n%q\n\nwant:\n%q", string(got), want)
	}
}

func Test_BuildNotifyByeBye_OmitsCacheControl(t *testing.T) {
	target := NotifyTarget{NT: "upnp:rootdevice", USN: "uuid:test::upnp:rootdevice"}
	got := BuildNotifyByeBye("http://x", "Server/1.0", target)
	if strings.Contains(string(got), "CACHE-CONTROL") {
		t.Errorf("byebye should omit CACHE-CONTROL (max-age is meaningless on byebye), got: %q", string(got))
	}
}

// -----------------------------------------------------------------------------
// BuildMSearchResponse — golden byte shape with fixed timestamp
// -----------------------------------------------------------------------------

func Test_BuildMSearchResponse_GoldenBytes(t *testing.T) {
	// Fixed timestamp for golden stability — UTC noon May 26, 2026.
	date := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	got := BuildMSearchResponse(
		"http://192.168.0.14:7790/dlna/description.xml",
		"Server/1.0",
		"urn:schemas-upnp-org:device:MediaServer:1",
		"uuid:test::urn:schemas-upnp-org:device:MediaServer:1",
		date,
	)
	want := "HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=1800\r\n" +
		// HTTP-compliant IMF-fixdate format per RFC 7231 §7.1.1.1 —
		// literal "GMT" instead of the TZ name "UTC" that
		// time.RFC1123 produced pre-PR-303 fix.
		"DATE: Tue, 26 May 2026 12:00:00 GMT\r\n" +
		"EXT:\r\n" +
		"LOCATION: http://192.168.0.14:7790/dlna/description.xml\r\n" +
		"SERVER: Server/1.0\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"USN: uuid:test::urn:schemas-upnp-org:device:MediaServer:1\r\n" +
		"\r\n"
	if string(got) != want {
		t.Errorf("M-SEARCH response byte shape mismatch:\ngot:\n%q\n\nwant:\n%q", string(got), want)
	}
}

// -----------------------------------------------------------------------------
// MSearchTargets — truth table per UPnP spec
// -----------------------------------------------------------------------------

func Test_MSearchTargets(t *testing.T) {
	const udn = "uuid:f1b3a5c2-8e7d-4f3b-9c1a-0d2e3f4a5b6c"
	all := NotifyTargetsFor(udn)

	t.Run("ssdp_all_returns_all_targets", func(t *testing.T) {
		got := MSearchTargets("ssdp:all", all)
		if len(got) != len(all) {
			t.Errorf("ssdp:all should return all %d targets, got %d", len(all), len(got))
		}
	})

	t.Run("rootdevice_returns_only_rootdevice", func(t *testing.T) {
		got := MSearchTargets("upnp:rootdevice", all)
		if len(got) != 1 {
			t.Fatalf("upnp:rootdevice should return 1 target, got %d", len(got))
		}
		if got[0].NT != "upnp:rootdevice" {
			t.Errorf("rootdevice match returned %q, want upnp:rootdevice", got[0].NT)
		}
	})

	t.Run("udn_returns_only_uuid_target", func(t *testing.T) {
		got := MSearchTargets(udn, all)
		if len(got) != 1 {
			t.Fatalf("UDN should return 1 target, got %d", len(got))
		}
		if got[0].NT != udn {
			t.Errorf("UDN match returned %q, want %q", got[0].NT, udn)
		}
	})

	t.Run("media_server_urn", func(t *testing.T) {
		got := MSearchTargets("urn:schemas-upnp-org:device:MediaServer:1", all)
		if len(got) != 1 {
			t.Fatalf("MediaServer URN should return 1 target, got %d", len(got))
		}
		if got[0].NT != "urn:schemas-upnp-org:device:MediaServer:1" {
			t.Errorf("MediaServer URN match returned %q", got[0].NT)
		}
	})

	t.Run("content_directory_service", func(t *testing.T) {
		got := MSearchTargets("urn:schemas-upnp-org:service:ContentDirectory:1", all)
		if len(got) != 1 {
			t.Fatalf("ContentDirectory service URN should return 1 target, got %d", len(got))
		}
	})

	t.Run("connection_manager_service", func(t *testing.T) {
		got := MSearchTargets("urn:schemas-upnp-org:service:ConnectionManager:1", all)
		if len(got) != 1 {
			t.Fatalf("ConnectionManager service URN should return 1 target, got %d", len(got))
		}
	})

	t.Run("unknown_st_returns_nil", func(t *testing.T) {
		got := MSearchTargets("urn:schemas-upnp-org:device:MediaRenderer:1", all)
		if got != nil {
			t.Errorf("unknown ST should return nil, got %d targets", len(got))
		}
	})

	t.Run("empty_st_returns_nil", func(t *testing.T) {
		got := MSearchTargets("", all)
		if got != nil {
			t.Errorf("empty ST should return nil, got %d targets", len(got))
		}
	})

	t.Run("ssdp_all_returns_defensive_copy", func(t *testing.T) {
		got := MSearchTargets("ssdp:all", all)
		// Mutating the returned slice MUST NOT affect the canonical
		// `all` slice's contents.
		if len(got) == 0 {
			t.Fatal("ssdp:all returned empty — precondition failure")
		}
		got[0] = NotifyTarget{NT: "mutated", USN: "mutated"}
		if all[0].NT == "mutated" {
			t.Errorf("ssdp:all returned a slice that shares backing array with input — must return defensive copy")
		}
	})
}

// -----------------------------------------------------------------------------
// ParseMSearchST — real-world M-SEARCH packet shapes
// -----------------------------------------------------------------------------

func Test_ParseMSearchST(t *testing.T) {
	cases := []struct {
		name   string
		packet string
		want   string
	}{
		{
			name: "well_formed_uppercase_st",
			packet: "M-SEARCH * HTTP/1.1\r\n" +
				"HOST: 239.255.255.250:1900\r\n" +
				"MAN: \"ssdp:discover\"\r\n" +
				"MX: 3\r\n" +
				"ST: urn:schemas-upnp-org:device:MediaServer:1\r\n" +
				"\r\n",
			want: "urn:schemas-upnp-org:device:MediaServer:1",
		},
		{
			name: "lowercase_st_header_name",
			packet: "M-SEARCH * HTTP/1.1\r\n" +
				"HOST: 239.255.255.250:1900\r\n" +
				"st: upnp:rootdevice\r\n" +
				"\r\n",
			want: "upnp:rootdevice",
		},
		{
			name: "mixed_case_st",
			packet: "M-SEARCH * HTTP/1.1\r\n" +
				"St: ssdp:all\r\n" +
				"\r\n",
			want: "ssdp:all",
		},
		{
			name: "missing_st_returns_empty",
			packet: "M-SEARCH * HTTP/1.1\r\n" +
				"HOST: 239.255.255.250:1900\r\n" +
				"MAN: \"ssdp:discover\"\r\n" +
				"\r\n",
			want: "",
		},
		{
			name:   "not_msearch_returns_empty",
			packet: "NOTIFY * HTTP/1.1\r\nNT: upnp:rootdevice\r\nST: should-not-be-extracted\r\n\r\n",
			want:   "",
		},
		{
			name:   "empty_packet",
			packet: "",
			want:   "",
		},
		{
			name: "st_with_extra_whitespace",
			packet: "M-SEARCH * HTTP/1.1\r\n" +
				"ST:    urn:schemas-upnp-org:device:MediaServer:1   \r\n" +
				"\r\n",
			want: "urn:schemas-upnp-org:device:MediaServer:1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseMSearchST([]byte(tc.packet))
			if got != tc.want {
				t.Errorf("ParseMSearchST mismatch\n  packet: %q\n  got:    %q\n  want:   %q", tc.packet, got, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// SSDPMulticastAddr + SSDPMaxAge constants
// -----------------------------------------------------------------------------

// Test_SSDPConstants pins the load-bearing constant values. UPnP spec
// fixes the multicast address; max-age defaults to 1800 by convention.
// A future refactor that changes either would break renderer
// compatibility (multicast) or trip more frequent NOTIFY traffic (age).
func Test_SSDPConstants(t *testing.T) {
	if SSDPMulticastAddr != "239.255.255.250:1900" {
		t.Errorf("SSDPMulticastAddr = %q, MUST be 239.255.255.250:1900 per UPnP spec", SSDPMulticastAddr)
	}
	if SSDPMaxAge != 1800 {
		t.Errorf("SSDPMaxAge = %d, want 1800 (30 min — conventional UPnP MediaServer default)", SSDPMaxAge)
	}
}
