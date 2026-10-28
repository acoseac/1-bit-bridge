package qualification

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
)

// Identity is the persistent model, descriptor firmware and anchored endpoint.
// Enumeration numbers and serial numbers deliberately do not participate.
type Identity struct {
	Vendor, ProductID, Manufacturer, Product, BCDDevice, Port string
	Interface, Device, Subdevice                              int
}

// ApprovedPolicy is an indivisible, executed native transport and timing policy.
type ApprovedPolicy struct {
	ID                  string
	Tuple               Tuple
	Channels            int
	Ordering            string
	PauseReleaseSeconds int
	PCMTimePolicy       [2]int
	Constraints         []alsactl.Constraint
	Matrix, Lifecycle   Evidence
}

// Evidence binds a complete report by its relative name and SHA-256.
type Evidence struct{ Path, SHA256 string }

// Edge names a directional boundary, including both complete tuple/policy keys.
type Edge struct{ From, To string }

// PolicyRef names one approved policy by its ID and the canonical digest
// (Digest) of the whole policy as the record held it.
type PolicyRef struct{ ID, SHA256 string }

// TransitionScenario is the dsd-test scenario that plays an approved native
// policy, then an approved DoP policy, then the native one again, through
// the real engine and backend (B470 2d.2); its reports are an approval's
// Transitions.
const TransitionScenario = "approved-native-dop-native"

// ApprovalBody is the canonical JSON body over which SHA256 is computed.
// Transitions and QualifiedOn are schema 2's; both are omitted when empty,
// so a schema-1 body digests as it did before they existed.
type ApprovalBody struct {
	SchemaVersion, QualificationVersion               int
	Confirmation, Revision, ManifestHash, Environment string
	Identity                                          Identity
	Policies                                          []ApprovedPolicy
	Edges                                             []Edge
	// Transitions are the approved native↔DoP transition reports
	// (TransitionScenario), the only evidence for an edge between a native
	// and a DoP policy.
	Transitions []Evidence `json:",omitempty"`
	// QualifiedOn is the owner's qualification date, YYYY-MM-DD.
	QualifiedOn string `json:",omitempty"`
}

// Approval is an owner-confirmed local record. Hashes detect damage; filesystem
// ownership is the trust boundary, not a cryptographic signer.
type Approval struct {
	Body   ApprovalBody
	SHA256 string
}

// ApprovalPhrase is the owner's exact typed promotion confirmation.
const ApprovalPhrase = "I approve these complete matrix and backend-lifecycle reports"

// Digest returns the SHA-256 of encoding/json's canonical struct representation.
func Digest(v any) string { b, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(b)) }

// ReadApproval verifies a local record and its complete evidence under stateDir.
func ReadApproval(stateDir, name string, identity Identity) (Approval, error) {
	return readApproval(stateDir, name, identity, false)
}

