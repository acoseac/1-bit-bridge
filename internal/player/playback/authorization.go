package playback

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/alsactl"
	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
)

// Authorization configures local owner approval. It never accepts a candidate.
type Authorization struct {
	StateDir, Record, Mode string
	// AllowDoP is output.player.allowDoP: under dsdMode dop or auto it lets
	// an approved DoP policy play.
	AllowDoP bool
	Sysfs    alsa.SysfsReader
	Controls alsactl.Reader
	// Healthy observes authorization health independently of inventory generation.
	Healthy func() bool
	// ContactCheck is an optional additional CLI report interlock; it never grants permission.
	ContactCheck func(alsapcm.NodeInfo, qualification.ApprovedPolicy, string) error
}

// PlayerPolicy is a copy of one published permission epoch, never live hardware.
type PlayerPolicy struct {
	BootID           string
	Generation       uint64
	Revision, Reason string
	Tuples           []engine.NativePlan
	// Model, BCDDevice and QualifiedOn are the owner qualification the
	// epoch's record states: the DAC's model (its manufacturer and product
	// strings), its descriptor firmware code and the qualification date,
	// never its port or serial.
	Model, BCDDevice, QualifiedOn string
}

type permission struct {
	unavailable map[string]bool
	record      qualification.Approval
	file        os.FileInfo
	generation  uint64
	revoked     atomic.Bool
	// mode and allowDoP are the authorization's dsdMode (native, dop or
	// auto) and allowDoP, which the effective set is taken under.
	mode     string
	allowDoP bool
}

// The refusals of DSD content the effective set names (B470 2d.2), beside
// ReasonDSD (DSD not configured), ReasonRate (a rate no transport here
// carries) and native_revoked.
const (
	// ReasonDSDApproval is DSD while dsdMode asks for it and the owner's
	// approved record cannot be read and verified now: missing, stale,
	// tampered, a CLI candidate, another DAC's, the health stale, or no
	// policy's constraints holding.
	ReasonDSDApproval engine.Reason = "dsd_approval_unavailable"
	// ReasonDoPNotAllowed is DSD whose only approved transport at its rate,
	// under the mode, is DoP, while allowDoP is false.
	ReasonDoPNotAllowed engine.Reason = "dop_not_allowed"
	// ReasonDSDNotApproved is DSD at a rate where the record holds no policy
	// the mode may play.
	ReasonDSDNotApproved engine.Reason = "dsd_tuple_not_approved"
	// ReasonDSDUnavailable is DSD at a rate whose approved policy does not
	// hold now (a constrained control changed): never played in another
	// transport's place.
	ReasonDSDUnavailable engine.Reason = "dsd_policy_unavailable"
)

// errDSDOff is a permission read while dsdMode is off: DSD is refused as
// not configured, never as an approval that does not read.
var errDSDOff = errors.New("DSD is off")

// approvalDetail is why an approval did not read, as a refusal's detail: the
// reader's own words, and of an error a filesystem call returned its
// operation and cause alone, never the path, which names the data
// directory.
func approvalDetail(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return "the approved record does not read: " + pe.Op + ": " + pe.Err.Error()
	}
	return "the approved record does not read: " + err.Error()
}

