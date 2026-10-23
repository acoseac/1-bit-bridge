package openhome

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// scpdInventory is what an SCPD says of its actions and state variables,
// read as a controller reads it.
type scpdInventory struct {
	XMLName xml.Name `xml:"urn:schemas-upnp-org:service-1-0 scpd"`
	Major   int      `xml:"specVersion>major"`
	Minor   int      `xml:"specVersion>minor"`
	Actions []struct {
		Name string `xml:"name"`
		Args []struct {
			Name      string `xml:"name"`
			Direction string `xml:"direction"`
			Related   string `xml:"relatedStateVariable"`
		} `xml:"argumentList>argument"`
	} `xml:"actionList>action"`
	Variables []struct {
		SendEvents string   `xml:"sendEvents,attr"`
		Name       string   `xml:"name"`
		DataType   string   `xml:"dataType"`
		Allowed    []string `xml:"allowedValueList>allowedValue"`
	} `xml:"serviceStateTable>stateVariable"`
}

func readInventory(t *testing.T, what string, b []byte) scpdInventory {
	t.Helper()
	var inv scpdInventory
	if err := xml.Unmarshal(b, &inv); err != nil {
		t.Fatalf("%s does not read as an SCPD: %v", what, err)
	}
	return inv
}

// TestOurSCPDsAreUpstreamsInventory: the four services' descriptions are
// the bridge's own, generated from its tables, and they say exactly what
// OpenHome's own files at 22feef52 (testdata/upstream, test data alone) say:
// the same actions in the same order, each with the same arguments in the
// same order, the same directions and related state variables, and the
// same state variables with the same types, allowed values and eventing.
// Every argument's related variable is in the table.
func TestOurSCPDsAreUpstreamsInventory(t *testing.T) {
	for _, s := range services {
		t.Run(s.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "upstream", s.name+"1.xml"))
			if err != nil {
				t.Fatal(err)
			}
			upstream := readInventory(t, "upstream's "+s.name+"1.xml", raw)
			ours := readInventory(t, "our "+s.name+" SCPD", s.scpd())
			if len(upstream.Actions) == 0 || len(upstream.Variables) == 0 {
				t.Fatalf("upstream's file read as nothing: %+v", upstream)
			}
			if !reflect.DeepEqual(ours, upstream) {
				t.Errorf("our %s SCPD is not upstream's inventory:\nours:     %+v\nupstream: %+v", s.name, ours, upstream)
			}
			for _, a := range s.actions {
				for _, g := range a.args {
					if s.variable(g.related) == nil {
						t.Errorf("%s.%s's argument %s names %s, which the table has not", s.name, a.name, g.name, g.related)
					}
				}
			}
		})
	}
}

// TestTheUpstreamFilesAreTheirPinnedBytes: the copies in testdata are
// upstream's bytes, CRLF and all, as the README pins them, so the
// comparison above is with OpenHome's files and not with a hand-made copy.
func TestTheUpstreamFilesAreTheirPinnedBytes(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("testdata", "upstream", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	pinned := 0
	for _, line := range strings.Split(string(readme), "\n") {
		sum, name, ok := strings.Cut(line, "  ")
		if !ok || len(sum) != 64 {
			continue
		}
		pinned++
		b, err := os.ReadFile(filepath.Join("testdata", "upstream", name))
		if err != nil {
			t.Fatal(err)
		}
		got := sha256.Sum256(b)
		if hex.EncodeToString(got[:]) != sum {
			t.Errorf("%s is not the pinned file: sha256 %x, the README pins %s", name, got, sum)
		}
	}
	if pinned != 5 {
		t.Errorf("the README pins %d files, want the four SCPDs and the licence", pinned)
	}
}

// TestTheSourceCarriesFourServicesAndNoVolume: Product, Playlist, Info and
// Time, each with its type, a distinct stable ID and a path of its own, and
// no Volume service, since the player's output is fixed.
func TestTheSourceCarriesFourServicesAndNoVolume(t *testing.T) {
	want := map[string][3]string{
		"Product":  {"urn:av-openhome-org:service:Product:1", "urn:av-openhome-org:serviceId:Product", "product"},
		"Playlist": {"urn:av-openhome-org:service:Playlist:1", "urn:av-openhome-org:serviceId:Playlist", "playlist"},
		"Info":     {"urn:av-openhome-org:service:Info:1", "urn:av-openhome-org:serviceId:Info", "info"},
		"Time":     {"urn:av-openhome-org:service:Time:1", "urn:av-openhome-org:serviceId:Time", "time"},
	}
	if len(services) != len(want) {
		t.Fatalf("%d services, want %d", len(services), len(want))
	}
	ids := map[string]bool{}
	for _, s := range services {
		w, ok := want[s.name]
		if !ok {
			t.Errorf("a service %q the Source does not carry", s.name)
			continue
		}
		if s.serviceType() != w[0] || s.serviceID() != w[1] || s.path != w[2] {
			t.Errorf("%s: type %q, ID %q, path %q; want %v", s.name, s.serviceType(), s.serviceID(), s.path, w)
		}
		if ids[s.serviceID()] {
			t.Errorf("the service ID %s is not distinct", s.serviceID())
		}
		ids[s.serviceID()] = true
		if bytes.Contains(s.scpd(), []byte("Volume")) {
			t.Errorf("%s's SCPD names Volume", s.name)
		}
	}
}
