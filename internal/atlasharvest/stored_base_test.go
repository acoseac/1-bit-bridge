package atlasharvest

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/logging/loggingtest"
)

// storedSecret is what a hand edit writes into a stored base: a token as the
// user name, a password, a path segment, a query value. Mixed case, and every
// search for it ignores case: url.Parse lowercases a scheme, so a base written
// without one (`user:pw@host`) reaches an error as `user` lowercased, and a
// search that minds the case passes over it (the lesson of backlog B54).
const storedSecret = "s3cret-Pw"

// storedToken is the bearer token the edited state files hold.
const storedToken = "bh-harvest-token"

// containsFold reports whether s holds sub, whatever the case of either.
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// storedBaseShapes are the bases a hand edit can leave in atlas-harvest.json
// that the credential endpoint never stores (it stores
// baseurl.CredentialBase's scheme://host). hostPort is where the base points:
// a connRecorder, so a request built from it is seen, and fails.
var storedBaseShapes = []struct {
	name string
	base func(hostPort string) string
}{
	{"a token written as the user name", func(h string) string { return "https://" + storedSecret + "@" + h }},
	{"a user name and a password", func(h string) string { return "https://user:" + storedSecret + "@" + h }},
	{"a password alone", func(h string) string { return "https://:" + storedSecret + "@" + h }},
	{"no scheme, so the user name reads as one", func(h string) string { return storedSecret + ":pw@" + h }},
	{"a path", func(h string) string { return "https://" + h + "/" + storedSecret }},
	{"a query", func(h string) string { return "https://" + h + "?key=" + storedSecret }},
	{"a fragment", func(h string) string { return "https://" + h + "#" + storedSecret }},
	{"plain http", func(h string) string { return "http://" + h }},
	{"a port and no host", func(h string) string {
		_, port, _ := net.SplitHostPort(h)
		return "https://:" + port
	}},
	{"no host at all", func(string) string { return "https://" }},
}

// connRecorder is a TCP listener on the loopback interface that records the
// first bytes of every connection made to it (a TLS ClientHello, or a request
// in the clear) and then closes it. Every request to it fails, so a request
// built from a base on it reaches an error, and that error names the request
// URL: the path a stored base's user information took into the journal.
type connRecorder struct {
	ln   net.Listener
	mu   sync.Mutex
	seen [][]byte
}

func newConnRecorder(t *testing.T) *connRecorder {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &connRecorder{ln: ln}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
			buf := make([]byte, 4096)
			n, _ := c.Read(buf)
			_ = c.Close()
			r.mu.Lock()
			r.seen = append(r.seen, buf[:n])
			r.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	return r
}

func (r *connRecorder) hostPort() string { return r.ln.Addr().String() }

// connections returns the first bytes of every connection made so far.
func (r *connRecorder) connections() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.seen...)
}

