package qualification

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/alsactl"
)

func dopObserved() *DoPObservations { yes := true; return &DoPObservations{&yes, &yes, &yes} }
func dopApprovalFixture(t *testing.T) (string, Approval, Identity) {
	dir, a, id := approvalFixture(t)
	a.Body.Policies[0].Tuple.Format = "S32_LE"
	a.Body.Policies[0].Tuple.Transport = "dop"
	a.Body.Environment = Digest([]string{"linux/arm64", "test-go", "test-kernel"})
	for _, ev := range []*Evidence{&a.Body.Policies[0].Matrix, &a.Body.Policies[0].Lifecycle} {
		b, e := os.ReadFile(filepath.Join(dir, ev.Path))
		if e != nil {
			t.Fatal(e)
		}
		var r map[string]any
		json.Unmarshal(b, &r)
		r["SchemaVersion"] = 2
		r["Environment"] = a.Body.Environment
		r["PackingVersion"] = 1
		r["SourceRate"] = 2822400
		r["CarrierRate"] = 176400
		r["Kernel"] = "test-kernel"
		r["Platform"] = "linux/arm64"
		r["GoVersion"] = "test-go"
		r["Opened"] = []map[string]any{{"Format": "S32_LE", "RateHz": 176400, "Channels": 2, "Msbits": 32, "PeriodFrames": 8820, "BufferFrames": 35280, "RateNum": 176400, "RateDen": 1}}

		r["Tuple"] = a.Body.Policies[0].Tuple
		r["NativeTuple"] = a.Body.Policies[0].Tuple
		r["DoP"] = dopObserved()
		b, _ = json.Marshal(r)
		if e = os.WriteFile(filepath.Join(dir, ev.Path), b, 0600); e != nil {
			t.Fatal(e)
		}
		ev.SHA256 = hashBytes(b)
	}
	a.SHA256 = Digest(a.Body)
	writeApproval(t, dir, a)
	return dir, a, id
}
func TestDoPEvidenceValidation(t *testing.T) {
	for _, name := range []string{"receipt_indicator", "receipt_listening", "candidate_indicator", "candidate_listening", "approval_indicator", "approval_listening", "transport"} {
		t.Run(name, func(t *testing.T) {
			if name == "transport" {
				if (Tuple{Format: "S32_LE"}).ValidTransport() {
					t.Fatal("PCM S32 inferred as DoP")
				}
				return
			}
			if name[:7] == "receipt" {
				// Version 3 receipt and otherwise matching endpoint/evidence.
				tuple := Tuple{Transport: "dop", Format: "S32_LE", BitRate: 2822400, Pause: "kernel"}
				a := Attachment{BootID: "boot", Port: "port", Manufacturer: "m", Product: "p", BCDDevice: "1", DevNum: 1}
				c := Contact{SchemaVersion: 3, QualificationVersion: Version, Device: new(int), Subdevice: new(int), Tuple: tuple, Attachment: a, MixerHash: "m", ManifestHash: "f", KnobPosition: "1", PCMAudible: PCMConfirmation, DSDMatched: DSDConfirmation, DoP: dopObserved()}
				if e := c.Validate(tuple, a, 0, 0, "m", "f"); e != nil {
					t.Fatal("fresh receipt:", e)
				}
				if name == "receipt_indicator" {
					c.DoP.Indicator = nil
				} else {
					no := false
					c.DoP.Channels = &no
				}
				if c.Validate(tuple, a, 0, 0, "m", "f") == nil {
					t.Fatal("missing DoP observation accepted")
				}
				return
			}
			dir, a, id := dopApprovalFixture(t)
			candidate := name[:9] == "candidate"
			read := ReadApproval
			if candidate {
				read = ReadCandidate
				a.Body.Confirmation = "CLI candidate; not approved"
				a.SHA256 = Digest(a.Body)
				writeApproval(t, dir, a)
			}
			if _, e := read(dir, "approved.json", id); e != nil {
				t.Fatal("fresh DoP evidence:", e)
			}
			ev := &a.Body.Policies[0].Matrix
			b, e := os.ReadFile(filepath.Join(dir, ev.Path))
			if e != nil {
				t.Fatal(e)
			}
			var r map[string]any
			json.Unmarshal(b, &r)
			o := dopObserved()
			if name == "candidate_indicator" || name == "approval_indicator" {
				o.Indicator = nil
			} else {
				no := false
				o.PitchDuration = &no
			}
			r["DoP"] = o
			b, _ = json.Marshal(r)
			os.WriteFile(filepath.Join(dir, ev.Path), b, 0600)
			ev.SHA256 = hashBytes(b)
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, a)
			if _, e = read(dir, "approved.json", id); e == nil {
				t.Fatal("missing DoP observation accepted")
			}
		})
	}
}

