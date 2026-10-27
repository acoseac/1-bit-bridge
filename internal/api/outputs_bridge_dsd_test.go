package api

import (
	"context"
	"encoding/json"
	"testing"
)

func TestBridgeDSDPlaybackWireStates(t *testing.T) {
	for _, state := range []string{"open", "released", "revoked"} {
		t.Run(state, func(t *testing.T) {
			f := &fakeBridgeOutput{enabled: true, facts: BridgeOutputFacts{Listed: true, UUID: testPlayerUUID, Codecs: []string{"FLAC"}, Playback: &OutputPlayback{BootID: "boot", PolicyGeneration: 8, Occurrence: 11, State: state, Format: "DSD_U32_BE", RateHz: 88200, LogicalRate: 2822400, Channels: 2, ValidBits: 1}, DSDPlayback: &OutputDSDPlayback{BootID: "boot", PolicyGeneration: 9, Tuples: []OutputDSDTuple{}}}}
			s := &Server{bridgeOutput: f}
			element, ok := s.bridgeElement(context.Background())
			if !ok {
				t.Fatal("bridge absent")
			}
			b, e := json.Marshal(element)
			if e != nil {
				t.Fatal(e)
			}
			var wire map[string]any
			if e = json.Unmarshal(b, &wire); e != nil {
				t.Fatal(e)
			}
			bridge := wire["bridge"].(map[string]any)
			live := bridge["playback"].(map[string]any)
			cap := bridge["dsdPlayback"].(map[string]any)
			if live["state"] != state || live["occurrence"] != float64(11) || live["logicalRate"] != float64(2822400) || live["policyGeneration"] != float64(8) || cap["policyGeneration"] != float64(9) {
				t.Fatal("live observation confused with capability epoch")
			}
		})
	}
}
