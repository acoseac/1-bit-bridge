package openhome

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// A control request is checked in this order, and each check has one
// outcome (the table is the rules' too: .claude/rules/openhome.md):
//
//	row  check                                                  refused with
//	1    admission at accept; then Host is exactly the address  closed unread; 400
//	     and port the request arrived on
//	2    the method is POST                                     405, Allow: POST
//	3    no Origin header at all (empty and "null" included),   403
//	     before the body is read
//	4    Content-Type is text/xml, charset utf-8 (any case,      415
//	     quoted or not) or none, parsed by mime.ParseMediaType
//	5    one SOAPAction, quoted or not, <type>#<action>, naming  fault 401
//	     this URL's service and one of its actions
//	6    the body, through http.MaxBytesReader at maxBody, read  413; 408 at its
//	     to EOF under its own deadline (Limits.BodyRead)         deadline; 400
//	--   a concurrency slot, taken only now                     503 at once
//	7    the XML: no directive (DOCTYPE, ENTITY); the           400
//	     declaration first or not at all; other processing
//	     instructions ignored, at most maxProcInsts; at most
//	     maxElements elements, maxDepth deep; text at most
//	     textLimit; nothing after the envelope but white space
//	8    one Envelope in SOAP 1.1's namespace, whatever its      fault 401;
//	     prefix; an optional Header, at most maxHeaderEntries    MustUnderstand
//	     entries and maxHeaderBytes, its entries ignored unless  fault; 400 past
//	     one carries mustUnderstand="1"; one Body holding one    the Header's
//	     action element whose name and namespace are             bounds
//	     SOAPAction's
//	9    the action's in-arguments, in the SCPD's order, each    fault 402
//	     with no namespace or the action's own, none missing,
//	     repeated, unknown or nested, each parsing as its type;
//	     for every action, those with none included
//	then every request that passes is answered fault 602 (Optional Action
//	Not Implemented): nothing is read from or done to the engine.
//
// A fault is a UPnP SOAP fault: HTTP 500, faultcode s:Client, faultstring
// UPnPError and the code and its description in UPnPError (UPnP Device
// Architecture 1.1, §3.2.2), or faultcode s:MustUnderstand with no detail,
// which SOAP 1.1 (§4.4) keeps for the Body. Every reply carries an empty
// EXT header.
const (
	// maxBody bounds a control request's body.
	maxBody = 256 << 10
	// maxDepth bounds the element nesting: Envelope, Body, the action and
	// an argument are four.
	maxDepth = 8
	// maxElements bounds the elements of one request: a request of the
	// action with the most arguments (Insert) is seven.
	maxElements = 128
	// maxProcInsts bounds the processing instructions ignored, the
	// declaration aside.
	maxProcInsts = 4
	// maxHeaderEntries and maxHeaderBytes bound a SOAP Header.
	maxHeaderEntries = 8
	maxHeaderBytes   = 8 << 10
	// textBase and textPerArgument make an action's text bound: every
	// argument's text and the white space between the elements, decoded.
	// The base holds the 64 KiB of metadata an entry may carry (the
	// engine's bound), escaped or not.
	textBase        = 64 << 10
	textPerArgument = 8 << 10
)

// The namespaces of SOAP 1.1's envelope and encoding, and of UPnP's fault
// detail.
const (
	soapEnvelopeNS = "http://schemas.xmlsoap.org/soap/envelope/"
	soapEncodingNS = "http://schemas.xmlsoap.org/soap/encoding/"
	upnpControlNS  = "urn:schemas-upnp-org:control-1-0"
)

// The UPnP error codes this layer answers, with their descriptions, from
// UPnP Device Architecture 1.1, §3.2.2 (the action error codes): the same
// three in UDA 1.0. No other code is defined here; the PRs that implement
// the actions (5a3, 5a4) add theirs, each with its source.
const (
	errInvalidAction     = 401
	errInvalidArgs       = 402
	errActionUnavailable = 602
)

// faultDescription is a code's description.
var faultDescription = map[int]string{
	errInvalidAction:     "Invalid Action",
	errInvalidArgs:       "Invalid Args",
	errActionUnavailable: "Optional Action Not Implemented",
}

// textLimit is a's text bound: textBase and textPerArgument for each of its
// in-arguments.
func textLimit(a *action) int { return textBase + textPerArgument*len(a.ins()) }