// ReadCandidate validates a visibly non-approved CLI candidate against its
// matrix report. An integrated lifecycle report does not yet exist.
func ReadCandidate(stateDir, name string, identity Identity) (Approval, error) {
	return readApproval(stateDir, name, identity, true)
}
func readApproval(stateDir, name string, identity Identity, candidate bool) (Approval, error) {
	var a Approval
	b, err := trustedFile(stateDir, name)
	if err != nil {
		return a, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&a); err != nil {
		return a, err
	}
	if dec.Decode(new(any)) != io.EOF {
		return a, errors.New("trailing approval data")
	}
	body := a.Body
	if a.SHA256 != Digest(body) {
		return a, errors.New("approval hash mismatch")
	}
	if body.QualificationVersion != Version {
		return a, errors.New("stale approval version")
	}
	switch body.SchemaVersion {
	case 1:
		if len(body.Transitions) > 0 || body.QualifiedOn != "" {
			return a, errors.New("approval schema 1 carries no transitions or qualification date")
		}
	case 2:
		// A candidate is qualified on no date yet, and holds no transition.
		if !(candidate && body.QualifiedOn == "") && !validDate(body.QualifiedOn) {
			return a, errors.New("approval qualification date invalid")
		}
		if candidate && len(body.Transitions) > 0 {
			return a, errors.New("a CLI candidate carries no transitions")
		}
	default:
		return a, errors.New("stale approval version")
	}
	if body.Identity != identity || !identity.valid() {
		return a, errors.New("stale approval identity")
	}
	phrase := ApprovalPhrase
	if candidate {
		phrase = "CLI candidate; not approved"
	}
	if body.Confirmation != phrase || body.Revision == "" || body.Environment == "" || !validHash(body.ManifestHash) {
		return a, errors.New("owner approval incomplete")
	}
	if len(body.Policies) == 0 || len(body.Policies) > 64 || len(body.Edges) > 4096 || len(body.Transitions) > 64 {
		return a, errors.New("approval bounds")
	}
	seen := map[string]bool{}
	executedEdges := map[Edge]bool{}
	for _, p := range body.Policies {
		key := fmt.Sprintf("%s/%s/%d", p.Tuple.TransportName(), p.Tuple.Format, p.Tuple.BitRate)
		if seen[key] {
			return a, errors.New("duplicate base tuple")
		}
		seen[key] = true
		if p.ID == "" || p.Channels != 2 || p.Ordering != "msb" || p.PauseReleaseSeconds != 30 || p.PCMTimePolicy != [2]int{} {
			return a, errors.New("invalid policy")
		}
		t := p.Tuple
		if !t.ValidTransport() {
			return a, errors.New("invalid policy transport")
		}
		if (t.Format != "DSD_U32_BE" && t.Format != "DSD_U32_LE" && t.Format != "DSD_U16_LE" && t.Format != "S32_LE") || (t.BitRate != 2822400 && t.BitRate != 5644800 && t.BitRate != 11289600) || (t.Pause != "kernel" && t.Pause != "silence") || t.TailMS < 0 || t.TailMS > 100 || t.PrefillMS < 0 || t.PrefillMS > 500 {
			return a, errors.New("invalid native policy")
		}
		for scenario, evidence := range map[string]Evidence{"matrix": p.Matrix, "backend-lifecycle": p.Lifecycle} {
			if candidate && scenario == "backend-lifecycle" {
				continue
			}
			report, err := trustedFile(stateDir, evidence.Path)
			if err != nil {
				return a, err
			}
			if !validHash(evidence.SHA256) || evidence.SHA256 != fmt.Sprintf("%x", sha256.Sum256(report)) {
				return a, errors.New("evidence hash mismatch")
			}
			var r executedReport
			if err = json.Unmarshal(report, &r); err != nil {
				return a, err
			}
			if r.QualificationVersion != Version || r.Tuple != t || r.Identity != identity || r.ManifestHash != body.ManifestHash || r.Environment != body.Environment || r.Scenario != scenario || r.End != "complete" || r.Error != "" || r.Modified || r.BuildSHA == "" || r.BuildSHA == "unverified" || r.PauseReleaseSeconds != 30 || r.PCMTimePolicy != [2]int{} || len(r.Completed) == 0 {
				return a, errors.New("report does not execute approved policy")
			}
			if t.Transport == "dop" {
				if r.SchemaVersion != 2 {
					return a, errors.New("DoP report version mismatch")
				}
				if err := r.DoP.Validate(); err != nil {
					return a, err
				}
				if err := r.validateDoPGeometry(); err != nil {
					return a, err
				}
			}
			if scenario == "backend-lifecycle" {
				if r.NativeCompleted <= 0 || r.NativeTuple != t {
					return a, errors.New("lifecycle has no completion of the candidate native tuple")
				}
				for _, e := range r.PassedEdges {
					executedEdges[e] = true
				}
				for _, action := range []string{"natural", "next", "previous", "pause", "resume", "released", "reopened", "stop"} {
					if !slices.Contains(r.LifecycleActions, action) {
						return a, errors.New("incomplete backend lifecycle actions")
					}
				}
			}
			for _, done := range r.Completed {
				if !done {
					return a, errors.New("incomplete occurrence evidence")
				}
			}
			if r.FeatureUnit == nil {
				return a, errors.New("missing USB gain evidence")
			}
			if t.Transport == "dop" && *r.FeatureUnit {
				if err := CheckDoPExperiments(stateDir, r.Experiments, t, identity, body.ManifestHash, body.Environment); err != nil {
					return a, err
				}
			}
			if len(p.Constraints) == 0 {
				if *r.FeatureUnit {
					return a, errors.New("unconstrained gain")
				}
				for _, snap := range []alsactl.Snapshot{r.Before, r.After} {
					for _, c := range snap.Controls {
						if c.Interface == 2 || c.ReadError != "" {
							return a, errors.New("unconstrained or unreadable mixer evidence")
						}
					}
				}
			}
			if err = r.Before.CheckConstraints(p.Constraints); err != nil {
				return a, err
			}
			if err = r.After.CheckConstraints(p.Constraints); err != nil {
				return a, err
			}
		}
	}
	for _, ev := range body.Transitions {
		if err := checkTransition(stateDir, ev, body, identity, executedEdges); err != nil {
			return a, err
		}
	}
	edges := map[Edge]bool{}
	for _, e := range body.Edges {
		if !candidate && !executedEdges[e] {
			return a, errors.New("unexecuted transition edge")
		}
		if e.From == "" || e.To == "" || edges[e] {
			return a, errors.New("invalid transition edge")
		}
		edges[e] = true
	}
	return a, nil
}

