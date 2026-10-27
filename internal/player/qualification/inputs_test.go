package qualification

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func goodManifest() Manifest {
	return Manifest{SchemaVersion: 1, Calibration: Calibration{Reference: "independent analog calibration", PeakDBFS: -30, LeadingSilenceMS: 1000, TrailingSilenceMS: 1000, Attestation: CalibrationPhrase}, Fixtures: []Fixture{{ID: "pcm", SHA256: strings.Repeat("a", 64), Path: "pcm.wav", Container: "WAV", Order: "LR", Rate: 44100, Channels: 2, Frames: 352800}, {ID: "dsd", SHA256: strings.Repeat("b", 64), Path: "dsd.dsf", Container: "DSF", Order: "LR", Rate: 2822400, Channels: 2, Frames: 22579200, PCMReference: "pcm"}}}
}

// TestQualificationManifest requires calibrated, explicit listening assets.
func TestQualificationManifest(t *testing.T) {
	for _, name := range []string{"valid", "schema", "calibration", "silence", "reference"} {
		t.Run(name, func(t *testing.T) {
			m := goodManifest()
			switch name {
			case "schema":
				m.SchemaVersion = 2
			case "calibration":
				m.Calibration.Attestation = "yes"
			case "silence":
				m.Calibration.TrailingSilenceMS = 0
			case "reference":
				m.Fixtures[1].PCMReference = "absent"
			}
			b, _ := json.Marshal(m)
			_, e := ReadManifest(bytes.NewReader(b))
			if (e == nil) != (name == "valid") {
				t.Fatalf("manifest %s: %v", name, e)
			}
		})
	}
}

// TestQualificationAssets rejects bytes changed after calibration.
func TestQualificationAssets(t *testing.T) {
	dir := t.TempDir()
	m := goodManifest()
	for i, f := range m.Fixtures {
		b := []byte(f.ID)
		h := sha256.Sum256(b)
		m.Fixtures[i].SHA256 = hex.EncodeToString(h[:])
		if e := os.WriteFile(filepath.Join(dir, f.Path), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := m.CheckAssets(dir); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "dsd.dsf"), []byte("altered"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := m.CheckAssets(dir); e == nil {
		t.Fatal("altered asset accepted")
	}
}

// TestQualificationContact rejects stale attachment and incomplete listening.
func TestQualificationContact(t *testing.T) {
	a := Attachment{BootID: "boot", Port: "port", Manufacturer: "iFi", Product: "GO bar", BCDDevice: "0123", Card: 5, DevNum: 3}
	tuple := Tuple{Format: "DSD_U32_BE", BitRate: 2822400, Pause: "kernel"}
	base := Contact{Device: new(int), Subdevice: new(int), SchemaVersion: 2, QualificationVersion: Version, Tuple: tuple, Attachment: a, MixerHash: "m", ManifestHash: "f", KnobPosition: "just audible", PCMAudible: PCMConfirmation, DSDMatched: DSDConfirmation}
	for _, name := range []string{"valid", "tuple", "replug", "restart", "mixer", "manifest", "pcm", "dsd", "version"} {
		t.Run(name, func(t *testing.T) {
			c := base
			switch name {
			case "tuple":
				c.Tuple.BitRate *= 2
			case "replug":
				c.Attachment.DevNum++
			case "restart":
				c.Attachment.BootID = "old"
			case "mixer":
				c.MixerHash = "old"
			case "manifest":
				c.ManifestHash = "old"
			case "pcm":
				c.PCMAudible = ""
			case "dsd":
				c.DSDMatched = ""
			case "version":
				c.QualificationVersion++
			}
			e := c.Validate(tuple, a, 0, 0, "m", "f")
			if (e == nil) != (name == "valid") {
				t.Fatalf("contact: %v", e)
			}
		})
	}
}