// mediaTypeIsXMLUTF8 is row 4: one Content-Type, text/xml with a charset of
// utf-8 in any case, quoted or not, or none.
func mediaTypeIsXMLUTF8(h http.Header) bool {
	vs := h.Values("Content-Type")
	if len(vs) != 1 {
		return false
	}
	mt, params, err := mime.ParseMediaType(vs[0])
	if err != nil || mt != "text/xml" {
		return false
	}
	if cs, ok := params["charset"]; ok && !strings.EqualFold(cs, "utf-8") {
		return false
	}
	return true
}

// soapAction is row 5: the service type and action of the one SOAPAction
// header, quoted or not; ok is false for none, more than one, an unbalanced
// quote, or no single '#' between a type and an action.
func soapAction(h http.Header) (serviceType, name string, ok bool) {
	vs := h.Values("SOAPAction")
	if len(vs) != 1 {
		return "", "", false
	}
	v := strings.Trim(vs[0], " \t")
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	if strings.ContainsRune(v, '"') {
		return "", "", false
	}
	i := strings.IndexByte(v, '#')
	if i <= 0 || i == len(v)-1 || strings.LastIndexByte(v, '#') != i {
		return "", "", false
	}
	return v[:i], v[i+1:], true
}

// xmlNode is one element of a request, as row 7 read it.
type xmlNode struct {
	name     xml.Name
	attrs    []xml.Attr
	text     []byte // the character data directly inside, white space included
	children []*xmlNode
	// start and end are the input offsets of its start tag and past its end
	// tag.
	start, end int64
}

// errXML is a row-7 refusal: the XML is no request.
type errXML string

func (e errXML) Error() string { return "the body is not a request: " + string(e) }

// parseRequestXML is row 7: the body read into its elements, with the
// bounds the table names.
func parseRequestXML(body []byte, maxText int) (*xmlNode, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.Strict = true
	var (
		root   *xmlNode
		stack  []*xmlNode
		closed bool
		first  = true
		elems  int
		pis    int
		text   int
	)
	for {
		at := d.InputOffset()
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errXML("it does not parse as XML")
		}
		switch t := tok.(type) {
		case xml.ProcInst:
			if strings.EqualFold(t.Target, "xml") {
				if !first || at != 0 {
					return nil, errXML("an XML declaration that is not at its start")
				}
				break
			}
			if closed {
				return nil, errXML("something after the envelope")
			}
			if pis++; pis > maxProcInsts {
				return nil, errXML("more processing instructions than it takes")
			}
		case xml.Directive:
			return nil, errXML("a directive (DOCTYPE, ENTITY and the like)")
		case xml.Comment:
			if closed {
				return nil, errXML("something after the envelope")
			}
		case xml.CharData:
			if text += len(t); text > maxText {
				return nil, errXML("more text than the action takes")
			}
			if len(stack) == 0 {
				if !isXMLSpace(t) {
					return nil, errXML("text outside the envelope")
				}
				break
			}
			top := stack[len(stack)-1]
			top.text = append(top.text, t...)
		case xml.StartElement:
			if closed {
				return nil, errXML("something after the envelope")
			}
			if elems++; elems > maxElements {
				return nil, errXML("more elements than it takes")
			}
			if len(stack) == maxDepth {
				return nil, errXML("elements nested deeper than it takes")
			}
			n := &xmlNode{name: t.Name, attrs: t.Copy().Attr, start: at}
			if len(stack) == 0 {
				root = n
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, n)
			}
			stack = append(stack, n)
		case xml.EndElement:
			n := stack[len(stack)-1]
			n.end = d.InputOffset()
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				closed = true
			}
		}
		first = false
	}
	if root == nil || !closed {
		return nil, errXML("no envelope")
	}
	return root, nil
}

// isXMLSpace reports whether b is XML's white space alone.
func isXMLSpace(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
		default:
			return false
		}
	}
	return true
}

// faultReply is a SOAP fault the handler answers: a UPnP code, or
// mustUnderstand.
type faultReply struct {
	code           int
	mustUnderstand bool
}

// envelopeReply is row 8's answer: the action element, or a fault, or (for
// a Header past its bounds) a 400.
type envelopeReply struct {
	action  *xmlNode
	fault   *faultReply
	tooBig  bool
	problem string
}

