package mpd

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MaxLineBytes is the longest line a reply may contain. MPD's own
// replies are a key, a colon and a short value; a peer that sends more
// is not a reply.
const MaxLineBytes = 8 << 10

// MaxReplyBytes is the most one command's reply may contain, greeting
// aside. The commands list is the longest of the replies this client
// asks for, and it is a couple of kilobytes.
const MaxReplyBytes = 256 << 10

// Pair is one "key: value" line of a reply, in the order MPD sent it.
// outputs repeats outputid, so a map would drop lines.
type Pair struct {
	Key   string
	Value string
}

// Reply is one successful command's body: the lines before the OK.
// Pairs is never nil.
type Reply struct {
	Pairs []Pair
}

// Get returns the first value of key.
func (r Reply) Get(key string) (string, bool) {
	for _, p := range r.Pairs {
		if p.Key == key {
			return p.Value, true
		}
	}
	return "", false
}

// ACKError is MPD's failure line, `ACK [code@index] {command} message`.
// Code 4 is the permission failure config returns to a TCP client.
type ACKError struct {
	Code    int
	Index   int
	Command string
	Message string
}

// Error is the line MPD sent, without whatever argument this client
// wrote. The command field is the command name MPD echoed, which for a
// rejected password is "password" and not the password.
func (e *ACKError) Error() string {
	return fmt.Sprintf("mpd: ACK [%d@%d] {%s} %s", e.Code, e.Index, e.Command, e.Message)
}

// ACKPermission is the code MPD returns when a command is not allowed
// to this client. config over TCP is this code.
const ACKPermission = 4

// ACKPassword is the code MPD returns for a wrong password.
const ACKPassword = 3

var ackLine = regexp.MustCompile(`^ACK \[(\d+)@(\d+)\] \{([^}]*)\} ?(.*)$`)

// ParseGreeting reads the line a server sends on connect, `OK MPD 0.24.0`.
func ParseGreeting(line string) (string, error) {
	line = strings.TrimSpace(strings.TrimRight(line, "\r"))
	const prefix = "OK MPD "
	if !strings.HasPrefix(line, prefix) {
		return "", errors.New("mpd: greeting is not OK MPD")
	}
	version := strings.TrimSpace(line[len(prefix):])
	if version == "" || strings.ContainsAny(version, " \t") {
		return "", errors.New("mpd: greeting has no version")
	}
	return version, nil
}

// ParseReply reads one command's reply, lines of `key: value` and a
// closing OK, or an ACK. An ACK is returned as an *ACKError; any pairs
// before it are still on the Reply.
func ParseReply(text string) (Reply, error) {
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return Reply{}, errors.New("mpd: empty reply")
	}
	lines := strings.Split(text, "\n")
	last := strings.TrimSuffix(lines[len(lines)-1], "\r")
	pairs := make([]Pair, 0, len(lines)-1)
	for _, line := range lines[:len(lines)-1] {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			return Reply{}, errors.New("mpd: empty line in reply")
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Reply{}, errors.New("mpd: reply line has no key")
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return Reply{}, errors.New("mpd: empty key")
		}
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		pairs = append(pairs, Pair{Key: key, Value: value})
	}
	switch {
	case strings.TrimSpace(last) == "OK":
		return Reply{Pairs: pairs}, nil
	case strings.HasPrefix(last, "ACK "):
		ack, err := parseACK(last)
		if err != nil {
			return Reply{}, err
		}
		return Reply{Pairs: pairs}, ack
	default:
		return Reply{}, errors.New("mpd: reply does not end with OK or ACK")
	}
}

func parseACK(line string) (*ACKError, error) {
	m := ackLine.FindStringSubmatch(line)
	if m == nil {
		return nil, errors.New("mpd: malformed ACK")
	}
	code, err := strconv.Atoi(m[1])
	if err != nil {
		return nil, errors.New("mpd: malformed ACK")
	}
	index, err := strconv.Atoi(m[2])
	if err != nil {
		return nil, errors.New("mpd: malformed ACK")
	}
	return &ACKError{Code: code, Index: index, Command: m[3], Message: m[4]}, nil
}

// ServerOutput is one output from the outputs command. Attributes are
// the `attribute: key=value` lines MPD appends (dop, allowed_formats).
type ServerOutput struct {
	ID         int
	Name       string
	Plugin     string
	Enabled    bool
	Attributes []Pair
}

// ServerOutputs groups an outputs reply into one value per outputid.
func ServerOutputs(r Reply) []ServerOutput {
	var out []ServerOutput
	var cur *ServerOutput
	for _, p := range r.Pairs {
		if p.Key == "outputid" {
			out = append(out, ServerOutput{})
			cur = &out[len(out)-1]
			if n, err := strconv.Atoi(p.Value); err == nil {
				cur.ID = n
			}
			continue
		}
		if cur == nil {
			continue
		}
		switch p.Key {
		case "outputname":
			cur.Name = p.Value
		case "plugin":
			cur.Plugin = p.Value
		case "outputenabled":
			cur.Enabled = p.Value == "1"
		case "attribute":
			k, v, _ := strings.Cut(p.Value, "=")
			cur.Attributes = append(cur.Attributes, Pair{Key: k, Value: v})
		}
	}
	if out == nil {
		return []ServerOutput{}
	}
	return out
}

// Attribute returns the first attribute value of key.
func (o ServerOutput) Attribute(key string) (string, bool) {
	for _, a := range o.Attributes {
		if a.Key == key {
			return a.Value, true
		}
	}
	return "", false
}