// writeHarvestState writes a state file as a hand edit leaves it: a
// credential against base, a sync position, and a pending cover.
func writeHarvestState(t *testing.T, path, base string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"token":         storedToken,
		"atlasBaseUrl":  base,
		"expiresAt":     time.Now().Add(time.Hour).UTC(),
		"resultCursor":  42,
		"pendingCovers": map[string]int{relCoverOnly: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// dueTickClient is a client over state whose submit and poll legs are both
// due, with one artist to submit, logging through slog.Default (which the
// caller's loggingtest.Record captures).
func dueTickClient(state *StateStore) *Client {
	return &Client{
		State:          state,
		MBIDs:          &fakeMBIDs{ids: []string{"a1"}},
		Sink:           &fakeSink{},
		RequestTimeout: 5 * time.Second,
		BulkTimeout:    5 * time.Second,
	}
}

// mustNotHold fails t for each of subs that text holds, whatever the case of
// either; what names where text came from.
func mustNotHold(t *testing.T, what, text string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if containsFold(text, s) {
			t.Errorf("%s holds %q:\n%s", what, s, text)
		}
	}
}

// mustFileNotHold fails t for each of subs the file at path holds.
func mustFileNotHold(t *testing.T, path string, subs ...string) {
	t.Helper()
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mustNotHold(t, "the state file", string(onDisk), subs...)
}

// mustLogNeither fails t for each line rec holds, at any level, that carries
// the stored token or the stored secret.
func mustLogNeither(t *testing.T, rec *loggingtest.Recorder) {
	t.Helper()
	for _, line := range rec.All() {
		mustNotHold(t, "a log line", line, storedToken, storedSecret)
	}
}

// mustHoldNoCredential fails t unless state offers the premium cover fetch no
// credential and holds none, and kept the sync position writeHarvestState
// wrote: the drop is the credential's, not the file's.
func mustHoldNoCredential(t *testing.T, state *StateStore) {
	t.Helper()
	if _, _, ok := state.AtlasCredential(); ok {
		t.Error("AtlasCredential offers the premium cover fetch a credential against the stored base")
	}
	snap := state.Snapshot()
	if snap.Token != "" || snap.AtlasBaseURL != "" || !snap.ExpiresAt.IsZero() {
		t.Errorf("the store still holds the credential: token %q, base %q, expiry %v",
			snap.Token, snap.AtlasBaseURL, snap.ExpiresAt)
	}
	if snap.ResultCursor != 42 || snap.PendingCovers[relCoverOnly] != 1 {
		t.Errorf("the drop took the sync position with it: cursor %d, pending %v", snap.ResultCursor, snap.PendingCovers)
	}
}

// loggedTickError reports whether rec holds a tick_error of phase that names
// hostPort, the address the request went to.
func loggedTickError(rec *loggingtest.Recorder, phase, hostPort string) bool {
	for _, line := range rec.Failures("atlasharvest.tick_error") {
		if strings.Contains(line, "phase="+phase) && strings.Contains(line, hostPort) {
			return true
		}
	}
	return false
}

// TestAStoredBaseThatIsNotSchemeAndHostIsNeverUsed pins backlog B97 (and the
// stored-base half of B49): the harvest state store holds the Atlas base only
// in the one form the credential endpoint stores, scheme://host naming a
// host, so no reader of the store (the harvest client's legs, the booklet
// fetch, the lyrics tier, the premium cover fetch, which asks
// AtlasCredential) ever builds a request from anything else. A base a hand
// edit left in the file is dropped when the file is opened, with the
// credential held against it, the drop is written back so neither stays on
// disk, and one line says so. Before the fix the harvest client built every
// request URL from the stored base as written, so user information, a path,
// a query or a fragment reached every transport error it logged
// (`atlasharvest.tick_error`), plain http carried the bearer token in the
// clear, and a port with no host was dialled on this machine.
func TestAStoredBaseThatIsNotSchemeAndHostIsNeverUsed(t *testing.T) {
	for _, shape := range storedBaseShapes {
		t.Run(shape.name, func(t *testing.T) {
			rec := loggingtest.Record(t)
			atlas := newConnRecorder(t)
			path := filepath.Join(t.TempDir(), "atlas-harvest.json")
			writeHarvestState(t, path, shape.base(atlas.hostPort()))

			state, err := OpenStateStore(path)
			if err != nil {
				t.Fatalf("OpenStateStore: %v", err)
			}
			mustHoldNoCredential(t, state)
			mustFileNotHold(t, path, storedToken, storedSecret)
			if got := rec.Lines("atlasharvest.state.base_refused"); len(got) != 1 {
				t.Errorf("logged the drop %d times, want once: %v", len(got), rec.All())
			}

			dueTickClient(state).tick(context.Background())

			for _, c := range atlas.connections() {
				t.Errorf("the harvest client connected to the stored base's address (first bytes %q)", c)
			}
			mustLogNeither(t, rec)
		})
	}

	// The harness sees a request and its error when there is one: over a
	// base in the stored form, on the same address, every due leg connects,
	// fails, and logs an error that names the address. Without this, the
	// cases above would pass over a client that never asked the store.
	t.Run("a base in the stored form is used, and its errors are logged", func(t *testing.T) {
		rec := loggingtest.Record(t)
		atlas := newConnRecorder(t)
		path := filepath.Join(t.TempDir(), "atlas-harvest.json")
		writeHarvestState(t, path, "https://"+atlas.hostPort())

		state, err := OpenStateStore(path)
		if err != nil {
			t.Fatalf("OpenStateStore: %v", err)
		}
		if _, base, ok := state.AtlasCredential(); !ok || base != "https://"+atlas.hostPort() {
			t.Fatalf("AtlasCredential = %q, %v; want the stored base", base, ok)
		}
		dueTickClient(state).tick(context.Background())

		if n := len(atlas.connections()); n < 2 {
			t.Errorf("the harvest client made %d connections, want one per due leg (submit, poll)", n)
		}
		for _, phase := range []string{"submit", "poll"} {
			if !loggedTickError(rec, phase, atlas.hostPort()) {
				t.Errorf("no tick_error with phase=%s naming %s: %v", phase, atlas.hostPort(), rec.All())
			}
		}
		if got := rec.Lines("atlasharvest.state.base_refused"); len(got) != 0 {
			t.Errorf("a base in the stored form was reported as refused: %v", got)
		}
	})
}

// TestARevokeLeavesNoStoredBaseBehind pins the harvest-off revoke over an
// edited file: ClearStoredCredential opens the store, whose drop is written
// back, so neither the token nor the base it could not be used with stays in
// the file. Before the fix the revoke cleared the token and kept the base,
// user information included.
func TestARevokeLeavesNoStoredBaseBehind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atlas-harvest.json")
	writeHarvestState(t, path, "https://"+storedSecret+"@atlas.example")
	if err := ClearStoredCredential(path); err != nil {
		t.Fatal(err)
	}
	mustFileNotHold(t, path, storedToken, storedSecret)
}