func (a *Authorization) identity(u Unit) (qualification.Identity, error) {
	q, e := a.Sysfs.Qualification(u.Endpoint.Card)
	if e != nil {
		return qualification.Identity{}, e
	}
	id, ok := a.Sysfs.Card(u.Endpoint.Card)
	if !ok || id.USB == nil {
		return qualification.Identity{}, errors.New("USB identity unavailable")
	}
	return qualification.Identity{Vendor: fmt.Sprintf("%04x", id.USB.ID.Vendor), ProductID: fmt.Sprintf("%04x", id.USB.ID.Product), Manufacturer: q.Manufacturer, Product: q.Product, BCDDevice: q.BCDDevice, Port: q.Port, Interface: q.Interface, Device: u.Endpoint.Device, Subdevice: u.Subdevice}, nil
}
func (a *Authorization) path() string {
	if filepath.IsAbs(a.Record) {
		return a.Record
	}
	return filepath.Join(a.StateDir, a.Record)
}
func (b *Backend) readPermission(ctx context.Context) (*permission, error) {
	a := b.cfg.Authorization
	if a == nil || a.Mode == "" || a.Mode == "off" {
		return nil, errDSDOff
	}
	if a.Mode != "native" && a.Mode != "auto" && a.Mode != "dop" {
		return nil, errors.New("dsdMode is no DSD mode")
	}
	if a.Record == "" {
		return nil, errors.New("approval missing")
	}
	before, e := os.Stat(a.path())
	if e != nil {
		return nil, e
	}
	if a.Healthy != nil && !a.Healthy() {
		return nil, errors.New("DSD health stale")
	}
	u, e := b.cfg.Locator.Locate(ctx)
	if e != nil {
		return nil, e
	}
	id, e := a.identity(u)
	if e != nil {
		return nil, e
	}
	readerApproval := qualification.ReadApproval
	if b.cliCandidate {
		readerApproval = qualification.ReadCandidate
	}
	record, e := readerApproval(a.StateDir, a.Record, id)
	if e != nil {
		return nil, e
	}
	after, e := os.Stat(a.path())
	if e != nil || !os.SameFile(before, after) {
		return nil, errors.New("approval replaced")
	}
	reader := a.Controls
	reader.AllowUnreadable = true
	controls, e := reader.Read(u.Endpoint.Card)
	if e != nil {
		return nil, e
	}
	gain, e := a.Sysfs.QualificationGain(u.Endpoint.Card)
	if e != nil {
		return nil, e
	}
	unavailable := map[string]bool{}
	for _, p := range record.Body.Policies {
		if len(p.Constraints) == 0 {
			unavailable[qualification.NativeKey(p)] = gain
			for _, c := range controls.Controls {
				if c.Interface == 2 {
					unavailable[qualification.NativeKey(p)] = true
				}
			}
		}
		if controls.CheckConstraints(p.Constraints) != nil {
			unavailable[qualification.NativeKey(p)] = true
		}
	}
	available := false
	for _, p := range record.Body.Policies {
		if !unavailable[qualification.NativeKey(p)] {
			available = true
		}
	}
	if !available {
		return nil, errors.New("no tuple satisfies its approved constraints")
	}
	return &permission{record: record, file: after, unavailable: unavailable, mode: a.Mode, allowDoP: a.AllowDoP}, nil
}
func planOf(p qualification.ApprovedPolicy, a qualification.Approval) engine.NativePlan {
	plan := engine.NativePlan{Tuple: engine.NativeTuple{Transport: p.Tuple.Transport, Format: p.Tuple.Format, BitRate: p.Tuple.BitRate}, PolicyID: p.ID, Pause: p.Tuple.Pause, Tail: time.Duration(p.Tuple.TailMS) * time.Millisecond, Prefill: time.Duration(p.Tuple.PrefillMS) * time.Millisecond}
	for _, e := range a.Body.Edges {
		plan.Edges = append(plan.Edges, engine.TransitionEdge{From: e.From, To: e.To})
	}
	return plan
}

// dsdRates are the logical rates an approved policy may name: DSD64, DSD128
// and DSD256.
var dsdRates = []int{2822400, 5644800, 11289600}

// effectiveSet is a permission's effective DSD set (B470 R10, R11): the one
// answer selection and publication both take, so what the epoch advertises
// is what plays.
type effectiveSet struct {
	// plans are the policies that play, by rate, each rate's in selection
	// order.
	plans []engine.NativePlan
	// refused is why a rate with no plan has none.
	refused map[int]*engine.Unplayable
	// reason is the epoch's when no plan plays at any rate.
	reason string
}

