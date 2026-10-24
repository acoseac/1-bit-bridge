package openhome

import (
	"encoding/xml"
	"io"
	"strings"
)

// The OpenHome Source and the four services it carries (no Volume: the
// player's output is fixed). The tables below are the bridge's own,
// written from OpenHome's action tables (ohNetGenerated at 22feef52,
// OpenHome/Net/Service/Upnp/OpenHome/{Product,Playlist,Info,Time}1.xml):
// each action's name, its arguments in order with their directions and
// related state variables, and each state variable's type, allowed values
// and eventing. The upstream files are test data alone
// (testdata/upstream), which TestOurSCPDsAreUpstreamsInventory compares
// these with, exactly; no file of theirs is built into the binary.
const (
	// DeviceType is the device description's deviceType.
	DeviceType = "urn:av-openhome-org:device:Source:1"
	// servicePrefix and serviceIDPrefix make a service's type and its ID.
	servicePrefix   = "urn:av-openhome-org:service:"
	serviceIDPrefix = "urn:av-openhome-org:serviceId:"
)

// The data types the four services' state variables take.
const (
	typeString  = "string"
	typeBoolean = "boolean"
	typeUI4     = "ui4"
	typeI4      = "i4"
	typeBase64  = "bin.base64"
)

// argument is one argument of an action.
type argument struct {
	name string
	in   bool
	// related is the state variable whose type the argument takes.
	related string
}

// action is one action of a service and its arguments, in order.
type action struct {
	name string
	args []argument
}

// ins are the action's in-arguments, in order: what a request carries.
func (a *action) ins() []argument {
	var out []argument
	for _, g := range a.args {
		if g.in {
			out = append(out, g)
		}
	}
	return out
}

// outs are the action's out-arguments, in order: what its response carries.
func (a *action) outs() []argument {
	var out []argument
	for _, g := range a.args {
		if !g.in {
			out = append(out, g)
		}
	}
	return out
}

// variable is one state variable of a service.
type variable struct {
	name, dataType string
	allowed        []string
	sendEvents     bool
}

// service is one of the four services: its name (the type's and the ID's
// last part), the path segment its URLs are under (/openhome/<path>/), and
// its tables.
type service struct {
	name, path string
	actions    []action
	variables  []variable
}

// serviceType is the service's type: urn:av-openhome-org:service:<name>:1.
func (s *service) serviceType() string { return servicePrefix + s.name + ":1" }

// serviceID is the service's stable ID: urn:av-openhome-org:serviceId:<name>.
func (s *service) serviceID() string { return serviceIDPrefix + s.name }

// action is the action named name, or nil.
func (s *service) action(name string) *action {
	for i := range s.actions {
		if s.actions[i].name == name {
			return &s.actions[i]
		}
	}
	return nil
}

// variable is the state variable named name, or nil.
func (s *service) variable(name string) *variable {
	for i := range s.variables {
		if s.variables[i].name == name {
			return &s.variables[i]
		}
	}
	return nil
}

// in and out are an in- and an out-argument.
func in(name, related string) argument  { return argument{name: name, in: true, related: related} }
func out(name, related string) argument { return argument{name: name, related: related} }

// evented and plain are a state variable that is evented and one that is
// not.
func evented(name, dataType string, allowed ...string) variable {
	return variable{name: name, dataType: dataType, allowed: allowed, sendEvents: true}
}
func plain(name, dataType string) variable { return variable{name: name, dataType: dataType} }

