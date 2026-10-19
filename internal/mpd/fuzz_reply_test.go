package mpd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

// FuzzReadReplyMatchesParseReply holds the client loop to the same
// framing ParseReply accepts. A text the reader treats as one complete
// reply parses, twice, as that reply. A binary payload is part of that
// text, so an OK inside the payload is not a second reply.
func FuzzReadReplyMatchesParseReply(f *testing.F) {
	f.Add([]byte("OK\n"))
	f.Add([]byte("key: value\nOK\n"))
	f.Add([]byte("binary: 4\n\nOK\nkey: kept\nOK\n"))
	f.Add([]byte("ACK [2@0] {play} bad\n"))
	f.Add([]byte("  OK  \n"))
	f.Add([]byte("OK \n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxReplyBytes {
			data = data[:MaxReplyBytes]
		}
		text, err := readReplyText(bufio.NewReader(bytes.NewReader(data)))
		if err != nil {
			return
		}
		a, errA := ParseReply(text)
		b, errB := ParseReply(text)
		// An ACK is a complete reply: ParseReply returns *ACKError and
		// the pairs that preceded it. Any other error means the reader
		// accepted text the parser rejects.
		if !replyFramed(errA) || !replyFramed(errB) || len(a.Pairs) != len(b.Pairs) {
			t.Fatalf("framed reply %q did not parse: %v %v", text, errA, errB)
		}
		for i := range a.Pairs {
			if a.Pairs[i] != b.Pairs[i] {
				t.Fatalf("framed reply is not deterministic")
			}
		}
		if fmt.Sprint(errA) != fmt.Sprint(errB) {
			t.Fatalf("framed reply is not deterministic: %v %v", errA, errB)
		}
	})
}

// replyFramed is true when ParseReply accepted the text: a reply that
// ends in OK, or an ACK, which is a complete reply that carries an error.
func replyFramed(err error) bool {
	if err == nil {
		return true
	}
	var ack *ACKError
	return errors.As(err, &ack)
}
