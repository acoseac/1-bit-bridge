package mpd

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

// DefaultTimeout is the deadline one command runs under when the
// caller's context has none, or a later one. A dead daemon must not
// hold bridge output player open.
const DefaultTimeout = 5 * time.Second

// allowedCommands is the only set exchange will write. password is
// here so Dial can authenticate; it is not a method, and the argument
// is the one value the client ever sends. Every other command takes
// none. A name that changes playback, the queue or the database is
// absent on purpose.
var allowedCommands = map[string]struct{}{
	"status":             {},
	"stats":              {},
	"outputs":            {},
	"replay_gain_status": {},
	"currentsong":        {},
	"config":             {},
	"commands":           {},
	"password":           {},
}

// Client is one connection to an MPD. It holds no password: Dial sends
// one, if the caller passed one, and then drops it.
type Client struct {
	conn    net.Conn
	br      *bufio.Reader
	version string
}

// Dial connects to endpoint, reads the greeting, and authenticates when
// password is not empty. endpoint is "host:port", an absolute unix-socket
// path, or an abstract name starting with @. A path that still starts
// with '~' or '$', or a relative path, is refused before the dial: the
// caller expands those (resolveDial) and a relative socket would be a
// file in the working directory. The password is not stored on the
// Client. On any error the connection is closed and the Client is not
// returned, so the bufio.Reader that may hold an ACK echoing the
// password does not outlive the call. The error does not contain the
// password.
func Dial(ctx context.Context, endpoint, password string) (*Client, error) {
	network, err := dialNetwork(endpoint)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, network, endpoint)
	if err != nil {
		return nil, errors.New("mpd: dial " + endpoint + ": " + err.Error())
	}
	c := &Client{conn: conn, br: bufio.NewReaderSize(conn, MaxLineBytes)}
	done := c.arm(ctx)
	line, err := c.readLine()
	done()
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	version, err := ParseGreeting(line)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	c.version = version
	if password != "" {
		_, err = c.exchange(ctx, "password", password)
		if err != nil {
			_ = c.Close()
			return nil, redact(err, password)
		}
	}
	return c, nil
}

// Version is the version from the greeting, such as "0.24.0".
func (c *Client) Version() string {
	if c == nil {
		return ""
	}
	return c.version
}

// Close closes the connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// Status reads playback status.
func (c *Client) Status(ctx context.Context) (Reply, error) {
	return c.exchange(ctx, "status", "")
}

// Stats reads the daemon's counters.
func (c *Client) Stats(ctx context.Context) (Reply, error) {
	return c.exchange(ctx, "stats", "")
}

// Outputs reads the configured outputs.
func (c *Client) Outputs(ctx context.Context) (Reply, error) {
	return c.exchange(ctx, "outputs", "")
}

// ReplayGainStatus reads the replay-gain mode in effect.
func (c *Client) ReplayGainStatus(ctx context.Context) (Reply, error) {
	return c.exchange(ctx, "replay_gain_status", "")
}

// CurrentSong reads the song MPD is on.
func (c *Client) CurrentSong(ctx context.Context) (Reply, error) {
	return c.exchange(ctx, "currentsong", "")
}

// Config reads the configuration. Over a TCP connection MPD refuses
// this with an *ACKError of code ACKPermission; a unix socket is the
// connection it allows.
func (c *Client) Config(ctx context.Context) (Reply, error) {
	return c.exchange(ctx, "config", "")
}

// Commands reads the commands this connection may send.
func (c *Client) Commands(ctx context.Context) (Reply, error) {
	return c.exchange(ctx, "commands", "")
}

// exchange is the only place a command is written. The name is checked
// against allowedCommands before the line is built, so a refused
// command's argument is never written and never appears in the error.
func (c *Client) exchange(ctx context.Context, name, arg string) (Reply, error) {
	line, err := commandLine(name, arg)
	if err != nil {
		return Reply{}, err
	}
	defer zero(line)
	done := c.arm(ctx)
	defer done()
	if _, err := c.conn.Write(line); err != nil {
		return Reply{}, wrapIO(err)
	}
	return c.readReply()
}

func commandLine(name, arg string) ([]byte, error) {
	if _, ok := allowedCommands[name]; !ok || (arg != "" && name != "password") {
		return nil, errors.New("mpd: command refused")
	}
	if name == "password" {
		if strings.ContainsAny(arg, "\r\n\x00") {
			return nil, errors.New("mpd: password contains a line break")
		}
		b := make([]byte, 0, len("password ")+len(arg)+3)
		b = append(b, "password "...)
		b = appendQuoted(b, arg)
		b = append(b, '\n')
		return b, nil
	}
	return append([]byte(name), '\n'), nil
}