// checkTransition takes a transition report (TransitionScenario, B470 2d.2)
// as the evidence for the two edges between its endpoints, a native and a
// DoP policy of this very record: the report executed them on the record's
// identity, manifest and environment, under the DoP evidence's schema,
// observations and geometry and both policies' constraints, heard every
// occurrence to its end, moved across the transports by next and previous,
// stopped, and passed both edges. Its endpoints are named by their
// canonical digests, so a record whose policy differs from the one the
// transition played, by a byte, is refused.
func checkTransition(stateDir string, ev Evidence, body ApprovalBody, identity Identity, executed map[Edge]bool) error {
	report, err := trustedFile(stateDir, ev.Path)
	if err != nil {
		return err
	}
	if !validHash(ev.SHA256) || ev.SHA256 != fmt.Sprintf("%x", sha256.Sum256(report)) {
		return errors.New("evidence hash mismatch")
	}
	var r executedReport
	if err = json.Unmarshal(report, &r); err != nil {
		return err
	}
	if r.QualificationVersion != Version || r.Identity != identity || r.ManifestHash != body.ManifestHash || r.Environment != body.Environment || r.Scenario != TransitionScenario || r.End != "complete" || r.Error != "" || r.Modified || r.BuildSHA == "" || r.BuildSHA == "unverified" || r.PauseReleaseSeconds != 30 || r.PCMTimePolicy != [2]int{} || len(r.Completed) != 3 || r.NativeCompleted <= 0 {
		// The scenario plays three fixtures, native, DoP, native, and the runner
		// writes "complete" only when all three finished and an endpoint played
		// to its end; the lifecycle report is held to the same completion.
		return errors.New("transition report does not execute approved policies")
	}
	native, dop, ok := transitionEndpoints(body.Policies, r.Endpoints)
	if !ok || r.NativeTuple != native.Tuple || r.Tuple != dop.Tuple {
		return errors.New("transition endpoints do not match approved policies")
	}
	if r.SchemaVersion != 2 {
		return errors.New("DoP report version mismatch")
	}
	if err := r.DoP.Validate(); err != nil {
		return err
	}
	if err := r.validateDoPGeometry(); err != nil {
		return err
	}
	for _, done := range r.Completed {
		if !done {
			return errors.New("incomplete occurrence evidence")
		}
	}
	for _, action := range []string{"natural", "next", "previous", "stop"} {
		if !slices.Contains(r.LifecycleActions, action) {
			return errors.New("incomplete transition actions")
		}
	}
	if r.FeatureUnit == nil {
		return errors.New("missing USB gain evidence")
	}
	if *r.FeatureUnit {
		if err := CheckDoPExperiments(stateDir, r.Experiments, dop.Tuple, identity, body.ManifestHash, body.Environment); err != nil {
			return err
		}
	}
	for _, p := range []ApprovedPolicy{native, dop} {
		if len(p.Constraints) == 0 {
			if *r.FeatureUnit {
				return errors.New("unconstrained gain")
			}
			for _, snap := range []alsactl.Snapshot{r.Before, r.After} {
				for _, c := range snap.Controls {
					if c.Interface == 2 || c.ReadError != "" {
						return errors.New("unconstrained or unreadable mixer evidence")
					}
				}
			}
		}
		if err = r.Before.CheckConstraints(p.Constraints); err != nil {
			return err
		}
		if err = r.After.CheckConstraints(p.Constraints); err != nil {
			return err
		}
	}
	for _, e := range []Edge{{From: NativeKey(native), To: NativeKey(dop)}, {From: NativeKey(dop), To: NativeKey(native)}} {
		if !slices.Contains(r.PassedEdges, e) {
			return errors.New("transition report lacks a native↔DoP edge")
		}
		executed[e] = true
	}
	return nil
}