// effective is the effective set of record's policies under mode and
// allowDoP, unavailable naming those whose constraints do not hold now. A
// policy plays only where a transport the sink opens implements it
// (nativeTransport): under native, only native policies, DSD_U32_BE, then
// DSD_U32_LE, then DSD_U16_LE; under dop, only DoP's S32_LE, and only with
// allowDoP; under auto, the native policies first, and DoP only at a rate
// where the record holds no native policy at all: one there whose
// constraints do not hold now is that rate's refusal, never a reason to
// play DoP in its place.
func effective(record qualification.Approval, unavailable map[string]bool, mode string, allowDoP bool) effectiveSet {
	set := effectiveSet{refused: map[int]*engine.Unplayable{}}
	notAllowed := mode == "dop" && !allowDoP
	for _, rate := range dsdRates {
		var natives, dops []qualification.ApprovedPolicy
		for _, f := range []string{"DSD_U32_BE", "DSD_U32_LE", "DSD_U16_LE"} {
			for _, p := range record.Body.Policies {
				if p.Tuple.BitRate == rate && p.Tuple.TransportName() == "native" && p.Tuple.Format == f && implemented(p) {
					natives = append(natives, p)
				}
			}
		}
		for _, p := range record.Body.Policies {
			if p.Tuple.BitRate == rate && p.Tuple.TransportName() == TransportDoP && implemented(p) {
				dops = append(dops, p)
			}
		}
		var candidates []qualification.ApprovedPolicy
		var refusal *engine.Unplayable
		switch mode {
		case "native":
			candidates = natives
			if len(natives) == 0 {
				refusal = &engine.Unplayable{Reason: ReasonDSDNotApproved, Detail: "the approval holds no native policy at this rate, and dsdMode native plays no DoP"}
			}
		case "dop":
			switch {
			case !allowDoP:
				refusal = &engine.Unplayable{Reason: ReasonDoPNotAllowed, Detail: "dsdMode dop plays DoP alone, and allowDoP is false"}
			case len(dops) == 0:
				refusal = &engine.Unplayable{Reason: ReasonDSDNotApproved, Detail: "the approval holds no DoP policy at this rate, and dsdMode dop plays no native DSD"}
			default:
				candidates = dops
			}
		case "auto":
			switch {
			case len(natives) > 0:
				candidates = natives
			case len(dops) == 0:
				refusal = &engine.Unplayable{Reason: ReasonDSDNotApproved, Detail: "the approval holds no policy at this rate"}
			case !allowDoP:
				notAllowed = true
				refusal = &engine.Unplayable{Reason: ReasonDoPNotAllowed, Detail: "only DoP is approved at this rate, and allowDoP is false"}
			default:
				candidates = dops
			}
		default:
			refusal = &engine.Unplayable{Reason: ReasonDSD, Detail: "DSD is off"}
		}
		n := 0
		for _, p := range candidates {
			if unavailable[qualification.NativeKey(p)] {
				continue
			}
			set.plans = append(set.plans, planOf(p, record))
			n++
		}
		if n == 0 {
			if refusal == nil {
				refusal = &engine.Unplayable{Reason: ReasonDSDUnavailable, Detail: "the approved policy at this rate does not hold now: a control it constrains has changed"}
			}
			set.refused[rate] = refusal
		}
	}
	if len(set.plans) == 0 {
		set.reason = "no_eligible_tuple"
		if notAllowed {
			set.reason = "dop_not_allowed"
		}
	}
	return set
}

// implemented reports whether a transport the sink opens carries p.
func implemented(p qualification.ApprovedPolicy) bool {
	return nativeTransport(planOf(p, qualification.Approval{})) != (alsapcm.Transport{})
}

// selectNative is the DSD plan for content of f under p: the first of the
// effective set's plans at f's rate, or that rate's refusal. The decoders
// take DSD512, which no transport here carries: it is refused before the
// set is asked.
func (p *permission) selectNative(f format.NativeDSD) (engine.NativePlan, error) {
	if p == nil || p.revoked.Load() {
		return productionNativeAdmission(f)
	}
	if !nativeLogicalRate(f.BitRateHz) {
		return engine.NativePlan{}, &engine.Unplayable{Reason: ReasonRate, Detail: "DSD64, DSD128 and DSD256 only: no native or DoP transport here carries this DSD rate"}
	}
	if f.Channels != 2 {
		return engine.NativePlan{}, &engine.Unplayable{Reason: ReasonDSDNotApproved, Detail: "an approved policy is stereo"}
	}
	set := effective(p.record, p.unavailable, p.mode, p.allowDoP)
	for _, plan := range set.plans {
		if plan.Tuple.BitRate == f.BitRateHz {
			return plan, nil
		}
	}
	if u := set.refused[f.BitRateHz]; u != nil {
		return engine.NativePlan{}, u
	}
	return engine.NativePlan{}, &engine.Unplayable{Reason: ReasonDSDNotApproved, Detail: "the approval holds no policy at this rate"}
}

