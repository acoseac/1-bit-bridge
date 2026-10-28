package playback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/acoseac/1-bit-bridge/internal/alsapcm"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/binding"
	"github.com/acoseac/1-bit-bridge/internal/player/engine"
)

// Content is what a Resolver opened for a reference: the content's bytes,
// read at offsets from 0 to Size and never past it, and how to release
// them.
type Content struct {
	// Data is the content. The backend reads it through an io.SectionReader
	// of Size bytes, so content that grows while it plays is read as the
	// resolver opened it.
	Data io.ReaderAt
	// Size is the content's length in bytes as the resolver opened it.
	Size int64
	// Closer releases Data once the occurrence is done with it, nil when
	// nothing needs releasing.
	Closer io.Closer
}

// Resolver maps an engine.Reference to the content it names. The engine
// hands the backend references, never paths: the harness's resolver opens
// the files it was given, and release 5a's will open the library's tracks,
// with its URI confinement. The backend opens nothing a resolver did not
// hand it.
//
// Open answers the content, or a refusal: an *engine.Unplayable, which the
// backend reports as it is, or any other error, which it reports as
// engine.ReasonMissing (a file that is not there or cannot be opened). Either
// way the occurrence is refused before any of its content played. ctx ends
// at the occurrence's release or replacement, or at the call's deadline.
type Resolver interface {
	Open(ctx context.Context, ref engine.Reference) (Content, error)
}

// Unit is the bound unit as the binding resolves now: the endpoint to open
// and the check after open of the node opened there.
type Unit struct {
	// Subdevice is the bound ALSA endpoint within the PCM device.
	Subdevice int
	// Endpoint is the card and PCM device the binding resolves to.
	Endpoint alsapcm.Endpoint
	// Admit is the check after open (binding.Checker.Check): a sink asks it
	// about each node it opens, at the open and at every reopen after a
	// release, before a frame is written (alsapcm.SinkConfig.Admit).
	Admit func(alsapcm.NodeInfo) error
	// Label names the unit for a person, never by its serial.
	Label string
}

// Locator resolves the configured binding to the unit it names now. The
// backend asks it at every open of the device: at a session's start, at a
// change of transport within a session, and at a sink's reopen after a
// release, where a card renumbered while the device was free is found where
// it is.
type Locator interface {
	Locate(ctx context.Context) (Unit, error)
}

// UnitError is a Locate that found no unit to open: the binding's state,
// as binding.Resolve gives it, and why.
type UnitError struct {
	State  binding.State
	Reason string
	Err    error
}

func (e *UnitError) Error() string {
	var b strings.Builder
	b.WriteString("player: the bound unit")
	if e.State != "" {
		b.WriteString(" is " + string(e.State))
	} else {
		b.WriteString(" could not be resolved")
	}
	if e.Reason != "" {
		b.WriteString(": " + e.Reason)
	}
	if e.Err != nil {
		b.WriteString(" (" + e.Err.Error() + ")")
	}
	return b.String()
}

func (e *UnitError) Unwrap() error { return e.Err }

// BindingLocator is the Locator over the box: it reads /proc/asound and
// sysfs through Observer, resolves Device with binding.Settled, and answers
// the unit when the binding is available on a settled, whole reading, with
// Observer's trees for the check after open, so the check reads what the
// resolve read. A reading that fails, or is not whole (Inventory.Partial,
// which the hotplug watcher publishes no answer from either), and every
// state but available, is a UnitError.
type BindingLocator struct {
	Device   config.PlayerDevice
	Observer binding.Observer
}

// Locate resolves the binding on one reading of the box.
func (l BindingLocator) Locate(ctx context.Context) (Unit, error) {
	inv, err := l.Observer.ObserveContext(ctx)
	if err != nil {
		return Unit{}, &UnitError{Reason: "the box's sound cards could not be read", Err: err}
	}
	if inv.Partial {
		return Unit{}, &UnitError{Reason: "the box could not be read whole (" + strings.Join(inv.Problems, "; ") + "): resolve again"}
	}
	r, settled := binding.Settled(l.Device, inv)
	switch {
	case !settled:
		return Unit{}, &UnitError{State: r.State, Reason: r.Reason}
	case r.State != binding.StateAvailable:
		return Unit{}, &UnitError{State: r.State, Reason: r.Reason}
	}
	check := binding.Checker{Proc: l.Observer.Proc, Sysfs: l.Observer.Sysfs, Want: r}
	return Unit{
		Endpoint:  alsapcm.Endpoint{Card: r.Card, Device: r.Endpoint.Device},
		Subdevice: r.Endpoint.Subdevice,
		Admit:     check.Check,
		Label:     fmt.Sprintf("card %d, %s", r.Card, binding.Describe(r.Identity)),
	}, nil
}

// ErrOverlap is a Start while another session still holds the device, or
// opens it: the engine starts a session only once the last one's release is
// acknowledged, so this is a defect, which the backend refuses rather than
// open a second time (Stats.Overlaps counts them).
var ErrOverlap = errors.New("player: a session started while another still held the device")
