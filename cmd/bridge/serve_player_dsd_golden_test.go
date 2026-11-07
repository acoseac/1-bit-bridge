package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/api"
	"github.com/acoseac/1-bit-bridge/internal/config"
	"github.com/acoseac/1-bit-bridge/internal/player/dacprofile"
	"github.com/acoseac/1-bit-bridge/internal/player/dsdstate"
	"github.com/acoseac/1-bit-bridge/internal/player/format"
	"github.com/acoseac/1-bit-bridge/internal/player/playback"
	"github.com/acoseac/1-bit-bridge/internal/player/playback/playbacktest"
)

// The whole GET /v1/outputs response for each kind of DAC the read side of
// the player's DSD describes (B728 B8): one golden a kind, from the real
// backend's published set through serve's view (bridgeOutput) and the API's
// route, and the iOS mirror's fixtures of them (BRIDGE_EXPORT_IOS_FIXTURES),
// exported after the manifest's and the API's exporters, sequentially
// (-p=1). The element's state is its engine's, which these cases do not run:
// each says the state its kind stands for. The backend's boot ID is random
// and stands as golden-boot; nothing else is normalized.

// dsdGoldenDir holds the goldens, one file a kind.
const dsdGoldenDir = "testdata/dsd-outputs"

// writeDSDGoldens, set in the environment, has the golden test write the
// goldens rather than compare them, for a change that means to move them.
const writeDSDGoldens = "BRIDGE_WRITE_DSD_GOLDENS"

// dsdGoldenCase is one kind of DAC: its name, and the element's facts.
type dsdGoldenCase struct {
	name  string
	facts api.BridgeOutputFacts
}

// dsdGoldenCases are the kinds: a DAC the database knows, playing native
// DSD at every rate, its evidence from each rung but driver acceptance; an
// unknown DAC, converting every rate; a known DAC whose explicit DoP waits
// for its first use and its confirmation; DSD migrated off; no DAC
// available, the state damaged; and a converted occurrence playing.
func dsdGoldenCases(t *testing.T) []dsdGoldenCase {
	t.Helper()
	known := playbacktest.KnownProfiles(nil)
	known.Profiles[0].Caveats = []dacprofile.Caveat{{Code: dacprofile.CaveatTransitionClicks, Transport: "dop", Note: "This DAC clicks at a transition into DSD128 or DSD256."}}
	withKnown := func(p *playback.DSDPolicy) { p.Profiles = known }
	noDAC := func(p *playback.DSDPolicy) {
		p.Bound = func() (playback.BoundDAC, bool) { return playback.BoundDAC{}, false }
	}

	nativeState := playbacktest.ChoiceState(dsdstate.ModeAuto)
	nativeState.DACs[0].FirstUse[0].ObservedAt = playbacktest.T0
	nativeState.DACs[0].Evidence = []dsdstate.Evidence{{Transport: dsdstate.TransportNative, Format: string(format.DSDU32BE), BitRate: playbacktest.DSD64,
		BCDDevice: playbacktest.DACBCD, StreamingObservedAt: playbacktest.T0}}
	nativeState.DACs[0].Listener = []dsdstate.Answer{{Transport: dsdstate.TransportNative, BitRate: playbacktest.DSD128, BCDDevice: playbacktest.DACBCD,
		Answer: dsdstate.AnswerClean, On: "2026-11-05"}}

	dopState := dsdstate.Defaults()
	dopState.DACs = []dsdstate.DAC{{Key: playbacktest.DACKey(), Choice: &dsdstate.Choice{Mode: dsdstate.ModeDoP, Revision: 2, SetAt: playbacktest.T0, SetBy: dsdstate.ByApp}}}

	off, _, err := dsdstate.Migrate(dsdstate.Defaults(), dsdstate.Input{Legacy: dsdstate.Legacy{DSDMode: dsdstate.ModeOff}, Now: playbacktest.T0})
	if err != nil {
		t.Fatal(err)
	}

	type kind struct {
		name, state, reason string
		st                  dsdstate.State
		edit                func(*playback.DSDPolicy)
		playing             bool
	}
	var out []dsdGoldenCase
	for i, k := range []kind{
		{name: "known_native", state: bridgeStateStopped, st: nativeState, edit: withKnown},
		{name: "unknown_converted", state: bridgeStateStopped, st: dsdstate.Defaults()},
		{name: "dop_pending", state: bridgeStateStopped, st: dopState, edit: withKnown},
		{name: "off", state: bridgeStateStopped, st: off},
		{name: "no_dac_damaged_state", state: bridgeStateUnavailable, reason: "device_missing", st: dsdstate.Refusing(playbacktest.T0), edit: noDAC},
		{name: "converted_playing", state: bridgeStatePlaying, st: dsdstate.Defaults(), playing: true},
	} {
		var edits []func(*playback.DSDPolicy)
		if k.edit != nil {
			edits = append(edits, k.edit)
		}
		b := readSideBackend(t, playbacktest.NewBox(5), playbacktest.NewMemStore(k.st), edits...)
		f := readSideFacts(t, b, config.PlayerDevice{})
		if k.playing {
			policy, _ := b.Facts()
			live := convertedObservation(t, policy, playbacktest.DSD128, 3)
			_, _, f.Playback = bridgeDSDFacts(policy, &live, config.PlayerDevice{})
		}
		f.UUID = fmt.Sprintf("7d444840-9dc0-11d1-b245-5ffdce74fb%02d", i+1)
		f.State, f.Reason = k.state, k.reason
		if f.DSDPlayback != nil {
			f.DSDPlayback.BootID = "golden-boot"
		}
		if f.Playback != nil {
			f.Playback.BootID = "golden-boot"
		}
		out = append(out, dsdGoldenCase{name: k.name, facts: f})
	}
	return out
}

