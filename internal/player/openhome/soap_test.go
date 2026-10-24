package openhome

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

// arrival is the address and port the handler's test requests arrive on.
var arrival = netip.MustParseAddrPort("192.168.1.20:7792")

// testHandler is a handler with production's limits and no engine: every
// read is 501.
func testHandler() *handler {
	return newHandler(Device{UUID: testUUID, ModelNumber: "test"}, nil, DefaultLimits.orDefaults(), nil)
}

// knownFault reports whether code and description are a fault this layer
// defines: a UPnP code with its description, or one of the reads' OpenHome
// codes with its own.
func knownFault(code int, description string) bool {
	if d, ok := faultDescription[code]; ok && d == description {
		return true
	}
	for _, f := range []faultReply{faultIDNotFound, faultSourceIndex} {
		if f.code == code && f.description == description {
			return true
		}
	}
	return false
}

// request is a request to path that arrived on arrival, its Host that
// address and port, carrying body.
func request(method, path string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, "http://"+arrival.String()+path, body)
	r.Host = arrival.String()
	return r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.TCPAddrFromAddrPort(arrival)))
}

// soapRequest is a control request for service s's action, with the
// SOAPAction header, the content type, and the envelope env.
func soapRequest(s *service, soapAction, contentType, env string) *http.Request {
	r := request(http.MethodPost, "/openhome/"+s.path+"/control", strings.NewReader(env))
	if soapAction != "" {
		r.Header.Set("SOAPAction", soapAction)
	}
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	return r
}

