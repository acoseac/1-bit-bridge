// Package alsactl reads bounded ALSA control snapshots. It never changes gain,
// restores values or offers a write ioctl.
package alsactl

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
)

// Control is one semantic control and its complete read value.
type Control struct {
	Interface, Device, Subdevice, Index uint32
	Name                                string
	Type                                uint32
	Access                              uint32
	Count                               uint32
	Values                              []byte
	Info                                []byte
	TLV                                 []byte
}

// Snapshot contains every readable control, including controls without gain.
type Snapshot struct {
	Card     int
	Controls []Control
}

// Excluded reports the only controls whose values may change when PCM opens:
// volatile controls on the PCM interface. Mixer and card controls always bind.
func (c Control) Excluded() bool { return c.Interface == 3 && c.Access&(1<<2) != 0 }

// Hash is the stable activation fingerprint. The complete snapshot remains
// available for reporting; only volatile PCM controls are excluded here.
func (s Snapshot) Hash() string {
	stable := Snapshot{Card: s.Card, Controls: make([]Control, 0, len(s.Controls))}
	for _, c := range s.Controls {
		if !c.Excluded() {
			stable.Controls = append(stable.Controls, c)
		}
	}
	b, _ := json.Marshal(stable)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// Device is the injected read-only control-device seam.
type Device interface {
	Controls() ([]Control, error)
	Close() error
}

// Reader takes its opener through an instance, so tests never require hardware.
type Reader struct {
	Open func(card int) (Device, error)
}

// Read obtains one complete snapshot and always closes its control descriptor.
func (r Reader) Read(card int) (Snapshot, error) {
	if card < 0 || card > 255 {
		return Snapshot{}, errors.New("invalid control card")
	}
	open := r.Open
	if open == nil {
		open = productionOpen
	}
	d, e := open(card)
	if e != nil {
		return Snapshot{}, e
	}
	v, e := d.Controls()
	ce := d.Close()
	if e != nil {
		return Snapshot{}, e
	}
	if ce != nil {
		return Snapshot{}, ce
	}
	if len(v) > 1024 {
		return Snapshot{}, errors.New("control count exceeds bound")
	}
	sort.Slice(v, func(i, j int) bool {
		a, b := v[i], v[j]
		if a.Interface != b.Interface {
			return a.Interface < b.Interface
		}
		if a.Device != b.Device {
			return a.Device < b.Device
		}
		if a.Subdevice != b.Subdevice {
			return a.Subdevice < b.Subdevice
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Index < b.Index
	})
	return Snapshot{Card: card, Controls: v}, nil
}

var guard atomic.Pointer[func(string)]

// GuardNodeOpens refuses and reports control-node access in a test binary.
func GuardNodeOpens(report func(string)) func() {
	guard.Store(&report)
	return func() { guard.CompareAndSwap(&report, nil) }
}
func productionOpen(card int) (Device, error) {
	if g := guard.Load(); g != nil {
		(*g)(fmt.Sprintf("/dev/snd/controlC%d", card))
		return nil, errors.New("control node guarded")
	}
	if testing.Testing() {
		return nil, errors.New("control hardware prohibited in tests")
	}
	return platformOpen(card)
}
