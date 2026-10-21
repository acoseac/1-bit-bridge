package admin

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// pairDialogOf is the pairing dialog's markup in a rendered page.
func pairDialogOf(t *testing.T, page, body string) string {
	t.Helper()
	start := strings.Index(body, `<dialog id="pair-modal">`)
	if start < 0 {
		t.Fatalf("%s renders no pairing dialog", page)
	}
	end := strings.Index(body[start:], "</dialog>")
	if end < 0 {
		t.Fatalf("%s: the pairing dialog is not closed", page)
	}
	return body[start : start+end+len("</dialog>")]
}

// TestTheDashboardCarriesThePlayerChecklistAndThePairingQR: the dashboard
// renders the player checklist's panel, hidden until GET
// /api/output/checklist says the bridge is a player, and the pairing
// dialog its Phone line opens: the Devices page's own, the same markup,
// pre-filled with the same dial URL.
func TestTheDashboardCarriesThePlayerChecklistAndThePairingQR(t *testing.T) {
	s, cfg, _ := newTestServer(t)
	h := s.Handler()
	pages := map[string]string{}
	for _, p := range []string{"/stats", "/devices"} {
		rr := smokeRequest(t, h, p)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", p, rr.Code)
		}
		pages[p] = rr.Body.String()
	}
	stats := pages["/stats"]
	for _, want := range []string{
		`<div class="panel" id="player-checklist-panel" hidden>`,
		`id="player-checklist-refresh"`,
		`<ul class="doctor-list" id="player-checklist"></ul>`,
	} {
		if !strings.Contains(stats, want) {
			t.Errorf("the dashboard lacks %s", want)
		}
	}
	dash, dev := pairDialogOf(t, "/stats", stats), pairDialogOf(t, "/devices", pages["/devices"])
	if dash != dev {
		t.Fatalf("the dashboard's pairing dialog differs from the Devices page's:\n%s\n---\n%s", dash, dev)
	}
	if url := defaultBridgeURL(cfg); url == "" || !strings.Contains(dash, `value="`+url+`"`) {
		t.Fatalf("the dialog is not pre-filled with the dial URL %q", url)
	}
}

// TestThePlayerChecklistScriptIsWired pins the console script's half, read
// from the shipped app.js with its comments stripped (node is not on every
// host this runs on): the dashboard loads the checklist and opens the
// pairing dialog through the one opener, the Devices page uses that opener
// too, the token is minted in one place, and the Phone line offers the QR
// only when it is not green, painted with textContent.
func TestThePlayerChecklistScriptIsWired(t *testing.T) {
	raw, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := stripJSComments(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	stats := extractJSFunction(t, src, "initStats")
	for _, want := range []string{"initPairModal()", "loadPlayerChecklist(openPair)", `"player-checklist-refresh"`} {
		if !strings.Contains(stats, want) {
			t.Errorf("initStats does not call %s", want)
		}
	}
	devices := extractJSFunction(t, src, "initDevices")
	if !strings.Contains(devices, "initPairModal()") {
		t.Error("initDevices does not open the pairing dialog through initPairModal")
	}
	if n := strings.Count(src, `API.post("/api/tokens"`); n != 1 {
		t.Errorf("the console mints a pairing token in %d places, want 1 (initPairModal)", n)
	}
	if !strings.Contains(extractJSFunction(t, src, "initPairModal"), `API.post("/api/tokens"`) {
		t.Error("initPairModal does not mint the token")
	}
	load := extractJSFunction(t, src, "loadPlayerChecklist")
	for _, want := range []string{`"/api/output/checklist"`, "d.active", "panel.hidden = true"} {
		if !strings.Contains(load, want) {
			t.Errorf("loadPlayerChecklist lacks %s", want)
		}
	}
	render := extractJSFunction(t, src, "renderPlayerChecklist")
	for _, want := range []string{`l.key === "phone"`, `l.status !== "ok"`, `"Show pairing QR"`, "openPair"} {
		if !strings.Contains(render, want) {
			t.Errorf("renderPlayerChecklist lacks %s", want)
		}
	}
	if strings.Contains(render, "innerHTML") {
		t.Error("renderPlayerChecklist writes innerHTML; a summary names a card and a player")
	}
}