// selectTransition is the transition scenario's CLI-only admission
// (TransitionPlans): the approved policy plans name for ref, at the
// content's own rate and holding now, with the edges under test beside the
// record's own.
func (p *permission) selectTransition(ref engine.Reference, f format.NativeDSD, plans *TransitionPlans) (engine.NativePlan, error) {
	if p == nil || p.revoked.Load() {
		return productionNativeAdmission(f)
	}
	id, ok := plans.Policies[ref.String()]
	if !ok {
		return engine.NativePlan{}, &engine.Unplayable{Reason: ReasonDSDNotApproved, Detail: "the transition scenario names no policy for this occurrence"}
	}
	for _, policy := range p.record.Body.Policies {
		if policy.ID != id {
			continue
		}
		switch {
		case policy.Tuple.BitRate != f.BitRateHz || f.Channels != 2 || !implemented(policy):
			return engine.NativePlan{}, &engine.Unplayable{Reason: ReasonDSDNotApproved, Detail: "the occurrence is not its policy's rate and stereo"}
		case p.unavailable[qualification.NativeKey(policy)]:
			return engine.NativePlan{}, &engine.Unplayable{Reason: ReasonDSDUnavailable, Detail: "the approved policy does not hold now: a control it constrains has changed"}
		}
		plan := planOf(policy, p.record)
		plan.Edges = append(plan.Edges, plans.Edges...)
		return plan, nil
	}
	return engine.NativePlan{}, &engine.Unplayable{Reason: ReasonDSDNotApproved, Detail: "the approval holds no policy of that ID"}
}

// qualificationFacts are the owner qualification a record states for the
// wire: the DAC's model, its manufacturer and product strings with the
// record's anchored port taken out wherever it shows, its descriptor
// firmware code and the qualification date. The serial is no part of a
// record.
func qualificationFacts(a qualification.Approval) (model, bcdDevice, qualifiedOn string) {
	id := a.Body.Identity
	model = strings.TrimSpace(strings.TrimSpace(id.Manufacturer) + " " + strings.TrimSpace(id.Product))
	if id.Port != "" {
		model = strings.ReplaceAll(model, id.Port, "[redacted]")
	}
	return model, id.BCDDevice, a.Body.QualifiedOn
}

func (b *Backend) publishPermission(p *permission, reason string) {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	if p != nil && p.revoked.Load() {
		p = nil
		reason = "native_revoked"
	}
	b.publishedPermission = p
	b.policy.Generation++
	b.policy.Reason = reason
	b.policy.Tuples = nil
	b.policy.Revision = ""
	b.policy.Model, b.policy.BCDDevice, b.policy.QualifiedOn = "", "", ""
	if p != nil {
		p.generation = b.policy.Generation
		b.policy.Revision = p.record.Body.Revision
		b.policy.Model, b.policy.BCDDevice, b.policy.QualifiedOn = qualificationFacts(p.record)
		set := effective(p.record, p.unavailable, p.mode, p.allowDoP)
		b.policy.Tuples = set.plans
		if len(set.plans) == 0 {
			b.policy.Reason = set.reason
		}
	}
}

// Policy returns a detached snapshot; reading it performs no hardware probes.
func (b *Backend) Policy() PlayerPolicy {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	return b.copyPolicyLocked()
}
func (b *Backend) copyPolicyLocked() PlayerPolicy {
	p := b.policy
	p.Tuples = slices.Clone(p.Tuples)
	for i := range p.Tuples {
		p.Tuples[i].Edges = slices.Clone(p.Tuples[i].Edges)
	}
	return p
}
func (b *Backend) checkPermission(ctx context.Context, p *permission, n *alsapcm.NodeInfo, policyID ...string) error {
	if p == nil || p.revoked.Load() {
		return errors.New("DSD permission revoked")
	}
	current, e := b.readPermission(ctx)
	if e != nil {
		return e
	}
	if current.record.SHA256 != p.record.SHA256 || !os.SameFile(current.file, p.file) {
		return errors.New("approved record replaced")
	}
	for _, id := range policyID {
		if current.unavailable[id] {
			return errors.New("selected policy constraints changed")
		}
	}

	if n != nil {
		a := b.cfg.Authorization
		id, e := a.identity(Unit{Endpoint: alsapcm.Endpoint{Card: n.Card, Device: n.Device}})
		if e != nil {
			return e
		}
		id.Subdevice = n.Subdevice
		if id != p.record.Body.Identity {
			return errors.New("opened identity mismatch")
		}
		reader := a.Controls
		reader.AllowUnreadable = true
		controls, e := reader.Read(n.Card)
		if e != nil {
			return e
		}
		for _, v := range p.record.Body.Policies {
			if len(policyID) > 0 && !slices.Contains(policyID, qualification.NativeKey(v)) {
				continue
			}
			if e = controls.CheckConstraints(v.Constraints); e != nil {
				return e
			}
		}
	}
	if p.revoked.Load() {
		return errors.New("DSD permission revoked")
	}
	return nil
}
func (s *session) monitorPermission() {
	if s.permission == nil {
		return
	}
	s.b.spawn(func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-s.down:
				return
			case <-ticker.C:
				next, err := s.b.readPermission(s.ctx)
				s.reconcilePermission(next, err)
			}
		}
	})
}

