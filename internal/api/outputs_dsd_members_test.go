package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/acoseac/1-bit-bridge/internal/config"
)

// Compare raw HTTP objects with the member table, including zero values that a
// round trip through the emitting DTO would silently restore after omission.
func TestDSDOutputProtocolMembers(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	auth := outputsToken(t)
	token, _, err := auth.Mint("member contract")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeBridgeOutput{enabled: true}
	handler := New(&config.Config{}, auth, nil, "fixture").WithBridgeOutput(fake).Handler()
	for _, name := range []string{"absent", "empty", "zero_tuple", "populated"} {
		t.Run(name, func(t *testing.T) {
			f := listedPlayer()
			f.DSDPlayback, f.Playback = nil, nil
			if name != "absent" {
				f.DSDPlayback = &OutputDSDPlayback{Tuples: []OutputDSDTuple{}}
				f.Playback = &OutputPlayback{}
			}
			if name == "zero_tuple" {
				f.DSDPlayback.Tuples = []OutputDSDTuple{{}}
			}
			if name == "populated" {
				f.DSDPlayback = &OutputDSDPlayback{BootID: "boot", PolicyGeneration: ^uint64(0), QualificationRevision: "revision", Reason: "reason", Tuples: []OutputDSDTuple{{Format: "DSD_U32_BE", BitRate: 2822400, Channels: 2, PolicyID: "native", Pause: "kernel", TailMS: 10, PrefillMS: 20}}}
				f.Playback = &OutputPlayback{BootID: "boot", PolicyGeneration: ^uint64(0), Session: ^uint64(0), Occurrence: ^uint64(0), State: "open", Format: "DSD_U32_BE", RateHz: 88200, LogicalRate: 2822400, Channels: 2, ValidBits: 1, PolicyID: "native"}
			}
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
			if name == "absent" {
				for _, key := range []string{"dsdPlayback", "playback"} {
					if _, present := bridge[key]; present {
						t.Errorf("%s must be absent without backend facts", key)
					}
				}
				return
			}
			cap := checkDSDProtocolMembers(t, string(doc), "`dsdPlayback`", bridge["dsdPlayback"], name == "populated")
			checkDSDProtocolMembers(t, string(doc), "`playback`", bridge["playback"], name == "populated")
			var tuples []json.RawMessage
			if err := json.Unmarshal(cap["tuples"], &tuples); err != nil || tuples == nil {
				t.Fatalf("tuples must be an array, including when empty: %s (%v)", cap["tuples"], err)
			}
			if len(tuples) != len(f.DSDPlayback.Tuples) {
				t.Fatalf("got %d tuples, want %d", len(tuples), len(f.DSDPlayback.Tuples))
			}
			for _, tuple := range tuples {
				checkDSDProtocolMembers(t, string(doc), "Each tuple", tuple, name == "populated")
			}
		})
	}
}

func checkDSDProtocolMembers(t *testing.T, doc, object string, raw json.RawMessage, populated bool) map[string]json.RawMessage {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil || got == nil {
		t.Fatalf("%s must be an object: %s (%v)", object, raw, err)
	}
	rows := 0
	want := map[string]bool{}
	for line := range strings.SplitSeq(doc, "\n") {
		if !strings.HasPrefix(line, "| "+object+" |") {
			continue
		}
		rows++
		columns := strings.Split(line, "|")
		if len(columns) != 5 {
			t.Fatalf("malformed member table row: %s", line)
		}
		for i, cell := range columns[2:4] {
			if strings.TrimSpace(cell) == "None" {
				continue
			}
			for member := range strings.SplitSeq(cell, ";") {
				m := regexp.MustCompile("^`([^`]+)`: (string|integer|array of tuple objects \\(never null, `\\[\\]` when empty\\))$").FindStringSubmatch(strings.TrimSpace(member))
				if m == nil || want[m[1]] {
					t.Fatalf("unrecognized or duplicate member: %s", member)
				}
				key, kind := m[1], m[2]
				want[key] = true
				value, present := got[key]
				required := i == 0 || populated
				if present != required {
					t.Errorf("%s.%s present=%t, want %t", object, key, present, required)
					continue
				}
				if !present {
					continue
				}
				valid := false
				switch kind {
				case "string":
					var v any
					if err := json.Unmarshal(value, &v); err == nil {
						_, valid = v.(string)
					}
				case "integer":
					valid = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`).Match(value)
				default:
					var v []json.RawMessage
					valid = json.Unmarshal(value, &v) == nil && v != nil
				}
				if !valid {
					t.Errorf("%s.%s = %s, want JSON %s", object, key, value, kind)
				}
			}
		}
	}
	if rows != 1 || len(want) == 0 {
		t.Fatalf("%s: expected one nonempty member row, got %d", object, rows)
	}
	for key := range got {
		if !want[key] {
			t.Errorf("%s.%s is not documented in the member table", object, key)
		}
	}
	return got
}