func appendQuoted(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\', '"':
			b = append(b, '\\', s[i])
		default:
			b = append(b, s[i])
		}
	}
	return append(b, '"')
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func redact(err error, secret string) error {
	if err == nil || secret == "" {
		return err
	}
	var ack *ACKError
	if errors.As(err, &ack) {
		if strings.Contains(ack.Message, secret) || strings.Contains(ack.Command, secret) {
			cp := *ack
			if strings.Contains(cp.Message, secret) {
				cp.Message = "[redacted]"
			}
			if strings.Contains(cp.Command, secret) {
				cp.Command = "[redacted]"
			}
			return &cp
		}
	}
	msg := err.Error()
	if strings.Contains(msg, secret) {
		return errors.New(strings.ReplaceAll(msg, secret, "[redacted]"))
	}
	return err
}

// arm sets one deadline for the command: the sooner of DefaultTimeout
// and the context's deadline. It is not re-armed per line, so a server
// that dribbles bytes cannot refresh it. Cancelling the context expires
// the deadline immediately.
func (c *Client) arm(ctx context.Context) func() {
	if ctx == nil {
		ctx = context.Background()
	}
	deadline := time.Now().Add(DefaultTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() {
		_ = c.conn.SetDeadline(time.Now())
	})
	return func() {
		stop()
		_ = c.conn.SetDeadline(time.Time{})
	}
}

func (c *Client) readLine() (string, error) {
	return readLineFrom(c.br)
}

func readLineFrom(r io.ByteReader) (string, error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", wrapIO(err)
		}
		if b == '\n' {
			if len(buf) > 0 && buf[len(buf)-1] == '\r' {
				buf = buf[:len(buf)-1]
			}
			return string(buf), nil
		}
		if len(buf) >= MaxLineBytes {
			return "", errors.New("mpd: line exceeds the length cap")
		}
		buf = append(buf, b)
	}
}

func (c *Client) readReply() (Reply, error) {
	text, err := readReplyText(c.br)
	if err != nil {
		return Reply{}, err
	}
	return ParseReply(text)
}

// readReplyText reads one reply the way ParseReply reads it. A
// `binary: N` line consumes the next N bytes and the newline after
// them before the next line is parsed, so a payload containing
// `\nOK\n` is not the end of the reply. A binary line that is not that
// form is the same framing error ParseReply returns.
func readReplyText(r *bufio.Reader) (string, error) {
	var b strings.Builder
	for {
		line, err := readLineFrom(r)
		if err != nil {
			return "", err
		}
		if b.Len()+len(line)+1 > MaxReplyBytes {
			return "", errors.New("mpd: reply exceeds the size cap")
		}
		n, isBinary, err := binaryHeader(line)
		if err != nil {
			return "", err
		}
		if isBinary {
			if n > MaxReplyBytes-b.Len()-len(line)-2 {
				return "", errors.New("mpd: reply exceeds the size cap")
			}
			payload := make([]byte, n)
			if _, err := io.ReadFull(r, payload); err != nil {
				return "", wrapIO(err)
			}
			nl, err := r.ReadByte()
			if err != nil {
				return "", wrapIO(err)
			}
			if nl != '\n' {
				return "", errors.New("mpd: binary chunk is not terminated")
			}
			b.WriteString(line)
			b.WriteByte('\n')
			b.Write(payload)
			b.WriteByte('\n')
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
		// The same lines ParseReply accepts. A malformed ACK or a line
		// with no key is an error here, not a wait for a later OK: the
		// two readers have to agree, and a deadline is the wrong answer
		// to a line that is already not a reply.
		if line == "OK" {
			return b.String(), nil
		}
		if strings.HasPrefix(line, "ACK ") {
			if _, err := parseACK(line); err != nil {
				return "", err
			}
			return b.String(), nil
		}
		if line == "" || strings.TrimSpace(line) == "" {
			return "", errors.New("mpd: empty line in reply")
		}
		key, _, found := strings.Cut(line, ":")
		if !found || key == "" || strings.ContainsAny(key, " \t") {
			return "", errors.New("mpd: reply line has no key")
		}
	}
}

func wrapIO(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return errors.New("mpd: connection closed")
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errors.New("mpd: deadline exceeded")
	}
	return errors.New("mpd: " + err.Error())
}

// dialNetwork classifies an endpoint that is ready to dial. '~' and '$'
// are still unexpanded, and a relative path is not a socket this client
// will open in the working directory. Both are errors, not a TCP dial.
func dialNetwork(endpoint string) (string, error) {
	if endpoint == "" {
		return "", errors.New("mpd: empty endpoint")
	}
	switch endpoint[0] {
	case '/':
		return "unix", nil
	case '@':
		return "unix", nil
	case '~', '$':
		return "", errors.New("mpd: endpoint was not expanded")
	}
	if strings.Contains(endpoint, "/") {
		return "", errors.New("mpd: relative socket path")
	}
	return "tcp", nil
}