func TestDoPReportGeometry(t *testing.T) {
	for _, name := range []string{"packer", "source", "carrier", "kernel", "precision", "layout", "rational", "buffer", "environment"} {
		t.Run(name, func(t *testing.T) {
			dir, a, id := dopApprovalFixture(t)
			if _, e := ReadApproval(dir, "approved.json", id); e != nil {
				t.Fatal("fresh geometry", e)
			}
			ev := &a.Body.Policies[0].Matrix
			b, e := os.ReadFile(filepath.Join(dir, ev.Path))
			if e != nil {
				t.Fatal(e)
			}
			var r map[string]any
			json.Unmarshal(b, &r)
			switch name {
			case "packer":
				r["PackingVersion"] = 0
			case "source":
				r["SourceRate"] = 176400
			case "carrier":
				r["CarrierRate"] = 88200
				g := r["Opened"].([]any)[0].(map[string]any)
				g["RateHz"] = 88200
				g["RateNum"] = 88200
			case "environment":
				r["Platform"] = "other/arch"
			case "kernel":
				r["Kernel"] = ""
			default:
				g := r["Opened"].([]any)[0].(map[string]any)
				switch name {
				case "precision":
					g["Msbits"] = 20
				case "layout":
					g["Format"] = "S24_LE"
				case "rational":
					g["RateDen"] = 0
				case "buffer":
					g["BufferFrames"] = 176400
				}
			}
			b, _ = json.Marshal(r)
			os.WriteFile(filepath.Join(dir, ev.Path), b, 0600)
			ev.SHA256 = hashBytes(b)
			a.SHA256 = Digest(a.Body)
			writeApproval(t, dir, a)
			if _, e = ReadApproval(dir, "approved.json", id); e == nil {
				t.Fatal("invalid DoP geometry accepted")
			}
		})
	}
}

func TestDoPExperiments(t *testing.T) {
	for _, name := range []string{"valid", "hash", "context", "indicator", "states", "unbound", "endpoint", "attachment", "unreadable"} {
		t.Run(name, func(t *testing.T) {
			dir, a, id := dopApprovalFixture(t)
			tuple := a.Body.Policies[0].Tuple
			b, e := os.ReadFile(filepath.Join(dir, a.Body.Policies[0].Matrix.Path))
			if e != nil {
				t.Fatal(e)
			}
			var refs []Evidence
			for i, state := range []string{"nonzero-attenuation", "restored"} {
				var r executedReport
				if e = json.Unmarshal(b, &r); e != nil {
					t.Fatal(e)
				}
				snap := alsactl.Snapshot{Controls: []alsactl.Control{{Interface: 2, Name: "Playback Volume", Values: []byte{byte(i + 1)}, Access: 1}}}
				if i == 0 && name == "unreadable" {
					snap.Controls[0].ReadError = "unreadable"
				}
				yes := true
				r.FeatureUnit = &yes
				r.Scenario = "level-check"
				r.ExperimentState = state
				r.Before = snap
				r.After = snap
				attach := Attachment{BootID: "boot", Port: id.Port, Manufacturer: id.Manufacturer, Product: id.Product, BCDDevice: id.BCDDevice, Interface: id.Interface, DevNum: 1}
				r.Contact = &Contact{SchemaVersion: 3, QualificationVersion: Version, Device: new(int), Subdevice: new(int), Tuple: tuple, Attachment: attach, MixerHash: snap.Hash(), ManifestHash: a.Body.ManifestHash, KnobPosition: "1", PCMAudible: PCMConfirmation, DSDMatched: DSDConfirmation, DoP: dopObserved()}
				if i == 0 {
					switch name {
					case "endpoint":
						*r.Contact.Device = 1
					case "attachment":
						r.Contact.Attachment.Product = "other"
					case "context":
						r.Environment = "other"
					case "indicator":
						r.DoP.Indicator = nil
					case "states":
						r.ExperimentState = "restored"
					}
				}
				data, _ := json.Marshal(r)
				path := "experiment-" + state + ".json"
				if e = os.WriteFile(filepath.Join(dir, path), data, 0600); e != nil {
					t.Fatal(e)
				}
				ref := Evidence{Path: path, SHA256: hashBytes(data)}
				if i == 0 && name == "hash" {
					ref.SHA256 = Digest("wrong")
				}
				refs = append(refs, ref)
			}
			if name == "unbound" {
				refs = nil
			}
			e = CheckDoPExperiments(dir, refs, tuple, id, a.Body.ManifestHash, a.Body.Environment)
			if (e == nil) != (name == "valid") {
				t.Fatalf("experiment validation: %v", e)
			}
		})
	}
}
