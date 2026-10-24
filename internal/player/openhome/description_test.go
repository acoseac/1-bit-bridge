package openhome

import (
	"encoding/xml"
	"net/netip"
	"strings"
	"testing"
	"unicode/utf8"
)

// testUUID is the player identity the tests' descriptions carry.
const testUUID = "7d444840-9dc0-11d1-b245-5ffdce74fad2"

// deviceDoc is a device description, read as a controller reads it.
type deviceDoc struct {
	XMLName xml.Name `xml:"urn:schemas-upnp-org:device-1-0 root"`
	Device  struct {
		DeviceType       string  `xml:"deviceType"`
		FriendlyName     string  `xml:"friendlyName"`
		Manufacturer     string  `xml:"manufacturer"`
		ManufacturerURL  string  `xml:"manufacturerURL"`
		ModelDescription string  `xml:"modelDescription"`
		ModelName        string  `xml:"modelName"`
		ModelNumber      string  `xml:"modelNumber"`
		SerialNumber     *string `xml:"serialNumber"`
		UDN              string  `xml:"UDN"`
		Services         []struct {
			Type     string `xml:"serviceType"`
			ID       string `xml:"serviceId"`
			SCPDURL  string `xml:"SCPDURL"`
			Control  string `xml:"controlURL"`
			EventSub string `xml:"eventSubURL"`
		} `xml:"serviceList>service"`
	} `xml:"device"`
}

func readDevice(t *testing.T, b []byte) deviceDoc {
	t.Helper()
	var d deviceDoc
	if err := xml.Unmarshal(b, &d); err != nil {
		t.Fatalf("the description does not read: %v\n%s", err, b)
	}
	return d
}

// TestTheDeviceDescriptionIsTheSourcesOwn: an OpenHome Source, the
// player's UDN (never the MediaServer's), the fixed vendor fields, no
// serial number, the default friendlyName, and the four services, each
// with its type and distinct ID, and its SCPD, control and event URLs
// absolute on the address and port the request arrived on, under
// /openhome/<service>/; a request on another address, or on a port the
// kernel picked, gets that address and port in every URL.
func TestTheDeviceDescriptionIsTheSourcesOwn(t *testing.T) {
	dev := Device{UUID: testUUID, ModelNumber: "0.3.0-test"}
	for _, local := range []string{"192.168.1.20:7792", "10.0.0.2:7792", "169.254.7.7:51234"} {
		d := readDevice(t, dev.description(netip.MustParseAddrPort(local)))
		got := d.Device
		if got.DeviceType != "urn:av-openhome-org:device:Source:1" || got.UDN != "uuid:"+testUUID ||
			got.Manufacturer != "1-bit" || got.ManufacturerURL != "https://1-bit.app" ||
			got.ModelName != "1-bit Bridge Player" || got.ModelNumber != "0.3.0-test" ||
			got.ModelDescription != "The 1-bit bridge's own player" || got.FriendlyName != "1-bit Bridge Player" {
			t.Errorf("%s: the device %+v", local, got)
		}
		if got.SerialNumber != nil {
			t.Errorf("%s: the description carries a serialNumber", local)
		}
		if len(got.Services) != 4 {
			t.Fatalf("%s: %d services, want 4", local, len(got.Services))
		}
		ids := map[string]bool{}
		for i, s := range got.Services {
			want := services[i]
			base := "http://" + local + "/openhome/" + want.path + "/"
			if s.Type != want.serviceType() || s.ID != want.serviceID() || ids[s.ID] ||
				s.SCPDURL != base+"scpd.xml" || s.Control != base+"control" || s.EventSub != base+"event" {
				t.Errorf("%s: service %d is %+v; want %s, %s under %s", local, i, s, want.serviceType(), want.serviceID(), base)
			}
			ids[s.ID] = true
		}
	}
}

// TestTheModelDescriptionIsNoOneElses: the vendor fields match none of what
// 1-bit sets a Source aside by (upmpdcli's manufacturer and
// modelDescription, which it does not offer), and the friendlyName is no
// renderer's with -UPnP/AV, which would pair the Source with that renderer.
func TestTheModelDescriptionIsNoOneElses(t *testing.T) {
	for _, field := range []string{manufacturer, modelDescription, modelName, DefaultName} {
		for _, theirs := range []string{"JF Light Industries", "UPnP front-end to MPD", "upmpdcli", "-UPnP/AV"} {
			if strings.Contains(field, theirs) {
				t.Errorf("%q holds %q", field, theirs)
			}
		}
	}
}

// TestTheFriendlyNameIsTheRoomAsXMLCanCarryIt: the room, when set, made into
// a name XML 1.0 carries: what it cannot (a control character, bytes that
// are not UTF-8, U+FFFE) dropped, then cut to 64 runes at a rune boundary;
// the default when the room is unset or keeps nothing; and escaped in the
// description, which reads back as the name.
func TestTheFriendlyNameIsTheRoomAsXMLCanCarryIt(t *testing.T) {
	sixtyFive := strings.Repeat("é", 40) + strings.Repeat("ß", 25)
	for _, tc := range []struct {
		name, room, want string
	}{
		{"unset", "", DefaultName},
		{"a room", "Listening Room", "Listening Room"},
		{"65 runes, multi-byte", sixtyFive, strings.Repeat("é", 40) + strings.Repeat("ß", 24)},
		{"a control character", "Kitchen\x01 Left", "Kitchen Left"},
		{"bytes that are not UTF-8", "Caf\xe9 Bar", "Caf Bar"},
		{"a noncharacter XML refuses", "Den￾", "Den"},
		{"nothing XML carries", "\x01\x02￿", DefaultName},
		{"what XML escapes", `Tom & "Jerry" <Den>`, `Tom & "Jerry" <Den>`},
		{"64 runes and a control after them", strings.Repeat("r", 64) + "\x7fs", strings.Repeat("r", 64)},
		{"a control inside the first 64", "\x01" + strings.Repeat("r", 64), strings.Repeat("r", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FriendlyName(tc.room)
			if got != tc.want {
				t.Fatalf("FriendlyName = %q, want %q", got, tc.want)
			}
			if n := utf8.RuneCountInString(got); n > 64 || !utf8.ValidString(got) {
				t.Errorf("%d runes, valid UTF-8 %v", n, utf8.ValidString(got))
			}
			d := readDevice(t, Device{UUID: testUUID, Room: tc.room}.description(netip.MustParseAddrPort("192.168.1.20:7792")))
			if d.Device.FriendlyName != tc.want {
				t.Errorf("the description's friendlyName reads %q, want %q", d.Device.FriendlyName, tc.want)
			}
		})
	}
}
