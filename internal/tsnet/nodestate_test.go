package tsnet

import (
	"bytes"
	"context"
	"crypto"
	"encoding/json"
	"errors"
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
// applies the upload policy, and the variable is put back as it was when the
// test ends.
func keepNoLogsEnv(t *testing.T) {
	t.Helper()
	t.Setenv(NoLogsEnv, "")
	if err := os.Unsetenv(NoLogsEnv); err != nil {
		t.Fatal(err)
	}
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

// TestTheNodeNameIsTheRegisteredOneOrTheDefault: every state a start can
// find, and the name the upstream server is built with.
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
		{"an empty state file", "", func(t *testing.T, dir string) {
			writeRaw(t, dir, "")
		}, DefaultHostname, false},
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

// writeRaw writes the state file's bytes as given: a file no upstream writer
// makes.
func writeRaw(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
