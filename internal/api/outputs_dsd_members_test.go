package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// memberRows are the objects of PROTOCOL.md's DSD member table, each by the
// label of its row, and where each sits in the bridge object: the path from
// it, an element of an array spelled "[]".
var memberRows = []struct{ label, path string }{
	{"`dsdPlayback`", "dsdPlayback"},
	{"Each tuple", "dsdPlayback.tuples[]"},
	{"Each converted entry", "dsdPlayback.converted[]"},
	{"Each refused rate", "dsdPlayback.refused[]"},
	{"Each warning", "dsdPlayback.warnings[]"},
	{"`playback`", "playback"},
	{"`playback.converted`", "playback.converted"},
	{"`dsdDAC`", "dsdDAC"},
	{"`dsdDAC.profile`", "dsdDAC.profile"},
	{"Each caveat", "dsdDAC.profile.caveats[]"},
	{"`dsdDAC.choice`", "dsdDAC.choice"},
	{"Each option", "dsdDAC.options[]"},
	{"Each option rate", "dsdDAC.options[].rates[]"},
	{"Each first use", "dsdDAC.firstUse[]"},
}

// populatedDSD is a player's element facts with every optional DSD member
// set, B728 B8's among them: a DoP tuple and its evidence, a converted rate
// and a refused one, two warnings, a converted occurrence's observation with
// every member an observation can carry, and the bound DAC with its profile,
// its choice, one option of each mode and both first uses.
func populatedDSD() BridgeOutputFacts {
	f := listedPlayer()
	observed := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	yes := true
	native := []OutputDSDOptionRate{{BitRate: 2822400, Evidence: "listener_confirmed", EvidenceOn: "2026-11-05"}, {BitRate: 5644800}}
	dop := []OutputDSDOptionRate{{BitRate: 2822400, Evidence: "driver_accepted"}}
	f.DSDPlayback = &OutputDSDPlayback{BootID: "boot", PolicyGeneration: ^uint64(0), Reason: "reason", Model: "iFi GO bar", BCDDevice: "0123",
		Tuples:    []OutputDSDTuple{{Format: "S32_LE", BitRate: 2822400, Channels: 2, PolicyID: "dop64", Pause: "kernel", TailMS: 10, PrefillMS: 20, Transport: "dop", Evidence: "streaming_observed", EvidenceOn: "2026-11-05"}},
		Converted: []OutputDSDConverted{{BitRate: 11289600, RateHz: 176400, ValidBits: 24, PassbandHz: 30000, Dither: "none", Filter: "dsdconv-1"}},
		Refused:   []OutputDSDRefused{{BitRate: 5644800, Reason: "first_use_pending"}},
		Warnings:  []OutputDSDWarning{{Code: "migration_record_refused", Reason: "stale_version"}, {Code: "migrated_off"}}}
	f.Playback = &OutputPlayback{BootID: "boot", PolicyGeneration: ^uint64(0), Session: ^uint64(0), Occurrence: ^uint64(0), State: "open", Format: "S32_LE", RateHz: 176400, LogicalRate: 2822400, Channels: 2, ValidBits: 1, PolicyID: "dop64",
		Transport: "dop", PackingVersion: 1, ObservedAt: &observed, Evidence: "streaming_observed", EvidenceOn: "2026-11-05",
		Converted: &OutputPlaybackConverted{FromBitRate: 2822400, Filter: "dsdconv-1", PassbandHz: 30000, Dither: "none", GainDB: 0, ClippedSamples: 3}}
	f.DSDDAC = &OutputDSDDAC{DACKey: "d1-0123456789abcdef",
		Profile: &OutputDSDProfile{ID: "test.dac", Name: "Test DAC", Caveats: []OutputDSDCaveat{{Code: "transition_clicks", Transport: "dop", Note: "It clicks into DSD128."}}},
		Choice:  OutputDSDChoice{Mode: "auto", MaxBitRate: 5644800, Revision: 4, DoPConfirmed: true},
		Options: []OutputDSDOption{{Mode: "native", Offered: true, Rates: &native}, {Mode: "dop", Offered: true, Rates: &dop, NeedsConfirmation: &yes, Blocked: "dop_gain_not_unity"},
			{Mode: "convert", Offered: true, RateHz: 176400, ValidBits: 24, PassbandHz: 30000, Dither: "none"}, {Mode: "off", Offered: true}},
		FirstUse: []OutputDSDFirstUse{{Transport: "native", State: "observed", BitRates: []int{2822400}}, {Transport: "dop", State: "pending"}}}
	return f
}

