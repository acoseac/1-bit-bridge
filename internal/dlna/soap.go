package dlna

import (
	"fmt"
	"strings"
)

// UPnP-canonical error codes per UDA 1.0 Annex A. Bridge currently uses
// a subset corresponding to the Browse / GetProtocolInfo actions we
// expose; new actions added later can add their own codes as needed.
const (
	// UPnPErrInvalidAction — caller invoked an action the service doesn't
	// implement. Returned for SOAPAction headers that don't match any
	// dispatch arm.
	UPnPErrInvalidAction = 401

	// UPnPErrInvalidArgs — caller's action arguments are malformed
	// (wrong type, missing required field, out-of-range value).
	UPnPErrInvalidArgs = 402

	// UPnPErrActionFailed — generic server-side failure (database
	// error, internal exception, etc.).
	UPnPErrActionFailed = 501

	// UPnPErrNoSuchObject — Browse / Search received an ObjectID that
	// doesn't exist in the ContentDirectory.
	UPnPErrNoSuchObject = 701
)

// upnpErrorDescription returns the spec-canonical human-readable error
// string for the given UPnP error code. Used in SOAPFault response
// `errorDescription` fields. Renderers don't typically display these to
// end users (the numeric code drives renderer-side behavior) but the
// string is required by the UPnP control protocol spec.
func upnpErrorDescription(code int) string {
	switch code {
	case UPnPErrInvalidAction:
		return "Invalid Action"
	case UPnPErrInvalidArgs:
		return "Invalid Args"
	case UPnPErrActionFailed:
		return "Action Failed"
	case UPnPErrNoSuchObject:
		return "No Such Object"
	default:
		return "Unknown Error"
	}
}

// ParseSOAPAction extracts the (serviceType, actionName) pair from a
// SOAPAction HTTP header value. SOAPAction header format per spec is:
//
//	SOAPAction: "urn:schemas-upnp-org:service:ContentDirectory:1#Browse"
//
// Quotes are typical but optional — handler accepts both forms. Returns
// ("", "") if the header is malformed (missing # separator, empty after
// trim, etc.); the caller should respond with a SOAPFault carrying
// UPnPErrInvalidAction.
//
// Pure function — testable with literal strings, no dependency on
// `*http.Request`.
func ParseSOAPAction(header string) (serviceType, actionName string) {
	v := strings.TrimSpace(header)
	v = strings.Trim(v, `"`) // strip surrounding quotes if present
	hash := strings.LastIndexByte(v, '#')
	if hash < 0 || hash == len(v)-1 {
		return "", ""
	}
	return v[:hash], v[hash+1:]
}

// SOAPResponseEnvelope returns the bytes of a SOAP response envelope
// wrapping a single action-response with the given service type +
// action name + inner XML body. Wire shape per UPnP UDA + SOAP 1.1:
//
//	<?xml version="1.0" encoding="UTF-8"?>
//	<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"
//	            s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
//	  <s:Body>
//	    <u:{Action}Response xmlns:u="{serviceType}">{innerXML}</u:{Action}Response>
//	  </s:Body>
//	</s:Envelope>
//
// `innerXML` is the raw XML for the response's argument list (e.g., the
// `<Result>...</Result><NumberReturned>...</NumberReturned>...` block
// for Browse). Caller is responsible for any escaping inside `innerXML`
// (Browse responses, for instance, embed DIDL-Lite as an escaped
// string inside `<Result>`).
//
// Returned bytes are ready to be sent as the HTTP response body with
// Content-Type `text/xml; charset="utf-8"`.
func SOAPResponseEnvelope(serviceType, actionName, innerXML string) []byte {
	return []byte(soapXMLPreamble +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body>` +
		`<u:` + actionName + `Response xmlns:u="` + serviceType + `">` +
		innerXML +
		`</u:` + actionName + `Response>` +
		`</s:Body>` +
		`</s:Envelope>`)
}

// SOAPFaultEnvelope returns the bytes of a SOAP fault envelope carrying
// the given UPnP error code + spec-canonical description. Wire shape
// per UPnP UDA Annex A:
//
//	<?xml version="1.0" encoding="UTF-8"?>
//	<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"
//	            s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">
//	  <s:Body>
//	    <s:Fault>
//	      <faultcode>s:Client</faultcode>
//	      <faultstring>UPnPError</faultstring>
//	      <detail>
//	        <UPnPError xmlns="urn:schemas-upnp-org:control-1-0">
//	          <errorCode>{code}</errorCode>
//	          <errorDescription>{description}</errorDescription>
//	        </UPnPError>
//	      </detail>
//	    </s:Fault>
//	  </s:Body>
//	</s:Envelope>
//
// Returned bytes ship as the HTTP response body with status 500 and
// Content-Type `text/xml; charset="utf-8"` per the SOAP 1.1 fault
// convention.
//
// `faultcode` is always `s:Client` for UPnP errors; the UPnP spec
// doesn't distinguish between client-fault and server-fault at the
// SOAP layer (the numeric error code carries that information).
func SOAPFaultEnvelope(code int) []byte {
	desc := upnpErrorDescription(code)
	return []byte(soapXMLPreamble +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body>` +
		`<s:Fault>` +
		`<faultcode>s:Client</faultcode>` +
		`<faultstring>UPnPError</faultstring>` +
		`<detail>` +
		`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0">` +
		`<errorCode>` + fmt.Sprintf("%d", code) + `</errorCode>` +
		`<errorDescription>` + desc + `</errorDescription>` +
		`</UPnPError>` +
		`</detail>` +
		`</s:Fault>` +
		`</s:Body>` +
		`</s:Envelope>`)
}

// soapXMLPreamble is the XML 1.0 prolog every SOAP envelope ships with.
// Some renderers (older UPnP stacks) strictly require the encoding
// declaration; omitting it produces silent parse failures on the
// renderer side. Centralized constant so a future protocol bump (e.g.,
// SOAP 1.2) only needs one site update.
const soapXMLPreamble = `<?xml version="1.0" encoding="UTF-8"?>`

// SOAPContentType is the canonical Content-Type header value for SOAP
// response bodies (request bodies use the same value per spec).
// Centralized so the file handler + SOAP handler + tests reference the
// same string.
const SOAPContentType = `text/xml; charset="utf-8"`

// SOAPResponseHeader is the SOAP-mandated EXT response header. Per UDA
// 1.0, every SOAP response MUST include an empty EXT header. Most
// renderers ignore its absence but a strict subset (older UPnP stacks)
// drop the response without it.
const SOAPResponseHeader = "EXT"
