package admin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestASameSizeEditDoesNotResume runs the shipped resume match. A file
// retagged in place keeps its path and its size, and the staged prefix then
// belongs to a different version of the file. The match has to see that.
func TestASameSizeEditDoesNotResume(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped client source")
	}
	js := readFile(t, "static/app.js")
	driver := `let resumableSessions = [];
` + extractJSFunction(t, js, "resumeIdentityComplete") + `
` + extractJSFunction(t, js, "sessionKey") + `
` + extractJSFunction(t, js, "findResumable") + `
` + extractJSFunction(t, js, "findFilesOnlyMatch") + `
const errors = [];
function check(cond, msg) { if (!cond) errors.push(msg); }
const fp1 = "ab".repeat(64);
const fp2 = "cd".repeat(64);
const pick = (modified, fingerprint) => [{
  path: "Album/01.flac",
  file: { size: 64 },
  modified,
  fingerprint,
}];
resumableSessions = [{
  id: "staged",
  overwrite: false,
  files: [{ path: "Album/01.flac", size: 64, modified: 1700000000000, fingerprint: fp1 }],
}];
check(!!findResumable(pick(1700000000000, fp1), false), "the same file did not resume");
check(!findResumable(pick(1700000000001, fp2), false), "a same-size edit resumed");
check(!findResumable(pick(1700000000000, fp2), false), "a file with the same mtime and a different head resumed");
check(!findResumable(pick(1700000000000, fp1), true), "a resume ignored the overwrite choice");
check(!!findFilesOnlyMatch(pick(1700000000000, fp1)), "an overwrite mismatch was not explained");
resumableSessions = [{
  id: "old",
  overwrite: false,
  files: [{ path: "Album/01.flac", size: 64 }],
}];
check(!findResumable(pick(1700000000000, fp1), false), "a session written before identity resumed");
check(!findFilesOnlyMatch(pick(1700000000000, fp1)), "a session written before identity still counted as the same files");
if (errors.length) {
  process.stderr.write(errors.join("\n") + "\n");
  process.exit(1);
}
process.stdout.write("ok\n");
`
	dir := t.TempDir()
	script := filepath.Join(dir, "resume.mjs")
	if err := os.WriteFile(script, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// TestUploadSHA256MatchesNode runs the shipped hasher and the session
// create against node:crypto. The console sends that digest so a resumed
// upload of a different version of the same-sized file fails the server's
// existing whole-file check.
func TestUploadSHA256MatchesNode(t *testing.T) {
	runUploadClient(t, []string{
		"createSHA256", "sha256Bytes", "sha256File", "fileFingerprint",
		"uploadAbortError", "attachUploadIdentity", "createUploadSession",
	}, `import { createHash } from "node:crypto";
let uploadState = { aborted: false };
function setText() {}
function formatBytes(n) { return String(n); }
const els = {};
const document = {
  getElementById(id) {
    if (!els[id]) els[id] = { hidden: true, value: 0 };
    return els[id];
  },
};
const posted = [];
const API = {
  async post(_url, body) {
    posted.push(body);
    return { id: "s", files: body.files };
  },
};
`, `
function nodeHash(bytes) {
  return createHash("sha256").update(bytes).digest("hex");
}
const errors = [];
function check(cond, msg) { if (!cond) errors.push(msg); }
const abc = new TextEncoder().encode("abc");
check(sha256Bytes(new Uint8Array(0)) === nodeHash(new Uint8Array(0)), "empty");
check(sha256Bytes(abc) === "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "abc");
for (const n of [63, 64, 65, 200]) {
  const buf = new Uint8Array(n);
  for (let i = 0; i < n; i++) buf[i] = (i * 17 + 3) & 255;
  const h = createSHA256();
  const mid = Math.floor(n / 2);
  h.update(buf.subarray(0, mid));
  h.update(buf.subarray(mid));
  check(h.hex() === nodeHash(buf), "len " + n);
}
const big = new Uint8Array(70000);
for (let i = 0; i < big.length; i++) big[i] = (i * 13 + 7) & 255;
const file = new File([big], "01.flac", { lastModified: 1700000000000 });
const digest = await sha256File(file);
check(digest === nodeHash(big), "file digest");
const fp = await fileFingerprint(file);
check(fp === nodeHash(big.subarray(0, 65536)) + nodeHash(big.subarray(big.length - 65536)), "fingerprint");
const small = new File([abc], "a.flac", { lastModified: 5 });
const smallFP = await fileFingerprint(small);
check(smallFP === nodeHash(abc) + nodeHash(abc), "small fingerprint");
const empty = new File([], "empty.flac", { lastModified: 1 });
const emptyHex = nodeHash(new Uint8Array(0));
check(await fileFingerprint(empty) === emptyHex + emptyHex, "empty fingerprint");
const picked = [{ path: "Album/01.flac", file }];
await attachUploadIdentity(picked);
check(picked[0].modified === 1700000000000, "modified");
check(picked[0].fingerprint === fp, "attached fingerprint");
await createUploadSession(picked, false);
const declared = posted[0].files[0];
check(declared.digest === digest, "declared digest");
check(declared.fingerprint === fp, "declared fingerprint");
check(declared.modified === 1700000000000, "declared modified");
check(els["upload-progress"] && els["upload-progress"].hidden === false, "progress shown");
check(els["upload-review"] === undefined, "review left alone");
picked[0].file = new File([abc], "01.flac", { lastModified: 1700000000000 });
await createUploadSession(picked, false);
check(posted[1].files[0].digest === digest, "digest not recomputed");
const fresh = [{ path: "Album/02.flac", file: new File([abc], "02.flac", { lastModified: 9 }) }];
uploadState.aborted = true;
let aborted = false;
try { await createUploadSession(fresh, false); } catch (e) { aborted = e && e.code === "aborted"; }
check(aborted, "abort during hash");
if (errors.length) {
  process.stderr.write(errors.join("\n") + "\n");
  process.exit(1);
}
process.stdout.write("ok\n");
`)
}

func runUploadClient(t *testing.T, names []string, prelude, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; this test executes the shipped client source")
	}
	js := readFile(t, "static/app.js")
	var b strings.Builder
	b.WriteString(prelude)
	for _, name := range names {
		b.WriteString(extractJSFunction(t, js, name))
		b.WriteByte('\n')
	}
	b.WriteString(body)
	dir := t.TempDir()
	script := filepath.Join(dir, "upload-client.mjs")
	if err := os.WriteFile(script, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
