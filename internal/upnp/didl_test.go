package upnp

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"testing"
)

// wrapBrowse builds a Browse SOAP response with the DIDL document
// entity-escaped inside <Result>, exactly as a server emits it — so the
// test exercises the real two-pass un-escape (SOAP Result, then DIDL).
func wrapBrowse(didl string, numberReturned, totalMatches int) []byte {
	var esc bytes.Buffer
	_ = xml.EscapeText(&esc, []byte(didl))
	return []byte(`<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<u:BrowseResponse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1">` +
		`<Result>` + esc.String() + `</Result>` +
		fmt.Sprintf(`<NumberReturned>%d</NumberReturned><TotalMatches>%d</TotalMatches><UpdateID>1</UpdateID>`,
			numberReturned, totalMatches) +
		`</u:BrowseResponse></s:Body></s:Envelope>`)
}

func TestParseDIDL_RootContainers(t *testing.T) {
	// The validated 2Go root under Browse Folders' parent.
	didl := `<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/">` +
		`<container id="64" parentID="0" childCount="2"><dc:title>Browse Folders</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>` +
		`<container id="1" parentID="0" childCount="7"><dc:title>Music</dc:title><upnp:class>object.container.storageFolder</upnp:class></container>` +
		`</DIDL-Lite>`
	res, err := parseDIDL(didl)
	if err != nil {
		t.Fatalf("parseDIDL: %v", err)
	}
	if len(res.Items) != 0 || len(res.Containers) != 2 {
		t.Fatalf("got %d containers / %d items; want 2 / 0", len(res.Containers), len(res.Items))
	}
	if res.Containers[0].ID != "64" || res.Containers[0].Title != "Browse Folders" || res.Containers[0].ChildCount != 2 {
		t.Fatalf("container[0] = %+v", res.Containers[0])
	}
	if res.Containers[0].Class != "object.container.storageFolder" {
		t.Fatalf("container[0].Class = %q", res.Containers[0].Class)
	}
	if res.Containers[1].Title != "Music" || res.Containers[1].ChildCount != 7 {
		t.Fatalf("container[1] = %+v", res.Containers[1])
	}
}

func TestParseDIDL_RealItem_AllFields(t *testing.T) {
	// Verbatim shape captured from the live 2Go ("Browse Folders" leaf).
	didl := `<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/" xmlns:dlna="urn:schemas-dlna-org:metadata-1-0/">` +
		`<item id="64$0$C7$1$0" parentID="64$0$C7$1">` +
		`<dc:title>What's Up?</dc:title>` +
		`<upnp:class>object.item.audioItem.musicTrack</upnp:class>` +
		`<dc:creator>4 Non Blondes</dc:creator>` +
		`<dc:date>2019-01-01</dc:date>` +
		`<upnp:artist>4 Non Blondes</upnp:artist>` +
		`<upnp:album>What's Up?</upnp:album>` +
		`<upnp:originalTrackNumber>1</upnp:originalTrackNumber>` +
		`<res size="34486815" duration="0:04:52.162" bitrate="944326" sampleFrequency="44100" nrAudioChannels="2" protocolInfo="http-get:*:audio/x-flac:*">http://192.168.0.62:8200/MediaItems/64.flac</res>` +
		`<upnp:albumArtURI dlna:profileID="JPEG_TN">http://192.168.0.62:8200/AlbumArt/2-25.jpg</upnp:albumArtURI>` +
		`</item></DIDL-Lite>`
	res, err := parseDIDL(didl)
	if err != nil {
		t.Fatalf("parseDIDL: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d items; want 1", len(res.Items))
	}
	it := res.Items[0]
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"ID", it.ID, "64$0$C7$1$0"},
		{"ParentID", it.ParentID, "64$0$C7$1"},
		{"Title", it.Title, "What's Up?"},
		{"Artist", it.Artist, "4 Non Blondes"},
		{"Album", it.Album, "What's Up?"},
		{"Creator", it.Creator, "4 Non Blondes"},
		{"Class", it.Class, "object.item.audioItem.musicTrack"},
		{"Date", it.Date, "2019-01-01"},
		{"TrackNumber", it.TrackNumber, 1},
		{"Res", it.Res, "http://192.168.0.62:8200/MediaItems/64.flac"},
		{"ProtocolInfo", it.ProtocolInfo, "http-get:*:audio/x-flac:*"},
		{"Size", it.Size, int64(34486815)},
		{"Duration", it.Duration, "0:04:52.162"},
		{"SampleRate", it.SampleRate, 44100},
		{"Channels", it.Channels, 2},
		{"AlbumArtURI", it.AlbumArtURI, "http://192.168.0.62:8200/AlbumArt/2-25.jpg"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v; want %v", c.field, c.got, c.want)
		}
	}
	if it.BitsPerSample != 0 {
		t.Errorf("BitsPerSample = %d; want 0 (absent for FLAC)", it.BitsPerSample)
	}
}