// Compare raw HTTP objects with the member table, including zero values that a
// round trip through the emitting DTO would silently restore after omission.
// Each object of a row holds the row's always-present members, each of its
// documented JSON type, and no member the row does not name; in the populated
// element every member a row names is sent by some object of that row, the
// optional ones a mode or a transport has alone (an option's) included, so no
// row names a member nothing sends.
func TestDSDOutputProtocolMembers(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]memberRow{}
	for _, r := range memberRows {
		rows[r.label] = parseMemberRow(t, string(doc), r.label)
	}
	auth := outputsToken(t)
	token, _, err := auth.Mint("member contract")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeBridgeOutput{enabled: true}
	handler := New(&config.Config{}, auth, nil, "fixture").WithBridgeOutput(fake).Handler()
	bridgeOf := func(t *testing.T, f BridgeOutputFacts) map[string]json.RawMessage {
		t.Helper()
		fake.set(f)
		r := httptest.NewRequest(http.MethodGet, "/v1/outputs", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
		}
		elements := rawElements(t, w.Body.Bytes())
		if len(elements) != 1 {
			t.Fatalf("got %d outputs", len(elements))
		}
		var bridge map[string]json.RawMessage
		if err := json.Unmarshal(elements[0]["bridge"], &bridge); err != nil {
			t.Fatal(err)
		}
		return bridge
	}
	for _, name := range []string{"absent", "empty", "zero_tuple", "populated"} {
		t.Run(name, func(t *testing.T) {
			f := listedPlayer()
			switch name {
			case "empty":
				// Facts with no list filled: every array the element sends is
				// [] all the same.
				f.DSDPlayback, f.Playback, f.DSDDAC = &OutputDSDPlayback{}, &OutputPlayback{}, &OutputDSDDAC{}
			case "zero_tuple":
				f.DSDPlayback = &OutputDSDPlayback{Tuples: []OutputDSDTuple{{}}, Converted: []OutputDSDConverted{{}}, Refused: []OutputDSDRefused{{}}, Warnings: []OutputDSDWarning{{}}}
				f.Playback = &OutputPlayback{Converted: &OutputPlaybackConverted{}}
				f.DSDDAC = &OutputDSDDAC{Profile: &OutputDSDProfile{Caveats: []OutputDSDCaveat{{}}}, Options: []OutputDSDOption{{}}, FirstUse: []OutputDSDFirstUse{{}}}
			case "populated":
				f = populatedDSD()
			}
			bridge := bridgeOf(t, f)
			if name == "absent" {
				for _, key := range []string{"dsdPlayback", "playback", "dsdDAC"} {
					if _, present := bridge[key]; present {
						t.Errorf("%s must be absent without backend facts", key)
					}
				}
				return
			}
			for _, r := range memberRows {
				objects := objectsAt(t, bridge, r.path)
				row := rows[r.label]
				sent := map[string]bool{}
				for _, obj := range objects {
					for key := range obj {
						sent[key] = true
					}
					row.check(t, r.label, obj)
				}
				if name != "populated" {
					continue
				}
				if len(objects) == 0 {
					t.Errorf("%s: the populated element sends no such object", r.label)
				}
				for member := range row.kinds {
					if !sent[member] {
						t.Errorf("%s.%s is documented and the populated element never sends it", r.label, member)
					}
				}
			}
		})
	}

	// The members B728's B8 stopped sending (the design's section 7.1): no
	// record stands behind them since the cutover, so no field of the DTO
	// emits them, the populated element carries neither, and the member table
	// names neither in its row.
	populated := bridgeOf(t, populatedDSD())
	for _, rm := range []struct {
		label, path, member string
		dto                 reflect.Type
	}{
		{"`dsdPlayback`", "dsdPlayback", "qualificationRevision", reflect.TypeOf(OutputDSDPlayback{})},
		{"`dsdPlayback`", "dsdPlayback", "qualifiedOn", reflect.TypeOf(OutputDSDPlayback{})},
		{"`playback`", "playback", "qualificationRevision", reflect.TypeOf(OutputPlayback{})},
	} {
		t.Run("removed "+rm.path+"."+rm.member, func(t *testing.T) {
			for i := range rm.dto.NumField() {
				if tag, _, _ := strings.Cut(rm.dto.Field(i).Tag.Get("json"), ","); tag == rm.member {
					t.Errorf("%s still has a field %s that sends %s", rm.dto.Name(), rm.dto.Field(i).Name, rm.member)
				}
			}
			if _, documented := rows[rm.label].kinds[rm.member]; documented {
				t.Errorf("the member table's %s row still names %s", rm.label, rm.member)
			}
			for _, obj := range objectsAt(t, populated, rm.path) {
				if v, sent := obj[rm.member]; sent {
					t.Errorf("%s carries %s: %s", rm.path, rm.member, v)
				}
			}
		})
	}
}

