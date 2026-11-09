package admin

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestTheTailscaleTileShowsWhatHoldsTheUploadsOff: the Tailscale tile has a
// line of its own for what holds the embedded node's diagnostic-log uploads
// off where the setting allows them (backlog B749: the launch environment,
// this process earlier, or something else in it), apart from LastError, so a
// node the tailnet accepted keeps its badge and says it all the same. The
// line is hidden while the status carries nothing, written as text, and read
// by the name the server marshals the field as.
func TestTheTailscaleTileShowsWhatHoldsTheUploadsOff(t *testing.T) {
	f, ok := reflect.TypeOf(TailscaleStatus{}).FieldByName("UploadsHeldOff")
	if !ok {
		t.Fatal("TailscaleStatus has no UploadsHeldOff field")
	}
	if tag := f.Tag.Get("json"); tag != "uploadsHeldOff,omitempty" {
		t.Fatalf("TailscaleStatus.UploadsHeldOff marshals as %q, want uploadsHeldOff,omitempty", tag)
	}

	html, err := os.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatal(err)
	}
	panel := string(html)
	i := strings.Index(panel, `id="tailscale-panel"`)
	if i < 0 {
		t.Fatal("settings.html has no Tailscale panel")
	}
	panel = panel[i:]
	if j := strings.Index(panel, "</div>\n\n"); j > 0 {
		panel = panel[:j]
	}
	for _, want := range []string{`<dt id="tailscale-uploads-label" hidden>`, `<dd id="tailscale-uploads" hidden>`} {
		if !strings.Contains(panel, want) {
			t.Errorf("the Tailscale panel has no %s", want)
		}
	}

	js := readConsoleJS(t, "static/app.js")
	const decl = "function renderTailscaleTile("
	k := strings.Index(js, decl)
	if k < 0 {
		t.Fatal("renderTailscaleTile not found in app.js")
	}
	body := js[k:]
	if e := strings.Index(body[1:], "\nfunction "); e > 0 {
		body = body[:e+1]
	}
	for _, want := range []string{
		`document.getElementById("tailscale-uploads")`,
		`document.getElementById("tailscale-uploads-label")`,
		`uploadsEl.textContent = s.uploadsHeldOff`,
		`uploadsEl.hidden = !s.uploadsHeldOff`,
		`uploadsLabel.hidden = !s.uploadsHeldOff`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderTailscaleTile does not carry %s", want)
		}
	}
	if strings.Contains(body, "innerHTML = s.uploadsHeldOff") || strings.Contains(body, "${s.uploadsHeldOff}") {
		t.Error("renderTailscaleTile writes the uploads line as HTML; it is text")
	}
}
