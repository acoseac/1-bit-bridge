package mpd

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// FuzzParseReply holds the reply parser to a peer it does not trust: no
// panic, two parses that agree, and every key and value text the reply
// itself holds. The nightly job finds this by name.
func FuzzParseReply(f *testing.F) {
	b, err := os.ReadFile("testdata/volumio-mpd-replies.json")
	if err != nil {
		f.Fatal(err)
	}
	var fx replyFixture
	if err := json.Unmarshal(b, &fx); err != nil {
		f.Fatal(err)
	}
	for _, body := range []string{
		fx.Greeting, fx.Status, fx.Stats, fx.Outputs, fx.ReplayGainStatus,
		fx.Config, fx.CurrentSong, fx.Commands,
		"OK\n", "ACK [4@0] {config} no\n", "key: value\nOK\n",
	} {
		f.Add(body)
	}
	f.Fuzz(func(t *testing.T, text string) {
		a, errA := ParseReply(text)
		b, errB := ParseReply(text)
		if (errA == nil) != (errB == nil) || len(a.Pairs) != len(b.Pairs) {
			t.Fatalf("parse is not deterministic")
		}
		for i := range a.Pairs {
			if a.Pairs[i] != b.Pairs[i] {
				t.Fatalf("parse is not deterministic")
			}
			if !strings.Contains(text, a.Pairs[i].Key) || !strings.Contains(text, a.Pairs[i].Value) {
				t.Fatalf("pair %q %q is not in the reply", a.Pairs[i].Key, a.Pairs[i].Value)
			}
		}
		if errA != nil && strings.Contains(errA.Error(), "\n") {
			t.Fatalf("an error spans a line: %q", errA.Error())
		}
	})
}
