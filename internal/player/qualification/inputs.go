// Package qualification validates provisional listening evidence. It grants no
// production playback permission and never writes an approval.
package qualification

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/acoseac/1-bit-bridge/internal/fsutil"
	"io"
	"os"
	"path/filepath"
)

// Manifest is an owner's calibrated fixture list, not decoder test goldens.
type Manifest struct {
	SchemaVersion int         `json:"schemaVersion"`
	Calibration   Calibration `json:"calibration"`
	Fixtures      []Fixture   `json:"fixtures"`
}

// Calibration records the owner's reference and typed confirmation.
type Calibration struct {
	Reference         string  `json:"reference"`
	PeakDBFS          float64 `json:"peakDBFS"`
	LeadingSilenceMS  int     `json:"leadingSilenceMS"`
	TrailingSilenceMS int     `json:"trailingSilenceMS"`
	Attestation       string  `json:"attestation"`
}

// Fixture names one hashed listening asset and its matched PCM reference.
type Fixture struct {
	ID, Path, SHA256, Container, Order, PCMReference string
	Rate, Channels                                   int
	Frames                                           int64
}

// Tuple includes the explicit transport and its single lifecycle policy.
type Tuple struct {
	Format            string
	BitRate           int
	Pause             string
	TailMS, PrefillMS int
}

// Attachment changes on host reboot or USB re-enumeration, not CLI exit.
type Attachment struct {
	BootID, Port, Manufacturer, Product, BCDDevice string
	Card, DevNum, Interface                        int
}

// Contact is a provisional receipt; its confirmations are typed, not signed.
type Contact struct {
	SchemaVersion, QualificationVersion                           int
	Tuple                                                         Tuple
	Attachment                                                    Attachment
	MixerHash, ManifestHash, KnobPosition, PCMAudible, DSDMatched string
}

// CalibrationPhrase is the required exact typed owner confirmation.
const CalibrationPhrase = "I confirm the matched -30 dBFS listening fixtures and end silences"

// PCMConfirmation is typed only after an audible off-ear PCM reference.
const PCMConfirmation = "PCM audible"

// DSDConfirmation is typed only after matched-level off-ear DSD listening.
const DSDConfirmation = "DSD matched"

// ReadManifest checks a bounded versioned manifest before opening audio devices.
func ReadManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	b, e := io.ReadAll(io.LimitReader(r, 1<<20+1))
	if e != nil {
		return m, e
	}
	if len(b) > 1<<20 {
		return m, errors.New("manifest exceeds 1 MiB")
	}
	if e = json.Unmarshal(b, &m); e != nil {
		return m, e
	}
	if m.SchemaVersion != 1 {
		return m, errors.New("manifest schemaVersion must be 1")
	}
	c := m.Calibration
	if c.Attestation != CalibrationPhrase || c.Reference == "" || c.PeakDBFS != -30 {
		return m, errors.New("missing matched-level calibration")
	}
	if c.LeadingSilenceMS < 1000 || c.TrailingSilenceMS < 1000 {
		return m, errors.New("missing end silence")
	}
	if len(m.Fixtures) == 0 || len(m.Fixtures) > 128 {
		return m, errors.New("fixture list bound")
	}
	ids := map[string]Fixture{}
	for _, f := range m.Fixtures {
		if f.ID == "" || f.Path == "" || !filepath.IsLocal(f.Path) || f.Channels != 2 || f.Rate <= 0 || f.Frames <= 0 {
			return m, errors.New("invalid fixture")
		}
		hash, he := hex.DecodeString(f.SHA256)
		if he != nil || len(hash) != 32 || (f.Order != "LR" && f.Order != "RL") || f.Frames > int64(f.Rate)*10 || f.Frames < int64(f.Rate)*2 {
			return m, errors.New("invalid fixture hash, order or duration")
		}
		if _, ok := ids[f.ID]; ok {
			return m, errors.New("duplicate fixture")
		}
		ids[f.ID] = f
	}
	for _, f := range m.Fixtures {
		if f.Container == "DSF" || f.Container == "DFF" {
			ref, ok := ids[f.PCMReference]
			if !ok || ref.Container != "WAV" || ref.Frames*int64(f.Rate) != f.Frames*int64(ref.Rate) {
				return m, errors.New("missing matched PCM reference")
			}
			if f.Frames > int64(f.Rate)*10 {
				return m, errors.New("contact DSD exceeds ten seconds")
			}
		} else if f.Container != "WAV" {
			return m, errors.New("unsupported listening fixture")
		}
	}
	return m, nil
}

// CheckAssets checks all content hashes beneath root before any device write.
func (m Manifest) CheckAssets(root string) error {
	r, e := os.OpenRoot(root)
	if e != nil {
		return e
	}
	defer r.Close()
	for _, f := range m.Fixtures {
		st, e := r.Stat(f.Path)
		if e != nil {
			return e
		}
		if !st.Mode().IsRegular() {
			return errors.New("fixture is not regular")
		}
		file, e := r.Open(f.Path)
		if e != nil {
			return e
		}
		st, e = file.Stat()
		if e != nil {
			file.Close()
			return e
		}
		if !st.Mode().IsRegular() {
			file.Close()
			return errors.New("fixture is not regular")
		}
		h := sha256.New()
		_, e = io.Copy(h, io.LimitReader(file, 64<<20+1))
		ce := file.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
		if hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
			return fmt.Errorf("fixture %s hash mismatch", f.ID)
		}
	}
	return nil
}

// Validate checks a receipt against the current tuple and live attachment.
func (c Contact) Validate(t Tuple, a Attachment, mixer, manifest string) error {
	if c.SchemaVersion != 1 || c.QualificationVersion != Version {
		return errors.New("contact version mismatch")
	}
	if c.Tuple != t {
		return errors.New("contact tuple mismatch")
	}
	if a.BootID == "" || a.DevNum <= 0 || a.Port == "" || a.Manufacturer == "" || a.Product == "" || a.BCDDevice == "" || c.Attachment != a {
		return errors.New("contact attachment mismatch")
	}
	if mixer == "" || manifest == "" || c.MixerHash != mixer || c.ManifestHash != manifest {
		return errors.New("contact evidence mismatch")
	}
	if c.KnobPosition == "" || c.PCMAudible != PCMConfirmation || c.DSDMatched != DSDConfirmation {
		return errors.New("contact listening incomplete")
	}
	return nil
}

// ReadEvidenceFile reads only a bounded regular local evidence file.
func ReadEvidenceFile(path string) ([]byte, error) {
	f, _, e := fsutil.OpenAsFile(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if e == nil && len(b) > 1<<20 {
		return nil, errors.New("evidence exceeds 1 MiB")
	}
	return b, e
}
