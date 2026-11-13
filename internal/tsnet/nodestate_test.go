package tsnet

import (
	"bytes"
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/ipn/store"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/types/key"
	"tailscale.com/types/persist"
)

// The node's name (backlog B749): tailscale.hostname wins; a node registered
// in the state dir keeps the name its persisted current profile carries; a new
// node is DefaultHostname; a state that is there and cannot be read keeps the
// name earlier versions gave every node, with one line saying why. The state's
// fixtures are written by upstream's own writers wherever its API allows: its
// file store (store.NewFileStore, which writes "{}" as a first start does, and
// WriteState), its prefs' serializer (Prefs.ToBytes) and its profile types'
// JSON, as the profile manager writes them. Only a file that does not parse is
// typed by hand, since no writer of upstream's makes one.

// stateFixture is a state dir written as upstream writes it.
type stateFixture struct {
	t   *testing.T
	dir string
	st  ipn.StateStore
}

// newStateFixture opens upstream's file store at dir's state file, which
// writes "{}" there, as upstream's start does before any authentication.
func newStateFixture(t *testing.T, dir string) *stateFixture {
	t.Helper()
	st, err := store.NewFileStore(t.Logf, filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatalf("upstream's file store: %v", err)
	}
	return &stateFixture{t: t, dir: dir, st: st}
}

func (f *stateFixture) write(k ipn.StateKey, v []byte) {
	f.t.Helper()
	if err := f.st.WriteState(k, v); err != nil {
		f.t.Fatalf("WriteState(%q): %v", k, err)
	}
}

// machineKey writes the machine key, which upstream's start writes before
// any authentication: the state an abandoned authentication leaves.
func (f *stateFixture) machineKey() {
	f.t.Helper()
	b, err := key.NewMachine().MarshalText()
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(ipn.MachineKeyStateKey, b)
}

const fixtureProfile = ipn.ProfileID("b749")

// register writes a registered node's profile, as the profile manager does
// once control has given the node an identity: the known profiles, the
// profile's prefs (with the node's identity in Persist) under its key, and
// the current profile's key. attest, when set, is a hardware attestation key
// the prefs carry.
func (f *stateFixture) register(hostname string, nodeID tailcfg.StableNodeID, attest key.HardwareAttestationKey) {
	f.t.Helper()
	k := ipn.StateKey("profile-" + string(fixtureProfile))
	prefs := ipn.NewPrefs()
	prefs.Hostname = hostname
	prefs.WantRunning = true
	prefs.Persist = &persist.Persist{
		PrivateNodeKey: key.NewNode(),
		NodeID:         nodeID,
		UserProfile:    tailcfg.UserProfile{LoginName: "owner@example.com"},
		AttestationKey: attest,
	}
	known := map[ipn.ProfileID]ipn.LoginProfileView{
		fixtureProfile: (&ipn.LoginProfile{ID: fixtureProfile, Key: k, Name: "owner@example.com", NodeID: nodeID}).View(),
	}
	b, err := json.Marshal(known)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(ipn.KnownProfilesStateKey, b)
	f.write(k, prefs.ToBytes())
	f.write(ipn.CurrentProfileKey(""), []byte(k))
}

// logout leaves the state as upstream's logout does once it deleted the
// profile: its prefs and its entry in the known profiles gone, the current
// profile's key left as it was.
func (f *stateFixture) logout() {
	f.t.Helper()
	f.write(ipn.StateKey("profile-"+string(fixtureProfile)), nil)
	b, err := json.Marshal(map[ipn.ProfileID]ipn.LoginProfileView{})
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(ipn.KnownProfilesStateKey, b)
}

// testAttestationKey is a hardware attestation key whose JSON is not null,
// as a node that holds one stores it with its prefs.
type testAttestationKey struct{}

