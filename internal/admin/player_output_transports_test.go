package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The Player page's transports table as its owner sees it (B1029): the
// served page's #player-output-transports-panel (its h2, its .hint, its
// thead's th cells and its empty tbody#player-output-transports), and the
// rows the shipped renderPlayerOutput draws there from the status GET
// /api/output/player serves.

// transportsStatusRows are the four rows cmd/bridge's view answers for a DAC
// whose own choice of auto is capped at DSD64: DSD64 native, DSD128 and
// DSD256 converted to 176.4 kHz at 24 bits, in the status's JSON.
const transportsStatusRows = `[
{"kind":"pcm","plays":true,"capable":"yes","mode":"always on: no DSD choice applies to it","rates":["every rate the DAC takes"],
 "note":"PCM plays bit-exact, as the file holds it: nothing is resampled, converted or mixed."},
{"kind":"dop","plays":false,"capable":"yes","mode":"auto up to DSD64, this DAC's own choice; DoP allowed","rates":[],
 "note":"DoP travels in 24-bit PCM at 176.4 kHz or more."},
{"kind":"native","plays":true,"capable":"yes","mode":"auto up to DSD64, this DAC's own choice","rates":["DSD64 DSD_U32_BE"],
 "note":"Native DSD is the DAC's own DSD format, at the rates its altsets list."},
{"kind":"converted","plays":true,"capable":"yes","mode":"auto up to DSD64, this DAC's own choice",
 "rates":["DSD128 to 176.4 kHz 24-bit PCM","DSD256 to 176.4 kHz 24-bit PCM"],
 "note":"The player converts these DSD rates to the PCM each names: what the DAC receives is not bit-exact."}]`

// transportsHint is the panel's hint: what each row's facts are.
const transportsHint = "What the player plays on the DAC it started with, and the three facts each answer rests on: what the DAC takes, " +
	"the DSD choice that applies to the DAC (its own choice, which the 1-bit app changes, or the bridge's default for a DAC with none), " +
	"and the DSD rates the player plays each way now. PCM plays bit-exact at every rate the DAC takes; DSD the player converts to PCM is not bit-exact."

// transportsHarness runs the shipped renderPlayerOutput over the status, on a
// DOM whose elements start hidden where the served page ships them hidden,
// and prints whether the panel shows and each row of its tbody: each cell's
// text, and the hint a cell carries after it.
const transportsHarness = `
class El {
  constructor(tag) {
    this.tagName = tag; this.children = []; this.hidden = false;
    this.className = ""; this.dataset = {}; this.disabled = false; this.title = "";
  }
  get textContent() { return this.children.map((c) => c.textContent).join(""); }
  set textContent(v) { this.children = String(v) === "" ? [] : [{ textContent: String(v) }]; }
  appendChild(c) { this.children.push(c); return c; }
  append(...nodes) { for (const n of nodes) this.children.push(typeof n === "string" ? { textContent: n } : n); }
  replaceChildren(...nodes) { this.children = []; this.append(...nodes); }
  addEventListener() {}
}
const els = new Map();
const el = (id) => { if (!els.has(id)) els.set(id, new El("#" + id)); return els.get(id); };
globalThis.document = { getElementById: (id) => el(id), createElement: (tag) => new El(tag) };
let playerOutputLatest = null;
function renderPlayerOutputPending() {}
function renderPlayerOutputTone() {}
const input = JSON.parse(await (await import("node:fs/promises")).readFile(process.argv[2], "utf8"));
for (const id of input.hidden) el(id).hidden = true;
renderPlayerOutput(input.status);
const body = el("player-output-transports");
console.log(JSON.stringify({
  panelHidden: el("player-output-transports-panel").hidden,
  rows: body.children.map((tr) => ({ tag: tr.tagName, cells: tr.children.map((td) => ({
    tag: td.tagName,
    text: td.children[0] ? td.children[0].textContent : "",
    hint: td.children[1] ? { className: td.children[1].className, text: td.children[1].textContent } : null,
  })) })),
}));
`

