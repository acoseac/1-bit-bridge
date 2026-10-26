package openhome

import (
	"net/netip"
	"strings"
	"unicode/utf8"
)

// The device description's fixed fields. modelDescription matches no
// substring 1-bit's renderer profiles key on (the iOS research note: "1-bit"
// matches none) and is not upmpdcli's "UPnP front-end to MPD", which 1-bit
// does not offer.
const (
	manufacturer     = "1-bit"
	manufacturerURL  = "https://1-bit.app"
	modelName        = "1-bit Bridge Player"
	modelDescription = "The 1-bit bridge's own player"
	// DefaultName is the friendlyName when output.player.room is not set.
	DefaultName = "1-bit Bridge Player"
	// maxNameRunes bounds the friendlyName, as config bounds the room.
	maxNameRunes = 64
)

// The surface's paths: /openhome/ alone, the device description, and each
// service's description, control and event URLs under /openhome/<service>/.
const (
	pathPrefix      = "/openhome/"
	descriptionPath = pathPrefix + "description.xml"
	scpdName        = "scpd.xml"
	controlName     = "control"
	eventName       = "event"
)

// Device is what the device description names.
type Device struct {
	// UUID is the player's identity (backup.EnsurePlayerUUID): the UDN is
	// "uuid:" and it, never the MediaServer's.
	UUID string
	// Room is output.player.room, from the boot snapshot; "" is DefaultName.
	Room string
	// ModelNumber is the bridge's version (version.ServerVersion).
	ModelNumber string
}

// FriendlyName is the name a controller shows the player by: room, or
// DefaultName when room is not set; built by dropping what XML 1.0 cannot
// carry (bytes that are not UTF-8, and runes outside its Char production:
// controls other than tab, newline and carriage return, surrogates, U+FFFE
// and U+FFFF), then cutting to 64 runes at a rune boundary. A room that
// keeps nothing is DefaultName. Escaping is the description's. Config's
// validation of room is unchanged: this holds whatever reaches it.
func FriendlyName(room string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.ToValidUTF8(room, "") {
		if !xmlChar(r) {
			continue
		}
		if n == maxNameRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	if b.Len() == 0 {
		return DefaultName
	}
	return b.String()
}

// xmlChar reports whether r is in XML 1.0's Char production.
func xmlChar(r rune) bool {
	switch {
	case r == 0x9 || r == 0xA || r == 0xD:
		return true
	case r >= 0x20 && r <= 0xD7FF:
		return true
	case r >= 0xE000 && r <= 0xFFFD:
		return true
	case r >= 0x10000 && r <= utf8.MaxRune:
		return true
	}
	return false
}

// description is the device description a request that arrived on local
// is answered: every URL in it absolute, on local's address and port, the
// ones the request arrived on (an ephemeral port included), so a
// controller on any of the box's LANs reaches the address it found.
func (d Device) description(local netip.AddrPort) []byte {
	base := "http://" + local.String() + pathPrefix
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<root xmlns="urn:schemas-upnp-org:device-1-0">` + "\n")
	b.WriteString("  <specVersion><major>1</major><minor>0</minor></specVersion>\n")
	b.WriteString("  <device>\n")
	b.WriteString("    <deviceType>" + DeviceType + "</deviceType>\n")
	b.WriteString("    <friendlyName>" + escape(FriendlyName(d.Room)) + "</friendlyName>\n")
	b.WriteString("    <manufacturer>" + escape(manufacturer) + "</manufacturer>\n")
	b.WriteString("    <manufacturerURL>" + escape(manufacturerURL) + "</manufacturerURL>\n")
	b.WriteString("    <modelDescription>" + escape(modelDescription) + "</modelDescription>\n")
	b.WriteString("    <modelName>" + escape(modelName) + "</modelName>\n")
	b.WriteString("    <modelNumber>" + escape(d.ModelNumber) + "</modelNumber>\n")
	b.WriteString("    <UDN>uuid:" + escape(d.UUID) + "</UDN>\n")
	b.WriteString("    <serviceList>\n")
	for _, s := range services {
		at := base + s.path + "/"
		b.WriteString("      <service>\n")
		b.WriteString("        <serviceType>" + s.serviceType() + "</serviceType>\n")
		b.WriteString("        <serviceId>" + s.serviceID() + "</serviceId>\n")
		b.WriteString("        <SCPDURL>" + escape(at+scpdName) + "</SCPDURL>\n")
		b.WriteString("        <controlURL>" + escape(at+controlName) + "</controlURL>\n")
		b.WriteString("        <eventSubURL>" + escape(at+eventName) + "</eventSubURL>\n")
		b.WriteString("      </service>\n")
	}
	b.WriteString("    </serviceList>\n")
	b.WriteString("  </device>\n")
	b.WriteString("</root>\n")
	return []byte(b.String())
}
