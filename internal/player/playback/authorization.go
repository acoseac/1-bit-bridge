package playback

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
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
		return nil, errors.New("DSD is off")
	}
	if a.Mode != "native" && a.Mode != "auto" {
		return nil, errors.New("DoP is not implemented")
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
	return &permission{record: record, file: after, unavailable: unavailable}, nil
}
func planOf(p qualification.ApprovedPolicy, a qualification.Approval) engine.NativePlan {
	plan := engine.NativePlan{Tuple: engine.NativeTuple{Transport: p.Tuple.Transport, Format: p.Tuple.Format, BitRate: p.Tuple.BitRate}, PolicyID: p.ID, Pause: p.Tuple.Pause, Tail: time.Duration(p.Tuple.TailMS) * time.Millisecond, Prefill: time.Duration(p.Tuple.PrefillMS) * time.Millisecond}
	for _, e := range a.Body.Edges {
		plan.Edges = append(plan.Edges, engine.TransitionEdge{From: e.From, To: e.To})
	}
	return plan
}
func (p *permission) selectNative(f format.NativeDSD) (engine.NativePlan, error) {
	if p == nil || p.revoked.Load() {
		return productionNativeAdmission(f)
	}
	for _, preferred := range []string{"DSD_U32_BE", "DSD_U32_LE", "DSD_U16_LE"} {
		for _, t := range p.record.Body.Policies {
			if !p.unavailable[qualification.NativeKey(t)] && t.Tuple.Format == preferred && t.Tuple.BitRate == f.BitRateHz && f.Channels == 2 {
				return planOf(t, p.record), nil
			}
		}
	}
	return engine.NativePlan{}, errors.New("no approved native tuple")
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
	if p != nil {
		p.generation = b.policy.Generation
		b.policy.Revision = p.record.Body.Revision
		for _, v := range p.record.Body.Policies {
			if p.unavailable[qualification.NativeKey(v)] || format.DSDWordBytes(format.ALSAFormat(v.Tuple.Format)) == 0 {
				continue
			}
			b.policy.Tuples = append(b.policy.Tuples, planOf(v, p.record))
		}
		if len(b.policy.Tuples) == 0 {
			b.policy.Reason = "no_implemented_tuple"
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