// checkEnvelope is row 8, for s's action a.
func checkEnvelope(root *xmlNode, s *service, a *action) envelopeReply {
	invalid := func(why string) envelopeReply {
		return envelopeReply{fault: &faultReply{code: errInvalidAction}, problem: why}
	}
	if root.name != (xml.Name{Space: soapEnvelopeNS, Local: "Envelope"}) {
		return invalid("no SOAP 1.1 Envelope")
	}
	if !isXMLSpace(root.text) {
		return invalid("text in the Envelope")
	}
	kids := root.children
	var header *xmlNode
	if len(kids) > 0 && kids[0].name == (xml.Name{Space: soapEnvelopeNS, Local: "Header"}) {
		header, kids = kids[0], kids[1:]
	}
	if len(kids) != 1 || kids[0].name != (xml.Name{Space: soapEnvelopeNS, Local: "Body"}) {
		return invalid("not one Body after an optional Header")
	}
	if header != nil {
		if len(header.children) > maxHeaderEntries || header.end-header.start > maxHeaderBytes {
			return envelopeReply{tooBig: true, problem: "a Header past its bounds"}
		}
		if !isXMLSpace(header.text) {
			return invalid("text in the Header")
		}
		for _, entry := range header.children {
			if mustUnderstand(entry) {
				return envelopeReply{fault: &faultReply{mustUnderstand: true}, problem: "a Header entry it must understand"}
			}
		}
	}
	body := kids[0]
	if !isXMLSpace(body.text) || len(body.children) != 1 {
		return invalid("not one action in the Body")
	}
	act := body.children[0]
	if act.name != (xml.Name{Space: s.serviceType(), Local: a.name}) {
		return invalid("an action other than SOAPAction's")
	}
	return envelopeReply{action: act}
}

// mustUnderstand reports whether a Header entry carries mustUnderstand="1":
// SOAP 1.1's attribute, or one with no namespace, which no header this
// layer knows carries either.
func mustUnderstand(entry *xmlNode) bool {
	for _, at := range entry.attrs {
		if at.Name.Local == "mustUnderstand" && (at.Name.Space == soapEnvelopeNS || at.Name.Space == "") && at.Value == "1" {
			return true
		}
	}
	return false
}

// checkArguments is row 9: the action element's children are a's
// in-arguments, in order, each with no namespace or the action's own, none
// nested, each parsing as its state variable's type.
func checkArguments(act *xmlNode, s *service, a *action) (string, bool) {
	if !isXMLSpace(act.text) {
		return "text beside the arguments", false
	}
	ins := a.ins()
	if len(act.children) != len(ins) {
		return "not the action's arguments", false
	}
	for i, g := range ins {
		c := act.children[i]
		if c.name.Local != g.name || (c.name.Space != "" && c.name.Space != s.serviceType()) {
			return "an argument missing, out of order, unknown or in another namespace", false
		}
		if len(c.children) > 0 {
			return "an argument that holds elements", false
		}
		v := s.variable(g.related)
		if v == nil || !valueOfType(string(c.text), v.dataType) {
			return "an argument that is not its type", false
		}
	}
	return "", true
}

// valueOfType reports whether text is a value of UPnP type t: ui4 and i4 in
// decimal and in range, ui4 with no sign; boolean as 0, 1, true, false,
// yes or no, in any case; bin.base64 strictly (padded, its trailing bits
// zero, no line breaks); string anything within the text bound. Nothing is
// trimmed.
func valueOfType(text, t string) bool {
	switch t {
	case typeUI4:
		_, err := strconv.ParseUint(text, 10, 32)
		return err == nil
	case typeI4:
		_, err := strconv.ParseInt(text, 10, 32)
		return err == nil
	case typeBoolean:
		switch strings.ToLower(text) {
		case "0", "1", "true", "false", "yes", "no":
			return true
		}
		return false
	case typeBase64:
		if strings.ContainsAny(text, "\r\n") {
			return false
		}
		_, err := base64.StdEncoding.Strict().DecodeString(text)
		return err == nil
	case typeString:
		return true
	}
	return false
}

// faultEnvelope is f's SOAP fault body.
func faultEnvelope(f faultReply) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<s:Envelope xmlns:s="` + soapEnvelopeNS + `" s:encodingStyle="` + soapEncodingNS + `">`)
	b.WriteString("<s:Body><s:Fault>")
	if f.mustUnderstand {
		b.WriteString("<faultcode>s:MustUnderstand</faultcode><faultstring>MustUnderstand</faultstring>")
	} else {
		b.WriteString("<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring>")
		b.WriteString(`<detail><UPnPError xmlns="` + upnpControlNS + `">`)
		b.WriteString("<errorCode>" + strconv.Itoa(f.code) + "</errorCode>")
		b.WriteString("<errorDescription>" + escape(faultDescription[f.code]) + "</errorDescription>")
		b.WriteString("</UPnPError></detail>")
	}
	b.WriteString("</s:Fault></s:Body></s:Envelope>\n")
	return []byte(b.String())
}
