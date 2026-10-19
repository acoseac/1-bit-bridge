package alsa

import (
	"encoding/json"
	"testing"
)

// TestParseUSBIDOnEveryCapture reads the iFi GO bar's id from each tree.
func TestParseUSBIDOnEveryCapture(t *testing.T) {
	for _, c := range captures {
		t.Run(c.name, func(t *testing.T) {
			id, err := ParseUSBID(readFixture(t, c.name, "asound/card5/usbid"))
			if err != nil {
				t.Fatal(err)
			}
			if id != (USBID{Vendor: 0x20b1, Product: 0x3009}) || id.String() != "20b1:3009" {
				t.Errorf("usbid = %+v (%s), want 20b1:3009", id, id)
			}
		})
	}
}

// TestParseUSBIDShapes takes only "vvvv:pppp" in hex; anything else names
// no device and is an error.
func TestParseUSBIDShapes(t *testing.T) {
	good := map[string]USBID{
		"20b1:3009\n":   {0x20b1, 0x3009},
		"20B1:3009\r\n": {0x20b1, 0x3009},
		"0000:ffff":     {0, 0xffff},
	}
	for in, want := range good {
		got, err := ParseUSBID([]byte(in))
		if err != nil || got != want {
			t.Errorf("ParseUSBID(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "\n", "20b1", "20b1:", "20b1:300", "20b1:30090", "0x20:3009", "+0b1:3009", "20g1:3009", "20b1 3009", "20b1:3009:1"} {
		if got, err := ParseUSBID([]byte(in)); err == nil {
			t.Errorf("ParseUSBID(%q) = %+v, want an error", in, got)
		}
	}
}

// TestUSBIDMarshalsAsItsPrintedForm pins the id's JSON: a string, as the
// kernel and lsusb print it, which decodes back into the id.
func TestUSBIDMarshalsAsItsPrintedForm(t *testing.T) {
	b, err := json.Marshal(struct {
		ID *USBID `json:"id"`
	}{&USBID{0x20b1, 0x3009}})
	if err != nil || string(b) != `{"id":"20b1:3009"}` {
		t.Errorf("json = %s, %v", b, err)
	}
	var back struct {
		ID *USBID `json:"id"`
	}
	if err := json.Unmarshal(b, &back); err != nil || back.ID == nil || *back.ID != (USBID{0x20b1, 0x3009}) {
		t.Errorf("%s decodes as %+v, %v", b, back.ID, err)
	}
	if err := json.Unmarshal([]byte(`{"id":"20b1"}`), &back); err == nil {
		t.Error("a malformed id decoded without an error")
	}
}
