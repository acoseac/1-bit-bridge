package main

import (
	"context"
	"errors"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/admin"
)

// TestTheConsoleRefusesAManualUpstreamOnACloudMetadataAddress: a manual
// upstream URL the operator types into the console (POST
// /api/upnp/servers) that names a cloud metadata address as a literal is
// refused as a validation error, nothing written (backlog B54). No media
// server serves on one, the poller would refuse to fetch it and warn in
// the journal, which the console does not show, and the typed value is the
// operator's to correct now. A config that already holds one still loads:
// the poller refuses it at runtime. A manual URL on a direct-cable
// device's own link-local address is added, the control.
func TestTheConsoleRefusesAManualUpstreamOnACloudMetadataAddress(t *testing.T) {
	a, rt, _ := upnpAdapterForPersistTest(t, newUPnPTestCfg(t))
	for _, u := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://[fd00:ec2::254]:80/latest/",
		"http://user:pw@168.63.129.16/machine/?comp=goalstate",
	} {
		err := a.AddServer(context.Background(), admin.UPnPServerAddRequest{Name: "Metadata", ManualDescriptionURL: u})
		if !errors.Is(err, admin.ErrUPnPValidation) {
			t.Errorf("AddServer(%q) = %v, want a validation refusal", u, err)
		}
		if n := len(rt.Load().UPnPUpstream.Servers); n != 0 {
			t.Fatalf("a refused add stored a server (%d configured)", n)
		}
	}
	const cable = "http://169.254.7.7:8200/rootDesc.xml"
	if err := a.AddServer(context.Background(), admin.UPnPServerAddRequest{Name: "Cable", ManualDescriptionURL: cable}); err != nil {
		t.Fatalf("AddServer(%q): %v", cable, err)
	}
	if got := rt.Load().UPnPUpstream.Servers; len(got) != 1 || got[0].ManualDescriptionURL != cable {
		t.Errorf("servers = %+v, want the direct-cable device alone", got)
	}
}