// services are the Source's four, in the order the device description
// lists them.
var services = []*service{
	{
		name: "Product", path: "product",
		actions: []action{
			{"Manufacturer", []argument{out("Name", "ManufacturerName"), out("Info", "ManufacturerInfo"), out("Url", "ManufacturerUrl"), out("ImageUri", "ManufacturerImageUri")}},
			{"Model", []argument{out("Name", "ModelName"), out("Info", "ModelInfo"), out("Url", "ModelUrl"), out("ImageUri", "ModelImageUri")}},
			{"Product", []argument{out("Room", "ProductRoom"), out("Name", "ProductName"), out("Info", "ProductInfo"), out("Url", "ProductUrl"), out("ImageUri", "ProductImageUri")}},
			{"Standby", []argument{out("Value", "Standby")}},
			{"SetStandby", []argument{in("Value", "Standby")}},
			{"SourceCount", []argument{out("Value", "SourceCount")}},
			{"SourceXml", []argument{out("Value", "SourceXml")}},
			{"SourceIndex", []argument{out("Value", "SourceIndex")}},
			{"SetSourceIndex", []argument{in("Value", "SourceIndex")}},
			{"SetSourceIndexByName", []argument{in("Value", "SourceName")}},
			{"Source", []argument{in("Index", "SourceIndex"), out("SystemName", "SourceName"), out("Type", "SourceType"), out("Name", "SourceName"), out("Visible", "SourceVisible")}},
			{"Attributes", []argument{out("Value", "Attributes")}},
			{"SourceXmlChangeCount", []argument{out("Value", "SourceXmlChangeCount")}},
		},
		variables: []variable{
			evented("ManufacturerName", typeString), evented("ManufacturerInfo", typeString),
			evented("ManufacturerUrl", typeString), evented("ManufacturerImageUri", typeString),
			evented("ModelName", typeString), evented("ModelInfo", typeString),
			evented("ModelUrl", typeString), evented("ModelImageUri", typeString),
			evented("ProductRoom", typeString), evented("ProductName", typeString), evented("ProductInfo", typeString),
			evented("ProductUrl", typeString), evented("ProductImageUri", typeString),
			evented("Standby", typeBoolean), evented("SourceIndex", typeUI4), evented("SourceCount", typeUI4),
			evented("SourceXml", typeString), evented("Attributes", typeString),
			plain("SourceXmlChangeCount", typeUI4), plain("SourceType", typeString), plain("SourceName", typeString),
			plain("SourceVisible", typeBoolean),
		},
	},
	{
		name: "Playlist", path: "playlist",
		actions: []action{
			{"Play", nil},
			{"Pause", nil},
			{"Stop", nil},
			{"Next", nil},
			{"Previous", nil},
			{"SetRepeat", []argument{in("Value", "Repeat")}},
			{"Repeat", []argument{out("Value", "Repeat")}},
			{"SetShuffle", []argument{in("Value", "Shuffle")}},
			{"Shuffle", []argument{out("Value", "Shuffle")}},
			{"SeekSecondAbsolute", []argument{in("Value", "Absolute")}},
			{"SeekSecondRelative", []argument{in("Value", "Relative")}},
			{"SeekId", []argument{in("Value", "Id")}},
			{"SeekIndex", []argument{in("Value", "Index")}},
			{"TransportState", []argument{out("Value", "TransportState")}},
			{"Id", []argument{out("Value", "Id")}},
			{"Read", []argument{in("Id", "Id"), out("Uri", "Uri"), out("Metadata", "Metadata")}},
			{"ReadList", []argument{in("IdList", "IdList"), out("TrackList", "TrackList")}},
			{"Insert", []argument{in("AfterId", "Id"), in("Uri", "Uri"), in("Metadata", "Metadata"), out("NewId", "Id")}},
			{"DeleteId", []argument{in("Value", "Id")}},
			{"DeleteAll", nil},
			{"TracksMax", []argument{out("Value", "TracksMax")}},
			{"IdArray", []argument{out("Token", "IdArrayToken"), out("Array", "IdArray")}},
			{"IdArrayChanged", []argument{in("Token", "IdArrayToken"), out("Value", "IdArrayChanged")}},
			{"ProtocolInfo", []argument{out("Value", "ProtocolInfo")}},
		},
		variables: []variable{
			evented("TransportState", typeString, "Playing", "Paused", "Stopped", "Buffering"),
			evented("Repeat", typeBoolean), evented("Shuffle", typeBoolean), evented("Id", typeUI4),
			evented("IdArray", typeBase64), evented("TracksMax", typeUI4), evented("ProtocolInfo", typeString),
			plain("Index", typeUI4), plain("Relative", typeI4), plain("Absolute", typeUI4),
			plain("IdList", typeString), plain("TrackList", typeString), plain("Uri", typeString),
			plain("Metadata", typeString), plain("IdArrayToken", typeUI4), plain("IdArrayChanged", typeBoolean),
		},
	},
	{
		name: "Info", path: "info",
		actions: []action{
			{"Counters", []argument{out("TrackCount", "TrackCount"), out("DetailsCount", "DetailsCount"), out("MetatextCount", "MetatextCount")}},
			{"Track", []argument{out("Uri", "Uri"), out("Metadata", "Metadata")}},
			{"Details", []argument{out("Duration", "Duration"), out("BitRate", "BitRate"), out("BitDepth", "BitDepth"),
				out("SampleRate", "SampleRate"), out("Lossless", "Lossless"), out("CodecName", "CodecName")}},
			{"Metatext", []argument{out("Value", "Metatext")}},
		},
		variables: []variable{
			evented("TrackCount", typeUI4), evented("DetailsCount", typeUI4), evented("MetatextCount", typeUI4),
			evented("Uri", typeString), evented("Metadata", typeString), evented("Duration", typeUI4),
			evented("BitRate", typeUI4), evented("BitDepth", typeUI4), evented("SampleRate", typeUI4),
			evented("Lossless", typeBoolean), evented("CodecName", typeString), evented("Metatext", typeString),
		},
	},
	{
		name: "Time", path: "time",
		actions: []action{
			{"Time", []argument{out("TrackCount", "TrackCount"), out("Duration", "Duration"), out("Seconds", "Seconds")}},
		},
		variables: []variable{evented("TrackCount", typeUI4), evented("Duration", typeUI4), evented("Seconds", typeUI4)},
	},
}

