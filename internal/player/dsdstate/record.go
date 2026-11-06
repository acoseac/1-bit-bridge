package dsdstate

import (
	"errors"
	"slices"

	"github.com/acoseac/1-bit-bridge/internal/alsa"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/qualification"
	"github.com/acoseac/1-bit-bridge/internal/usbid"
)

// Bound is the DAC the binding resolves to now, as the migration asks of it:
// its model's key, which the state keys it by, and the legacy reader's
// identity of it (qualification.Identity), which holds the anchored port,
// since the reader requires one, and which the migration's comparison
// (sameModel) passes over. BoundOf makes one from the box's readings.
type Bound struct {
	Key      dacprofile.Key
	Identity qualification.Identity
}

// BoundOf is the DAC the binding resolves to, from what the box reads of its
// card (alsa.SysfsReader.Card and Qualification) and the PCM endpoint the
// binding names on it (device, subdevice): the key of its model, the USB
// vendor and product IDs, the interface the card is and its product string,
// and the identity the legacy reader compares, built as production's
// readPermission builds it. A serial, a card number and a device number go
// into neither, and the port into the identity alone.
func BoundOf(card alsa.Identity, q alsa.QualificationIdentity, device, subdevice int) (Bound, error) {
	if card.Kind != alsa.IdentityUSB || card.USB == nil {
		return Bound{}, errors.New("dsdstate: the card is not a USB unit whose identity was read")
	}
	iface, product := card.USB.Interface, q.Product
	key, err := dacprofile.NewKey(card.USB.ID.Vendor, card.USB.ID.Product, &iface, &product)
	if err != nil {
		return Bound{}, err
	}
	return Bound{Key: key, Identity: qualification.Identity{
		Vendor: usbid.Hex16(card.USB.ID.Vendor), ProductID: usbid.Hex16(card.USB.ID.Product),
		Manufacturer: q.Manufacturer, Product: q.Product, BCDDevice: q.BCDDevice, Port: q.Port,
		Interface: q.Interface, Device: device, Subdevice: subdevice,
	}}, nil
}

// sameModel is the migration's identity comparison (the design's section
// 3.5): the model alone, the vendor and product IDs, manufacturer, product,
// interface, device and subdevice. Not the port, which a record made on
// another port of this model must pass, and not the bcdDevice either: the
// record's firmware goes with each answer it migrates, which publishes only
// on that firmware (section 1.2). So neither qualification.SameIdentity nor
// every field but the port.
func sameModel(recorded, live qualification.Identity) bool {
	return recorded.Vendor == live.Vendor && recorded.ProductID == live.ProductID &&
		recorded.Manufacturer == live.Manufacturer && recorded.Product == live.Product &&
		recorded.Interface == live.Interface && recorded.Device == live.Device && recorded.Subdevice == live.Subdevice
}

// ReadRecord is the migration's read of the owner's record for the bound DAC
// (the design's sections 3.5 and 3.6): name is the legacy dsdQualification,
// a record within stateDir, the bridge's state directory, as the legacy
// reader takes one. It reads through qualification.ReadLegacyApproval alone,
// which takes versions from LegacyVersion (35) to this build's and makes
// every other check ReadApproval makes, with the model comparison
// (sameModel). An unset name is no record (RecordMissing), as an unset
// dsdQualification refused DSD before ("approval missing"), and so is a
// record that does not exist (qualification.ErrRecordMissing); a report or
// an experiment the record names that does not exist is the record not
// reading (unreadable). A refusal's reason is its kind
// (qualification.ErrStaleVersion: stale_version, ErrIdentityMismatch:
// identity_mismatch, ErrHashMismatch: hash_mismatch), and unreadable for
// every other, never read from its text; so is a record taken whose
// firmware is not four hexadecimal digits. A record taken gives its digest,
// its firmware, its qualification date and the rates it approved.
func ReadRecord(stateDir, name string, b Bound) Record {
	if name == "" {
		return Record{Outcome: RecordMissing}
	}
	a, err := qualification.ReadLegacyApproval(stateDir, name, b.Identity, sameModel)
	switch {
	case err == nil:
		return taken(a)
	case errors.Is(err, qualification.ErrRecordMissing):
		return Record{Outcome: RecordMissing, Err: err}
	case errors.Is(err, qualification.ErrStaleVersion):
		return Record{Outcome: RecordRefused, Reason: ReasonStaleVersion, Err: err}
	case errors.Is(err, qualification.ErrIdentityMismatch):
		return Record{Outcome: RecordRefused, Reason: ReasonIdentityMismatch, Err: err}
	case errors.Is(err, qualification.ErrHashMismatch):
		return Record{Outcome: RecordRefused, Reason: ReasonHashMismatch, Err: err}
	}
	return Record{Outcome: RecordRefused, Reason: ReasonUnreadable, Err: err}
}

// taken is what the migration takes of a record the reader took.
func taken(a qualification.Approval) Record {
	bcd, err := usbid.ParseHex16(a.Body.Identity.BCDDevice)
	if err != nil {
		return Record{Outcome: RecordRefused, Reason: ReasonUnreadable, Err: errors.New("dsdstate: the record's bcdDevice is not four hexadecimal digits")}
	}
	r := Record{Outcome: RecordTaken, SHA256: a.SHA256, BCDDevice: usbid.Hex16(bcd), QualifiedOn: a.Body.QualifiedOn}
	for _, p := range a.Body.Policies {
		switch p.Tuple.TransportName() {
		case TransportNative:
			r.Native = append(r.Native, p.Tuple.BitRate)
		case TransportDoP:
			r.DoP = append(r.DoP, p.Tuple.BitRate)
		}
	}
	slices.Sort(r.Native)
	slices.Sort(r.DoP)
	r.Native, r.DoP = slices.Compact(r.Native), slices.Compact(r.DoP)
	return r
}