// indentedJSON is body indented as the goldens hold it, a newline after it.
func indentedJSON(t *testing.T, body []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Indent(&b, bytes.TrimSpace(body), "", "  "); err != nil {
		t.Fatal(err)
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// hostLike is an IPv4 address, a URL, a .local name or a host and its port,
// none of which a fixture may hold.
var hostLike = regexp.MustCompile(`\d+\.\d+\.\d+\.\d+|://|\.local\b|^[a-z0-9.-]+:\d{2,5}$`)

// TestEachKindOfDACsWholeResponseIsItsGolden pins GET /v1/outputs' whole
// response for each kind of DAC (B728 B8's evidence): the same bytes as the
// golden, built twice alike, with no host, no serial and no port in it.
func TestEachKindOfDACsWholeResponseIsItsGolden(t *testing.T) {
	first, second := dsdGoldenCases(t), dsdGoldenCases(t)
	for i, c := range first {
		t.Run(c.name, func(t *testing.T) {
			body := indentedJSON(t, outputsBodyOf(t, c.facts))
			if again := indentedJSON(t, outputsBodyOf(t, second[i].facts)); !bytes.Equal(body, again) {
				t.Fatalf("the response differs between two builds:\n%s\n%s", body, again)
			}
			for _, s := range stringsIn(t, body) {
				if strings.Contains(s, "TESTDAC-0001") || strings.Contains(s, "usb2-1") || hostLike.MatchString(s) {
					t.Errorf("the response holds %q: no serial, port or host goes in a fixture", s)
				}
			}
			path := filepath.Join(dsdGoldenDir, c.name+".json")
			if os.Getenv(writeDSDGoldens) != "" {
				if err := os.WriteFile(path, body, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("the golden: %v (set %s=1 to write it)", err, writeDSDGoldens)
			}
			if !bytes.Equal(body, want) {
				t.Errorf("GET /v1/outputs for %s is not its golden %s:\n got %s\nwant %s", c.name, path, body, want)
			}
		})
	}
}

// TestExportIOSBridgeDSDFixtures collects the kinds into one OutputsResponse
// fixture for the iOS mirror, each element from the real route, and records
// each kind's facts beside it. Publication alone waits on
// BRIDGE_EXPORT_IOS_FIXTURES, after the manifest's and the API's exporters
// (-p=1), whose fixtures-info.json it adds its records to.
func TestExportIOSBridgeDSDFixtures(t *testing.T) {
	document := api.OutputsResponse{Outputs: []api.OutputInfo{}}
	records := []map[string]any{}
	for i, c := range dsdGoldenCases(t) {
		body := outputsBodyOf(t, c.facts)
		var rendered api.OutputsResponse
		if err := json.Unmarshal(body, &rendered); err != nil || len(rendered.Outputs) != 1 {
			t.Fatalf("%s: %s (%v)", c.name, body, err)
		}
		document.Outputs = append(document.Outputs, rendered.Outputs...)
		input, err := json.Marshal(c.facts)
		if err != nil {
			t.Fatal(err)
		}
		records = append(records, map[string]any{"case": c.name, "document": "dsd_dac_outputs.json", "outputIndex": i, "golden": "cmd/bridge/" + dsdGoldenDir + "/" + c.name + ".json",
			"inputKind": "BridgeOutputFacts JSON from the real backend's published set", "inputSHA256": fmt.Sprintf("%x", sha256.Sum256(input)), "input": json.RawMessage(input)})
	}
	b, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// The records are built whether or not they are published.
	metadata, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("BRIDGE_EXPORT_IOS_FIXTURES")
	if dir == "" {
		return
	}
	path := filepath.Join(dir, "fixtures-info.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("run the manifest's and the API's exporters first, with -p=1: %v", err)
	}
	var info map[string]json.RawMessage
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	info["dacRecords"] = metadata
	data, err = json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dsd_dac_outputs.json"), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestIOSBridgeDSDFixturesWithoutProvenance: a source archive's run, with no
// export directory, no Git and no HEAD, still builds every kind through the
// real route.
func TestIOSBridgeDSDFixturesWithoutProvenance(t *testing.T) {
	t.Setenv("BRIDGE_EXPORT_IOS_FIXTURES", "")
	t.Setenv("BRIDGE_FIXTURE_COMMIT", "")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "missing.git"))
	t.Setenv("PATH", t.TempDir())
	TestExportIOSBridgeDSDFixtures(t)
}
