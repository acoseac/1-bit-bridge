package dlna

import (
	"fmt"
	"net/http"
)

// DeviceDescriptionOpts is the input shape for `DeviceDescriptionXML`.
// The device description XML is what UPnP control points fetch from
// the `LOCATION` header URL advertised in SSDP NOTIFY packets — it
// tells them the service tree (ContentDirectory + ConnectionManager
// endpoints, friendly name, manufacturer info, etc.).
type DeviceDescriptionOpts struct {
	// UDN is the device's stable unique identifier WITH the `uuid:`
	// prefix (e.g. "uuid:f1b3a5c2-..."). Same string used in SSDP
	// NotifyTargets.
	UDN string

	// FriendlyName is the user-visible device name renderers display
	// in their server picker UI. Configurable via bridge.yaml's
	// `dlna.friendlyName`; defaults to "1-bit Bridge".
	FriendlyName string

	// Manufacturer, ManufacturerURL, ModelDescription, ModelName,
	// ModelNumber — vendor identity fields. The bridge fills these
	// with project-canonical values; not user-configurable.
	Manufacturer     string
	ManufacturerURL  string
	ModelDescription string
	ModelName        string
	ModelNumber      string
}

// DeviceDescriptionXML returns the bytes of the bridge's UPnP device
// description XML, ready to ship as the body of GET /dlna/description.xml.
//
// The XML declares the device as a MediaServer:1 with two services:
// ContentDirectory:1 + ConnectionManager:1. Service URLs are relative
// (`/dlna/cds/...`, `/dlna/cm/...`) so they resolve against the URL
// the renderer fetched the description from — which automatically
// adapts to multi-interface deployments (Tailscale IP, LAN IP, etc.).
//
// The `<dlna:X_DLNADOC>DMS-1.50</dlna:X_DLNADOC>` element is the DLNA
// MediaServer profile token — required by some strict renderers to
// recognize us as DLNA-conforming. Without it, those renderers treat
// us as a generic UPnP MediaServer and may decline to surface DSD /
// high-res formats in their picker.
func DeviceDescriptionXML(opts DeviceDescriptionOpts) []byte {
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<root xmlns="urn:schemas-upnp-org:device-1-0" xmlns:dlna="urn:schemas-dlna-org:device-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <device>
    <dlna:X_DLNADOC xmlns:dlna="urn:schemas-dlna-org:device-1-0">DMS-1.50</dlna:X_DLNADOC>
    <deviceType>urn:schemas-upnp-org:device:MediaServer:1</deviceType>
    <friendlyName>%s</friendlyName>
    <manufacturer>%s</manufacturer>
    <manufacturerURL>%s</manufacturerURL>
    <modelDescription>%s</modelDescription>
    <modelName>%s</modelName>
    <modelNumber>%s</modelNumber>
    <UDN>%s</UDN>
    <serviceList>
      <service>
        <serviceType>%s</serviceType>
        <serviceId>urn:upnp-org:serviceId:ContentDirectory</serviceId>
        <SCPDURL>/dlna/cds.xml</SCPDURL>
        <controlURL>/dlna/cds/control</controlURL>
        <eventSubURL>/dlna/cds/event</eventSubURL>
      </service>
      <service>
        <serviceType>%s</serviceType>
        <serviceId>urn:upnp-org:serviceId:ConnectionManager</serviceId>
        <SCPDURL>/dlna/cm.xml</SCPDURL>
        <controlURL>/dlna/cm/control</controlURL>
        <eventSubURL>/dlna/cm/event</eventSubURL>
      </service>
    </serviceList>
  </device>
</root>`,
		escapeXMLText(opts.FriendlyName),
		escapeXMLText(opts.Manufacturer),
		escapeXMLText(opts.ManufacturerURL),
		escapeXMLText(opts.ModelDescription),
		escapeXMLText(opts.ModelName),
		escapeXMLText(opts.ModelNumber),
		escapeXMLText(opts.UDN),
		ContentDirectoryServiceType,
		ConnectionManagerServiceType,
	))
}

// DeviceDescriptionHandler returns an http.HandlerFunc that responds
// with the bridge's device description XML. Configured once at bridge
// startup with stable opts; the handler is a pure response producer
// (no per-request logic).
func DeviceDescriptionHandler(opts DeviceDescriptionOpts) http.HandlerFunc {
	body := DeviceDescriptionXML(opts)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// -----------------------------------------------------------------------------
// SCPDs (Service Control Protocol Descriptions)
//
// Renderers fetch these to know what SOAP actions our services
// expose + each action's argument schema. We emit the minimum subset
// of actions actually implemented (Browse on CDS, GetProtocolInfo +
// GetCurrentConnectionIDs on CM). Renderers strictly compare their
// SOAP invocations against the SCPD; emitting an action in the SCPD
// that the handler doesn't support trips a fault on the renderer side.
// -----------------------------------------------------------------------------

// ContentDirectorySCPDXML is the SCPD for the ContentDirectory:1
// service. Declares Browse + Search + the 3 spec-mandatory introspection
// actions (GetSearchCapabilities, GetSortCapabilities, GetSystemUpdateID).
//
// Search is genuinely optional per spec, but we DO implement it
// (FTS5-backed free-text query — see handleSearch), so it MUST be
// declared here: a control point reads the SCPD action list to decide
// which actions to offer, and an undeclared Search action means the
// search UI never appears even though GetSearchCapabilities advertises
// searchable fields.
//
// The 3 introspection actions are REQUIRED per UPnP CDS:1 §2.3
// regardless of whether Search is implemented. Empirically validated
// 2026-05-28 against a minimal Go-based mconnect-compat test server
// at /tmp/upnp-test: strict controllers (mconnect Player, BubbleUPnP-
// flavoured stacks) silently abort container navigation when these
// actions are absent from the SCPD. The user-visible symptom is
// "All Tracks [N] shows in mconnect, but tap-to-drill does nothing" —
// mconnect renders root via Browse(0) successfully, but never
// dispatches the downstream Browse(child) because it polls
// GetSystemUpdateID between every navigation step to verify directory
// freshness, and abandons the drill when that call returns
// InvalidAction (401). Adding the 3 actions (returning empty
// SearchCaps + empty SortCaps + Id=1) unblocks mconnect end-to-end.
//
// Pre-fix this SCPD declared ONLY Browse on the strength of the
// reasoning "renderers tolerate optional-action absence gracefully" —
// that's TRUE for Search / CreateObject / DestroyObject (genuinely
// optional per spec) but FALSE for the 3 introspection actions
// (mandatory per spec, strictly required by some controllers). The
// fix is additive — Browse argument list unchanged.
//
// Don't drop any of the 3 introspection actions at any future
// refactor — would re-open the mconnect-silent-drill-abort
// regression PR #316 closes.
const ContentDirectorySCPDXML = `<?xml version="1.0" encoding="UTF-8"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <actionList>
    <action>
      <name>GetSearchCapabilities</name>
      <argumentList>
        <argument><name>SearchCaps</name><direction>out</direction><relatedStateVariable>SearchCapabilities</relatedStateVariable></argument>
      </argumentList>
    </action>
    <action>
      <name>GetSortCapabilities</name>
      <argumentList>
        <argument><name>SortCaps</name><direction>out</direction><relatedStateVariable>SortCapabilities</relatedStateVariable></argument>
      </argumentList>
    </action>
    <action>
      <name>GetSystemUpdateID</name>
      <argumentList>
        <argument><name>Id</name><direction>out</direction><relatedStateVariable>SystemUpdateID</relatedStateVariable></argument>
      </argumentList>
    </action>
    <action>
      <name>Browse</name>
      <argumentList>
        <argument><name>ObjectID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable></argument>
        <argument><name>BrowseFlag</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_BrowseFlag</relatedStateVariable></argument>
        <argument><name>Filter</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Filter</relatedStateVariable></argument>
        <argument><name>StartingIndex</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Index</relatedStateVariable></argument>
        <argument><name>RequestedCount</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
        <argument><name>SortCriteria</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_SortCriteria</relatedStateVariable></argument>
        <argument><name>Result</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable></argument>
        <argument><name>NumberReturned</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
        <argument><name>TotalMatches</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
        <argument><name>UpdateID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_UpdateID</relatedStateVariable></argument>
      </argumentList>
    </action>
    <action>
      <name>Search</name>
      <argumentList>
        <argument><name>ContainerID</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_ObjectID</relatedStateVariable></argument>
        <argument><name>SearchCriteria</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_SearchCriteria</relatedStateVariable></argument>
        <argument><name>Filter</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Filter</relatedStateVariable></argument>
        <argument><name>StartingIndex</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Index</relatedStateVariable></argument>
        <argument><name>RequestedCount</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
        <argument><name>SortCriteria</name><direction>in</direction><relatedStateVariable>A_ARG_TYPE_SortCriteria</relatedStateVariable></argument>
        <argument><name>Result</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Result</relatedStateVariable></argument>
        <argument><name>NumberReturned</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
        <argument><name>TotalMatches</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_Count</relatedStateVariable></argument>
        <argument><name>UpdateID</name><direction>out</direction><relatedStateVariable>A_ARG_TYPE_UpdateID</relatedStateVariable></argument>
      </argumentList>
    </action>
  </actionList>
  <serviceStateTable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_ObjectID</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_BrowseFlag</name><dataType>string</dataType><allowedValueList><allowedValue>BrowseMetadata</allowedValue><allowedValue>BrowseDirectChildren</allowedValue></allowedValueList></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Filter</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Index</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Count</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_SortCriteria</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_SearchCriteria</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_Result</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>A_ARG_TYPE_UpdateID</name><dataType>ui4</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>SearchCapabilities</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="no"><name>SortCapabilities</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="yes"><name>SystemUpdateID</name><dataType>ui4</dataType></stateVariable>
  </serviceStateTable>
</scpd>`

// ConnectionManagerSCPDXML is the SCPD for the ConnectionManager:1
// service. Declares the two actions we implement: GetProtocolInfo +
// GetCurrentConnectionIDs.
const ConnectionManagerSCPDXML = `<?xml version="1.0" encoding="UTF-8"?>
<scpd xmlns="urn:schemas-upnp-org:service-1-0">
  <specVersion><major>1</major><minor>0</minor></specVersion>
  <actionList>
    <action>
      <name>GetProtocolInfo</name>
      <argumentList>
        <argument><name>Source</name><direction>out</direction><relatedStateVariable>SourceProtocolInfo</relatedStateVariable></argument>
        <argument><name>Sink</name><direction>out</direction><relatedStateVariable>SinkProtocolInfo</relatedStateVariable></argument>
      </argumentList>
    </action>
    <action>
      <name>GetCurrentConnectionIDs</name>
      <argumentList>
        <argument><name>ConnectionIDs</name><direction>out</direction><relatedStateVariable>CurrentConnectionIDs</relatedStateVariable></argument>
      </argumentList>
    </action>
  </actionList>
  <serviceStateTable>
    <stateVariable sendEvents="yes"><name>SourceProtocolInfo</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="yes"><name>SinkProtocolInfo</name><dataType>string</dataType></stateVariable>
    <stateVariable sendEvents="yes"><name>CurrentConnectionIDs</name><dataType>string</dataType></stateVariable>
  </serviceStateTable>
</scpd>`

// SCPDHandler returns an http.HandlerFunc that ships the given SCPD
// XML body with the canonical Content-Type. Used at /dlna/cds.xml
// and /dlna/cm.xml.
func SCPDHandler(scpdXML string) http.HandlerFunc {
	body := []byte(scpdXML)
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `text/xml; charset="utf-8"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