// RefreshAuthorization publishes changes from a supervisor's periodic health
// check. It revokes old occurrences before publishing a replacement epoch.
func (b *Backend) RefreshAuthorization(ctx context.Context) {
	if b.cfg.Authorization == nil || b.cfg.Authorization.Mode == "off" || b.cfg.Authorization.Mode == "" {
		return
	}
	next, err := b.readPermission(ctx)
	b.policyMu.Lock()
	previous := b.publishedPermission
	unchanged := err == nil && previous != nil && next.record.SHA256 == previous.record.SHA256 && os.SameFile(next.file, previous.file) && maps.Equal(next.unavailable, previous.unavailable)
	alreadyEmpty := err != nil && len(b.policy.Tuples) == 0 && b.policy.Reason == "approval_unavailable"
	b.policyMu.Unlock()
	if unchanged || alreadyEmpty {
		return
	}
	b.mu.Lock()
	var ids []engine.SessionID
	for id := range b.sessions {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		if s := b.known(id); s != nil {
			s.reconcilePermission(next, err)
		}
	}
	if err != nil {
		b.publishPermission(nil, "approval_unavailable")
	} else {
		b.publishPermission(next, "")
	}
}

// NewQualificationBackend is the CLI-only integrated candidate constructor.
// Serve and output-play use New, which only accepts owner-approved records.
func NewQualificationBackend(cfg Config) (*Backend, error) {
	if cfg.Authorization == nil {
		return nil, errors.New("candidate record required")
	}
	b, e := New(cfg)
	if e != nil {
		return nil, e
	}
	b.cliCandidate = true
	return b, nil
}

// TransitionPlans is the approved-native-dop-native scenario's CLI-only
// admission (B470 2d.2): the policy each reference plays under, by its ID in
// the owner-approved record, and the directional edges under test, admitted
// here beside the record's own and nowhere else.
type TransitionPlans struct {
	Policies map[string]string
	Edges    []engine.TransitionEdge
}

// NewTransitionQualificationBackend is the CLI-only constructor of the
// approved native↔DoP transition scenario: it reads only an owner-approved
// record (qualification.ReadApproval, never a candidate) and admits each
// occurrence under the approved policy plans names for its reference.
// Serve and output-play use New.
func NewTransitionQualificationBackend(cfg Config, plans TransitionPlans) (*Backend, error) {
	if cfg.Authorization == nil || len(plans.Policies) == 0 {
		return nil, errors.New("approved record and transition plans required")
	}
	b, e := New(cfg)
	if e != nil {
		return nil, e
	}
	b.transitions = &TransitionPlans{Policies: maps.Clone(plans.Policies), Edges: slices.Clone(plans.Edges)}
	return b, nil
}

// reconcilePermission drops only ineligible prepared occurrences; an invalid
// open native output (including a retiring tail) is fenced immediately.
func (s *session) reconcilePermission(next *permission, err error) {
	s.mu.Lock()
	prior := s.permission
	replaced := err != nil || next == nil || prior == nil || prior.revoked.Load()
	if !replaced {
		replaced = next.record.SHA256 != prior.record.SHA256 || !os.SameFile(next.file, prior.file)
	}
	active := false
	for _, out := range s.outs {
		if out.native != nil && !out.closed && (replaced || next.unavailable[edgeKey(out.transport, out.plan)]) {
			active = true
		}
	}
	for _, o := range s.chain {
		if o.opened() && o.plan != nil && o.out == nil && (replaced || next.unavailable[edgeKey(o.transport, o.plan)]) {
			o.dropped = true
			o.refused = &engine.Unplayable{Reason: "native_revoked", Detail: "selected DSD policy is no longer eligible"}
		}
	}
	if replaced {
		s.nativeRevoked = true
		if prior != nil {
			prior.revoked.Store(true)
		}
	}
	s.mu.Unlock()
	if active {
		s.b.Revoke(engine.RevokeInput{Session: s.id, Reason: "selected DSD authorization changed"})
	}
	wake(s.wakeP)
	wake(s.wakeR)
}