func TestParseBrowseResponse_RoundTrip(t *testing.T) {
	didl := `<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">` +
		`<item id="x1" parentID="0"><dc:title>Lilac Wine</dc:title>` +
		`<res protocolInfo="http-get:*:audio/x-dsf:*" size="192389345">http://192.168.0.62:8200/MediaItems/6936.dsf</res></item>` +
		`</DIDL-Lite>`
	res, err := ParseBrowseResponse(wrapBrowse(didl, 1, 1))
	if err != nil {
		t.Fatalf("ParseBrowseResponse: %v", err)
	}
	if res.NumberReturned != 1 || res.TotalMatches != 1 {
		t.Fatalf("counts = %d/%d; want 1/1", res.NumberReturned, res.TotalMatches)
	}
	if len(res.Items) != 1 || res.Items[0].Res != "http://192.168.0.62:8200/MediaItems/6936.dsf" {
		t.Fatalf("items = %+v", res.Items)
	}
	if res.Items[0].ProtocolInfo != "http-get:*:audio/x-dsf:*" || res.Items[0].Size != 192389345 {
		t.Fatalf("DSF res = %+v", res.Items[0])
	}
}

func TestParseBrowseResponse_DoubleEscapedTitle(t *testing.T) {
	// Title with '&' is single-escaped in the DIDL; wrapBrowse escapes
	// the whole DIDL again. The two un-escape passes must compose to the
	// literal "Rock & Roll".
	didl := `<DIDL-Lite xmlns:dc="http://purl.org/dc/elements/1.1/"><item id="1" parentID="0"><dc:title>Rock &amp; Roll</dc:title></item></DIDL-Lite>`
	res, err := ParseBrowseResponse(wrapBrowse(didl, 1, 1))
	if err != nil {
		t.Fatalf("ParseBrowseResponse: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].Title != "Rock & Roll" {
		t.Fatalf("title = %q; want %q", res.Items[0].Title, "Rock & Roll")
	}
}

func TestParseBrowseResponse_Fault(t *testing.T) {
	fault := []byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>` +
		`<s:Fault><faultcode>s:Client</faultcode><detail><UPnPError><errorCode>701</errorCode>` +
		`<errorDescription>No such object</errorDescription></UPnPError></detail></s:Fault>` +
		`</s:Body></s:Envelope>`)
	_, err := ParseBrowseResponse(fault)
	if !errors.Is(err, ErrSOAPFault) {
		t.Fatalf("err = %v; want ErrSOAPFault", err)
	}
}

func TestParseBrowseResponse_MissingResponse(t *testing.T) {
	empty := []byte(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body></s:Body></s:Envelope>`)
	_, err := ParseBrowseResponse(empty)
	if !errors.Is(err, ErrMissingResponseElement) {
		t.Fatalf("err = %v; want ErrMissingResponseElement", err)
	}
}

func TestParseDIDL_Empty(t *testing.T) {
	res, err := parseDIDL("")
	if err != nil {
		t.Fatalf("parseDIDL(empty): %v", err)
	}
	if len(res.Containers) != 0 || len(res.Items) != 0 {
		t.Fatalf("empty DIDL produced %d/%d", len(res.Containers), len(res.Items))
	}
}

func TestPickAudioRes_PrefersAudioMIME(t *testing.T) {
	// A non-audio res first (e.g. a thumbnail), then the audio res:
	// the audio one must win.
	res := []didlRes{
		{ProtocolInfo: "http-get:*:image/jpeg:*", URL: "http://h/thumb.jpg"},
		{ProtocolInfo: "http-get:*:audio/x-flac:*", URL: "http://h/MediaItems/1.flac"},
	}
	r, ok := pickAudioRes(res)
	if !ok || r.URL != "http://h/MediaItems/1.flac" {
		t.Fatalf("pickAudioRes = (%+v, %v); want the FLAC res", r, ok)
	}
}

func TestPickAudioRes_FallsBackToFirstNonEmpty(t *testing.T) {
	res := []didlRes{
		{ProtocolInfo: "", URL: ""},
		{ProtocolInfo: "http-get:*:application/octet-stream:*", URL: "http://h/MediaItems/2.bin"},
	}
	r, ok := pickAudioRes(res)
	if !ok || r.URL != "http://h/MediaItems/2.bin" {
		t.Fatalf("pickAudioRes = (%+v, %v); want the octet-stream fallback", r, ok)
	}
}
