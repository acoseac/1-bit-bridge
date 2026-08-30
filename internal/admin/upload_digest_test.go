package admin

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The client computes a per-chunk SHA-256 and sends it as RFC 9530
// Content-Digest; the server has always verified that header and skipped the
// check when absent. These tests pin the two halves that a source scan alone
// cannot: that the header the BROWSER builds is one the SERVER accepts, and
// that a rejected chunk is re-sent rather than killing the upload.

// extractJSFunction slices one top-level function out of app.js by name.
// Shared by the app.js-scanning tests in this package.
//
// The "(" in the anchor is load-bearing: without it a name prefix-matches any
// LONGER name that starts the same way, and the scan silently examines the
// wrong function. That is not hypothetical — putUploadChunkVerified wraps
// putUploadChunk, and a bare-prefix scan for the latter found the former.
//
// readFile normalizes CRLF, so the "\n}\n" terminator is present on every
// platform (see its docblock — this has bitten three times).
func extractJSFunction(t *testing.T, src, name string) string {
	t.Helper()
	start := strings.Index(src, "async function "+name+"(")
	if start < 0 {
		t.Fatalf("app.js: no function %s", name)
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		t.Fatalf("app.js: unterminated function %s", name)
	}
	return src[start : start+end+3]
}

// TestClientChunkDigestIsAcceptedByTheServerParser runs the REAL client
// function under node and feeds its output to the REAL server parser.
//
// A format disagreement here — padding, the byte-sequence colons, the member
// name — would 400 every chunk and break uploads outright, and neither a
// source scan nor a hand-written Go replica of the client would catch it,
// because a replica asserts its author's beliefs rather than the shipped code.
func TestClientChunkDigestIsAcceptedByTheServerParser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped client source")
	}
	fn := extractJSFunction(t, readFile(t, "static/app.js"), "chunkDigestHeaders")

	// Arbitrary but fixed BINARY, carried as explicit byte values.
	//
	// A chunk is arbitrary bytes, so the fixture must be too — and it cannot
	// travel as a string: an earlier version of this test sent one through
	// TextEncoder, where Go's json.Marshal had already replaced the invalid
	// UTF-8 byte with U+FFFD, so the two sides hashed different inputs. A
	// payload chosen to be valid UTF-8 would have hidden that rather than
	// fixing it.
	payload := make([]byte, 0, 300)
	for i := 0; i < 300; i++ {
		payload = append(payload, byte(i*7%256))
	}
	driver := "let uploadState = null;\n" + fn + `
(async () => {
  const bytes = new Uint8Array(` + jsBytes(payload) + `);
  const h = await chunkDigestHeaders(bytes);
  process.stdout.write(JSON.stringify(h));
})();
`
	dir := t.TempDir()
	script := filepath.Join(dir, "digest.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var hdr map[string]string
	if err := json.Unmarshal(out, &hdr); err != nil {
		t.Fatalf("client returned %q, not a header object: %v", out, err)
	}
	got, ok := hdr["content-digest"]
	if !ok {
		t.Fatalf("client sent no content-digest header: %v", hdr)
	}

	parsed, err := parseContentDigestSHA256(got)
	if err != nil {
		t.Fatalf("the server refuses the header the client builds (%q): %v", got, err)
	}
	want := sha256.Sum256(payload)
	if string(parsed) != string(want[:]) {
		t.Fatalf("digest mismatch:\n client header %q\n parsed        %x\n want          %x", got, parsed, want)
	}
}

// jsBytes renders bytes as a JS array literal, which survives the trip
// unambiguously where a string literal does not.
func jsBytes(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = strconv.Itoa(int(c))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestChunkUploadSendsAndRetriesTheDigest pins the wiring a runtime test
// cannot reach without a DOM: that the chunk PUT carries the header at all,
// and that a rejected chunk is retried rather than aborting the upload.
func TestChunkUploadSendsAndRetriesTheDigest(t *testing.T) {
	js := readFile(t, "static/app.js")

	put := extractJSFunction(t, js, "putUploadChunk")
	if !strings.Contains(put, "chunkDigestHeaders(") {
		t.Error("putUploadChunk does not attach a Content-Digest; chunks would be size-verified only")
	}

	// The digest must cover the bytes actually sent. Handing fetch a Blob
	// while hashing something else would verify nothing.
	if !strings.Contains(put, "body: bytes") {
		t.Error("putUploadChunk must send the same buffer it hashed")
	}

	retry := extractJSFunction(t, js, "putUploadChunkVerified")
	if !strings.Contains(retry, `e.code !== "digest_mismatch"`) {
		t.Error("the retry must be scoped to digest_mismatch, not to every failure")
	}
	if !strings.Contains(retry, "UPLOAD_DIGEST_RETRIES") {
		t.Error("the retry must be bounded")
	}
	if !strings.Contains(js, "const UPLOAD_DIGEST_RETRIES") {
		t.Error("UPLOAD_DIGEST_RETRIES is not defined")
	}

	// Recognising the failure depends on the code surviving errorFromResponse,
	// which used to keep only the human-readable message.
	efr := extractJSFunction(t, js, "errorFromResponse")
	if !strings.Contains(efr, "e.code = j.error") {
		t.Error("errorFromResponse must preserve the server's error code")
	}
}