func (*testAttestationKey) Public() crypto.PublicKey { return nil }
func (*testAttestationKey) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return nil, errors.New("test key")
}
func (*testAttestationKey) MarshalJSON() ([]byte, error)        { return []byte(`{"tpm":"sealed"}`), nil }
func (*testAttestationKey) UnmarshalJSON([]byte) error          { return nil }
func (*testAttestationKey) Close() error                        { return nil }
func (k *testAttestationKey) Clone() key.HardwareAttestationKey { return k }
func (*testAttestationKey) IsZero() bool                        { return false }

// keepNoLogsEnv makes the test's changes to NoLogsEnv its own: NewServer
// applies the upload policy, and the variable, and what the policy keeps of
// what it did to it in this process, are put back as they were when the test
// ends. The test starts as a process that inherited no NoLogsEnv and has
// built no node.
func keepNoLogsEnv(t *testing.T) {
	t.Helper()
	t.Setenv(NoLogsEnv, "")
	if err := os.Unsetenv(NoLogsEnv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ForgetUploadPolicyForTest())
}

// startForName runs one Start of a server on stateDir built from cfg, its
// upstream node stood in for, and returns the name the upstream server was
// built with and what the server logged.
func startForName(t *testing.T, cfg Config) (string, string) {
	t.Helper()
	keepNoLogsEnv(t)
	var logs bytes.Buffer
	cfg.Logger = captureLogger(&logs)
	var name string
	cfg.BeforeStart = func(ts *tsnet.Server) error {
		name = ts.Hostname
		return nil
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	s.newNode = func(*tsnet.Server) node { return &fakeNode{} }
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return name, logs.String()
}

const unreadableLine = "so this start cannot tell which name the node has"

// TestTheNodeNameIsTheRegisteredOneOrTheDefault: every state a start names
// the node from, and the name the upstream server is built with. A state file
// that holds nothing names none: it refuses the start
// (TestAStateFileThatHoldsNothingRefusesTheStart, backlog B828).
func TestTheNodeNameIsTheRegisteredOneOrTheDefault(t *testing.T) {
	const library = "Living Room"
	for _, c := range []struct {
		name     string
		hostname string // tailscale.hostname
		state    func(t *testing.T, dir string)
		want     string
		warned   bool
	}{
		{"no state dir yet", "", nil, DefaultHostname, false},
		{"no state file", "", func(t *testing.T, dir string) {}, DefaultHostname, false},
		{"the {} a first start writes", "", func(t *testing.T, dir string) {
			newStateFixture(t, dir)
		}, DefaultHostname, false},
		{"an authentication that never finished", "", func(t *testing.T, dir string) {
			newStateFixture(t, dir).machineKey()
		}, DefaultHostname, false},
		{"a profile with no node identity", "", func(t *testing.T, dir string) {
			f := newStateFixture(t, dir)
			f.machineKey()
			f.register("Half Way", "", nil)
		}, DefaultHostname, false},
		{"a node logged out", "", func(t *testing.T, dir string) {
			f := newStateFixture(t, dir)
			f.machineKey()
			f.register("Gone Node", "nStable1", nil)
			f.logout()
		}, DefaultHostname, false},
		{"a current profile the known profiles do not list", "", func(t *testing.T, dir string) {
			// Upstream's start takes such a profile for none
			// (profileManager.findProfileByKey) and starts a new one.
			f := newStateFixture(t, dir)
			f.register("Unlisted Node", "nStable1", nil)
			b, err := json.Marshal(map[ipn.ProfileID]ipn.LoginProfileView{})
			if err != nil {
				t.Fatal(err)
			}
			f.write(ipn.KnownProfilesStateKey, b)
		}, DefaultHostname, false},
		{"a registered node", "", func(t *testing.T, dir string) {
			f := newStateFixture(t, dir)
			f.machineKey()
			f.register("Old Library", "nStable1", nil)
		}, "Old Library", false},
		{"a registered node holding an attestation key", "", func(t *testing.T, dir string) {
			f := newStateFixture(t, dir)
			f.machineKey()
			f.register("Old Library", "nStable1", &testAttestationKey{})
		}, "Old Library", false},
		{"tailscale.hostname over a registered node", "Chosen Name", func(t *testing.T, dir string) {
			f := newStateFixture(t, dir)
			f.register("Old Library", "nStable1", nil)
		}, "Chosen Name", false},
		{"tailscale.hostname over a new node", "Chosen Name", nil, "Chosen Name", false},
		{"tailscale.hostname over a state that does not read", "Chosen Name", func(t *testing.T, dir string) {
			writeRaw(t, dir, "not json")
		}, "Chosen Name", false},
		{"a state file that does not parse", "", func(t *testing.T, dir string) {
			writeRaw(t, dir, `{"_current-profile":`)
		}, library, true},
		{"a directory where the state file goes", "", func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, stateFileName), 0o700); err != nil {
				t.Fatal(err)
			}
		}, library, true},
		{"a list of profiles that does not parse", "", func(t *testing.T, dir string) {
			f := newStateFixture(t, dir)
			f.register("Old Library", "nStable1", nil)
			f.write(ipn.KnownProfilesStateKey, []byte("["))
		}, library, true},
		{"prefs that do not parse", "", func(t *testing.T, dir string) {
			f := newStateFixture(t, dir)
			f.register("Old Library", "nStable1", nil)
			f.write(ipn.StateKey("profile-"+string(fixtureProfile)), []byte("{not prefs"))
		}, library, true},
		{"a registered node whose profile names no hostname", "", func(t *testing.T, dir string) {
			f := newStateFixture(t, dir)
			f.register("", "nStable1", nil)
		}, library, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "tailscale")
			if c.state != nil {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				c.state(t, dir)
			}
			got, logs := startForName(t, Config{StateDir: dir, Hostname: c.hostname, UnreadableStateHostname: library})
			if got != c.want {
				t.Errorf("the node registers as %q, want %q", got, c.want)
			}
			if warned := strings.Contains(logs, unreadableLine); warned != c.warned {
				t.Errorf("the start's line on a state it cannot read: said=%v, want %v; logs=%s", warned, c.warned, logs)
			}
			if c.warned && !strings.Contains(logs, "tailscale.hostname") {
				t.Errorf("the line on a state that does not read does not advise tailscale.hostname; logs=%s", logs)
			}
			if c.warned && strings.Contains(logs, dir) {
				t.Errorf("the line on a state that does not read names the state dir's path; logs=%s", logs)
			}
		})
	}
}

