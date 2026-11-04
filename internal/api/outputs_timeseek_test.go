package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestTheBridgeElementSaysWhatASeekTakes pins bridge.timeSeek (B674): the
// facts' words, pcm then dsd, as an object of the bridge element; omitted,
// key and all, when no fact says anything, as an older bridge's element is;
// a member omitted when its fact is empty, which a client reads as unknown;
// and the MPD element beside it carries no bridge object at all.
func TestTheBridgeElementSaysWhatASeekTakes(t *testing.T) {
	b := &fakeBridgeOutput{enabled: true}
	hs, token, _ := newBridgeOutputsHTTP(t, quietOutputs("linux", "on"), b)
	for _, tc := range []struct{ name, pcm, dsd, want string }{
		{"this bridge's", TimeSeekSupported, TimeSeekUnsupported, `{"pcm":"supported","dsd":"unsupported"}`},
		{"one member", TimeSeekSupported, "", `{"pcm":"supported"}`},
		{"no fact", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := listedPlayer()
			f.TimeSeekPCM, f.TimeSeekDSD = tc.pcm, tc.dsd
			b.set(f)
			_, raw := decodeOutputs(t, outputsGet(t, hs, token, "/v1/outputs"))
			els := rawElements(t, raw)
			if len(els) != 2 {
				t.Fatalf("%d elements, want MPD's and the bridge's: %s", len(els), raw)
			}
			if _, has := els[0]["bridge"]; has {
				t.Fatalf("the MPD element carries a bridge object: %s", raw)
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(els[1]["bridge"], &obj); err != nil {
				t.Fatal(err)
			}
			got, ok := obj["timeSeek"]
			if tc.want == "" {
				if ok {
					t.Errorf("no fact says what a seek takes, and bridge.timeSeek is %s: want it absent", got)
				}
				return
			}
			if !ok || string(got) != tc.want {
				t.Errorf("bridge.timeSeek %s (present %v), want %s", got, ok, tc.want)
			}
		})
	}
}

// TestTimeSeekIsInTheProtocol pins PROTOCOL.md's bridge.timeSeek: its word
// table lists exactly the words the element sends or names (TimeSeekWords),
// for both members, and the object this bridge sends is the one it shows.
func TestTimeSeekIsInTheProtocol(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "PROTOCOL.md"))
	if err != nil {
		t.Fatal(err)
	}
	const row = "| `pcm`, `dsd` | "
	rows := 0
	var words []string
	for line := range strings.SplitSeq(string(doc), "\n") {
		if strings.HasPrefix(line, row) {
			rows++
			for _, m := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(strings.TrimPrefix(line, row), -1) {
				words = append(words, m[1])
			}
		}
	}
	if rows != 1 || !slices.Equal(words, TimeSeekWords()) {
		t.Errorf("PROTOCOL.md's timeSeek words: %q in %d rows, want %q in one", words, rows, TimeSeekWords())
	}
	sent, err := json.Marshal(OutputTimeSeek{PCM: TimeSeekSupported, DSD: TimeSeekUnsupported})
	if err != nil {
		t.Fatal(err)
	}
	if string(sent) != `{"pcm":"supported","dsd":"unsupported"}` || !strings.Contains(string(doc), `"timeSeek": { "pcm": "supported", "dsd": "unsupported" }`) {
		t.Errorf("PROTOCOL.md does not show the object this bridge sends, %s", sent)
	}
}
