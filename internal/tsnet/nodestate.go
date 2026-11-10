package tsnet

import (
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"tailscale.com/ipn"
	"tailscale.com/types/key"
	"tailscale.com/types/persist"
)

// DefaultHostname is the name a node registers with when tailscale.hostname
// is not set and its state dir holds no registered node: generic, so that
// nothing of the library, the host, its user or a path reaches the tailnet
// in it. Tailscale suffixes a name another node already has.
const DefaultHostname = "1-bit-bridge"

// stateFileName is the file upstream's file store keeps the node's state in,
// in the state dir (tsnet.Server's Dir).
const stateFileName = "tailscaled.state"

// maxStateBytes bounds what readNodeState reads of the state file. A real one
// holds a few keys and one or two profiles' prefs, a few KiB.
const maxStateBytes = 16 << 20

// nodeState is what the state dir says of the node, read read-only.
type nodeState struct {
	// registered is whether the current profile's persisted prefs carry a
	// node identity: the node upstream's next start is.
	registered bool
	// hostname is the name that profile carries, the name the node has on
	// the tailnet; set only where registered.
	hostname string
}

// errStateUnreadable is a state file that is there and that readNodeState
// could not take for either answer: one it cannot open or read, one that
// does not parse as upstream writes it, or a registered profile that names
// no hostname. Its text names no path.
type errStateUnreadable struct{ why string }

func (e errStateUnreadable) Error() string { return "the node's state " + e.why }

// readNodeState reads the node's state from stateDir as upstream's file store
// keeps it (a JSON object of state keys to their bytes) and as its profile
// manager reads it at a start: the current profile's key
// (ipn.CurrentProfileStateKey), that profile in the known profiles
// (ipn.KnownProfilesStateKey), and its prefs (ipn.PrefsFromBytes). The node
// is registered where those prefs carry a node ID. A missing or empty file,
// one that holds no current profile ("{}", which upstream writes before any
// authentication), a current profile the known profiles do not list (one a
// logout deleted), and prefs with no node ID (an authentication that never
// finished) are no registered node.
//
// It opens the file for reading and nothing else: it never creates, writes
// or locks it, and never goes through upstream's store constructor, which
// writes "{}" where there is no file. A named pipe or a directory at its
// name is refused at once (fsutil.OpenAsFile), unread.
func readNodeState(stateDir string) (nodeState, error) {
	f, _, err := fsutil.OpenAsFile(filepath.Join(stateDir, stateFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nodeState{}, nil
	}
	if err != nil {
		return nodeState{}, errStateUnreadable{"cannot be opened (" + fsutil.ErrorWithoutPath(err) + ")"}
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return nodeState{}, errStateUnreadable{"cannot be read (" + fsutil.ErrorWithoutPath(err) + ")"}
	}
	if len(raw) > maxStateBytes {
		return nodeState{}, errStateUnreadable{fmt.Sprintf("is larger than the %d bytes it is read to", maxStateBytes)}
	}
	if len(raw) == 0 {
		// Upstream's store takes an empty file for none.
		return nodeState{}, nil
	}
	var keys map[ipn.StateKey][]byte
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nodeState{}, errStateUnreadable{"does not parse as Tailscale's state file"}
	}
	current := ipn.StateKey(keys[ipn.CurrentProfileStateKey])
	if current == "" {
		return nodeState{}, nil
	}
	var known map[ipn.ProfileID]ipn.LoginProfile
	if b, ok := keys[ipn.KnownProfilesStateKey]; ok {
		if err := json.Unmarshal(b, &known); err != nil {
			return nodeState{}, errStateUnreadable{"holds a list of profiles that does not parse"}
		}
	}
	listed := false
	for _, p := range known {
		if p.Key == current {
			listed = true
			break
		}
	}
	b := keys[current]
	if !listed || len(b) == 0 {
		return nodeState{}, nil
	}
	prefs := ipn.NewPrefs()
	// A profile whose node holds a hardware attestation key stores it with
	// its prefs; this read takes the key's JSON and keeps none of it, as
	// upstream's own fallback does when the key cannot be loaded.
	prefs.Persist = &persist.Persist{AttestationKey: &ignoredAttestationKey{}}
	if err := ipn.PrefsFromBytes(b, prefs); err != nil {
		return nodeState{}, errStateUnreadable{"holds its current profile's settings in a form that does not parse"}
	}
	if prefs.Persist == nil || prefs.Persist.NodeID == "" {
		return nodeState{}, nil
	}
	if prefs.Hostname == "" {
		return nodeState{}, errStateUnreadable{"holds a registered node whose profile names no hostname"}
	}
	return nodeState{registered: true, hostname: prefs.Hostname}, nil
}

// ignoredAttestationKey is a key.HardwareAttestationKey that takes any JSON
// and keeps nothing of it: readNodeState reads a node's name, never its keys.
type ignoredAttestationKey struct{}

func (*ignoredAttestationKey) Public() crypto.PublicKey { return nil }

func (*ignoredAttestationKey) Sign(io.Reader, []byte, crypto.SignerOpts) ([]byte, error) {
	return nil, errors.New("tsnet: the state reader holds no attestation key")
}

func (*ignoredAttestationKey) MarshalJSON() ([]byte, error)        { return []byte("null"), nil }
func (*ignoredAttestationKey) UnmarshalJSON([]byte) error          { return nil }
func (*ignoredAttestationKey) Close() error                        { return nil }
func (k *ignoredAttestationKey) Clone() key.HardwareAttestationKey { return k }
func (*ignoredAttestationKey) IsZero() bool                        { return true }