// heldCredential is a store holding a credential against
// https://atlas.example at cursor 42, the file it was written to, and that
// file's stat, for a test that must show a refused write changed neither.
type heldCredential struct {
	state   *StateStore
	path    string
	expires time.Time
	file    os.FileInfo
}

func newHeldCredential(t *testing.T) heldCredential {
	t.Helper()
	h := heldCredential{
		path:    filepath.Join(t.TempDir(), "atlas-harvest.json"),
		expires: time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	}
	h.state = mustOpenState(t, h.path)
	if err := h.state.SetCredential("bh-held", "https://atlas.example", h.expires); err != nil {
		t.Fatal(err)
	}
	if err := h.state.SetCursor(42); err != nil {
		t.Fatal(err)
	}
	var err error
	if h.file, err = os.Stat(h.path); err != nil {
		t.Fatal(err)
	}
	return h
}

// mustBeUnchanged fails t when the store or its file moved since
// newHeldCredential. Every write stages a new file and renames it over the
// path, so a rewrite is another file (CLAUDE.md's rule against mtimes).
func (h heldCredential) mustBeUnchanged(t *testing.T) {
	t.Helper()
	snap := h.state.Snapshot()
	if snap.Token != "bh-held" || snap.AtlasBaseURL != "https://atlas.example" ||
		!snap.ExpiresAt.Equal(h.expires) || snap.ResultCursor != 42 {
		t.Errorf("a refused SetCredential changed the store: %+v", snap)
	}
	after, err := os.Stat(h.path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(h.file, after) {
		t.Error("a refused SetCredential rewrote the state file")
	}
}

// TestSetCredentialRefusesABaseThatIsNotSchemeAndHost pins the store's other
// entry point: SetCredential refuses a base CredentialBase does not reduce to
// scheme://host, before it touches anything. The credential it held, the
// sync position (which a changed base resets) and the file stay as they
// were, and the error names no part of the base. The endpoint refuses such a
// base on the wire first, so this is the backstop for every other caller.
func TestSetCredentialRefusesABaseThatIsNotSchemeAndHost(t *testing.T) {
	for _, shape := range storedBaseShapes {
		t.Run(shape.name, func(t *testing.T) {
			h := newHeldCredential(t)
			err := h.state.SetCredential(storedToken, shape.base("atlas.example:8443"), time.Now().Add(2*time.Hour))
			if err == nil {
				t.Fatal("SetCredential stored the base")
			}
			mustNotHold(t, "the refusal", err.Error(), storedToken, storedSecret, "atlas.example:8443")
			h.mustBeUnchanged(t)
		})
	}
	t.Run("an empty base", func(t *testing.T) {
		state := mustOpenState(t, filepath.Join(t.TempDir(), "atlas-harvest.json"))
		if err := state.SetCredential("tok", "", time.Time{}); err == nil {
			t.Error("SetCredential stored a credential with no base")
		}
		if _, _, ok := state.AtlasCredential(); ok {
			t.Error("ok=true with an empty base URL")
		}
	})
}

// TestTheStoreHoldsABaseInItsCanonicalForm pins the other half of "one
// value": a base that reduces to scheme://host (a trailing slash, the default
// port or an empty one, an uppercase scheme, surrounding space) is held in
// that reduced form, through SetCredential and through a file that holds the
// long form, which keeps its credential and its sync position. So a
// re-provision of the same host, which the endpoint sends in the reduced
// form, is not taken for a new Atlas and does not reset the cursor.
func TestTheStoreHoldsABaseInItsCanonicalForm(t *testing.T) {
	const want = "https://atlas.example"
	for _, in := range []string{
		"https://atlas.example/",
		"https://atlas.example:443",
		"https://atlas.example:",
		"HTTPS://atlas.example:443/",
		" https://atlas.example ",
	} {
		t.Run("SetCredential "+in, func(t *testing.T) {
			state := mustOpenState(t, filepath.Join(t.TempDir(), "atlas-harvest.json"))
			if err := state.SetCredential("tok", in, time.Time{}); err != nil {
				t.Fatalf("SetCredential: %v", err)
			}
			if _, base, ok := state.AtlasCredential(); !ok || base != want {
				t.Errorf("AtlasCredential = %q, %v; want %q", base, ok, want)
			}
		})
		t.Run("a file holding "+in, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "atlas-harvest.json")
			writeHarvestState(t, path, in)
			state, err := OpenStateStore(path)
			if err != nil {
				t.Fatal(err)
			}
			token, base, ok := state.AtlasCredential()
			if !ok || token != storedToken || base != want {
				t.Fatalf("AtlasCredential = %q, %q, %v; want the stored token against %q", token, base, ok, want)
			}
			if err := state.SetCredential("bh-renewed", want, time.Time{}); err != nil {
				t.Fatal(err)
			}
			if got := state.Snapshot().ResultCursor; got != 42 {
				t.Errorf("a re-provision of the same host reset the cursor to %d, want 42", got)
			}
		})
	}
}

// TestOpeningAStoreThatCannotDropItsBaseFails pins what happens when the
// drop cannot be written back: the open fails, naming no part of the base,
// rather than answer as though the file no longer held the credential (a
// harvest-off revoke would then report gone a credential that is still on
// disk). serve reports the failure and runs without the harvest, as for any
// state file it cannot open.
func TestOpeningAStoreThatCannotDropItsBaseFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "atlas-harvest.json")
	writeHarvestState(t, path, "https://"+storedSecret+"@atlas.example")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot make the state directory read-only here")
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	_, err := OpenStateStore(path)
	if err == nil {
		onDisk, rerr := os.ReadFile(path)
		if rerr == nil && !containsFold(string(onDisk), storedToken) {
			t.Skip("the write went through a read-only directory (root, or Windows ACLs): nothing to observe")
		}
		t.Fatal("the open answered though the drop was not written, and the file still holds the credential")
	}
	mustNotHold(t, "the error", err.Error(), storedToken, storedSecret)
}