// transitionEndpoints are the native and the DoP policy of policies refs
// names, in that order, each its ID and the canonical digest of the whole
// policy the transition played.
func transitionEndpoints(policies []ApprovedPolicy, refs []PolicyRef) (native, dop ApprovedPolicy, ok bool) {
	if len(refs) != 2 {
		return native, dop, false
	}
	find := func(ref PolicyRef, transport string) (ApprovedPolicy, bool) {
		for _, p := range policies {
			if p.ID == ref.ID && p.Tuple.TransportName() == transport && Digest(p) == ref.SHA256 {
				return p, true
			}
		}
		return ApprovedPolicy{}, false
	}
	native, okNative := find(refs[0], "native")
	dop, okDoP := find(refs[1], "dop")
	return native, dop, okNative && okDoP
}

// validDate is a qualification date: YYYY-MM-DD, a real day.
func validDate(s string) bool {
	d, err := time.Parse(time.DateOnly, s)
	return err == nil && d.Format(time.DateOnly) == s
}

// NativeKey includes the policy in directional edge identity.
func NativeKey(p ApprovedPolicy) string {
	return fmt.Sprintf("%s/%s/%d/2/msb/%s/%s/%d/%d/30", p.Tuple.TransportName(), p.Tuple.Format, p.Tuple.BitRate, p.ID, p.Tuple.Pause, p.Tuple.TailMS, p.Tuple.PrefillMS)
}

func validHash(s string) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == 32 }
func (i Identity) valid() bool {
	return i.Vendor != "" && i.ProductID != "" && i.Manufacturer != "" && i.Product != "" && i.BCDDevice != "" && i.Port != "" && i.Interface >= 0 && i.Device >= 0 && i.Subdevice >= 0
}

type executedReport struct {
	PackingVersion, SourceRate, CarrierRate                   int
	Kernel, Platform, GoVersion                               string
	Opened                                                    []dopGeometry
	ExperimentState                                           string
	Experiments                                               []Evidence
	Contact                                                   *Contact
	SchemaVersion                                             int
	DoP                                                       *DoPObservations
	NativeTuple                                               Tuple
	NativeCompleted                                           int
	Endpoints                                                 []PolicyRef
	QualificationVersion                                      int
	Tuple                                                     Tuple
	Identity                                                  Identity
	ManifestHash, Environment, Scenario, End, Error, BuildSHA string
	Modified                                                  bool
	Completed                                                 []bool
	LifecycleActions                                          []string
	PassedEdges                                               []Edge
	PauseReleaseSeconds                                       int
	PCMTimePolicy                                             [2]int
	FeatureUnit                                               *bool
	Before, After                                             alsactl.Snapshot
}

func trustedFile(stateDir, name string) ([]byte, error) {
	root, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(root, name)
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return nil, errors.New("approval must live in state directory")
	}
	for dir := filepath.Dir(resolved); ; dir = filepath.Dir(dir) {
		st, e := os.Stat(dir)
		if e != nil {
			return nil, e
		}
		if !owned(st) || st.Mode().Perm()&0022 != 0 {
			return nil, errors.New("untrusted approval directory")
		}
		if dir == root {
			break
		}
		if filepath.Dir(dir) == dir {
			return nil, errors.New("approval must live in state directory")
		}
	}
	f, _, err := fsutil.OpenAsFile(resolved)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !owned(st) || st.Mode().Perm()&0022 != 0 {
		return nil, errors.New("untrusted approval owner or mode")
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 1<<20 {
		return nil, errors.New("approval exceeds 1 MiB")
	}
	after, err := os.Stat(name)
	if err != nil || !os.SameFile(st, after) {
		return nil, errors.New("approval replaced during read")
	}
	return b, nil
}