// TestAStateThisUserCannotReadKeepsTheEarlierName: a state file the bridge's
// user may not open is no answer either way.
func TestAStateThisUserCannotReadKeepsTheEarlierName(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a mode-0 file is readable to root, and Windows does not take the mode")
	}
	dir := filepath.Join(t.TempDir(), "tailscale")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := newStateFixture(t, dir)
	f.register("Old Library", "nStable1", nil)
	if err := os.Chmod(filepath.Join(dir, stateFileName), 0); err != nil {
		t.Fatal(err)
	}
	got, logs := startForName(t, Config{StateDir: dir, UnreadableStateHostname: "Living Room"})
	if got != "Living Room" || !strings.Contains(logs, unreadableLine) {
		t.Errorf("a state this user cannot open: the node registers as %q (want the earlier name), logs=%s", got, logs)
	}
}

// TestTheStateReaderWritesNothing: reading the state creates, writes and
// locks nothing, where upstream's store constructor writes "{}" over a
// missing file.
func TestTheStateReaderWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if _, err := readNodeState(dir); err != nil {
		t.Fatalf("readNodeState on an empty dir: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("reading the state of an empty state dir left %d entries there", len(entries))
	}
	f := newStateFixture(t, dir)
	f.register("Old Library", "nStable1", nil)
	path := filepath.Join(dir, stateFileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := readNodeState(dir)
	if err != nil || !st.registered || st.hostname != "Old Library" {
		t.Fatalf("readNodeState = %+v, %v; want the registered node", st, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !info2.ModTime().Equal(info.ModTime()) {
		t.Error("reading the state changed the state file")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("reading the state left %d entries in the state dir, want the state file alone", len(entries))
	}
}

// TestALibraryRenameDoesNotRenameARegisteredNode: the name a node registered
// with stays across a change of what the caller would give a new node.
func TestALibraryRenameDoesNotRenameARegisteredNode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tailscale")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	newStateFixture(t, dir).register("Living Room", "nStable1", nil)
	for _, library := range []string{"Living Room", "Kitchen"} {
		if got, _ := startForName(t, Config{StateDir: dir, UnreadableStateHostname: library}); got != "Living Room" {
			t.Errorf("library %q: the registered node registers as %q, want the name it has", library, got)
		}
	}
}

// A state file that holds nothing (backlog B828): empty, or white space
// alone, which a truncation leaves. Tailscale's start refuses it in its own
// words, about a TPM-sealed state (store.New, whose JSON check comes before
// the file store that would take an empty file for none), and the file is the
// node's identity, which the bridge never replaces: the start refuses it
// first, in the bridge's words, before the node is built.

// TestAStateFileThatHoldsNothingRefusesTheStart: over a state file that holds
// nothing, with tailscale.hostname set or not and an auth key or not, Start
// returns the bridge's words, which say what to move aside, what that costs
// and what keeps the node, and name no path, and keeps them as the last start
// error; no upstream node is built, the hook before its start never runs, and
// the state file's bytes and the state dir's entries are as they were. The
// node stood in for does what upstream's start does there (upstreamOverState).
func TestAStateFileThatHoldsNothingRefusesTheStart(t *testing.T) {
	for _, c := range []struct {
		name, body, hostname, authKey string
		holds                         string // what the words say the file holds
	}{
		{"an empty file", "", "", "", "is empty"},
		{"an empty file, tailscale.hostname set", "", "Chosen Name", "", "is empty"},
		{"an empty file beside an auth key", "", "", "tskey-auth-b828", "is empty"},
		{"a line feed alone", "\n", "", "", "holds only white space"},
		{"white space alone, tailscale.hostname set", " \t\r\n", "Chosen Name", "", "holds only white space"},
	} {
		t.Run(c.name, func(t *testing.T) {
			keepNoLogsEnv(t)
			dir := filepath.Join(t.TempDir(), "tailscale")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeRaw(t, dir, c.body)
			before := stateDirAsItIs(t, dir)
			var logs bytes.Buffer
			hooked := false
			s, err := NewServer(Config{StateDir: dir, Hostname: c.hostname, AuthKey: c.authKey,
				UnreadableStateHostname: "Living Room", Logger: captureLogger(&logs),
				BeforeStart: func(*tsnet.Server) error { hooked = true; return nil }})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			var built []*upstreamOverState
			s.newNode = func(ts *tsnet.Server) node {
				n := &upstreamOverState{fakeNode: &fakeNode{}, dir: ts.Dir}
				built = append(built, n)
				return n
			}
			t.Cleanup(func() { _ = s.Close() })

			// Every assertion reports, so a start that went on, or one that
			// moved the file aside, shows each thing it did.
			err = s.Start(context.Background())
			if err == nil {
				t.Error("Start over a state file that holds nothing brought the node up")
			} else {
				mustSayTheStateHoldsNothing(t, err.Error(), c.holds, dir)
				var empty *EmptyStateError
				if !errors.As(err, &empty) || empty.WhiteSpace != (c.body != "") {
					t.Errorf("Start = %#v, want an *EmptyStateError with WhiteSpace %v", err, c.body != "")
				} else if want := empty.Error(); err.Error() != want {
					t.Errorf("Start says %q, want the refusal's words alone: %q", err, want)
				}
				if last := s.LastStartError(); last == nil || last.Error() != err.Error() {
					t.Errorf("LastStartError = %v, want the refusal Start returned", last)
				}
			}
			for _, n := range built {
				if n.startCalls() != 0 {
					t.Error("the upstream node's Start ran over a state file that holds nothing")
				}
			}
			if len(built) != 0 {
				t.Errorf("the start built %d upstream node(s) over a state file that holds nothing, want none", len(built))
			}
			if hooked {
				t.Error("the start reached the hook before the upstream start")
			}
			if after := stateDirAsItIs(t, dir); after != before {
				t.Errorf("the start changed the state dir:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			mustNotBeStarted(t, s)
			for _, line := range []string{unreadableLine, "interactive auth required"} {
				if strings.Contains(logs.String(), line) {
					t.Errorf("a refused start logged %q beside its refusal: %s", line, logs.String())
				}
			}
		})
	}
}

// TestAStateFileThatIsNotThereStillStartsANewNode: no state file, and the {}
// upstream's store writes before any authentication, are a new node, as they
// were before B828: the start builds the upstream node, starts it, names it
// tailscale.hostname or DefaultHostname, and keeps no error.
func TestAStateFileThatIsNotThereStillStartsANewNode(t *testing.T) {
	for _, c := range []struct {
		name, hostname, want string
		state                func(t *testing.T, dir string)
	}{
		{"no state dir yet", "", DefaultHostname, nil},
		{"no state file", "", DefaultHostname, func(*testing.T, string) {}},
		{"no state file, tailscale.hostname set", "Chosen Name", "Chosen Name", func(*testing.T, string) {}},
		{"the {} a first start writes", "", DefaultHostname, func(t *testing.T, dir string) { newStateFixture(t, dir) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			keepNoLogsEnv(t)
			dir := filepath.Join(t.TempDir(), "tailscale")
			if c.state != nil {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				c.state(t, dir)
			}
			var name string
			s, err := NewServer(Config{StateDir: dir, Hostname: c.hostname, UnreadableStateHostname: "Living Room",
				Logger:      silentLogger(),
				BeforeStart: func(ts *tsnet.Server) error { name = ts.Hostname; return nil }})
			if err != nil {
				t.Fatalf("NewServer: %v", err)
			}
			var built []*upstreamOverState
			s.newNode = func(ts *tsnet.Server) node {
				n := &upstreamOverState{fakeNode: &fakeNode{}, dir: ts.Dir}
				built = append(built, n)
				return n
			}
			t.Cleanup(func() { _ = s.Close() })
			if err := s.Start(context.Background()); err != nil {
				t.Fatalf("Start = %v, want the new node up", err)
			}
			if len(built) != 1 || built[0].startCalls() != 1 {
				t.Fatalf("the start built %d upstream node(s), want one, started once", len(built))
			}
			if name != c.want {
				t.Errorf("the new node registers as %q, want %q", name, c.want)
			}
			if err := s.LastStartError(); err != nil {
				t.Errorf("LastStartError = %v after a start that succeeded", err)
			}
			if got := s.MetricsState(); got != 2 {
				t.Errorf("MetricsState = %d, want 2 (running)", got)
			}
		})
	}
}

// TestOnlyJSONWhiteSpaceIsAStateThatHoldsNothing: the reader takes a file for
// one that holds nothing exactly where it holds no JSON value, the bytes
// JSON's decoder skips (space, tab, carriage return, line feed) and nothing
// else, which is where Tailscale's start answers "unexpected end of JSON
// input"; any other byte is a state that does not parse, and an object
// with white space around it is the {} it holds.
func TestOnlyJSONWhiteSpaceIsAStateThatHoldsNothing(t *testing.T) {
	for _, c := range []struct {
		name, body string
		empty      bool // *EmptyStateError
		whiteSpace bool
		unreadable bool // errStateUnreadable
	}{
		{"no byte", "", true, false, false},
		{"a line feed", "\n", true, true, false},
		{"each byte JSON skips", " \t\r\n", true, true, false},
		{"a vertical tab", "\v", false, false, true},
		{"a form feed", "\f", false, false, true},
		{"a no-break space", "\u00a0", false, false, true},
		{"a byte order mark", "\ufeff", false, false, true},
		{"NUL bytes", "\x00\x00\x00\x00", false, false, true},
		{"{} between white space", " \n{}\n ", false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeRaw(t, dir, c.body)
			st, err := readNodeState(dir)
			var empty *EmptyStateError
			if got := errors.As(err, &empty); got != c.empty {
				t.Fatalf("readNodeState = %+v, %v: a state that holds nothing %v, want %v", st, err, got, c.empty)
			}
			if c.empty && empty.WhiteSpace != c.whiteSpace {
				t.Errorf("WhiteSpace = %v, want %v", empty.WhiteSpace, c.whiteSpace)
			}
			var unreadable errStateUnreadable
			if got := errors.As(err, &unreadable); got != c.unreadable {
				t.Errorf("readNodeState = %v: a state that does not read %v, want %v", err, got, c.unreadable)
			}
			if !c.empty && !c.unreadable && (err != nil || st.registered) {
				t.Errorf("readNodeState = %+v, %v, want no registered node", st, err)
			}
		})
	}
}

// upstreamOverState is an upstream node whose Start does what
// tailscale.com v1.104.2's does with the state file in its Dir, as far as
// these tests ask: outside a test binary that start writes its logger's
// configuration into Dir (startLogger) before it opens its store, and its
// store (store.New) refuses a state file that holds no JSON value, in words
// about a TPM-sealed state. Both were seen on a real start (B828's record in
// ops/engineering-log.md); a test binary's own upstream start skips the
// logger, so a stand-in is what shows it here.
type upstreamOverState struct {
	*fakeNode
	dir string
}

func (n *upstreamOverState) Start() error {
	if err := n.fakeNode.Start(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(n.dir, "tailscaled.log.conf"), []byte("{}\n"), 0o600); err != nil {
		return err
	}
	state := filepath.Join(n.dir, stateFileName)
	if b, err := os.ReadFile(state); err == nil && len(bytes.Trim(b, " \t\r\n")) == 0 {
		return fmt.Errorf("tsnet: failed to migrate existing TPM-sealed state file to plaintext format: "+
			"failed to unmarshal %q: unexpected end of JSON input", state)
	}
	return nil
}

// stateDirAsItIs is what a test compares to see that nothing changed the
// node's state dir: each entry's name and mode, and the state file's bytes,
// or that it is not there.
func stateDirAsItIs(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "%s %v\n", e.Name(), info.Mode())
	}
	switch state, err := os.ReadFile(filepath.Join(dir, stateFileName)); {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintf(&b, "%s is not there\n", stateFileName)
	case err != nil:
		t.Fatal(err)
	default:
		fmt.Fprintf(&b, "%s holds %q\n", stateFileName, state)
	}
	return b.String()
}

// mustSayTheStateHoldsNothing fails the test unless words are the bridge's
// refusal of a state file that holds nothing: the file by its name and what
// it holds (holds), what keeps the node, what to move aside and what a new
// device costs, that the bridge does neither itself, none of upstream's words,
// and no path (stateDir's, or anything below it).
func mustSayTheStateHoldsNothing(t *testing.T, words, holds, stateDir string) {
	t.Helper()
	for _, want := range []string{stateFileName, holds, "Tailscale does not start from it",
		"put back a copy of the file", "move the file aside", "new device", "authenticates as on a first run",
		"new tailnet address", "MagicDNS name", "moves and replaces nothing"} {
		if !strings.Contains(words, want) {
			t.Errorf("the refusal does not say %q: %q", want, words)
		}
	}
	for _, not := range []string{"TPM", "failed to migrate", "unexpected end of JSON input"} {
		if strings.Contains(words, not) {
			t.Errorf("the refusal carries upstream's %q: %q", not, words)
		}
	}
	if strings.Contains(words, stateDir) {
		t.Errorf("the refusal names the state dir's path: %q", words)
	}
}

// writeRaw writes the state file's bytes as given: a file no upstream writer
// makes.
func writeRaw(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