// envelope wraps body, the Body's content, in a SOAP 1.1 envelope.
func envelope(body string) string {
	return `<?xml version="1.0" encoding="utf-8"?>` + "\n" +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body>` + body + `</s:Body></s:Envelope>`
}

// call is an action element of s, its arguments args (each "<Name>v</Name>"
// already).
func call(s *service, name, args string) string {
	return `<u:` + name + ` xmlns:u="` + s.serviceType() + `">` + args + `</u:` + name + `>`
}

// header is the quoted SOAPAction of s's action.
func header(s *service, name string) string { return `"` + s.serviceType() + "#" + name + `"` }

// validArgs are arguments of each type that parse.
func validArgs(s *service, a *action) string {
	var b strings.Builder
	for _, g := range a.ins() {
		v := "0"
		switch s.variable(g.related).dataType {
		case typeBoolean:
			v = "1"
		case typeString:
			v = "x"
		}
		b.WriteString("<" + g.name + ">" + v + "</" + g.name + ">")
	}
	return b.String()
}

// soapFaultDoc is a SOAP fault, read as a controller reads it.
type soapFaultDoc struct {
	XMLName xml.Name `xml:"http://schemas.xmlsoap.org/soap/envelope/ Envelope"`
	Fault   struct {
		Code   string `xml:"faultcode"`
		String string `xml:"faultstring"`
		Detail *struct {
			Code        int    `xml:"UPnPError>errorCode"`
			Description string `xml:"UPnPError>errorDescription"`
		} `xml:"detail"`
	} `xml:"Body>Fault"`
}

// answer is a reply as the table names it: an HTTP status, and for a fault
// its UPnP code (0 for MustUnderstand).
type answer struct {
	status int
	code   int
}

func (a answer) String() string {
	if a.status == http.StatusInternalServerError {
		if a.code == 0 {
			return "fault MustUnderstand"
		}
		return fmt.Sprintf("fault %d", a.code)
	}
	return fmt.Sprint(a.status)
}

// serve runs r through h, checks what every reply carries (the EXT header,
// no CORS), and reads the answer.
func serve(t *testing.T, h *handler, r *http.Request) answer {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	res := rec.Result()
	if v, ok := res.Header["EXT"]; !ok || len(v) != 1 || v[0] != "" {
		t.Errorf("%s %s: the reply's EXT header is %q (%v), want one, empty", r.Method, r.URL.Path, v, ok)
	}
	for k := range res.Header {
		if strings.HasPrefix(k, "Access-Control-") {
			t.Errorf("%s %s: the reply carries %s", r.Method, r.URL.Path, k)
		}
	}
	a := answer{status: res.StatusCode}
	if res.StatusCode != http.StatusInternalServerError {
		return a
	}
	body, _ := io.ReadAll(res.Body)
	var f soapFaultDoc
	if err := xml.Unmarshal(body, &f); err != nil {
		t.Fatalf("a 500 that is no SOAP fault: %v\n%s", err, body)
	}
	if ct := res.Header.Get("Content-Type"); ct != `text/xml; charset="utf-8"` {
		t.Errorf("a fault's Content-Type is %q", ct)
	}
	switch f.Fault.Code {
	case "s:MustUnderstand":
		if f.Fault.Detail != nil {
			t.Errorf("a MustUnderstand fault carries a detail, which SOAP 1.1 keeps for the Body")
		}
		return a
	case "s:Client":
		if f.Fault.String != "UPnPError" || f.Fault.Detail == nil {
			t.Fatalf("a UPnP fault without its UPnPError: %s", body)
		}
		a.code = f.Fault.Detail.Code
		if !knownFault(a.code, f.Fault.Detail.Description) {
			t.Errorf("fault %d with the description %q, which this layer does not define", a.code, f.Fault.Detail.Description)
		}
		return a
	}
	t.Fatalf("a fault with the code %q: %s", f.Fault.Code, body)
	return a
}

var (
	product  = servicesByPath["product"]
	playlist = servicesByPath["playlist"]
	info     = servicesByPath["info"]
	timeSvc  = servicesByPath["time"]
)

// countingReader counts what is read of a body.
type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

// failingReader is a body whose read fails.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("the connection went away") }

// writes are the write actions, by service: every action of the four
// services that changes state, which this release answers fault 602 (B470
// PR 5a4 implements them); every other action is a read (reads).
var writes = map[string][]string{
	"Product": {"SetStandby", "SetSourceIndex", "SetSourceIndexByName"},
	"Playlist": {"Play", "Pause", "Stop", "Next", "Previous", "SetRepeat", "SetShuffle", "SeekSecondAbsolute",
		"SeekSecondRelative", "SeekId", "SeekIndex", "Insert", "DeleteId", "DeleteAll"},
}

// TestEveryValidActionIsAnswered602 pins the writes, which PR 5a3 leaves
// unimplemented: every write action of the four services, with arguments
// of each type that parse (none for those that take none), is answered
// fault 602 and nothing else, with no engine and with a running one that
// holds a queue and a session that plays, and nothing reaches the engine:
// its generation, queue, token, selection and transport are as they were.
// (5a2a asked it of every action, before the reads.)
func TestEveryValidActionIsAnswered602(t *testing.T) {
	answersEvery := func(t *testing.T, h *handler) {
		t.Helper()
		n := 0
		for svc, names := range writes {
			s := servicesByPath[strings.ToLower(svc)]
			for _, name := range names {
				a := s.action(name)
				got := serve(t, h, soapRequest(s, header(s, name), "text/xml; charset=\"utf-8\"", envelope(call(s, name, validArgs(s, a)))))
				if got != (answer{http.StatusInternalServerError, 602}) {
					t.Errorf("%s#%s: %v, want fault 602", s.name, name, got)
				}
				n++
			}
		}
		if n != 3+14 {
			t.Errorf("%d writes answered, want the 17 of the four services", n)
		}
	}
	t.Run("no engine", func(t *testing.T) { answersEvery(t, testHandler()) })
	t.Run("an engine that plays", func(t *testing.T) {
		p := newTestPlayer(t)
		p.add("a", []byte("meta:a"))
		p.add("b", []byte("meta:b"))
		must(t, "play", p.eng.Play(context.Background()))
		s := p.be.session(t, 1)
		s.hear(s.first.ID, nil)
		before := p.heardNow(s.first.ID)
		answersEvery(t, p.handler(""))
		after := p.eng.Snapshot()
		if after.Generation() != before.Generation() || !slices.Equal(after.IDs(), before.IDs()) || after.Token() != before.Token() ||
			after.Selected() != before.Selected() || after.State() != before.State() || after.Repeat() != before.Repeat() {
			t.Errorf("a write reached the engine: generation %s, IDs %v, token %d, selected %d, %s, repeat %v; was %s, %v, %d, %d, %s, %v",
				after.Generation(), after.IDs(), after.Token(), after.Selected(), after.State(), after.Repeat(),
				before.Generation(), before.IDs(), before.Token(), before.Selected(), before.State(), before.Repeat())
		}
	})
}

// TestTheSOAPTableRowByRow: each row of the table in soap.go, in its order,
// with its outcome; the request that passes every row is 602.
func TestTheSOAPTableRowByRow(t *testing.T) {
	const ct = `text/xml; charset="utf-8"`
	play := envelope(call(playlist, "Play", ""))
	insert := func(args string) string { return envelope(call(playlist, "Insert", args)) }
	okInsert := "<AfterId>0</AfterId><Uri>http://192.168.1.5:7790/dlna/file/0123456789abcdef</Uri><Metadata>&lt;DIDL-Lite/&gt;</Metadata>"
	seekID := func(v string) string { return envelope(call(playlist, "SeekId", "<Value>"+v+"</Value>")) }
	setRepeat := func(v string) string { return envelope(call(playlist, "SetRepeat", "<Value>"+v+"</Value>")) }
	seekRel := func(v string) string { return envelope(call(playlist, "SeekSecondRelative", "<Value>"+v+"</Value>")) }
	fault := func(code int) answer { return answer{http.StatusInternalServerError, code} }
	status := func(code int) answer { return answer{status: code} }
	mustUnderstand := answer{http.StatusInternalServerError, 0}
	withHeader := func(entries, body string) string {
		return `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Header>` + entries + `</s:Header><s:Body>` + body + `</s:Body></s:Envelope>`
	}
	type row struct {
		name   string
		req    func() *http.Request
		answer answer
	}
	soap := func(action, contentType, env string) func() *http.Request {
		return func() *http.Request { return soapRequest(playlist, action, contentType, env) }
	}
	edit := func(base func() *http.Request, change func(*http.Request)) func() *http.Request {
		return func() *http.Request {
			r := base()
			change(r)
			return r
		}
	}
	valid := soap(header(playlist, "Play"), ct, play)
	rows := []row{
		{"the request that passes every row", valid, fault(602)},

		// Row 1: the Host names exactly the address and port it arrived on.
		{"1: Host without its port", edit(valid, func(r *http.Request) { r.Host = "192.168.1.20" }), status(400)},
		{"1: Host with another port", edit(valid, func(r *http.Request) { r.Host = "192.168.1.20:80" }), status(400)},
		{"1: Host naming another address of the box", edit(valid, func(r *http.Request) { r.Host = "10.0.0.2:7792" }), status(400)},
		{"1: Host by name", edit(valid, func(r *http.Request) { r.Host = "bridge.local:7792" }), status(400)},
		{"1: Host as a mapped address", edit(valid, func(r *http.Request) { r.Host = "[::ffff:192.168.1.20]:7792" }), status(400)},
		{"1: Host with leading zeros", edit(valid, func(r *http.Request) { r.Host = "192.168.001.020:7792" }), status(400)},
		{"1: no Host", edit(valid, func(r *http.Request) { r.Host = "" }), status(400)},
		{"1: a foreign Host on a path that is no route", func() *http.Request {
			r := request(http.MethodGet, "/v1/health", nil)
			r.Host = "evil.example:7792"
			return r
		}, status(400)},

		// Row 2: POST only.
		{"2: GET", edit(valid, func(r *http.Request) { r.Method = http.MethodGet }), status(405)},
		{"2: PUT", edit(valid, func(r *http.Request) { r.Method = http.MethodPut }), status(405)},
		{"2: OPTIONS, a preflight", edit(valid, func(r *http.Request) {
			r.Method = http.MethodOptions
			r.Header.Set("Origin", "http://evil.example")
		}), status(405)},

		// Row 3: any Origin, before the body is read (TestAnOriginIsRefusedBeforeTheBodyIsRead).
		{"3: an Origin", edit(valid, func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }), status(403)},
		{"3: an empty Origin", edit(valid, func(r *http.Request) { r.Header["Origin"] = []string{""} }), status(403)},
		{"3: Origin null", edit(valid, func(r *http.Request) { r.Header.Set("Origin", "null") }), status(403)},

		// Row 4: text/xml, charset utf-8 or none.
		{"4: no Content-Type", soap(header(playlist, "Play"), "", play), status(415)},
		{"4: application/soap+xml", soap(header(playlist, "Play"), "application/soap+xml; charset=utf-8", play), status(415)},
		{"4: another charset", soap(header(playlist, "Play"), "text/xml; charset=iso-8859-1", play), status(415)},
		{"4: a Content-Type that does not parse", soap(header(playlist, "Play"), "text/xml; charset=", play), status(415)},
		{"4: two Content-Types", edit(valid, func(r *http.Request) { r.Header.Add("Content-Type", "text/xml") }), status(415)},
		{"4: text/xml alone", soap(header(playlist, "Play"), "text/xml", play), fault(602)},
		{"4: utf-8 in capitals, unquoted", soap(header(playlist, "Play"), "TEXT/XML; CHARSET=UTF-8", play), fault(602)},
		{"4: Utf-8 quoted", soap(header(playlist, "Play"), `text/xml;charset="Utf-8"`, play), fault(602)},

		// Row 5: one SOAPAction naming this URL's service and one of its actions.
		{"5: no SOAPAction", soap("", ct, play), fault(401)},
		{"5: unquoted", soap(playlist.serviceType()+"#Play", ct, play), fault(602)},
		{"5: a quote on one side", soap(`"`+playlist.serviceType()+"#Play", ct, play), fault(401)},
		{"5: another service's type", soap(header(product, "Standby"), ct, play), fault(401)},
		{"5: another service's type with this service's action", soap(`"`+product.serviceType()+`#Play"`, ct, play), fault(401)},
		{"5: another version", soap(`"urn:av-openhome-org:service:Playlist:2#Play"`, ct, play), fault(401)},
		{"5: an action the service has not", soap(header(playlist, "Volume"), ct, play), fault(401)},
		{"5: an action's name in another case", soap(header(playlist, "play"), ct, play), fault(401)},
		{"5: no action", soap(`"`+playlist.serviceType()+`#"`, ct, play), fault(401)},
		{"5: no type", soap(`"#Play"`, ct, play), fault(401)},
		{"5: two #", soap(`"`+playlist.serviceType()+`#Play#Play"`, ct, play), fault(401)},
		{"5: two SOAPActions", edit(valid, func(r *http.Request) { r.Header.Add("SOAPAction", header(playlist, "Play")) }), fault(401)},

		// Row 6: the body, bounded.
		{"6: a body past 256 KiB", soap(header(playlist, "Play"), ct, play+strings.Repeat(" ", maxBody)), status(413)},
		{"6: a body whose read fails", func() *http.Request {
			r := soapRequest(playlist, header(playlist, "Play"), ct, "")
			r.Body = io.NopCloser(failingReader{})
			return r
		}, status(400)},

		// Row 7: the XML.
		{"7: a DOCTYPE", soap(header(playlist, "Play"), ct, `<?xml version="1.0"?><!DOCTYPE s:Envelope [<!ENTITY x "y">]>`+strings.TrimPrefix(play, `<?xml version="1.0" encoding="utf-8"?>`)), status(400)},
		{"7: an ENTITY after the declaration", soap(header(playlist, "Play"), ct, `<?xml version="1.0"?><!ENTITY x "y">`+strings.TrimPrefix(play, `<?xml version="1.0" encoding="utf-8"?>`)), status(400)},
		{"7: an entity it does not know", soap(header(playlist, "Insert"), ct, insert("<AfterId>0</AfterId><Uri>&x;</Uri><Metadata></Metadata>")), status(400)},
		{"7: a processing instruction beside the declaration", soap(header(playlist, "Play"), ct, strings.Replace(play, "?>", "?><?note x?>", 1)), fault(602)},
		{"7: the declaration not at the start", soap(header(playlist, "Play"), ct, " "+play), status(400)},
		{"7: another encoding declared", soap(header(playlist, "Play"), ct, strings.Replace(play, "utf-8", "iso-8859-1", 1)), status(400)},
		{"7: bytes that are not UTF-8", soap(header(playlist, "Insert"), ct, insert("<AfterId>0</AfterId><Uri>\xff</Uri><Metadata></Metadata>")), status(400)},
		{"7: it does not parse", soap(header(playlist, "Play"), ct, play[:len(play)-5]), status(400)},
		{"7: an element after the envelope", soap(header(playlist, "Play"), ct, play+"<x/>"), status(400)},
		{"7: a comment after the envelope", soap(header(playlist, "Play"), ct, play+"<!-- x -->"), status(400)},
		{"7: text after the envelope", soap(header(playlist, "Play"), ct, play+"x"), status(400)},
		{"7: white space after the envelope", soap(header(playlist, "Play"), ct, play+"\r\n\t "), fault(602)},
		{"7: text before the envelope", soap(header(playlist, "Play"), ct, "x"+strings.TrimPrefix(play, `<?xml version="1.0" encoding="utf-8"?>`)), status(400)},
		{"7: no declaration", soap(header(playlist, "Play"), ct, strings.TrimPrefix(play, `<?xml version="1.0" encoding="utf-8"?>`)), fault(602)},
		{"7: an empty body", soap(header(playlist, "Play"), ct, ""), status(400)},
		{"7: elements nested past the depth", soap(header(playlist, "Play"), ct, envelope(call(playlist, "Play", strings.Repeat("<a>", 6)+strings.Repeat("</a>", 6)))), status(400)},
		{"7: more elements than it takes", soap(header(playlist, "Play"), ct, withHeader(strings.Repeat("<h/>", maxElements), call(playlist, "Play", ""))), status(400)},

		// Row 8: the envelope.
		{"8: SOAP 1.2's envelope", soap(header(playlist, "Play"), ct, strings.Replace(play, "http://schemas.xmlsoap.org/soap/envelope/", "http://www.w3.org/2003/05/soap-envelope", 1)), fault(401)},
		{"8: another namespace's Envelope around a SOAP 1.1 Body", soap(header(playlist, "Play"), ct,
			`<e:Envelope xmlns:e="http://www.w3.org/2003/05/soap-envelope" xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+
				call(playlist, "Play", "")+`</s:Body></e:Envelope>`), fault(401)},
		{"8: another prefix", soap(header(playlist, "Play"), ct, `<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://schemas.xmlsoap.org/soap/envelope/"><SOAP-ENV:Body>`+call(playlist, "Play", "")+`</SOAP-ENV:Body></SOAP-ENV:Envelope>`), fault(602)},
		{"8: no prefix at all", soap(header(playlist, "Play"), ct, `<Envelope xmlns="http://schemas.xmlsoap.org/soap/envelope/"><Body><Play xmlns="`+playlist.serviceType()+`"/></Body></Envelope>`), fault(602)},
		{"8: no Body", soap(header(playlist, "Play"), ct, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"></s:Envelope>`), fault(401)},
		{"8: two Bodies", soap(header(playlist, "Play"), ct, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+call(playlist, "Play", "")+`</s:Body><s:Body/></s:Envelope>`), fault(401)},
		{"8: a Header after the Body", soap(header(playlist, "Play"), ct, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+call(playlist, "Play", "")+`</s:Body><s:Header/></s:Envelope>`), fault(401)},
		{"8: text in the Envelope", soap(header(playlist, "Play"), ct, `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/">x<s:Body>`+call(playlist, "Play", "")+`</s:Body></s:Envelope>`), fault(401)},
		{"8: two actions", soap(header(playlist, "Play"), ct, envelope(call(playlist, "Play", "")+call(playlist, "Play", ""))), fault(401)},
		{"8: no action", soap(header(playlist, "Play"), ct, envelope("")), fault(401)},
		{"8: text beside the action", soap(header(playlist, "Play"), ct, envelope("x"+call(playlist, "Play", ""))), fault(401)},
		{"8: an action other than SOAPAction's", soap(header(playlist, "Pause"), ct, play), fault(401)},
		{"8: the action in another namespace", soap(header(playlist, "Play"), ct, envelope(`<u:Play xmlns:u="urn:av-openhome-org:service:Product:1"/>`)), fault(401)},
		{"8: the action in no namespace", soap(header(playlist, "Play"), ct, envelope(`<Play/>`)), fault(401)},
		{"8: a prefix bound to nothing", soap(header(playlist, "Play"), ct, envelope(`<u:Play/>`)), fault(401)},
		{"8: a Header entry it must understand", soap(header(playlist, "Play"), ct, withHeader(`<t:Tx xmlns:t="urn:x" s:mustUnderstand="1">5</t:Tx>`, call(playlist, "Play", ""))), mustUnderstand},
		{"8: mustUnderstand with no namespace", soap(header(playlist, "Play"), ct, withHeader(`<t:Tx xmlns:t="urn:x" mustUnderstand="1"/>`, call(playlist, "Play", ""))), mustUnderstand},
		{"8: a Header entry it may ignore", soap(header(playlist, "Play"), ct, withHeader(`<t:Tx xmlns:t="urn:x" s:mustUnderstand="0">5</t:Tx><t:Ty xmlns:t="urn:x"/>`, call(playlist, "Play", ""))), fault(602)},
		{"8: an empty Header", soap(header(playlist, "Play"), ct, withHeader("", call(playlist, "Play", ""))), fault(602)},
		{"8: a Header past its entries", soap(header(playlist, "Play"), ct, withHeader(strings.Repeat(`<t:Tx xmlns:t="urn:x"/>`, maxHeaderEntries+1), call(playlist, "Play", ""))), status(400)},
		{"8: a Header at its entries", soap(header(playlist, "Play"), ct, withHeader(strings.Repeat(`<t:Tx xmlns:t="urn:x"/>`, maxHeaderEntries), call(playlist, "Play", ""))), fault(602)},
		{"8: a Header past its bytes", soap(header(playlist, "Play"), ct, withHeader(`<t:Tx xmlns:t="urn:x">`+strings.Repeat("a", maxHeaderBytes)+`</t:Tx>`, call(playlist, "Play", ""))), status(400)},

		// Row 9: the arguments.
		{"9: Insert as it should be", soap(header(playlist, "Insert"), ct, insert(okInsert)), fault(602)},
		{"9: an argument missing", soap(header(playlist, "Insert"), ct, insert("<AfterId>0</AfterId><Uri>u</Uri>")), fault(402)},
		{"9: an argument repeated", soap(header(playlist, "Insert"), ct, insert("<AfterId>0</AfterId><AfterId>0</AfterId><Uri>u</Uri><Metadata></Metadata>")), fault(402)},
		{"9: an argument it does not know", soap(header(playlist, "Insert"), ct, insert(okInsert+"<Extra>1</Extra>")), fault(402)},
		{"9: arguments out of order", soap(header(playlist, "Insert"), ct, insert("<Uri>u</Uri><AfterId>0</AfterId><Metadata></Metadata>")), fault(402)},
		{"9: an argument that holds an element", soap(header(playlist, "Insert"), ct, insert("<AfterId>0</AfterId><Uri><b>u</b></Uri><Metadata></Metadata>")), fault(402)},
		{"9: an out-argument sent", soap(header(playlist, "Insert"), ct, insert(okInsert+"<NewId>1</NewId>")), fault(402)},
		{"9: an argument in another namespace", soap(header(playlist, "Insert"), ct, insert(`<x:AfterId xmlns:x="urn:x">0</x:AfterId><Uri>u</Uri><Metadata></Metadata>`)), fault(402)},
		{"9: arguments in the action's namespace", soap(header(playlist, "Insert"), ct, insert(`<u:AfterId>0</u:AfterId><u:Uri>u</u:Uri><u:Metadata></u:Metadata>`)), fault(602)},
		{"9: arguments inheriting a default namespace", soap(header(playlist, "Insert"), ct, envelope(`<Insert xmlns="`+playlist.serviceType()+`"><AfterId>0</AfterId><Uri>u</Uri><Metadata></Metadata></Insert>`)), fault(602)},
		{"9: text beside the arguments", soap(header(playlist, "Insert"), ct, insert("x"+okInsert)), fault(402)},
		{"9: an argument to an action that takes none", soap(header(playlist, "Play"), ct, envelope(call(playlist, "Play", "<Value>1</Value>"))), fault(402)},
		{"9: an argument to Info's Counters", func() *http.Request {
			return soapRequest(info, header(info, "Counters"), ct, envelope(call(info, "Counters", "<TrackCount>1</TrackCount>")))
		}, fault(402)},
		{"9: Time's Time, a read with no engine", func() *http.Request {
			return soapRequest(timeSvc, header(timeSvc, "Time"), ct, envelope(call(timeSvc, "Time", "")))
		}, fault(501)},
		{"9: ui4 at its top", soap(header(playlist, "SeekId"), ct, seekID("4294967295")), fault(602)},
		{"9: ui4 past its top", soap(header(playlist, "SeekId"), ct, seekID("4294967296")), fault(402)},
		{"9: ui4 with a minus", soap(header(playlist, "SeekId"), ct, seekID("-1")), fault(402)},
		{"9: ui4 with a plus", soap(header(playlist, "SeekId"), ct, seekID("+1")), fault(402)},
		{"9: ui4 in hexadecimal", soap(header(playlist, "SeekId"), ct, seekID("0x10")), fault(402)},
		{"9: ui4 with white space", soap(header(playlist, "SeekId"), ct, seekID(" 1")), fault(402)},
		{"9: ui4 empty", soap(header(playlist, "SeekId"), ct, seekID("")), fault(402)},
		{"9: ui4 with leading zeros", soap(header(playlist, "SeekId"), ct, seekID("007")), fault(602)},
		{"9: i4 at its bottom", soap(header(playlist, "SeekSecondRelative"), ct, seekRel("-2147483648")), fault(602)},
		{"9: i4 past its top", soap(header(playlist, "SeekSecondRelative"), ct, seekRel("2147483648")), fault(402)},
		{"9: i4 with a plus", soap(header(playlist, "SeekSecondRelative"), ct, seekRel("+5")), fault(602)},
		{"9: i4 not a number", soap(header(playlist, "SeekSecondRelative"), ct, seekRel("5s")), fault(402)},
		{"9: boolean yes in capitals", soap(header(playlist, "SetRepeat"), ct, setRepeat("YES")), fault(602)},
		{"9: boolean False", soap(header(playlist, "SetRepeat"), ct, setRepeat("False")), fault(602)},
		{"9: boolean 0", soap(header(playlist, "SetRepeat"), ct, setRepeat("0")), fault(602)},
		{"9: boolean 2", soap(header(playlist, "SetRepeat"), ct, setRepeat("2")), fault(402)},
		{"9: boolean with white space", soap(header(playlist, "SetRepeat"), ct, setRepeat("1 ")), fault(402)},
		{"9: boolean on", soap(header(playlist, "SetRepeat"), ct, setRepeat("on")), fault(402)},
		{"9: a string escaped", func() *http.Request {
			return soapRequest(product, header(product, "SetSourceIndexByName"), ct, envelope(call(product, "SetSourceIndexByName", "<Value>Play &amp; list</Value>")))
		}, fault(602)},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			if got := serve(t, testHandler(), tc.req()); got != tc.answer {
				t.Errorf("%v, want %v", got, tc.answer)
			}
		})
	}
}

// TestTheBoundsArePinnedWhereTheTableSaysTheyAre: the text bound is 64 KiB
// and 8 KiB for each of an action's in-arguments (Insert's three: 88 KiB),
// and a request whose text reaches it exactly passes, a byte past it is
// 400; processing instructions beside the declaration are ignored up to
// four, a fifth is 400; elements nest eight deep at most.
func TestTheBoundsArePinnedWhereTheTableSaysTheyAre(t *testing.T) {
	ins := playlist.action("Insert")
	if textLimit(ins) != 64<<10+3*(8<<10) || textLimit(playlist.action("Play")) != 64<<10 || maxDepth != 8 || maxProcInsts != 4 {
		t.Fatalf("the bounds moved: Insert's text %d, Play's %d, depth %d, processing instructions %d",
			textLimit(ins), textLimit(playlist.action("Play")), maxDepth, maxProcInsts)
	}
	const ct = "text/xml"
	h := testHandler()
	// Insert's envelope with its metadata padded so the request's text is n
	// bytes: the metadata is the only text but its other arguments' ("0",
	// "u"), and the envelope holds no white space.
	insertWithText := func(n int) *http.Request {
		meta := strings.Repeat("m", n-2)
		env := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
			call(playlist, "Insert", "<AfterId>0</AfterId><Uri>u</Uri><Metadata>"+meta+"</Metadata>") + `</s:Body></s:Envelope>`
		return soapRequest(playlist, header(playlist, "Insert"), ct, env)
	}
	if got := serve(t, h, insertWithText(textLimit(ins))); got != (answer{500, 602}) {
		t.Errorf("text at the bound: %v, want fault 602", got)
	}
	if got := serve(t, h, insertWithText(textLimit(ins)+1)); got != (answer{status: 400}) {
		t.Errorf("text a byte past the bound: %v, want 400", got)
	}
	pis := func(n int) *http.Request {
		return soapRequest(playlist, header(playlist, "Play"), ct, `<?xml version="1.0"?>`+strings.Repeat("<?note x?>", n)+
			`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+call(playlist, "Play", "")+`</s:Body></s:Envelope>`)
	}
	if got := serve(t, h, pis(maxProcInsts)); got != (answer{500, 602}) {
		t.Errorf("four processing instructions: %v, want fault 602", got)
	}
	if got := serve(t, h, pis(maxProcInsts+1)); got != (answer{status: 400}) {
		t.Errorf("five processing instructions: %v, want 400", got)
	}
	// Envelope, Body, the action and four levels inside an argument-less
	// action's element are eight; one more is nine.
	nested := func(levels int) *http.Request {
		return soapRequest(playlist, header(playlist, "Play"), ct, envelope(call(playlist, "Play", strings.Repeat("<a>", levels)+strings.Repeat("</a>", levels))))
	}
	if got := serve(t, h, nested(maxDepth-3)); got != (answer{500, 402}) {
		t.Errorf("eight deep: %v, want fault 402 (row 9 refuses the element, row 7 does not)", got)
	}
	if got := serve(t, h, nested(maxDepth-2)); got != (answer{status: 400}) {
		t.Errorf("nine deep: %v, want 400", got)
	}
}

// TestAnOriginIsRefusedBeforeTheBodyIsRead: row 3 reads nothing of the
// body: a request with any Origin is refused before it, and so is one the
// rows before the body refuse, and the connection is closed after the
// refusal.
func TestAnOriginIsRefusedBeforeTheBodyIsRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"an Origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, 403},
		{"no SOAPAction", func(r *http.Request) { r.Header.Del("SOAPAction") }, 500},
		{"a Content-Type it does not take", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"a GET", func(r *http.Request) { r.Method = http.MethodGet }, 405},
	} {
		body := &countingReader{r: strings.NewReader(envelope(call(playlist, "Play", "")))}
		r := soapRequest(playlist, header(playlist, "Play"), "text/xml", "")
		r.Body = io.NopCloser(body)
		tc.change(r)
		rec := httptest.NewRecorder()
		testHandler().ServeHTTP(rec, r)
		if rec.Code != tc.status || body.read != 0 || rec.Header().Get("Connection") != "close" {
			t.Errorf("%s: %d, %d bytes of the body read, Connection %q; want %d, none read, close", tc.name, rec.Code, body.read, rec.Header().Get("Connection"), tc.status)
		}
	}
}

// TestValueOfTypeIsUPnPs: each type's lexical forms, bin.base64 strictly
// (no argument of the four services takes one in, but the type is the
// table's).
func TestValueOfTypeIsUPnPs(t *testing.T) {
	for _, tc := range []struct {
		typ, text string
		ok        bool
	}{
		{typeUI4, "0", true}, {typeUI4, "4294967295", true}, {typeUI4, "4294967296", false}, {typeUI4, "-0", false}, {typeUI4, "1e3", false},
		{typeI4, "-2147483648", true}, {typeI4, "2147483647", true}, {typeI4, "-2147483649", false}, {typeI4, "", false},
		{typeBoolean, "true", true}, {typeBoolean, "No", true}, {typeBoolean, "y", false}, {typeBoolean, "", false},
		{typeBase64, "", true}, {typeBase64, "AAAAAQAAAAI=", true}, {typeBase64, "AAAAAQAAAAI", false},
		{typeBase64, "AAAAAQ\nAAAAI=", false}, {typeBase64, "AAAAAQAAAAJ=", false}, {typeBase64, "AA==AA==", false},
		{typeString, "", true}, {typeString, "anything at all", true},
		{"r8", "1.5", false},
	} {
		if got := valueOfType(tc.text, tc.typ); got != tc.ok {
			t.Errorf("valueOfType(%q, %s) = %v, want %v", tc.text, tc.typ, got, tc.ok)
		}
	}
}