// hiddenIDs are the ids of the page's elements that ship hidden.
func hiddenIDs(n *html.Node) []string {
	var out []string
	if n.Type == html.ElementNode && htmlHasAttr(n, "hidden") {
		for _, a := range n.Attr {
			if a.Key == "id" {
				out = append(out, a.Val)
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		out = append(out, hiddenIDs(c)...)
	}
	return out
}

// htmlElements are n's element descendants named tag, in document order.
func htmlElements(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && c.Data == tag {
			out = append(out, c)
		}
		out = append(out, htmlElements(c, tag)...)
	}
	return out
}

// TestTheOwnersPlayerPageShowsEachTransportRow serves the Player page as a
// loopback bridge's owner gets it: its #player-output-transports-panel ships
// hidden, its h2 is "Transports on this DAC", its .hint says what each row's
// three facts are (the choice, the rates; PCM bit-exact, conversion not), its
// thead's th cells are Transport, Plays, The DAC takes it, Set to and Rates,
// and its tbody#player-output-transports ships empty. The shipped
// renderPlayerOutput, run under node on the status GET /api/output/player
// serves, shows the panel and draws one row a transport under it, labelled
// PCM, DoP, Native DSD and DSD converted to PCM (never a kind's raw word),
// each cell the status's under its heading, a row with no rate "none", and
// each row's note in a .hint after its label.
func TestTheOwnersPlayerPageShowsEachTransportRow(t *testing.T) {
	node := barNode(t)
	srv, fake, _ := newPlayerTestServer(t)
	if err := json.Unmarshal([]byte(transportsStatusRows), &fake.report.Transports); err != nil {
		t.Fatal(err)
	}
	page := renderOutputPage(t, srv)
	if strings.Contains(page, `id="player-output-public"`) {
		t.Fatal("the page is a public bridge's, which serves none of the player's panels")
	}
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	panel := htmlByID(doc, "player-output-transports-panel")
	if panel == nil || !htmlHasAttr(panel, "hidden") {
		t.Fatal("the page serves no #player-output-transports-panel that ships hidden until the status paints it")
	}
	if h := htmlElements(panel, "h2"); len(h) != 1 || strings.TrimSpace(htmlText(h[0])) != "Transports on this DAC" {
		t.Errorf("#player-output-transports-panel's h2 is not \"Transports on this DAC\"")
	}
	var hint string
	for _, p := range htmlElements(panel, "p") {
		for _, a := range p.Attr {
			if a.Key == "class" && a.Val == "hint" {
				hint = strings.Join(strings.Fields(htmlText(p)), " ")
			}
		}
	}
	if hint != transportsHint {
		t.Errorf("#player-output-transports-panel's .hint says %q, want %q", hint, transportsHint)
	}
	var headings []string
	for _, th := range htmlElements(panel, "th") {
		headings = append(headings, strings.TrimSpace(htmlText(th)))
	}
	if strings.Join(headings, "|") != "Transport|Plays|The DAC takes it|Set to|Rates" {
		t.Fatalf("#player-output-transports-panel's thead th cells are %q, want Transport, Plays, The DAC takes it, Set to and Rates", headings)
	}
	body := htmlByID(panel, "player-output-transports")
	if body == nil || body.Data != "tbody" || body.FirstChild != nil {
		t.Fatal("#player-output-transports-panel holds no empty tbody#player-output-transports for the script to fill")
	}

	code, raw := playerRequest(t, srv.Handler(), http.MethodGet, "/api/output/player", "", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/output/player = %d: %s", code, raw)
	}
	script := withFunctions(t, transportsHarness, readConsoleJS(t, "static/app.js"), "renderPlayerOutput", "renderPlayerOutputRunning",
		"renderPlayerOutputTransports", "renderPlayerOutputConfigured", "renderPlayerOutputOpenHome", "playerOpenHomeNote", "playerOutputText")
	out := runScriptUnderNode(t, node, script, map[string]any{"status": json.RawMessage(raw), "hidden": hiddenIDs(doc)})
	var got struct {
		PanelHidden bool `json:"panelHidden"`
		Rows        []struct {
			Tag   string `json:"tag"`
			Cells []struct {
				Tag  string `json:"tag"`
				Text string `json:"text"`
				Hint *struct {
					ClassName string `json:"className"`
					Text      string `json:"text"`
				} `json:"hint"`
			} `json:"cells"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node answered %s: %v", out, err)
	}
	if got.PanelHidden {
		t.Error("the shipped renderer leaves #player-output-transports-panel hidden over a status the player is available in")
	}
	var status PlayerStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatal(err)
	}
	notes := map[string]string{}
	for _, tr := range status.Transports {
		notes[tr.Kind] = tr.Note
	}
	want := []struct {
		label, kind string
		cells       []string
	}{
		{"PCM", "pcm", []string{"yes", "yes", "always on: no DSD choice applies to it", "every rate the DAC takes"}},
		{"DoP", "dop", []string{"no", "yes", "auto up to DSD64, this DAC's own choice; DoP allowed", "none"}},
		{"Native DSD", "native", []string{"yes", "yes", "auto up to DSD64, this DAC's own choice", "DSD64 DSD_U32_BE"}},
		{"DSD converted to PCM", "converted", []string{"yes", "yes", "auto up to DSD64, this DAC's own choice",
			"DSD128 to 176.4 kHz 24-bit PCM, DSD256 to 176.4 kHz 24-bit PCM"}},
	}
	if len(got.Rows) != len(want) {
		t.Fatalf("tbody#player-output-transports holds %d rows, want %d: %s", len(got.Rows), len(want), out)
	}
	for i, w := range want {
		row := got.Rows[i]
		if row.Tag != "tr" || len(row.Cells) != len(headings) {
			t.Errorf("row %d of tbody#player-output-transports: a %s of %d cells, want a tr of %d", i, row.Tag, len(row.Cells), len(headings))
			continue
		}
		name := row.Cells[0]
		if name.Tag != "td" || name.Text != w.label {
			t.Errorf("row %d's %s cell is %q, want %q", i, headings[0], name.Text, w.label)
		}
		if name.Hint == nil || name.Hint.ClassName != "hint" || name.Hint.Text != notes[w.kind] {
			t.Errorf("the %s row's note is %+v, want the status's %q in a .hint", w.label, name.Hint, notes[w.kind])
		}
		for j, cell := range w.cells {
			if c := row.Cells[j+1]; c.Tag != "td" || c.Text != cell || c.Hint != nil {
				t.Errorf("the %s row under %q shows %q, want %q", w.label, headings[j+1], c.Text, cell)
			}
		}
	}
}
