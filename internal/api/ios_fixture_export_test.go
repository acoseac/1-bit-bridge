package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// TestExportIOSOutputFixtures collects independently simulated players into one
// OutputsResponse fixture. Each element comes from the real HTTP handler; a live
// bridge lists only its own player. Export after the manifest package with -p=1.
func TestExportIOSOutputFixtures(t *testing.T) {
	auth := outputsToken(t)
	token, _, err := auth.Mint("fixture")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeBridgeOutput{enabled: true}
	server := New(&config.Config{LibraryName: "iOS fixtures"}, auth, nil, "fixture-fingerprint").WithBridgeOutput(fake)
	handler := server.Handler()
	document := OutputsResponse{Outputs: []OutputInfo{}}
	records := []map[string]any{}
	for i, name := range []string{"dsd_open", "dsd_released", "dsd_revoked", "pcm_open", "empty_tuples", "no_backend"} {
		f := listedPlayer()
		f.UUID = fmt.Sprintf("7d444840-9dc0-11d1-b245-%012d", i+1)
		f.DSDMode = "native"
		f.DSDPlayback = &OutputDSDPlayback{BootID: "fixture-boot", PolicyGeneration: 9, QualificationRevision: "fixture-revision", Reason: "fixture display reason", Tuples: []OutputDSDTuple{
			{Format: "DSD_U32_BE", BitRate: 2822400, Channels: 2, PolicyID: "native-be", Pause: "kernel", TailMS: 10, PrefillMS: 20},
			{Format: "DSD_U32_LE", BitRate: 5644800, Channels: 2, PolicyID: "native-le", Pause: "silence", TailMS: 0, PrefillMS: 0},
			{Format: "DSD_U16_LE", BitRate: 2822400, Channels: 2, PolicyID: "native-16", Pause: "kernel", TailMS: 5, PrefillMS: 10},
		}}
		f.Codecs = append(f.Codecs, "dsf", "dff")
		f.Playback = &OutputPlayback{BootID: "fixture-boot", PolicyGeneration: 8, Session: 41, Occurrence: 51, State: "open", Format: "DSD_U32_BE", RateHz: 88200, LogicalRate: 2822400, Channels: 2, ValidBits: 1, PolicyID: "native-be"}
		switch name {
		case "dsd_open":
			f.State = "playing"
		case "dsd_released":
			f.State = "paused"
			f.Playback.State = "released"
		case "dsd_revoked":
			f.Playback.State = "revoked"
			f.DSDPlayback.Tuples = []OutputDSDTuple{}
			f.DSDPlayback.Reason = "approval_unavailable"
			f.DSDPlayback.QualificationRevision = ""
			f.Codecs = listedPlayer().Codecs
		case "pcm_open":
			f.State = "playing"
			f.Playback.Session = 42
			f.Playback.Occurrence = 52
			f.Playback.Format = "S32_LE"
			f.Playback.RateHz = 48000
			f.Playback.LogicalRate = 0
			f.Playback.ValidBits = 24
			f.Playback.PolicyID = ""
		case "empty_tuples":
			f.Playback = nil
			f.DSDPlayback.Tuples = []OutputDSDTuple{}
			f.DSDPlayback.QualificationRevision = ""
			f.DSDPlayback.Reason = ""
			f.Codecs = listedPlayer().Codecs
		case "no_backend":
			f.Playback = nil
			f.DSDPlayback = nil
			f.Codecs = listedPlayer().Codecs
			f.State = "unavailable"
			f.Reason = "starting"
		}
		fake.set(f)
		request := httptest.NewRequest(http.MethodGet, "/v1/outputs", nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", name, response.Code, response.Body.String())
		}
		elements := rawElements(t, response.Body.Bytes())
		if len(elements) != 1 {
			t.Fatalf("%s: %d outputs", name, len(elements))
		}
		// Assert every value survived the facts-to-DTO-to-HTTP pipeline,
		// including nonempty revision/reason and the exact transport fields.
		var decoded OutputBridge
		if err := json.Unmarshal(elements[0]["bridge"], &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded.DSDPlayback, f.DSDPlayback) || !reflect.DeepEqual(decoded.Playback, f.Playback) {
			t.Fatalf("%s: serialized policy or occurrence differs from source facts", name)
		}
		var bridge map[string]json.RawMessage
		if err := json.Unmarshal(elements[0]["bridge"], &bridge); err != nil {
			t.Fatal(err)
		}
		if name == "no_backend" {
			for _, key := range []string{"dsdPlayback", "playback"} {
				if _, ok := bridge[key]; ok {
					t.Fatalf("%s unexpectedly present", key)
				}
			}
		} else {
			var cap map[string]json.RawMessage
			if err := json.Unmarshal(bridge["dsdPlayback"], &cap); err != nil {
				t.Fatal(err)
			}
			if string(cap["policyGeneration"]) != "9" {
				t.Fatal("capability generation lost")
			}
			if name == "empty_tuples" || name == "dsd_revoked" {
				if string(cap["tuples"]) != "[]" {
					t.Fatal("empty tuples must be []")
				}
				if _, ok := cap["qualificationRevision"]; ok {
					t.Fatal("empty revision emitted")
				}
			} else {
				var tuples []OutputDSDTuple
				if err := json.Unmarshal(cap["tuples"], &tuples); err != nil {
					t.Fatal(err)
				}
				if len(tuples) != 3 || tuples[0].Pause != "kernel" || tuples[1].Pause != "silence" {
					t.Fatal("native tuple coverage lost")
				}
			}
			if name == "empty_tuples" {
				if _, ok := cap["reason"]; ok {
					t.Fatal("empty reason emitted")
				}
				if _, ok := bridge["playback"]; ok {
					t.Fatal("uninstalled playback emitted")
				}
			} else {
				var live map[string]json.RawMessage
				if err := json.Unmarshal(bridge["playback"], &live); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"bootId", "policyGeneration", "session", "occurrence", "state", "format", "rateHz", "logicalRate", "channels", "validBits"} {
					if _, ok := live[key]; !ok {
						t.Fatalf("missing %s", key)
					}
				}
				if string(live["policyGeneration"]) != "8" || string(live["state"]) != fmt.Sprintf("%q", f.Playback.State) {
					t.Fatal("observation replaced by capability")
				}
				want := "2822400"
				if name == "pcm_open" {
					want = "0"
					if _, ok := live["policyId"]; ok {
						t.Fatal("PCM policyId emitted")
					}
				}
				if string(live["logicalRate"]) != want {
					t.Fatalf("%s: logicalRate %s", name, live["logicalRate"])
				}
			}
		}
		input, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, map[string]any{"case": name, "document": "dsd_outputs.json", "outputIndex": i, "inputKind": "BridgeOutputFacts JSON", "inputSHA256": fmt.Sprintf("%x", sha256.Sum256(input)), "input": json.RawMessage(input)})
		var rendered OutputsResponse
		if err := json.Unmarshal(response.Body.Bytes(), &rendered); err != nil {
			t.Fatal(err)
		}
		document.Outputs = append(document.Outputs, rendered.Outputs...)
	}
	b, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// Provenance is built in memory even when publication is disabled.
	metadata, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if dir := os.Getenv("BRIDGE_EXPORT_IOS_FIXTURES"); dir != "" {
		path := filepath.Join(dir, "fixtures-info.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("run manifest exporter first with -p=1: %v", err)
		}
		var info map[string]json.RawMessage
		if err := json.Unmarshal(data, &info); err != nil {
			t.Fatal(err)
		}
		info["outputRecords"] = metadata
		data, err = json.MarshalIndent(info, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "dsd_outputs.json"), append(b, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
