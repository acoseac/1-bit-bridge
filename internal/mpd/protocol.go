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

// Response::Error writes the space after '}' unconditionally
// (src/client/Response.cxx). A line without it is not an ACK.
var ackLine = regexp.MustCompile(`^ACK \[(\d+)@(\d+)\] \{([^}]*)\} (.*)$`)

// ParseGreeting reads the line a server sends on connect. MPD 0.24.4
// writes exactly `OK MPD ` + PROTOCOL_VERSION + `\n` (src/client/New.cxx,
// GREETING). A space before OK, or more than one space before the
// version, is not that line.
func ParseGreeting(line string) (string, error) {
	line = strings.TrimSuffix(line, "\r")
	const prefix = "OK MPD "
	if !strings.HasPrefix(line, prefix) {
		return "", errors.New("mpd: greeting is not OK MPD")
	}
	version := line[len(prefix):]
	if version == "" || strings.ContainsAny(version, " \t") {
		return "", errors.New("mpd: greeting has no version")
	}
	return version, nil
}

// ParseReply reads one command's reply, lines of `key: value` and a
// closing OK, or an ACK. An ACK is returned as an *ACKError; any pairs
// before it are still on the Reply. A `binary: N` line is followed by
// exactly N bytes and a newline (Response::WriteBinary); those bytes
// are not lines, so a payload that contains `\nOK\n` does not end the
// reply. A line whose key is binary but whose size is missing,
// malformed or over the cap is a framing error, not a pair. The success
// line is exactly `OK` (doc/protocol.rst, Responses).
// Padding is not an OK.
func ParseReply(text string) (Reply, error) {
	if text == "" {
		return Reply{}, errors.New("mpd: empty reply")
	}
	pairs := make([]Pair, 0)
	rest := text
	for {
		line, next, ok := cutProtoLine(rest)
		if !ok {
			return Reply{}, errors.New("mpd: reply does not end with OK or ACK")
		}
		if line == "OK" || strings.HasPrefix(line, "ACK ") {
			if next != "" {
				return Reply{}, errors.New("mpd: reply does not end with OK or ACK")
			}
			if line == "OK" {
				return Reply{Pairs: pairs}, nil
			}
			ack, err := parseACK(line)
			if err != nil {
				return Reply{}, err
			}
			return Reply{Pairs: pairs}, ack
		}
		n, isBinary, err := binaryHeader(line)
		if err != nil {
			return Reply{}, err
		}
		if isBinary {
			if len(next) < n+1 || next[n] != '\n' {
				return Reply{}, errors.New("mpd: binary chunk is not terminated")
			}
			rest = next[n+1:]
			continue
		}
		if line == "" || strings.TrimSpace(line) == "" {
			return Reply{}, errors.New("mpd: empty line in reply")
		}
		key, value, found := strings.Cut(line, ":")
		if !found || key == "" || strings.ContainsAny(key, " \t") {
			return Reply{}, errors.New("mpd: reply line has no key")
		}
		if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		pairs = append(pairs, Pair{Key: key, Value: value})
		rest = next
	}
}

// errBadBinarySize is a line whose key is binary but which is not
// `binary: N`. N is a decimal without a sign or a leading zero, and it
// fits in MaxReplyBytes. A missing size, `binary: -1`, `binary: 01`,
// `binary: abc` and a size over the cap are this error. The bytes after
// the line are not lines, so keeping the line as a pair would end the
// reply on an OK inside the payload.
var errBadBinarySize = errors.New("mpd: binary chunk has a bad size")

// binaryHeader classifies a reply line. A line whose key is binary
// (`binary:` with or without a value) is a chunk header. ok is true
// only for the form Response::WriteBinary writes. Any other binary
// header is errBadBinarySize, in ParseReply and in readReplyText.
func binaryHeader(line string) (n int, ok bool, err error) {
	if !strings.HasPrefix(line, "binary:") {
		return 0, false, nil
	}
	n, ok = binarySize(line)
	if !ok {
		return 0, false, errBadBinarySize
	}
	return n, true, nil
}

// binarySize reports N for a line that is exactly `binary: N`, the
// form Response::WriteBinary writes. N is a decimal without a sign or
// a leading zero, and it is at most MaxReplyBytes. A line that merely
// starts with `binary:` is not this form; binaryHeader reports that.
func binarySize(line string) (int, bool) {
	rest, ok := strings.CutPrefix(line, "binary: ")
	if !ok || rest == "" {
		return 0, false
	}
	if len(rest) > 1 && rest[0] == '0' {
		return 0, false
	}
	n := 0
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		d := int(c - '0')
		if n > (MaxReplyBytes-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}

func cutProtoLine(s string) (line, rest string, ok bool) {
	i := strings.IndexByte(s, '\n')
	if i < 0 {
		return "", "", false
	}
	line = s[:i]
	line = strings.TrimSuffix(line, "\r")
	return line, s[i+1:], true
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
