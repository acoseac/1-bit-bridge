package admin

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The pages the home-network socket serves (backlog B989, the plan's §6.2,
// §6.3): each page knows the socket it was served on, which decides the
// pairing and rotation warnings, and the upload's notes say what a page with
// no SubtleCrypto checks.

// pageOn renders path on the LAN socket, or on loopback where lanSocket is
// false.
func pageOn(t *testing.T, srv *Server, path string, lanSocket bool) string {
	t.Helper()
	var r *http.Request
	if lanSocket {
		r = lanRequest(testLANPolicy(t, testLANAddr, testLANNet, 7789, ""), http.MethodGet, path, testLANPeer, testLANHost, nil)
	} else {
		r = loopbackRequest(http.MethodGet, path, testConsoleHost, nil)
	}
	rr := serveTo(srv, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", path, rr.Code)
	}
	return rr.Body.String()
}

// lanPageOf renders path's own page handler for a request the LAN socket
// carries, as Handler() hands it on once the request has passed the
// boundary: what the page says on that socket, apart from who may reach it.
func lanPageOf(t *testing.T, srv *Server, path string) string {
	t.Helper()
	r := lanRequest(testLANPolicy(t, testLANAddr, testLANNet, 7789, ""), http.MethodGet, path, testLANPeer, testLANHost, nil)
	rr := httptest.NewRecorder()
	switch path {
	case "/devices":
		srv.pageDevices(rr, r)
	case "/stats":
		srv.pageStats(rr, r)
	default:
		t.Fatalf("no page handler for %s", path)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("%s: %d", path, rr.Code)
	}
	return rr.Body.String()
}

// TestThePairingDialogWarnsBeforeGenerateOnTheLANSocket: a page the LAN
// socket served renders §6.2's warning in the dialog's form step, before
// Generate token in the document's order, and the recovery advice beside the
// result's token, which offers no rotation from the page; a page the
// loopback socket served renders neither (D9, F6, F8, F10). Asked of the
// page's own handler first, then through Handler().
func TestThePairingDialogWarnsBeforeGenerateOnTheLANSocket(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{"/devices", "/stats"} {
		lanPage := lanPageOf(t, srv, path)
		warn := strings.Index(lanPage, `id="pair-lan-warning"`)
		gen := strings.Index(lanPage, "Generate token")
		token := strings.Index(lanPage, `id="pair-token"`)
		advice := strings.Index(lanPage, `id="pair-lan-recovery"`)
		if warn < 0 || gen < 0 || warn > gen {
			t.Errorf("%s on the LAN socket: the warning at %d, Generate token at %d", path, warn, gen)
		}
		if advice < 0 || advice < token {
			t.Errorf("%s on the LAN socket: the recovery advice at %d, the token at %d", path, advice, token)
		}
		for _, want := range []string{
			"The token you generate here will cross your network in the clear",
			"pair from the bridge's own machine, or over an SSH tunnel to it",
			"This token crossed your network unencrypted",
			"Rotating from this page would send the new token across the network the same way.",
		} {
			if !strings.Contains(lanPage, want) {
				t.Errorf("%s on the LAN socket does not say %q", path, want)
			}
		}
		if !strings.Contains(lanPage, `data-socket="lan"`) {
			t.Errorf("%s on the LAN socket: the body does not say its socket", path)
		}
		loopPage := pageOn(t, srv, path, false)
		if strings.Contains(loopPage, `id="pair-lan-warning"`) || strings.Contains(loopPage, `id="pair-lan-recovery"`) {
			t.Errorf("%s on the loopback socket renders the LAN warnings", path)
		}
		if !strings.Contains(loopPage, `data-socket="loopback"`) {
			t.Errorf("%s on the loopback socket: the body does not say its socket", path)
		}
		if via := pageOn(t, srv, path, true); !strings.Contains(via, `id="pair-lan-warning"`) {
			t.Errorf("%s through Handler() on the LAN socket renders no warning", path)
		}
	}
}

// TestARotationOnTheLANSocketWarnsBeforeItIsSent: on a page the LAN socket
// served, Rotate's confirmation carries §6.2's warning before any request is
// sent, a confirmation refused sends nothing, and the warning offers no
// rotation from the page (F6, F10). Held to the shipped handler's source:
// the confirm decides before the rotate's POST, its words gain the warning
// only where the body says the LAN socket served the page, and the warning's
// words point at the bridge's own machine.
func TestARotationOnTheLANSocketWarnsBeforeItIsSent(t *testing.T) {
	src := readFile(t, "static/app.js")
	body := extractJSFunction(t, src, "initDevices")
	i := strings.Index(body, `document.querySelectorAll(".rotate-token")`)
	if i < 0 {
		t.Fatal("initDevices wires no rotate button")
	}
	handler := body[i:]
	ask := strings.Index(handler, `document.body.dataset.socket === "lan"`)
	warn := strings.Index(handler, "lanRotationWarning()")
	confirmAt := strings.Index(handler, "if (!confirm(ask)) return;")
	post := strings.Index(handler, "/rotate`")
	if ask < 0 || warn < 0 || confirmAt < 0 || post < 0 || !(ask < confirmAt && warn < confirmAt && confirmAt < post) {
		t.Fatalf("the rotate handler: LAN check at %d, warning at %d, confirm at %d, POST at %d", ask, warn, confirmAt, post)
	}
	line := extractJSFunction(t, src, "lanRotationWarning")
	for _, want := range []string{"not encrypted", "the bridge's own machine", "SSH tunnel"} {
		if !strings.Contains(line, want) {
			t.Errorf("the rotation's warning %q does not say %q", line, want)
		}
	}
	if strings.Contains(line, "Rotate from this page") {
		t.Errorf("the rotation's warning offers a rotation from the page: %q", line)
	}
}

// TestAnUploadWithoutSubtleCryptoSendsNoDigestAndSaysSo: with no
// SubtleCrypto (a page on plain HTTP, which is no secure context), a chunk
// carries no Content-Digest (run under node where node is installed), and
// the upload's end says length only, naming the real cause rather than the
// browser (R5).
func TestAnUploadWithoutSubtleCryptoSendsNoDigestAndSaysSo(t *testing.T) {
	src := readFile(t, "static/app.js")
	if strings.Contains(src, "this browser did not expose SHA-256") {
		t.Error("the upload's end still blames the browser")
	}
	report := extractJSFunction(t, src, "reportCommit")
	if !strings.Contains(report, "uploadLengthOnlyEndText()") {
		t.Error("reportCommit does not say what a page with no SubtleCrypto checked")
	}
	if !strings.Contains(extractJSFunction(t, src, "uploadLengthOnlyEndText"), "only on the bridge's own machine or over HTTPS") {
		t.Error("the end's words do not name the cause")
	}
	// Held structurally where node is not installed: with no SubtleCrypto the
	// chunk's headers are none, decided before any digest is asked for.
	digest := extractJSFunction(t, src, "chunkDigestHeaders")
	guard, use := strings.Index(digest, "if (!subtle) return null;"), strings.Index(digest, "subtle.digest(")
	if guard < 0 || use < 0 || guard > use {
		t.Errorf("chunkDigestHeaders does not answer no digest before it asks SubtleCrypto: the guard at %d, the digest at %d", guard, use)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the digest half executes the shipped client source")
	}
	driver := `globalThis.crypto = {};
` + extractJSFunction(t, src, "chunkDigestHeaders") + `
(async () => {
  const h = await chunkDigestHeaders(new Uint8Array([1, 2, 3]));
  process.stdout.write(JSON.stringify(h));
})();
`
	dir := t.TempDir()
	script := filepath.Join(dir, "nosubtle.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "null" {
		t.Fatalf("with no SubtleCrypto the chunk's headers were %s, want none", out)
	}
}

// TestTheUploadPageSaysChunksAreCheckedForLengthOnly: §6.3's note before an
// upload, on a page that is no secure context, and none on one that is (run
// under node where node is installed); the page carries its place and the
// upload's wiring fills it before any upload begins.
func TestTheUploadPageSaysChunksAreCheckedForLengthOnly(t *testing.T) {
	tmpl, err := os.ReadFile(filepath.Join("templates", "upload.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tmpl), `id="upload-length-only"`) {
		t.Fatal("the upload page has no place for the length-only note")
	}
	src := readFile(t, "static/app.js")
	if !strings.Contains(extractJSFunction(t, src, "wireUpload"), "uploadLengthOnlyNote();") {
		t.Fatal("wireUpload does not say what the page checks before an upload")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the note's half executes the shipped client source")
	}
	driver := extractJSFunction(t, src, "uploadLengthOnlyNoteText") + extractJSFunction(t, src, "uploadLengthOnlyNote") + `
const el = { textContent: "", hidden: true };
globalThis.document = { getElementById: () => el };
const out = [];
globalThis.isSecureContext = false;
globalThis.crypto = {};
uploadLengthOnlyNote();
out.push({ text: el.textContent, hidden: el.hidden });
globalThis.isSecureContext = true;
globalThis.crypto = { subtle: {} };
uploadLengthOnlyNote();
out.push({ text: el.textContent, hidden: el.hidden });
process.stdout.write(JSON.stringify(out));
`
	dir := t.TempDir()
	script := filepath.Join(dir, "note.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	got := string(out)
	if !strings.Contains(got, `"text":"From this device, uploads are checked for length only`) || !strings.Contains(got, `"hidden":false`) ||
		!strings.Contains(got, `{"text":"","hidden":true}`) {
		t.Fatalf("the note read %s", got)
	}
}

// TestThePairingCopyFallsBackWithoutTheClipboard: with no
// navigator.clipboard (a page that is no secure context), a Copy button
// selects its text for a copy by hand and throws nothing (R5): its handler's
// catch selects the node's contents (a green control: the catch's selection
// removed turns it red).
func TestThePairingCopyFallsBackWithoutTheClipboard(t *testing.T) {
	src := readFile(t, "static/app.js")
	body := extractJSFunction(t, src, "initPairModal")
	i := strings.Index(body, "navigator.clipboard.writeText(el.textContent)")
	if i < 0 {
		t.Fatal("the Copy handler no longer writes the clipboard")
	}
	rest := body[i:]
	catch := strings.Index(rest, "} catch {")
	sel := strings.Index(rest, "range.selectNodeContents(el);")
	add := strings.Index(rest, "sel.addRange(range);")
	if catch < 0 || sel < catch || add < sel {
		t.Fatalf("the Copy handler's catch does not select the text: catch at %d, selectNodeContents at %d, addRange at %d", catch, sel, add)
	}
}