// memberRow is one row of the member table: each member's JSON type, and
// whether it is always present.
type memberRow struct {
	kinds    map[string]string
	required map[string]bool
}

// memberKind is a member's documented JSON type.
var memberKind = regexp.MustCompile("^`([^`]+)`: (string|integer|number|boolean|object|array of integers|array of [a-z -]+ objects(?: \\(never null, `\\[\\]` when empty\\))?)$")

// parseMemberRow reads label's one row of doc's member table.
func parseMemberRow(t *testing.T, doc, label string) memberRow {
	t.Helper()
	row := memberRow{kinds: map[string]string{}, required: map[string]bool{}}
	found := 0
	for line := range strings.SplitSeq(doc, "\n") {
		if !strings.HasPrefix(line, "| "+label+" |") {
			continue
		}
		found++
		columns := strings.Split(line, "|")
		if len(columns) != 5 {
			t.Fatalf("malformed member table row: %s", line)
		}
		for i, cell := range columns[2:4] {
			if strings.TrimSpace(cell) == "None" {
				continue
			}
			for member := range strings.SplitSeq(cell, ";") {
				m := memberKind.FindStringSubmatch(strings.TrimSpace(member))
				if m == nil {
					t.Fatalf("%s: unrecognized member %q", label, member)
				}
				if _, dup := row.kinds[m[1]]; dup {
					t.Fatalf("%s: member %s documented twice", label, m[1])
				}
				row.kinds[m[1]], row.required[m[1]] = m[2], i == 0
			}
		}
	}
	if found != 1 || len(row.kinds) == 0 {
		t.Fatalf("%s: expected one nonempty member row, got %d", label, found)
	}
	return row
}

// check holds obj, an object of the row's, to it.
func (row memberRow) check(t *testing.T, label string, obj map[string]json.RawMessage) {
	t.Helper()
	for member, required := range row.required {
		if _, present := obj[member]; required && !present {
			t.Errorf("%s.%s is always present and missing: %v", label, member, keysOf(obj))
		}
	}
	for member, value := range obj {
		kind, documented := row.kinds[member]
		if !documented {
			t.Errorf("%s.%s is not documented in the member table", label, member)
			continue
		}
		if !jsonKind(value, kind) {
			t.Errorf("%s.%s = %s, want JSON %s", label, member, value, kind)
		}
	}
}

// jsonKind reports whether value is JSON of kind, as the table words it.
func jsonKind(value json.RawMessage, kind string) bool {
	integer := regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
	switch {
	case kind == "string":
		var v any
		if json.Unmarshal(value, &v) != nil {
			return false
		}
		_, ok := v.(string)
		return ok
	case kind == "integer":
		return integer.Match(value)
	case kind == "number":
		return regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`).Match(value)
	case kind == "boolean":
		return string(value) == "true" || string(value) == "false"
	case kind == "object":
		var v map[string]json.RawMessage
		return json.Unmarshal(value, &v) == nil && v != nil
	case kind == "array of integers":
		var v []json.RawMessage
		if json.Unmarshal(value, &v) != nil || v == nil {
			return false
		}
		return !slices.ContainsFunc(v, func(e json.RawMessage) bool { return !integer.Match(e) })
	default:
		var v []json.RawMessage
		if json.Unmarshal(value, &v) != nil || v == nil {
			return false
		}
		return !slices.ContainsFunc(v, func(e json.RawMessage) bool { return !jsonKind(e, "object") })
	}
}

// objectsAt are the objects at path in obj, an array's every element where
// a step ends in "[]"; none where a step is absent.
func objectsAt(t *testing.T, obj map[string]json.RawMessage, path string) []map[string]json.RawMessage {
	t.Helper()
	cur := []map[string]json.RawMessage{obj}
	for step := range strings.SplitSeq(path, ".") {
		name, each := strings.CutSuffix(step, "[]")
		var next []map[string]json.RawMessage
		for _, o := range cur {
			raw, ok := o[name]
			if !ok || string(raw) == "null" {
				continue
			}
			if each {
				var list []map[string]json.RawMessage
				if err := json.Unmarshal(raw, &list); err != nil {
					t.Fatalf("%s: %s is no array of objects: %s", path, name, raw)
				}
				next = append(next, list...)
				continue
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("%s: %s is no object: %s", path, name, raw)
			}
			next = append(next, m)
		}
		cur = next
	}
	return cur
}