// servicesByPath are the services by the path segment of their URLs.
var servicesByPath = func() map[string]*service {
	m := make(map[string]*service, len(services))
	for _, s := range services {
		m[s.path] = s
	}
	return m
}()

// scpd is the service's description (SCPD) as the surface serves it,
// generated from its tables: UPnP's service-1-0 schema, version 1.0, each
// action with its argument list (none for an action without arguments, as
// OpenHome's files write it), then the state table.
func (s *service) scpd() []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<scpd xmlns="urn:schemas-upnp-org:service-1-0">` + "\n")
	b.WriteString("  <specVersion><major>1</major><minor>0</minor></specVersion>\n")
	b.WriteString("  <actionList>\n")
	for _, a := range s.actions {
		b.WriteString("    <action><name>" + escape(a.name) + "</name>")
		if len(a.args) > 0 {
			b.WriteString("<argumentList>")
			for _, g := range a.args {
				dir := "out"
				if g.in {
					dir = "in"
				}
				b.WriteString("<argument><name>" + escape(g.name) + "</name><direction>" + dir +
					"</direction><relatedStateVariable>" + escape(g.related) + "</relatedStateVariable></argument>")
			}
			b.WriteString("</argumentList>")
		}
		b.WriteString("</action>\n")
	}
	b.WriteString("  </actionList>\n")
	b.WriteString("  <serviceStateTable>\n")
	for _, v := range s.variables {
		events := "no"
		if v.sendEvents {
			events = "yes"
		}
		b.WriteString(`    <stateVariable sendEvents="` + events + `"><name>` + escape(v.name) + "</name><dataType>" + v.dataType + "</dataType>")
		if len(v.allowed) > 0 {
			b.WriteString("<allowedValueList>")
			for _, a := range v.allowed {
				b.WriteString("<allowedValue>" + escape(a) + "</allowedValue>")
			}
			b.WriteString("</allowedValueList>")
		}
		b.WriteString("</stateVariable>\n")
	}
	b.WriteString("  </serviceStateTable>\n")
	b.WriteString("</scpd>\n")
	return []byte(b.String())
}

// escape is s as XML character data, through the surface's one escaper
// (escapeTo).
func escape(s string) string {
	var b strings.Builder
	escapeTo(&b, []byte(s))
	return b.String()
}

// escapeTo writes b to w as XML character data: the five special
// characters, and tab, newline and carriage return, as references; each
// byte that is not UTF-8, and each character XML 1.0 cannot carry (a
// control other than those three, a surrogate, U+FFFE, U+FFFF), as U+FFFD;
// the rest as it is (xml.EscapeText). It is the surface's one escaper:
// every string in a description, a fault and a response goes through it,
// once at each level of a value that is XML itself (B470 PR 5a3), so a value
// that is XML 1.0 text reads back byte for byte, and one that is not still
// makes a well-formed reply.
func escapeTo(w io.Writer, b []byte) { _ = xml.EscapeText(w, b) }
