package qualification

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
)

// DoPObservations records affirmative owner observations; nil is unobserved.
type DoPObservations struct {
	Indicator, Channels, PitchDuration *bool
}

// Validate checks listening and indication independently of a PCM carrier lock.
func (o *DoPObservations) Validate() error {
	if o == nil || o.Indicator == nil || !*o.Indicator {
		return errors.New("DoP DSD indicator unconfirmed")
	}
	if o.Channels == nil || !*o.Channels || o.PitchDuration == nil || !*o.PitchDuration {
		return errors.New("DoP listening unconfirmed")
	}
	return nil
}

// TransportName names the transport; legacy native tuples retain their meaning.
func (t Tuple) TransportName() string {
	if t.Transport == "" {
		return "native"
	}
	return t.Transport
}

// ValidTransport refuses arbitrary PCM shapes and inconsistent identities.
func (t Tuple) ValidTransport() bool {
	switch t.TransportName() {
	case "dop":
		return t.Format == "S32_LE"
	case "native":
		return t.Format == "DSD_U32_LE" || t.Format == "DSD_U32_BE" || t.Format == "DSD_U16_LE"
	}
	return false
}

// CheckDoPExperiments binds reviewed changed-state contacts to report bytes.
// Each file remains under the same owner-controlled evidence root.
func CheckDoPExperiments(root string, refs []Evidence, tuple Tuple, id Identity, manifest, environment string) error {
	if len(refs) < 2 || len(refs) > 16 {
		return errors.New("DoP feature unit requires bounded experiment references")
	}
	seen := map[string]bool{}
	states := map[string]bool{}
	mixers := map[string]bool{}
	for _, ref := range refs {
		if seen[ref.SHA256] {
			return errors.New("duplicate DoP experiment")
		}
		seen[ref.SHA256] = true
		b, e := trustedFile(root, ref.Path)
		if e != nil {
			return e
		}
		if !validHash(ref.SHA256) || fmt.Sprintf("%x", sha256.Sum256(b)) != ref.SHA256 {
			return errors.New("DoP experiment hash mismatch")
		}
		var r executedReport
		if e = json.Unmarshal(b, &r); e != nil {
			return e
		}
		if r.SchemaVersion != 2 || r.QualificationVersion != Version || r.Tuple != tuple || r.Identity != id || r.ManifestHash != manifest || r.Environment != environment || r.End != "complete" || r.Error != "" || r.Contact == nil || r.Scenario != "level-check" {
			return errors.New("DoP experiment context mismatch")
		}
		if e = r.DoP.Validate(); e != nil {
			return e
		}
		if e = r.validateDoPGeometry(); e != nil {
			return e
		}
		if r.Modified || r.BuildSHA == "" || r.BuildSHA == "unverified" || r.FeatureUnit == nil || !*r.FeatureUnit || len(r.Completed) != 1 || !r.Completed[0] {
			return errors.New("DoP experiment was not completely observed")
		}
		c := r.Contact
		if c.Device == nil || c.Subdevice == nil || *c.Device != id.Device || *c.Subdevice != id.Subdevice {
			return errors.New("DoP experiment endpoint missing")
		}
		if c.Attachment.Port != id.Port || c.Attachment.Manufacturer != id.Manufacturer || c.Attachment.Product != id.Product || c.Attachment.BCDDevice != id.BCDDevice || c.Attachment.Interface != id.Interface {
			return errors.New("DoP experiment attachment mismatch")
		}
		if e = c.Validate(tuple, c.Attachment, *c.Device, *c.Subdevice, r.Before.Hash(), manifest); e != nil {
			return e
		}
		for _, snap := range []alsactl.Snapshot{r.Before, r.After} {
			for _, control := range snap.Controls {
				if control.ReadError != "" {
					return errors.New("DoP experiment controls unreadable")
				}
			}
		}
		if r.Before.Hash() != r.After.Hash() {
			return errors.New("DoP experiment drift")
		}
		states[r.ExperimentState] = true
		mixers[r.Before.Hash()] = true
	}
	if !states["nonzero-attenuation"] || !states["restored"] || len(mixers) < 2 {
		return errors.New("DoP experiment lacks changed attenuation and restored state")
	}
	return nil
}

// dopGeometry decodes the hardware facts needed by DoP evidence admission.
type dopGeometry struct {
	Format                                               string
	RateHz, Channels, Msbits, PeriodFrames, BufferFrames int
	RateNum, RateDen                                     uint32
}

func (r executedReport) validateDoPGeometry() error {
	if r.PackingVersion != 1 {
		return errors.New("DoP packer version missing")
	}
	if r.SourceRate != r.Tuple.BitRate {
		return errors.New("DoP source rate mismatch")
	}
	if r.CarrierRate != r.Tuple.BitRate/16 {
		return errors.New("DoP carrier rate mismatch")
	}
	if r.Kernel == "" || r.Platform == "" || r.GoVersion == "" || r.Environment != Digest([]string{r.Platform, r.GoVersion, r.Kernel}) {
		return errors.New("DoP environment missing")
	}
	if len(r.Opened) == 0 || len(r.Opened) > 1024 {
		return errors.New("DoP geometry missing or unbounded")
	}
	for _, g := range r.Opened {
		if g.Msbits < 24 || g.Msbits > 32 {
			return errors.New("DoP precision insufficient")
		}
		if g.Format != "S32_LE" || g.Channels != 2 || g.RateHz != r.CarrierRate {
			return errors.New("DoP hardware layout mismatch")
		}
		if g.RateDen == 0 || uint64(g.RateNum) != uint64(g.RateHz)*uint64(g.RateDen) {
			return errors.New("DoP rational rate mismatch")
		}
		if g.PeriodFrames <= 0 || g.BufferFrames < g.PeriodFrames || g.BufferFrames > g.RateHz/2 {
			return errors.New("DoP hardware buffer out of bounds")
		}
	}
	return nil
}
